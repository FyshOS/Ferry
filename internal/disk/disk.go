// Package disk enumerates removable USB targets and writes ISO images to them.
package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Phase names reported through WriteProgress.
const (
	PhaseWriting      = "Writing image"
	PhaseSyncing      = "Flushing buffers"
	PhaseVerifying    = "Verifying"
	PhasePartitioning = "Creating data partition"
	PhaseFormatting   = "Formatting data partition"
	PhaseEjecting     = "Ejecting"
	PhaseDone         = "Complete"
)

// The write phases divide the overall progress bar between them. Writing and
// verifying stream real byte counts and so carry the bulk; the optional data
// partition steps report no byte stream and rest at their floor. The shares sum
// to just under 1 so ejecting can complete the bar.
const (
	writeShare  = 0.72 // writing fills [0, 0.72]
	verifyShare = 0.20 // verifying fills [0.72, 0.92]
	partShare   = 0.02 // partitioning fills [0.92, 0.94]
	formatShare = 0.05 // formatting fills [0.94, 0.99]
)

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
		return writeShare + within*verifyShare
	case PhasePartitioning:
		// No byte stream, so this rests at the end of the verify share.
		return writeShare + verifyShare + within*partShare
	case PhaseFormatting:
		return writeShare + verifyShare + partShare + within*formatShare
	case PhaseEjecting, PhaseDone:
		return 1
	}
	return within * writeShare
}

// ErrVerifyMismatch is returned when the data read back from the device does
// not match the source image, meaning the media is not a reliable copy.
var ErrVerifyMismatch = errors.New("verification failed: written data does not match the image")

// DefaultDataLabel is the exFAT volume label given to the optional data
// partition. It is the only contract between Ferry and the booted FyshOS, which
// finds the partition via /dev/disk/by-label/<label>.
const DefaultDataLabel = "Data"

const (
	// dataAlign is the boundary the data partition starts on, one mebibyte, so
	// it stays clear of the image and lands on an efficient offset.
	dataAlign = 1 << 20
	// minDataPartition is the smallest leftover worth offering as a data
	// partition; below this the feature is skipped.
	minDataPartition = 1 << 30 // 1 GiB
	// gptTailReserve leaves room for the backup GPT (33 sectors) that sgdisk/gpt
	// relocate to the end of the device, plus a sector of slack.
	gptTailReserve = 34 * 512
)

// DataFreeBytes reports how much space a data partition could claim on a device
// of devSize once an image of imageSize has been written, or 0 when too little
// would remain to be worth it. The partition starts aligned one mebibyte past
// the actual image end (not a fixed reservation: some images exceed 2 GiB) and
// runs to the end of the device, less the backup GPT tail.
func DataFreeBytes(devSize, imageSize int64) int64 {
	if imageSize <= 0 || devSize <= 0 {
		return 0
	}
	start := (imageSize + dataAlign - 1) / dataAlign * dataAlign
	free := devSize - start - gptTailReserve
	if free < minDataPartition {
		return 0
	}
	return free
}

// WriteOptions carries the caller's choices for a write beyond the image and
// device themselves.
type WriteOptions struct {
	// DataPartition asks Ferry to add an exFAT data partition in the free space
	// after the image, once the image has been written and verified.
	DataPartition bool
	// DataLabel is the volume label for that partition; empty means
	// DefaultDataLabel.
	DataLabel string
}

// label returns the data-partition label to use, applying the default.
func (o WriteOptions) label() string {
	if o.DataLabel == "" {
		return DefaultDataLabel
	}
	return o.DataLabel
}

// DataStatus reports what became of the optional data partition.
type DataStatus int

const (
	// DataSkipped means no data partition was requested or there was no room.
	DataSkipped DataStatus = iota
	// DataCreated means the partition was created and formatted successfully.
	DataCreated
	// DataFailed means the partition step failed. The image write still
	// succeeded and is verified good; this is reported as a warning, not an
	// error.
	DataFailed
)

// WriteResult reports the outcome of the optional data-partition step. The image
// write itself succeeded whenever Write returns a nil error, regardless of Data.
type WriteResult struct {
	Data      DataStatus
	DataNode  string // device node of the new partition, when created
	DataLabel string // the label it was given, when created
	DataWarn  string // human-readable reason, when DataFailed
}

// dataMarker prefixes the data-partition result the privileged step reports on
// stdout. Both platforms emit it and share parseDataResult.
const dataMarker = "FERRY-DATA "

// parseDataResult interprets a FERRY-DATA result line from the privileged step's
// stdout. The line is one of:
//
//	FERRY-DATA created <node> <label>
//	FERRY-DATA skipped <reason...>
//	FERRY-DATA error <message...>
//
// For "created", node and detail hold the partition node and its label; for the
// others detail holds the reason. ok is false for any line that is not a
// data-result line.
func parseDataResult(line string) (status DataStatus, node, detail string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), dataMarker)
	if !found {
		return 0, "", "", false
	}
	verb, args, _ := strings.Cut(rest, " ")
	args = strings.TrimSpace(args)
	switch verb {
	case "created":
		node, label, _ := strings.Cut(args, " ")
		return DataCreated, node, strings.TrimSpace(label), true
	case "skipped":
		return DataSkipped, "", args, true
	case "error":
		return DataFailed, "", args, true
	}
	return 0, "", "", false
}

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

// sha256File returns the hex sha256 of a file's contents.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
