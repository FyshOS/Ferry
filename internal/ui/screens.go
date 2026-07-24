package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/fyshos/ferry/internal/cache"
	"github.com/fyshos/ferry/internal/disk"
	"github.com/fyshos/ferry/internal/releases"
)

// nextButton makes the primary "advance" button, enabled only when the step is
// satisfied.
func nextButton(label string, enabled bool, onTap func()) *widget.Button {
	b := widget.NewButton(label, onTap)
	b.Importance = widget.HighImportance
	if !enabled {
		b.Disable()
	}
	return b
}

// ---- step 1: architecture ----

// showArch is the welcome screen: pick the kind of computer the stick will boot.
// Like the other steps, tapping a row only sets the choice; Next progresses.
func (wz *Wizard) showArch() {
	descs := map[releases.Arch]string{
		releases.ArchAMD64: "Most PCs and laptops",
		releases.ArchARM64: "Recent macOS and Raspberry Pi boards",
		releases.ArchI386:  "Older 32-bit PCs",
	}

	rows := make([]*optionRow, len(releases.Arches))
	update := func() {
		for i, a := range releases.Arches {
			rows[i].setSelected(a == wz.arch)
		}
	}

	var items []fyne.CanvasObject
	for i, a := range releases.Arches {
		i, a := i, a
		rows[i] = newOptionRow(theme.ComputerIcon(), a.Label(), descs[a], func() {
			wz.arch = a
			update()
		})
		items = append(items, rows[i])
	}
	update()

	footer := footerNav(nil, nextButton("Next", true, wz.showImage))
	wz.show(screenArch, wz.icon,
		"Create a FyshOS USB",
		"Which kind of computer will it boot?",
		container.NewVBox(items...), footer)
}

// ---- step 2: image ----

// showImage lets the user download the latest image, reuse a download, or open a
// file of their own.
func (wz *Wizard) showImage() {
	var rows []fyne.CanvasObject

	if wz.selPath != "" {
		sel := newOptionRow(theme.ConfirmIcon(), wz.selName,
			"Ready to write · "+disk.FormatSize(wz.selSize), nil)
		sel.setSelected(true)
		rows = append(rows, sel)
	}

	switch {
	case wz.loading:
		rows = append(rows, newOptionRow(theme.DownloadIcon(),
			"Checking for the latest FyshOS…", "Just a moment", nil))
	default:
		if latest, ok := releases.Latest(wz.images, wz.arch); ok {
			l := latest
			if wz.cch != nil && wz.cch.Has(l) {
				rows = append(rows, newOptionRow(theme.DownloadIcon(), "Use the latest FyshOS",
					l.Built.Format("2 Jan 2006")+" · already downloaded", func() {
						wz.setImage(wz.cch.PathFor(l), l.Name, l.Size)
						wz.showImage()
					}))
			} else {
				rows = append(rows, newOptionRow(theme.DownloadIcon(), "Download the latest FyshOS",
					l.Built.Format("2 Jan 2006")+" · "+disk.FormatSize(l.Size), func() {
						wz.startDownload(l)
					}))
			}
		} else {
			rows = append(rows, newOptionRow(theme.WarningIcon(),
				"No image online for this architecture", "Try another architecture, or open a file", nil))
		}
	}

	// Only offer to browse downloads when there is at least one to browse,
	// otherwise the row leads to an empty, dead-end screen.
	if wz.cch != nil {
		if entries, _ := wz.cch.List(); len(entries) > 0 {
			rows = append(rows, newOptionRow(theme.FolderOpenIcon(), "Choose a download",
				fmt.Sprintf("Pick from %d image(s) you already have", len(entries)), wz.showCache))
		}
	}
	rows = append(rows, newOptionRow(theme.FileIcon(), "Open an ISO file…",
		"Use any .iso from your computer", wz.openFile))

	footer := footerNav(backButton(wz.showArch),
		nextButton("Next", wz.selPath != "", wz.showDisk))
	wz.show(screenImage, theme.DownloadIcon(),
		"Choose an image", "Download the latest FyshOS, or pick your own",
		container.NewVBox(rows...), footer)
}

