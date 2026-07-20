//go:build !linux && !darwin

package disk

import (
	"context"
	"errors"
)

// errUnsupported is returned by disk operations on platforms Ferry does not yet
// know how to drive.
var errUnsupported = errors.New("writing USB media is only supported on Linux and macOS")

// Enumerate is unsupported on this platform.
func Enumerate() ([]Disk, error) { return nil, errUnsupported }

// Write is unsupported on this platform.
func Write(context.Context, string, Disk, func(WriteProgress)) error { return errUnsupported }

// ensure parseLsblk is referenced so shared code stays live on all platforms.
var _ = parseLsblk
