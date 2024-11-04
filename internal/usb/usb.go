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
	Hub    *gousb.Device
	Camera *gousb.Device
	Serial *gousb.Device
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

	hubPath := hub.Desc.Path
	devicePath := device.Desc.Path

	// The device path should be longer than the hub path
	if len(devicePath) <= len(hubPath) {
		return false
	}

	// Check if the device's path starts with the hub's path
	// This indicates the device is connected through this hub
	for i := range hubPath {
		if i >= len(devicePath) || hubPath[i] != devicePath[i] {
			return false
		}
	}

	return true
}

func FindUSBDevicePairs() ([]DevicePaths, []error) {
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
		log.Printf("Processing hub: %s (Path: %v)", hubPath, device.Desc.Path)

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
			log.Printf("Found camera device on hub %s: %s (Path: %v)",
				hubPath, otherDevice.String(), otherDevice.Desc.Path)
		}

		// Check if device is a serial device
		if otherDevice.Desc.Vendor == gousb.ID(serialVID) &&
			otherDevice.Desc.Product == gousb.ID(serialPID) {
			serialDevice = otherDevice
			log.Printf("Found serial device on hub %s: %s (Path: %v)",
				hubPath, otherDevice.String(), otherDevice.Desc.Path)
		}
	}

	// Only return a result if both devices were found on this specific hub
	if cameraDevice != nil && serialDevice != nil {
		return &DevicePaths{
			Hub:    hub,
			Camera: cameraDevice,
			Serial: serialDevice,
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

	// Try to read some basic device information
	manufacturer, err := device.Manufacturer()
	if err != nil {
		return fmt.Errorf("failed to read manufacturer: %v", err)
	}

	product, err := device.Product()
	if err != nil {
		return fmt.Errorf("failed to read product: %v", err)
	}

	log.Printf("Successfully read device: %s - %s (Path: %v)",
		manufacturer, product, device.Desc.Path)
	return nil
}

func extractUsbInfo(devicePath string) (bus, addr, usbPath string) {
	// Split path by '/' and search for "usbX/Y-..." format
	segments := strings.Split(devicePath, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "usb") && i+1 < len(segments) {
			bus = segment[3:]                              // Extract bus number from "usbX"
			addr = segments[i+1]                           // Next segment is the address (e.g., "Y")
			usbPath = strings.Split(segments[i+3], "-")[1] // Combine to form "usbX/Y"
			break
		}
	}
	return
}

// findV4L2Camera searches for a V4L2 camera device matching the given USB bus, address, and path.
func FindV4L2Camera(targetBus, targetAddr, targetPath string) (string, error) {
	// Directory containing video device links in sysfs
	v4l2Dir := "/sys/class/video4linux"

	var matchedDevice string
	err := filepath.Walk(v4l2Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Look for `video*` device directories
		if !info.IsDir() && strings.HasPrefix(info.Name(), "video") {

			// Resolve the symlink in `device` to find USB device path
			realPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}

			// Check if the path contains "usb" indicating it's a USB device
			if strings.Contains(realPath, "usb") {
				// Extract bus, addr, and path from the symlink path
				bus, _, usbPath := extractUsbInfo(realPath)
		
				// Match with target bus, addr, and path
				if bus == targetBus && usbPath == targetPath {
					matchedDevice = filepath.Join("/dev", filepath.Base(path))
					return filepath.SkipDir // Stop once we find the matching device
				}
			}
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("error walking through video devices: %v", err)
	}
	if matchedDevice == "" {
		return "", fmt.Errorf("no matching camera found")
	}
	return matchedDevice, nil
}

func JoinIntsToString(nums []int) string {
	// Convert each int to a string
	strNums := make([]string, len(nums))
	for i, num := range nums {
		strNums[i] = strconv.Itoa(num)
	}
	// Join with "."
	return strings.Join(strNums, ".")
}
