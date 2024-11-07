package table

import (
	"fmt"

	"github.com/fatih/color"
	"github.com/kashalls/openterface-switch/internal/usb"
	"github.com/rodaine/table"
)

func Generate(devices []usb.DevicePaths) {
	headerFmt := color.New(color.FgGreen, color.Underline).SprintfFunc()
	columnFmt := color.New(color.FgHiYellow).SprintfFunc()

	tbl := table.New("Hub", "Camera", "Sound", "Serial")
	tbl.WithHeaderFormatter(headerFmt).WithFirstColumnFormatter(columnFmt)

	for _, row := range devices {

		tbl.AddRow(
			usb.JoinIntsToString(row.HubDevice.Desc.Path),
			fmt.Sprintf(
				"%s - %s",
				usb.JoinIntsToString(row.CameraDevice.Desc.Path),
				row.CameraPath,
			),
			row.AudioPath,
			fmt.Sprintf(
				"%s - %s",
				usb.JoinIntsToString(row.SerialDevice.Desc.Path),
				row.SerialPath,
			),
		)
	}

	tbl.Print()
}
