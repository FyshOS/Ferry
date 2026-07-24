//go:build !linux && !darwin

package disk

import (
	"context"
	"errors"
)

// errUnsupported is returned by disk operations on platforms Ferry does not yet
// know how to drive.
var errUnsupported = errors.New("writing USB media is only supported on Linux and macOS")

// DataPartitionSupported reports whether this platform can add the optional
// exFAT data partition. Unsupported platforms cannot.
func DataPartitionSupported() bool { return false }

// Enumerate is unsupported on this platform.
func Enumerate() ([]Disk, error) { return nil, errUnsupported }

// Write is unsupported on this platform.
func Write(context.Context, string, Disk, WriteOptions, func(WriteProgress)) (WriteResult, error) {
	return WriteResult{}, errUnsupported
}

// ensure parseLsblk is referenced so shared code stays live on all platforms.
var _ = parseLsblk
