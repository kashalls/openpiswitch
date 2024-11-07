package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/kashalls/openterface-switch/internal/table"
	"github.com/kashalls/openterface-switch/internal/usb"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/tarm/serial"
	"github.com/vladimirvivien/go4vl/device"
	"golang.org/x/net/websocket"
)

type Openterface struct {
	ID          string
	Device      *device.Device
	SerialPort  *serial.Port
	Paths       usb.DevicePaths
	VideoFrames <-chan []byte
	AudioFrames <-chan []byte
	VideoTrack  *webrtc.TrackLocalStaticSample
	AudioTrack  *webrtc.TrackLocalStaticSample
	Width       int
	Height      int
	FPS         int
	StreamInfo  string
	ctx         context.Context
	cancel      context.CancelFunc
}

type DeviceManager struct {
	Devices map[string]*Openterface
	mu      sync.RWMutex
}

type WebsocketQuery struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

func NewDeviceManager() *DeviceManager {
	return &DeviceManager{
		Devices: make(map[string]*Openterface),
	}
}

// Additional method to open a serial port
func openSerialPort(devPath string, baudRate int) (*serial.Port, error) {
	config := &serial.Config{
		Name: devPath,
		Baud: baudRate,
	}
	return serial.OpenPort(config)
}

type PageData struct {
	Cameras []struct {
		ID          string
		StreamInfo  string
		StreamPath  string
		ImgWidth    int
		ImgHeight   int
		ControlPath string
	}
}

// Define signaling message types
type SignalingMessage struct {
	Count     int           `json:"count,omitempty"`
	Type      string        `json:"type"` // "offer", "answer", or "candidate", or "query"
	SDP       *string       `json:"sdp,omitempty"`
	Candidate *ICECandidate `json:"candidate,omitempty"`
}

// ICECandidate represents an ICE candidate message
type ICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid,omitempty"`
	SDPMLineIndex uint16 `json:"sdpMLineIndex,omitempty"`
}

func (dm *DeviceManager) InitDevice(paths usb.DevicePaths, fps int, bufferSize int) error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	serialPort, err := openSerialPort(paths.SerialPath, 115200) // Set baud rate as needed
	if err != nil {
		cancel()
		return fmt.Errorf("failed to open serial port %s: %v", paths.SerialPath, err)
	}

	deviceID := filepath.Base(paths.CameraPath)

	videoId := fmt.Sprintf("video-%s", deviceID)
	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", deviceID)
	if err != nil {
		log.Printf("Failed to create video track for device %s: %v", deviceID, err)
		cancel()
		return err
	}
	// pipelineForCodec("vp8", videoId, []*webrtc.TrackLocalStaticSample{videoTrack}, "videotestsrc")
	pipelineForCodec("vp8", videoId, []*webrtc.TrackLocalStaticSample{videoTrack}, fmt.Sprintf("v4l2src device=\"%s\"", paths.CameraPath))

	audioId := fmt.Sprintf("audio-%s", deviceID)
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "audio", deviceID)
	if err != nil {
		log.Printf("Failed to create audio track for device %s: %v", deviceID, err)
		cancel()
		return err
	}
	pipelineForCodec("opus", audioId, []*webrtc.TrackLocalStaticSample{audioTrack}, fmt.Sprintf("alsasrc device=\"hw:%s\"", paths.AudioPath[len(paths.AudioPath)-1:]))
	// pipelineForCodec("opus", audioId, []*webrtc.TrackLocalStaticSample{audioTrack}, "audiotestsrc")

	dm.Devices[deviceID] = &Openterface{
		ID: deviceID,
		// Device:      camera,
		SerialPort: serialPort,
		// VideoFrames: camera.GetOutput(),
		VideoTrack: videoTrack,
		AudioTrack: audioTrack,
		Paths:      paths,
		//Width:       int(currFmt.Width),
		//Height:      int(currFmt.Height),
		FPS: fps,
		//StreamInfo:  streamInfo,
		ctx:    ctx,
		cancel: cancel,
	}

	return nil
}

func (dm *DeviceManager) CloseAll() {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	for _, dev := range dm.Devices {
		dev.cancel()
		dev.Device.Close()
		if dev.SerialPort != nil {
			dev.SerialPort.Close()
		}
	}
}

// Additional method to write data to the serial adapter of a specific camera
func (dm *DeviceManager) WriteToSerial(deviceID, data string) error {

	device, exists := dm.Devices[deviceID]
	if !exists {
		return fmt.Errorf("device %s not found", deviceID)
	}

	_, err := device.SerialPort.Write([]byte(data))
	return err
}

func (dm *DeviceManager) ServePage(w http.ResponseWriter, r *http.Request) {
	pd := PageData{}
	for _, dev := range dm.Devices {
		camera := struct {
			ID          string
			StreamInfo  string
			StreamPath  string
			ImgWidth    int
			ImgHeight   int
			ControlPath string
		}{
			ID:          dev.ID,
			StreamInfo:  dev.StreamInfo,
			StreamPath:  fmt.Sprintf("/stream/%s?%d", dev.ID, time.Now().UnixNano()),
			ImgWidth:    dev.Width,
			ImgHeight:   dev.Height,
			ControlPath: fmt.Sprintf("/control/%s", dev.ID),
		}
		pd.Cameras = append(pd.Cameras, camera)
	}

	w.Header().Add("Content-Type", "text/html")
	t, err := template.ParseFiles("webcam.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := t.Execute(w, pd); err != nil {
		log.Printf("failed to execute template: %v", err)
	}
}

