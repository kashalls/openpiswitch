package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kashalls/openterface-switch/internal/table"
	"github.com/kashalls/openterface-switch/internal/usb"
	"github.com/pion/webrtc/v4"
	"github.com/tarm/serial"
	"github.com/vladimirvivien/go4vl/device"
	"github.com/vladimirvivien/go4vl/v4l2"
	"golang.org/x/net/websocket"
)

type Openterface struct {
	ID           string
	Paths        usb.DevicePaths
	Camera       *device.Device
	CameraFrames <-chan []byte
	AudioFrames  <-chan []byte
	Width        int
	Height       int
	FPS          int
	StreamInfo   string
	SerialPort   *serial.Port
	ctx          context.Context
	cancel       context.CancelFunc
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

func (dm *DeviceManager) InitDevice(paths usb.DevicePaths) error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	camera, err := device.Open(paths.CameraPath,
		device.WithIOType(v4l2.IOTypeMMAP),
		device.WithPixFormat(v4l2.PixFormat{
			PixelFormat: v4l2.PixelFmtMJPEG,
			Width:       uint32(width),
			Height:      uint32(height),
			Field:       v4l2.FieldAny,
		}),
		device.WithFPS(uint32(fps)),
		device.WithBufferSize(uint32(bufferSize)),
	)
	if err != nil {
		log.Fatalf("failed to open %s: %v", paths.CameraPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := camera.Start(ctx); err != nil {
		log.Fatalf("failed to capture stream of %s: %v", paths.CameraPath, err)
	}

	caps := camera.Capability()
	currentFormat, err := camera.GetPixFormat()
	if err != nil {
		cancel()
		return fmt.Errorf("failed to get format for device %s: %v", paths.CameraPath, err)
	}
	streamInfo := fmt.Sprintf("%s - %s [%dx%d] %d fps",
		caps.Card,
		v4l2.PixelFormats[currentFormat.PixelFormat],
		currentFormat.Width, currentFormat.Height, fps,
	)

	serialPort, err := openSerialPort(paths.SerialPath, 115200) // Set baud rate as needed
	if err != nil {
		cancel()
		return fmt.Errorf("failed to open serial port %s: %v", paths.SerialPath, err)
	}

	deviceID := filepath.Base(paths.CameraPath)

	dm.Devices[deviceID] = &Openterface{
		ID:           deviceID,
		Camera:       camera,
		SerialPort:   serialPort,
		CameraFrames: camera.GetOutput(),
		Paths:        paths,
		Width:        int(currentFormat.Width),
		Height:       int(currentFormat.Height),
		FPS:          fps,
		StreamInfo:   streamInfo,
		ctx:          ctx,
		cancel:       cancel,
	}

	return nil
}

func (dm *DeviceManager) CloseAll() {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	for _, dev := range dm.Devices {
		dev.cancel()
		dev.Camera.Close()
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
	dm         = NewDeviceManager()
	port       = ":9090"
	width      = 1920
	height     = 1080
	fps        = 30
	bufferSize = 1
)

func main() {
	defer dm.CloseAll()

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
	flag.IntVar(&bufferSize, "b", bufferSize, "device buffer size")
	flag.Parse()

	fmt.Printf("Found %d devices:\n", len(results))
	table.Generate(results)

	for _, paths := range results {
		if err := dm.InitDevice(paths); err != nil {
			log.Printf("Warning: failed to initialize camera %s - %s: %v", paths.CameraPath, paths.SerialPath, err)
			continue
		}
	}

	if len(dm.Devices) == 0 {
		log.Fatal("No cameras were successfully initialized")
	}

	http.HandleFunc("/", dm.ServePage)
	http.HandleFunc("/stream/", dm.ServeVideoStream)
	http.HandleFunc("/control/", dm.HandleControl)
	// http.Handle("/ws", websocket.Handler(websocketHandler))

	log.Printf("Starting server on port %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

func (dm *DeviceManager) ServeVideoStream(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/stream/")
	dm.mu.RLock()
	device, exists := dm.Devices[deviceID]
	dm.mu.RUnlock()
	if !exists {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	mimeWriter := multipart.NewWriter(w)
	w.Header().Set("Content-Type", fmt.Sprintf("multipart/x-mixed-replace; boundary=%s", mimeWriter.Boundary()))
	partHeader := make(textproto.MIMEHeader)
	partHeader.Add("Content-Type", "image/jpeg")
	for frame := range device.CameraFrames { // Use Broadcast channel
		if len(frame) == 0 {
			continue
		}
		partWriter, err := mimeWriter.CreatePart(partHeader)
		if err != nil {
			log.Printf("failed to create multi-part writer: %s", err)
			return
		}
		if _, err := partWriter.Write(frame); err != nil {
			log.Printf("failed to write image: %s", err)
		}
	}
}

func (dm *DeviceManager) HandleControl(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/control/")

	dm.mu.RLock()
	device, exists := dm.Devices[deviceID]
	dm.mu.RUnlock()

	if !exists {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	var ctrl struct {
		Name  string
		Value string
	}

	if err := json.NewDecoder(r.Body).Decode(&ctrl); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	val, err := strconv.Atoi(ctrl.Value)
	if err != nil {
		http.Error(w, "Invalid control value", http.StatusBadRequest)
		return
	}

	switch ctrl.Name {
	case "brightness":
		if err := device.Camera.SetControlBrightness(int32(val)); err != nil {
			http.Error(w, "Failed to set brightness", http.StatusInternalServerError)
		}
	case "contrast":
		if err := device.Camera.SetControlContrast(int32(val)); err != nil {
			http.Error(w, "Failed to set contrast", http.StatusInternalServerError)
		}
	case "saturation":
		if err := device.Camera.SetControlSaturation(int32(val)); err != nil {
			http.Error(w, "Failed to set saturation", http.StatusInternalServerError)
		}
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

	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())
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