// showCache is a sub-screen of the image step listing already-downloaded images.
func (wz *Wizard) showCache() {
	entries, err := wz.cch.List()
	if err != nil {
		dialog.ShowError(err, wz.win)
		return
	}
	var rows []fyne.CanvasObject
	if len(entries) == 0 {
		rows = append(rows, newOptionRow(theme.InfoIcon(), "No downloads yet",
			"Download the latest image or open a file", nil))
	}
	for _, e := range entries {
		e := e
		rows = append(rows, newOptionRow(theme.MediaVideoIcon(), e.Name,
			disk.FormatSize(e.Size), func() {
				wz.setImage(e.Path, e.Name, e.Size)
				wz.showImage()
			}))
	}
	body := container.NewVScroll(container.NewVBox(rows...))
	wz.show(screenImage, theme.FolderOpenIcon(),
		"Your downloads", "Choose an image you already have",
		body, footerNav(backButton(wz.showImage), nil))
}

// openFile shows the OS file picker filtered to .iso files.
func (wz *Wizard) openFile() {
	fd := dialog.NewFileOpen(func(r fyne.URIReadCloser, err error) {
		if err != nil {
			dialog.ShowError(err, wz.win)
			return
		}
		if r == nil {
			return // cancelled
		}
		defer r.Close()
		path := r.URI().Path()
		var size int64
		if info, serr := os.Stat(path); serr == nil {
			size = info.Size()
		}
		wz.setImage(path, filepath.Base(path), size)
		wz.showImage()
	}, wz.win)
	fd.SetFilter(storage.NewExtensionFileFilter([]string{".iso"}))
	// Show before Resize: FileDialog.Resize dereferences internal state that
	// only exists once the dialog has been shown (Fyne 2.8).
	fd.Show()
	fd.Resize(fyne.NewSize(700, 500))
}

// startDownload downloads an image, showing a cancellable progress screen.
func (wz *Wizard) startDownload(img releases.Image) {
	ctx, cancel := context.WithCancel(context.Background())

	bar := widget.NewProgressBar()
	status := widget.NewLabelWithStyle("Starting…", fyne.TextAlignCenter, fyne.TextStyle{})
	body := container.NewVBox(bar, status)

	cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() { cancel() })
	footer := footerNav(nil, cancelBtn)

	wz.show(screenDownloading, theme.DownloadIcon(),
		"Downloading FyshOS", img.Name, body, footer)

	go func() {
		path, err := wz.cch.Download(ctx, img, func(p cache.Progress) {
			fyne.Do(func() {
				bar.SetValue(p.Fraction())
				status.SetText(disk.FormatSize(p.Downloaded) + " of " + disk.FormatSize(p.Total))
			})
		})
		fyne.Do(func() {
			if err != nil {
				if ctx.Err() != nil {
					wz.showImage() // user cancelled — no error to report
					return
				}
				dialog.ShowError(err, wz.win)
				wz.showImage()
				return
			}
			wz.setImage(path, img.Name, img.Size)
			wz.showImage()
		})
	}()
}

// ---- step 3: USB device ----

// showDisk enumerates attached USB sticks and lets the user pick one.
func (wz *Wizard) showDisk() {
	if disks, err := disk.Enumerate(); err == nil {
		wz.disks = disks
	}

	next := nextButton("Next", false, wz.showConfirm)
	warn := canvas.NewText("", theme.Color(theme.ColorNameError))
	warn.Alignment = fyne.TextAlignCenter

	rows := make([]*optionRow, len(wz.disks))
	update := func() {
		for i := range rows {
			rows[i].setSelected(wz.selDisk != nil && wz.disks[i].Path == wz.selDisk.Path)
		}
		switch {
		case wz.selDisk == nil:
			next.Disable()
			warn.Text = ""
		case wz.selSize > 0 && wz.selDisk.Size < wz.selSize:
			next.Disable()
			warn.Text = "This stick is too small for the image."
		default:
			next.Enable()
			warn.Text = ""
		}
		warn.Refresh()
	}

	var items []fyne.CanvasObject
	if len(wz.disks) == 0 {
		items = append(items, newOptionRow(theme.WarningIcon(), "No USB stick found",
			"Insert one, then tap Rescan below", nil))
	}
	for i := range wz.disks {
		i := i
		d := wz.disks[i]
		state := "ready to use"
		if d.Mounted() {
			state = "in use — will be unmounted"
		}
		rows[i] = newOptionRow(theme.StorageIcon(), diskName(d),
			disk.FormatSize(d.Size)+" · "+state, func() {
				wz.selDisk = &wz.disks[i]
				update()
			})
		items = append(items, rows[i])
	}

	rescan := widget.NewButtonWithIcon("Rescan", theme.ViewRefreshIcon(), wz.showDisk)
	rescan.Importance = widget.LowImportance
	items = append(items, widget.NewSeparator(), container.NewCenter(rescan), warn)

	body := container.NewVScroll(container.NewVBox(items...))
	footer := footerNav(backButton(wz.showImage), next)
	wz.show(screenDisk, theme.StorageIcon(),
		"Choose your USB stick", "Everything on it will be erased",
		body, footer)
	update()
}