var (
	dm = NewDeviceManager()
)

func main() {
	defer dm.CloseAll()

	gst.Init(nil)

	var (
		port     = ":9090"
		width    = 1920
		height   = 1080
		fps      = 30
		buffSize = 1
	)

	results, errors := usb.FindUSBDevices()

	// Handle any errors
	for _, err := range errors {
		if hubErr, ok := err.(*usb.HubError); ok {
			fmt.Printf("Hub error: %v\n", hubErr)
		} else {
			fmt.Printf("General error: %v\n", err)
		}
	}

	flag.StringVar(&port, "p", port, "webcam service port")
	flag.IntVar(&width, "w", width, "capture width")
	flag.IntVar(&height, "h", height, "capture height")
	flag.IntVar(&fps, "r", fps, "frames per second")
	flag.IntVar(&buffSize, "b", buffSize, "device buffer size")
	flag.Parse()

	fmt.Printf("Found %d devices:\n", len(results))
	table.Generate(results)

	for _, paths := range results {
		if err := dm.InitDevice(paths, fps, buffSize); err != nil {
			log.Printf("Warning: failed to initialize camera %s - %s: %v", paths.CameraPath, paths.SerialPath, err)
			continue
		}
	}

	if len(dm.Devices) == 0 {
		log.Fatal("No cameras were successfully initialized")
	}

	http.HandleFunc("/", dm.ServePage)
	http.Handle("/ws", websocket.Handler(websocketHandler))

	log.Printf("Starting server on port %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

func websocketHandler(ws *websocket.Conn) {
	defer ws.Close()

	// Create a new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.cloudflare.com:3478"},
			},
		},
	})
	if err != nil {
		log.Println("Failed to create peer connection:", err)
		return
	}

	for _, dev := range dm.Devices {
		peerConnection.AddTrack(dev.VideoTrack)
		peerConnection.AddTrack(dev.AudioTrack)
	}

	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())
	})

	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnOpen(func() {
			fmt.Printf("[Data Channel Create] %s-%d ", d.Label(), d.ID())

		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("[Data Channel Message] '%s': '%s'\n", d.Label(), string(msg.Data))
		})
	})

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			if err := websocket.JSON.Send(ws, candidate.ToJSON()); err != nil {
				log.Println("Failed to send answer:", err)
			}
		}
	})

	// WebRTC signaling loop
	for {
		var msg SignalingMessage
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			log.Println("Failed to read message:", err)
			break
		}

		switch msg.Type {
		case "query":
			query := WebsocketQuery{
				Type:  "query",
				Count: len(dm.Devices),
			}
			if err := websocket.JSON.Send(ws, query); err != nil {
				log.Println("Failed to send query:", err)
			}
		case "offer":
			// Set the remote description first
			offer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  *msg.SDP,
			}

			if err := peerConnection.SetRemoteDescription(offer); err != nil {
				log.Println("Failed to set remote description:", err)
				break
			}

			// Create answer after setting remote description
			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				log.Println("Failed to create answer:", err)
				break
			}

			// Set local description
			if err := peerConnection.SetLocalDescription(answer); err != nil {
				log.Println("Failed to set local description:", err)
				break
			}

			if err := websocket.JSON.Send(ws, answer); err != nil {
				log.Println("Failed to send answer:", err)
			}
			log.Printf("Checking:\n%v\n", peerConnection.CurrentLocalDescription())

		case "candidate":
			// Add ICE candidate only if the remote description is set
			if peerConnection.RemoteDescription() != nil && msg.Candidate != nil {
				candidate := webrtc.ICECandidateInit{
					Candidate:     msg.Candidate.Candidate,
					SDPMid:        &msg.Candidate.SDPMid,
					SDPMLineIndex: &msg.Candidate.SDPMLineIndex,
				}
				if err := peerConnection.AddICECandidate(candidate); err != nil {
					log.Println("Failed to add ICE candidate:", err)
				}
			} else {
				log.Println("Cannot add ICE candidate: remote description is not set")
			}
		default:
			log.Printf("Dunno: %v", msg)
		}
	}
}

func pipelineForCodec(codecName string, trackName string, tracks []*webrtc.TrackLocalStaticSample, pipelineSrc string) {
	pipelineStr := fmt.Sprintf("appsink name=%s", trackName)
	switch codecName {
	case "vp8":
		pipelineStr = pipelineSrc + " ! video/x-raw, width=1920, height=1080, framerate=30/1 ! vp8enc error-resilient=partitions keyframe-max-dist=10 auto-alt-ref=true cpu-used=5 deadline=1 ! " + pipelineStr
	case "opus":
		pipelineStr = pipelineSrc + " ! opusenc ! " + pipelineStr
	default:
		panic("Unhandled codec " + codecName) //nolint
	}

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		panic(err)
	}

	if err = pipeline.SetState(gst.StatePlaying); err != nil {
		panic(err)
	}

	appSink, err := pipeline.GetElementByName(trackName)
	if err != nil {
		panic(err)
	}

	app.SinkFromElement(appSink).SetCallbacks(&app.SinkCallbacks{
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {
			sample := sink.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}

			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}

			samples := buffer.Map(gst.MapRead).Bytes()
			defer buffer.Unmap()

			for _, t := range tracks {
				if err := t.WriteSample(media.Sample{Data: samples, Duration: *buffer.Duration().AsDuration()}); err != nil {
					panic(err) //nolint
				}
			}

			return gst.FlowOK
		},
	})
}
