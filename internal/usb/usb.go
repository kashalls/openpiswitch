package usb

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/gousb"
)

type DevicePaths struct {
	HubDevice    *gousb.Device
	CameraDevice *gousb.Device
	CameraPath   string
	SerialDevice *gousb.Device
	SerialPath   string
	AudioPath    string
}

// Error types for different failure scenarios
type HubError struct {
	HubPath string
	Err     error
}

func (e *HubError) Error() string {
	return fmt.Sprintf("hub error at %s: %v", e.HubPath, e.Err)
}

// isChildDevice checks if the potential child device is connected to the parent hub
// by comparing their USB paths
func isChildDevice(hub, device *gousb.Device) bool {
	if hub == nil || device == nil {
		return false
	}

	// Compare the bus to check if the device is connected to the same hub
	if hub.Desc.Bus != device.Desc.Bus {
		return false
	}

	// On the same bus, check if the device's parent path has the hub's path as a prefix
	hubPath := hub.Desc.Path
	devicePath := device.Desc.Path
	if len(devicePath) > len(hubPath) && strings.HasPrefix(JoinIntsToString(devicePath), JoinIntsToString(hubPath)) {
		return true
	}

	return false
}

func FindUSBDevices() ([]DevicePaths, []error) {
	// Initialize USB context
	ctx := gousb.NewContext()
	//ctx.Debug(4)
	defer ctx.Close()

	// Constants for vendor and product IDs
	const (
		HubVendorID     = 0x1A40
		HubProductID    = 0x0101
		CameraVendorID  = 0x534D
		CameraProductID = 0x2109
		SerialVendorID  = 0x1A86
		SerialProductID = 0x7523
	)

	// Store results and errors
	var results []DevicePaths
	var errors []error

	// Get all USB devices
	devices, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		// We're interested in all devices for now
		return true
	})
	if err != nil {
		// If we can't list devices at all, this is fatal
		return nil, []error{fmt.Errorf("failed to list USB devices: %v", err)}
	}
	defer func() {
		for _, d := range devices {
			if d != nil {
				d.Close()
			}
		}
	}()

	// Find hubs with the specified vendor and product ID
	for _, device := range devices {
		if device == nil {
			continue
		}

		// Check if this is our target hub
		if device.Desc.Vendor != gousb.ID(HubVendorID) || device.Desc.Product != gousb.ID(HubProductID) {
			continue
		}

		hubPath := device.String()
		// Try to process this hub and its devices
		devicePaths, err := processHub(device, devices, CameraVendorID, CameraProductID, SerialVendorID, SerialProductID)
		if err != nil {
			// Log the error and continue with next hub
			hubErr := &HubError{
				HubPath: hubPath,
				Err:     err,
			}
			errors = append(errors, hubErr)
			log.Printf("Warning: %v", hubErr)
			continue
		}

		if devicePaths != nil {
			results = append(results, *devicePaths)
		}
	}

	// If we found no valid results at all, return all accumulated errors
	if len(results) == 0 {
		if len(errors) == 0 {
			errors = append(errors, fmt.Errorf("no matching device pairs found"))
		}
		return nil, errors
	}

	// Return both results and non-fatal errors
	return results, errors
}