// diskName is a friendly label for a disk: vendor and model, or a fallback.
func diskName(d disk.Disk) string {
	name := d.Model
	if d.Vendor != "" {
		if name != "" {
			name = d.Vendor + " " + name
		} else {
			name = d.Vendor
		}
	}
	if name == "" {
		name = "USB device (" + d.Path + ")"
	}
	return name
}

// ---- step 4: confirm & write ----

// dataOffer decides whether to offer a data partition for this device and, if
// so, how much free space it would use. It is gated on the platform supporting
// the step, the image being amd64 (the arm64 images ship no usable partition
// table to extend), and the stick having real space left beyond the image.
func (wz *Wizard) dataOffer(d disk.Disk) (free int64, ok bool) {
	if !disk.DataPartitionSupported() || wz.arch != releases.ArchAMD64 {
		return 0, false
	}
	free = disk.DataFreeBytes(d.Size, wz.selSize)
	return free, free > 0
}

// showConfirm summarises the choices and offers the point-of-no-return button.
func (wz *Wizard) showConfirm() {
	if wz.selPath == "" || wz.selDisk == nil {
		wz.showDisk()
		return
	}
	d := *wz.selDisk

	summary := widget.NewRichTextFromMarkdown(fmt.Sprintf(
		"**Image**\n\n%s\n\n**USB stick**\n\n%s  %s (%s)",
		wz.selName, diskName(d), d.Path, disk.FormatSize(d.Size)))
	summary.Wrapping = fyne.TextWrapWord

	warn := canvas.NewText("Everything on this USB stick will be permanently erased.",
		theme.Color(theme.ColorNameError))
	warn.Alignment = fyne.TextAlignCenter

	create := widget.NewButtonWithIcon("Erase & Create", theme.MediaPlayIcon(), func() {
		wz.confirmWrite(d)
	})
	create.Importance = widget.HighImportance

	items := []fyne.CanvasObject{summary, widget.NewSeparator()}

	// Offer a data partition only when it is possible: a supported platform, an
	// amd64 image (arm64 ships no usable partition table), and real space left.
	if free, ok := wz.dataOffer(d); ok {
		check := widget.NewCheck(
			fmt.Sprintf("Use the remaining %s for %q partition (exFAT)",
				disk.FormatSize(free), wz.dataLabel),
			func(b bool) { wz.wantData = b })
		check.SetChecked(wz.wantData)
		note := canvas.NewText("Keeps files across reboots, readable on Linux, macOS and Windows.",
			theme.Color(theme.ColorNamePlaceHolder))
		note.TextSize = 12
		items = append(items, check, note, widget.NewSeparator())
	}

	items = append(items, warn)
	body := container.NewVBox(items...)
	footer := footerNav(backButton(wz.showDisk), create)
	wz.show(screenConfirm, theme.WarningIcon(),
		"Ready to go", "Check the details, then create your USB", body, footer)
}

// confirmWrite is the gate between the confirm screen and the write. When a data
// partition was asked for, it checks up front that the host has the tools to make one.
// They can carry on without it or go back and install the tools.
func (wz *Wizard) confirmWrite(d disk.Disk) {
	_, canData := wz.dataOffer(d)
	withData := canData && wz.wantData
	if withData {
		if missing := disk.MissingDataTools(); len(missing) > 0 {
			wz.warnDataTools(d, missing)
			return
		}
	}
	wz.startWrite(d, withData)
}

