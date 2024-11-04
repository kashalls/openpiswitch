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

	"github.com/kashalls/openterface-switch/internal/usb"
	"github.com/pion/randutil"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/vladimirvivien/go4vl/device"
	"github.com/vladimirvivien/go4vl/v4l2"
)

type CameraDevice struct {
	ID         string
	Device     *device.Device
	Frames     <-chan []byte
	Broadcast  chan []byte // New broadcast channel for sharing frames
	Width      int
	Height     int
	FPS        int
	Format     string
	StreamInfo string
	ctx        context.Context
	cancel     context.CancelFunc
}

type CameraManager struct {
	Devices map[string]*CameraDevice
	mu      sync.RWMutex
}

func NewCameraManager() *CameraManager {
	return &CameraManager{
		Devices: make(map[string]*CameraDevice),
	}
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

func (cm *CameraManager) InitCamera(devPath string, width, height int, format string, fps int, bufferSize int) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	camera, err := device.Open(devPath,
		device.WithIOType(v4l2.IOTypeMMAP),
		device.WithPixFormat(v4l2.PixFormat{
			PixelFormat: getFormatType(format),
			Width:       uint32(width),
			Height:      uint32(height),
			Field:       v4l2.FieldAny,
		}),
		device.WithFPS(uint32(fps)),
		device.WithBufferSize(uint32(bufferSize)),
	)
	if err != nil {
		return fmt.Errorf("failed to open device %s: %v", devPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := camera.Start(ctx); err != nil {
		cancel()
		return fmt.Errorf("failed to start device %s: %v", devPath, err)
	}

	caps := camera.Capability()
	currFmt, err := camera.GetPixFormat()
	if err != nil {
		cancel()
		return fmt.Errorf("failed to get format for device %s: %v", devPath, err)
	}

	deviceID := filepath.Base(devPath)
	streamInfo := fmt.Sprintf("%s - %s [%dx%d] %d fps",
		caps.Card,
		v4l2.PixelFormats[currFmt.PixelFormat],
		currFmt.Width, currFmt.Height, fps,
	)

	broadcastChan := make(chan []byte, 1) // Buffered channel for the latest frame
	cm.Devices[deviceID] = &CameraDevice{
		ID:         deviceID,
		Device:     camera,
		Frames:     camera.GetOutput(),
		Broadcast:  broadcastChan, // Add the broadcast channel
		Width:      int(currFmt.Width),
		Height:     int(currFmt.Height),
		FPS:        fps,
		Format:     format,
		StreamInfo: streamInfo,
		ctx:        ctx,
		cancel:     cancel,
	}

	// Start a goroutine to read frames and broadcast
	go func() {
		for frame := range camera.GetOutput() {
			select {
			case broadcastChan <- frame: // Send to broadcast channel
			default: // Drop the frame if not consumed
			}
		}
		close(broadcastChan) // Ensure channel is closed when done
	}()

	return nil
}

func (cm *CameraManager) CloseAll() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for _, dev := range cm.Devices {
		dev.cancel()
		dev.Device.Close()
	}
}

func (cm *CameraManager) ServePage(w http.ResponseWriter, r *http.Request) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	pd := PageData{}
	for _, dev := range cm.Devices {
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (cm *CameraManager) ServeVideoStream(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/stream/")

	cm.mu.RLock()
	device, exists := cm.Devices[deviceID]
	cm.mu.RUnlock()

	if !exists {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	mimeWriter := multipart.NewWriter(w)
	w.Header().Set("Content-Type", fmt.Sprintf("multipart/x-mixed-replace; boundary=%s", mimeWriter.Boundary()))
	partHeader := make(textproto.MIMEHeader)
	partHeader.Add("Content-Type", "image/jpeg")

	for frame := range device.Broadcast { // Use Broadcast channel
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

func (cm *CameraManager) HandleControl(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/control/")

	cm.mu.RLock()
	device, exists := cm.Devices[deviceID]
	cm.mu.RUnlock()

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
		if err := device.Device.SetControlBrightness(int32(val)); err != nil {
			http.Error(w, "Failed to set brightness", http.StatusInternalServerError)
		}
	case "contrast":
		if err := device.Device.SetControlContrast(int32(val)); err != nil {
			http.Error(w, "Failed to set contrast", http.StatusInternalServerError)
		}
	case "saturation":
		if err := device.Device.SetControlSaturation(int32(val)); err != nil {
			http.Error(w, "Failed to set saturation", http.StatusInternalServerError)
		}
	}
}

func main() {
	var (
		port     = ":9090"
		format   = "h264"
		width    = 1920
		height   = 1080
		fps      = 30
		buffSize = 4
	)

	results, errors := usb.FindUSBDevicePairs()

	// Handle any errors
	for _, err := range errors {
		if hubErr, ok := err.(*usb.HubError); ok {
			fmt.Printf("Hub error: %v\n", hubErr)
		} else {
			fmt.Printf("General error: %v\n", err)
		}
	}

	flag.StringVar(&port, "p", port, "webcam service port")
	flag.StringVar(&format, "f", format, "pixel format (mjpeg, jpeg, yuyv)")
	flag.IntVar(&width, "w", width, "capture width")
	flag.IntVar(&height, "h", height, "capture height")
	flag.IntVar(&fps, "r", fps, "frames per second")
	flag.IntVar(&buffSize, "b", buffSize, "device buffer size")
	flag.Parse()

	cm := NewCameraManager()
	defer cm.CloseAll()

	for _, paths := range results {
		fmt.Printf("Found matching devices:\n")
		fmt.Printf("  Hub: %s\n", paths.Hub.String())
		fmt.Printf("  Camera: %s\n", paths.Camera.String())
		fmt.Printf("  Serial: %s\n", paths.Serial.String())
	}

	for _, paths := range results {

		device, err := usb.FindV4L2Camera(strconv.Itoa(paths.Camera.Desc.Bus), strconv.Itoa(paths.Camera.Desc.Address), usb.JoinIntsToString(paths.Camera.Desc.Path))

		if err != nil {
			log.Printf("Failed to find device: %v", err)
			continue
		}

		if err := cm.InitCamera(device, width, height, format, fps, buffSize); err != nil {
			log.Printf("Warning: failed to initialize camera %s: %v", device, err)
			continue
		}
		log.Printf("Initialized camera: %s", device)
	}

	if len(cm.Devices) == 0 {
		log.Fatal("No cameras were successfully initialized")
	}

	http.HandleFunc("/webcam", cm.ServePage)
	http.HandleFunc("/stream/", cm.ServeVideoStream)
	http.HandleFunc("/control/", cm.HandleControl)
	http.HandleFunc("/offer", cm.handleOffer)

	log.Printf("Starting server on port %s", port)
	log.Println("Use URL path /webcam")
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

func getFormatType(fmtStr string) v4l2.FourCCType {
	switch strings.ToLower(fmtStr) {
	case "jpeg":
		return v4l2.PixelFmtJPEG
	case "mpeg":
		return v4l2.PixelFmtMPEG
	case "mjpeg":
		return v4l2.PixelFmtMJPEG
	case "h264", "h.264":
		return v4l2.PixelFmtH264
	case "yuyv":
		return v4l2.PixelFmtYUYV
	case "rgb":
		return v4l2.PixelFmtRGB24
	}
	return v4l2.PixelFmtMPEG
}

func (cm *CameraManager) handleOffer(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Access-Control-Allow-Origin", "*") // Change * to your specific domain for production
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	// Decode the incoming WebRTC offer
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "Invalid offer", http.StatusBadRequest)
		return
	}

	// Create a new peer connection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.cloudflare.com:3478"},
			},
		},
	})
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create peer connection: %v", err), http.StatusInternalServerError)
		return
	}

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())
	})

	// Register data channel creation handling
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("New DataChannel %s %d\n", d.Label(), d.ID())

		// Register channel opening handling
		d.OnOpen(func() {
			fmt.Printf("Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n", d.Label(), d.ID())

			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				message, sendErr := randutil.GenerateCryptoRandomString(15, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
				if sendErr != nil {
					panic(sendErr)
				}

				// Send the message as text
				fmt.Printf("Sending '%s'\n", message)
				if sendErr = d.SendText(message); sendErr != nil {
					panic(sendErr)
				}
			}
		})

		// Register text message handling
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("Message from DataChannel '%s': '%s'\n", d.Label(), string(msg.Data))
		})
	})

	// Iterate over the devices and create a video track for each one
	cm.mu.RLock() // Lock for reading the devices map
	for deviceID, device := range cm.Devices {
		videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
			MimeType:  "video/h264", // Change according to your camera's output format
			ClockRate: 90000,
		}, "video", deviceID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to create video track for device %s: %v", deviceID, err), http.StatusInternalServerError)
			cm.mu.RUnlock() // Unlock before returning
			return
		}

		// Add the video track to the peer connection
		_, err = peerConnection.AddTrack(videoTrack)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to add video track for device %s: %v", deviceID, err), http.StatusInternalServerError)
			cm.mu.RUnlock() // Unlock before returning
			return
		}

		// Start a goroutine to send frames over the video track for this device
		go func(device *CameraDevice, videoTrack *webrtc.TrackLocalStaticSample) {
			for frame := range device.Broadcast {
				if len(frame) == 0 {
					continue
				}

				// Create a sample with the frame data
				sample := media.Sample{Data: frame, Duration: time.Second / time.Duration(device.FPS)}
				if err := videoTrack.WriteSample(sample); err != nil {
					log.Printf("Failed to write sample for device %s: %v", device.ID, err)
					return
				}
			}
		}(device, videoTrack) // Pass the current device and track to the goroutine
	}
	cm.mu.RUnlock() // Unlock after processing all devices

	// Set remote description from the offer
	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		http.Error(w, fmt.Sprintf("Failed to set remote description: %v", err), http.StatusInternalServerError)
		return
	}

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create answer: %v", err), http.StatusInternalServerError)
		return
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Set local description
	if err := peerConnection.SetLocalDescription(answer); err != nil {
		http.Error(w, fmt.Sprintf("Failed to set local description: %v", err), http.StatusInternalServerError)
		return
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Send the answer back to the client
	resp, err := json.Marshal(peerConnection.LocalDescription())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to encode answer: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}