func processHub(hub *gousb.Device, allDevices []*gousb.Device,
	cameraVID, cameraPID, serialVID, serialPID uint16) (*DevicePaths, error) {

	hubPath := hub.String()

	// Try to get hub configuration
	config, err := hub.Config(1)
	if err != nil {
		return nil, fmt.Errorf("failed to get hub configuration: %v", err)
	}
	defer config.Close()

	var cameraDevice, serialDevice *gousb.Device

	// Check all other devices to see if they're connected to this hub
	for _, otherDevice := range allDevices {
		if otherDevice == nil || !isChildDevice(hub, otherDevice) {
			continue
		}

		// Try to get device information
		if err := tryReadDevice(otherDevice); err != nil {
			log.Printf("Warning: Failed to read device on hub %s: %v", hubPath, err)
			continue
		}

		// Check if device is a camera
		if otherDevice.Desc.Vendor == gousb.ID(cameraVID) &&
			otherDevice.Desc.Product == gousb.ID(cameraPID) {
			cameraDevice = otherDevice
		}

		// Check if device is a serial device
		if otherDevice.Desc.Vendor == gousb.ID(serialVID) &&
			otherDevice.Desc.Product == gousb.ID(serialPID) {
			serialDevice = otherDevice
		}
	}

	// Only return a result if both devices were found on this specific hub
	if cameraDevice != nil && serialDevice != nil {
		cameraPath, err := DoJankThingsToTheSysBus("/sys/class/video4linux", "video", "usb", strconv.Itoa(cameraDevice.Desc.Bus), JoinIntsToString(cameraDevice.Desc.Path), "/dev")
		if err != nil {
			log.Printf("Failed to find device: %v", err)
		}

		serialPath, err := DoJankThingsToTheSysBus("/sys/class/tty", "ttyUSB", "usb", strconv.Itoa(serialDevice.Desc.Bus), JoinIntsToString(serialDevice.Desc.Path), "/dev")
		if err != nil {
			log.Printf("Failed to find serial for device: %v", err)
		}

		audioPath, err := DoJankThingsToTheSysBus("/sys/class/sound", "card", "sound", strconv.Itoa(cameraDevice.Desc.Bus), JoinIntsToString(cameraDevice.Desc.Path), "/dev/snd")
		if err != nil {
			log.Printf("Failed to find audio for device: %v", err)
		}

		log.Printf("%s", audioPath)

		return &DevicePaths{
			HubDevice:    hub,
			CameraDevice: cameraDevice,
			CameraPath:   cameraPath,
			SerialDevice: serialDevice,
			SerialPath:   serialPath,
			AudioPath:    audioPath,
		}, nil
	}

	return nil, fmt.Errorf("not all required devices found on hub %s", hubPath)
}

func tryReadDevice(device *gousb.Device) error {
	if device == nil {
		return fmt.Errorf("nil device")
	}

	// Try to read device configuration
	config, err := device.Config(1)
	if err != nil {
		return fmt.Errorf("failed to get device configuration: %v", err)
	}
	defer config.Close()

	return nil
}

func extractUsbInfo(devicePath string) (bus, usbPath string) {
	// Split path by '/' and search for "usbX/Y-..." format
	segments := strings.Split(devicePath, "/")

	for i, segment := range segments {
		if strings.HasPrefix(segment, "usb") && i+1 < len(segments) {
			bus = segment[3:] // Extract bus number from "usbX"

			// Start collecting USB path parts
			for j := i + 1; j < len(segments); j++ {
				if strings.Contains(segments[j], "-") && !strings.Contains(segments[j], ":") {
					// Extract the portion after the hyphen and add it to usbPathParts
					usbPath = strings.Split(segments[j], "-")[1]
				} else {
					// Stop if segment doesn't match USB path format or contains a colon
					break
				}
			}
			break
		}
	}
	return
}

func JoinIntsToString(nums []int) string {
	// Convert each int to a string
	strNums := make([]string, len(nums))
	for i, num := range nums {
		strNums[i] = strconv.Itoa(num)
	}
	return strings.Join(strNums, ".")
}

func DoJankThingsToTheSysBus(directory, filter, symlinkFilter, targetBus, targetPath string, resultPathPrefix string) (string, error) {
	var matched string

	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || !strings.HasPrefix(info.Name(), filter) {
			return nil
		}

		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}

		if !strings.Contains(realPath, symlinkFilter) && !strings.HasSuffix(realPath, "c") {
			return nil
		}

		bus, usbPath := extractUsbInfo(realPath)

		if bus == "" || usbPath == "" {
			return nil
		}

		if bus == targetBus && usbPath == targetPath {
			matched = filepath.Join(resultPathPrefix, filepath.Base(path))
			return filepath.SkipDir
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("error walking through devices: %v", err)
	}
	if matched == "" {
		return "", fmt.Errorf("no matching device found")
	}
	return matched, nil
}