// warnDataTools tells the user the data-partition tools are missing and lets
// them either continue without the partition or go back to install them.
func (wz *Wizard) warnDataTools(d disk.Disk, missing []string) {
	list := "- " + strings.Join(missing, "\n- ")
	body := widget.NewRichTextFromMarkdown(fmt.Sprintf(
		"The data partition can’t be created on this computer — these tools are "+
			"not installed:\n\n%s\n\nYou can create the USB **without** the data "+
			"partition (the image is still written and verified), or go back, "+
			"install them, and try again.", list))
	body.Wrapping = fyne.TextWrapWord

	dlg := dialog.NewCustomConfirm("Data partition tools missing",
		"Continue without data", "Go back", body, func(cont bool) {
			if cont {
				wz.startWrite(d, false)
			}
			// "Go back" just dismisses the dialog, leaving the confirm screen up.
		}, wz.win)
	dlg.Resize(fyne.NewSize(400, 260))
	dlg.Show()
}

// startWrite writes the image and moves to the done screen on success. withData
// asks for the exFAT data partition in the leftover space.
func (wz *Wizard) startWrite(d disk.Disk, withData bool) {
	bar := widget.NewProgressBar()
	status := widget.NewLabelWithStyle("Preparing…", fyne.TextAlignCenter, fyne.TextStyle{})
	note := canvas.NewText("Please leave the USB stick plugged in.", theme.Color(theme.ColorNamePlaceHolder))
	note.Alignment = fyne.TextAlignCenter
	body := container.NewVBox(bar, status, note)

	wz.show(screenWriting, theme.MediaPlayIcon(),
		"Creating your USB", diskName(d), body, nil)

	opts := disk.WriteOptions{DataPartition: withData, DataLabel: wz.dataLabel}
	go func() {
		res, err := disk.Write(context.Background(), wz.selPath, d, opts, func(p disk.WriteProgress) {
			fyne.Do(func() {
				// One continuous bar across the whole job, so 100% means the
				// image is written *and* verified rather than filling twice.
				bar.SetValue(p.Fraction())

				text := p.Phase
				// Writing and verifying both stream real byte counts.
				if p.Bytes > 0 && p.Total > 0 {
					text = fmt.Sprintf("%s · %s of %s", p.Phase,
						disk.FormatSize(p.Bytes), disk.FormatSize(p.Total))
				}
				status.SetText(text + "…")
			})
		})
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(err, wz.win)
				wz.showConfirm()
				return
			}
			wz.showDone(d, res)
		})
	}()
}

// ---- done ----

// showDone celebrates success and offers to make another or finish. The image
// write always succeeded here; the data partition may or may not have.
func (wz *Wizard) showDone(d disk.Disk, res disk.WriteResult) {
	msg := "Your FyshOS USB was written and verified.\nIt has been ejected — you can remove it now."
	switch res.Data {
	case disk.DataCreated:
		label := res.DataLabel
		if label == "" {
			label = wz.dataLabel
		}
		msg = fmt.Sprintf("Your FyshOS USB was written and verified, with data partition.\n" +
			"It has been ejected — you can remove it now.")
	case disk.DataFailed:
		// The image is fine; only the extra partition failed. Report it as
		// information, not an error.
		reason := res.DataWarn
		if reason == "" {
			reason = "the data partition could not be created"
		}
		dialog.ShowInformation("Data partition not created",
			"Your FyshOS USB was written and verified successfully.\n\n"+
				"The optional data partition was not created: "+reason+".", wz.win)
	}
	body := container.NewCenter(widget.NewLabelWithStyle(
		msg, fyne.TextAlignCenter, fyne.TextStyle{}))

	again := widget.NewButtonWithIcon("Make another", theme.ViewRefreshIcon(), func() {
		wz.reset()
		wz.showArch()
	})
	again.Importance = widget.LowImportance
	finish := widget.NewButtonWithIcon("Finish", theme.ConfirmIcon(), func() {
		wz.win.Close()
	})
	finish.Importance = widget.HighImportance

	footer := footerNav(again, finish)
	wz.show(screenDone, theme.ConfirmIcon(),
		"All done!", diskName(d)+" is ready", body, footer)
}
