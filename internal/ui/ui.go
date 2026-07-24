// Package ui presents Ferry as a friendly step-by-step wizard for turning a
// FyshOS ISO into a bootable USB stick.
package ui

import (
	_ "embed"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/fyshos/ferry/internal/cache"
	"github.com/fyshos/ferry/internal/disk"
	"github.com/fyshos/ferry/internal/releases"
)

// screen identifies which wizard step is currently on show, so background loads
// know whether they should refresh what the user is looking at.
type screen int

const (
	screenArch screen = iota
	screenImage
	screenDisk
	screenConfirm
	screenDownloading // progress view, belongs to the image step
	screenWriting     // progress view, belongs to the write step
	screenDone
)

// The wizard has four visible steps: arch, image, disk, write.
const totalSteps = 4

var (
	//go:embed "boxed.png"
	boxedPng []byte

	resourceBoxedPng = fyne.NewStaticResource("boxed.png", boxedPng)
)

// stepDot returns which of the four progress dots to light for a screen. The two
// progress views belong to the step that launched them — downloading to the
// image step, writing to the final step — rather than to a dot of their own.
func stepDot(s screen) int {
	switch s {
	case screenArch:
		return 0
	case screenImage, screenDownloading:
		return 1
	case screenDisk:
		return 2
	case screenConfirm, screenWriting, screenDone:
		return 3
	}
	return 0
}

// Wizard owns the window and walks the user through the workflow one screen at a
// time.
type Wizard struct {
	app  fyne.App
	win  fyne.Window
	icon fyne.Resource

	rel *releases.Client
	cch *cache.Cache

	// selection state, carried between screens
	arch    releases.Arch
	images  []releases.Image // all supported images, newest first (once loaded)
	loading bool             // release list still being fetched

	selPath string // local path of the chosen image
	selName string // its display name
	selSize int64  // its size in bytes

	disks   []disk.Disk
	selDisk *disk.Disk

	// data partition options, chosen on the confirm screen
	wantData  bool   // add an exFAT data partition in the leftover space
	dataLabel string // its volume label

	current screen
	body    *fyne.Container // the swappable screen content
}

// NewWizard creates a wizard bound to the given app, window and brand icon.
func NewWizard(app fyne.App, win fyne.Window, icon fyne.Resource) *Wizard {
	wz := &Wizard{
		app:       app,
		win:       win,
		icon:      icon,
		rel:       &releases.Client{},
		arch:      releases.HostArch(),
		loading:   true,
		wantData:  true, // offered pre-ticked when the stick has room
		dataLabel: disk.DefaultDataLabel,
	}
	if cch, err := cache.New(app); err == nil {
		wz.cch = cch
	}
	return wz
}

// BuildUI returns the window content: a single container whose contents we swap
// as the user moves between steps.
func (wz *Wizard) BuildUI() fyne.CanvasObject {
	bg := canvas.NewImageFromResource(resourceBoxedPng)
	bg.FillMode = canvas.ImageFillContain
	bg.Translucency = 0.9
	wz.body = container.NewStack()
	wz.showArch()
	return container.NewStack(bg, wz.body)
}

// Start kicks off the background load of the available images.
func (wz *Wizard) Start() {
	go func() {
		imgs, err := wz.rel.List()
		fyne.Do(func() {
			wz.loading = false
			if err == nil {
				wz.images = imgs
			}
			// If the user is looking at the image step, refresh it so the
			// latest-image suggestion appears as soon as it is known.
			if wz.current == screenImage {
				wz.showImage()
			}
		})
	}()
}

// ---- screen scaffolding ----

// setBody swaps the visible screen content.
func (wz *Wizard) setBody(o fyne.CanvasObject) {
	wz.body.Objects = []fyne.CanvasObject{o}
	wz.body.Refresh()
}

// show lays out a standard wizard page: a header (icon, title, subtitle), the
// step's body, and a footer of navigation buttons with progress dots beneath.
func (wz *Wizard) show(s screen, icon fyne.Resource, title, subtitle string, body, footer fyne.CanvasObject) {
	wz.current = s

	header := wz.header(icon, title, subtitle)

	dots := stepDots(stepDot(s), totalSteps)
	var bottom fyne.CanvasObject = dots
	if footer != nil {
		bottom = container.NewVBox(footer, dots)
	}

	page := container.NewBorder(header, bottom, nil, nil, body)
	wz.setBody(container.NewPadded(page))
}

// header builds the centred icon, title and subtitle at the top of a screen.
func (wz *Wizard) header(icon fyne.Resource, title, subtitle string) fyne.CanvasObject {
	items := []fyne.CanvasObject{}
	if icon != nil {
		items = append(items, container.NewCenter(bigIcon(icon)))
	}
	t := canvas.NewText(title, theme.Color(theme.ColorNameForeground))
	t.TextSize = 22
	t.TextStyle = fyne.TextStyle{Bold: true}
	t.Alignment = fyne.TextAlignCenter
	items = append(items, t)
	if subtitle != "" {
		s := canvas.NewText(subtitle, theme.Color(theme.ColorNamePlaceHolder))
		s.TextSize = 14
		s.Alignment = fyne.TextAlignCenter
		items = append(items, s)
	}
	items = append(items, widget.NewSeparator())
	return container.NewVBox(items...)
}

// footerNav builds a Back/primary button row. Either side may be nil.
func footerNav(back fyne.CanvasObject, right fyne.CanvasObject) fyne.CanvasObject {
	left := back
	if left == nil {
		left = layout.NewSpacer()
	}
	r := right
	if r == nil {
		r = layout.NewSpacer()
	}
	return container.NewBorder(nil, nil, left, r, layout.NewSpacer())
}

// backButton makes a low-key Back button running the given action.
func backButton(onTap func()) *widget.Button {
	b := widget.NewButtonWithIcon("Back", theme.NavigateBackIcon(), onTap)
	b.Importance = widget.LowImportance
	return b
}

// ---- shared selection helpers ----

// setImage records the chosen image for the following steps.
func (wz *Wizard) setImage(path, name string, size int64) {
	wz.selPath = path
	wz.selName = name
	wz.selSize = size
}

// reset clears the per-run selections (keeping the chosen architecture) so the
// user can make another stick.
func (wz *Wizard) reset() {
	wz.selPath, wz.selName, wz.selSize = "", "", 0
	wz.selDisk = nil
	wz.wantData = true
	wz.dataLabel = disk.DefaultDataLabel
}
