// Package disk enumerates removable USB targets and writes ISO images to them.
package disk

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Phase names reported through WriteProgress.
const (
	PhaseWriting   = "Writing image"
	PhaseSyncing   = "Flushing buffers"
	PhaseVerifying = "Verifying"
	PhaseEjecting  = "Ejecting"
	PhaseDone      = "Complete"
)

// writeShare is how much of the overall job the write accounts for; the
// read-back verify makes up the rest.
const writeShare = 3.0 / 4.0

// WriteProgress reports progress of a write operation. Bytes/Total give the
// byte-level progress within the current phase.
type WriteProgress struct {
	Phase string
	Bytes int64
	Total int64
}

// Fraction returns progress across the entire operation in the range [0,1].
func (p WriteProgress) Fraction() float64 {
	within := 0.0
	if p.Total > 0 {
		within = float64(p.Bytes) / float64(p.Total)
		within = min(max(within, 0), 1)
	}

	switch p.Phase {
	case PhaseWriting:
		return within * writeShare
	case PhaseSyncing:
		// Writing is done but the verify has not started.
		return writeShare
	case PhaseVerifying:
		return writeShare + within*(1-writeShare)
	case PhaseEjecting, PhaseDone:
		return 1
	}
	return within * writeShare
}

// ErrVerifyMismatch is returned when the data read back from the device does
// not match the source image, meaning the media is not a reliable copy.
var ErrVerifyMismatch = errors.New("verification failed: written data does not match the image")

// Disk describes a whole block device that could be a USB target.
type Disk struct {
	Name        string   // kernel name, e.g. "sdb"
	Path        string   // device node, e.g. "/dev/sdb"
	Size        int64    // capacity in bytes
	Model       string   // model string, may be empty
	Vendor      string   // vendor string, may be empty
	Removable   bool     // rm or hotplug flag
	Transport   string   // bus, e.g. "usb"
	Mountpoints []string // mountpoints of the disk or any of its partitions
}

// Mounted reports whether the disk (or any partition on it) is currently
// mounted.
func (d Disk) Mounted() bool { return len(d.Mountpoints) > 0 }

// Label returns a friendly one-line description for selection widgets.
func (d Disk) Label() string {
	name := d.Model
	if d.Vendor != "" {
		if name != "" {
			name = d.Vendor + " " + name
		} else {
			name = d.Vendor
		}
	}
	if name == "" {
		name = "USB device"
	}
	status := "available"
	if d.Mounted() {
		status = "mounted"
	}
	return fmt.Sprintf("%s — %s (%s, %s)", d.Path, name, FormatSize(d.Size), status)
}

// lsblkOutput and lsblkNode mirror the subset of `lsblk -J` we consume.
type lsblkOutput struct {
	BlockDevices []lsblkNode `json:"blockdevices"`
}

type lsblkNode struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Size       int64       `json:"size"`
	Type       string      `json:"type"`
	Mountpoint *string     `json:"mountpoint"`
	RM         bool        `json:"rm"`
	Hotplug    bool        `json:"hotplug"`
	Tran       string      `json:"tran"`
	Model      string      `json:"model"`
	Vendor     string      `json:"vendor"`
	Children   []lsblkNode `json:"children"`
}

// collectMounts walks a node and its children gathering non-empty mountpoints.
func collectMounts(n lsblkNode, out *[]string) {
	if n.Mountpoint != nil && *n.Mountpoint != "" {
		*out = append(*out, *n.Mountpoint)
	}
	for _, c := range n.Children {
		collectMounts(c, out)
	}
}

// parseLsblk converts `lsblk -b -J ...` output into the removable disks that
// Ferry is willing to write to. Non-removable, non-USB disks (the system's own
// drives) are excluded so we cannot offer to overwrite them.
func parseLsblk(data []byte) ([]Disk, error) {
	var out lsblkOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing lsblk output: %w", err)
	}
	var disks []Disk
	for _, n := range out.BlockDevices {
		if n.Type != "disk" {
			continue
		}
		// Only removable/hotpluggable or explicitly USB-attached devices are
		// eligible; this keeps fixed system drives off the list.
		if !n.RM && !n.Hotplug && n.Tran != "usb" {
			continue
		}
		if n.Size <= 0 {
			continue
		}
		var mounts []string
		collectMounts(n, &mounts)
		path := n.Path
		if path == "" {
			path = "/dev/" + n.Name
		}
		disks = append(disks, Disk{
			Name:        n.Name,
			Path:        path,
			Size:        n.Size,
			Model:       n.Model,
			Vendor:      n.Vendor,
			Removable:   n.RM || n.Hotplug,
			Transport:   n.Tran,
			Mountpoints: mounts,
		})
	}
	return disks, nil
}

// FormatSize renders a byte count using binary (GiB/MiB) units.
func FormatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
