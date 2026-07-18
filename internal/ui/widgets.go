package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// bigIcon renders a resource at a friendly, fixed size for screen headers.
func bigIcon(res fyne.Resource) fyne.CanvasObject {
	img := canvas.NewImageFromResource(res)
	img.FillMode = canvas.ImageFillContain
	return container.NewGridWrap(fyne.NewSize(72, 72), img)
}

// stepDots draws a row of dots, one per step, along the bottom of each screen.
// Inactive steps are small grey dots; the current step is a wider primary-colour
// pill so the user's place stands out at a glance.
func stepDots(active, total int) fyne.CanvasObject {
	const h = 9 // dot height, and the diameter of an inactive dot

	on := theme.Color(theme.ColorNamePrimary)
	off := theme.Color(theme.ColorNameDisabled)

	dots := []fyne.CanvasObject{layout.NewSpacer()}
	for i := 0; i < total; i++ {
		pill := canvas.NewRectangle(off)
		pill.CornerRadius = float32(h) / 2 // fully rounded ends
		w := float32(h)
		if i == active {
			pill.FillColor = on
			w = h * 2.6 // the current step reads as a wider pill
		}
		dots = append(dots, container.NewGridWrap(fyne.NewSize(w, h), pill))
	}
	dots = append(dots, layout.NewSpacer())
	return container.NewPadded(container.NewHBox(dots...))
}

// optionRow is a large, tappable choice: an icon, a bold title and a smaller
// description, with a trailing chevron (or a tick when selected) and a hover
// highlight. It is the friendly building block of every choice screen.
type optionRow struct {
	widget.BaseWidget
	icon     fyne.Resource
	title    string
	desc     string
	onTap    func()
	selected bool

	bg    *canvas.Rectangle
	trail *widget.Icon
}

func newOptionRow(icon fyne.Resource, title, desc string, onTap func()) *optionRow {
	r := &optionRow{icon: icon, title: title, desc: desc, onTap: onTap}
	r.ExtendBaseWidget(r)
	return r
}

// setSelected marks the row as the current choice, showing a tick and a tinted
// background.
func (r *optionRow) setSelected(sel bool) {
	r.selected = sel
	if r.bg == nil {
		return
	}
	r.refreshState()
}

func (r *optionRow) refreshState() {
	switch {
	case r.selected:
		r.bg.FillColor = theme.Color(theme.ColorNameSelection)
		r.trail.SetResource(theme.ConfirmIcon())
	case r.onTap != nil:
		r.bg.FillColor = color.Transparent
		r.trail.SetResource(theme.NavigateNextIcon())
	default:
		// No action and not selected: an informational row. Drop the chevron
		// and hover so it does not look tappable.
		r.bg.FillColor = color.Transparent
		r.trail.SetResource(nil)
	}
	r.bg.Refresh()
}

func (r *optionRow) CreateRenderer() fyne.WidgetRenderer {
	r.bg = canvas.NewRectangle(color.Transparent)
	r.bg.CornerRadius = theme.Size(theme.SizeNameInputRadius)

	icon := widget.NewIcon(r.icon)
	text := widget.NewRichTextFromMarkdown("### " + r.title + "\n" + r.desc)
	text.Wrapping = fyne.TextWrapWord
	r.trail = widget.NewIcon(theme.NavigateNextIcon())

	row := container.NewBorder(nil, nil,
		container.NewPadded(icon), container.NewPadded(r.trail), text)
	content := container.NewStack(r.bg, container.NewPadded(row))
	r.refreshState()
	return widget.NewSimpleRenderer(content)
}

func (r *optionRow) Tapped(*fyne.PointEvent) {
	if r.onTap != nil {
		r.onTap()
	}
}

func (r *optionRow) MouseIn(*desktop.MouseEvent) {
	if r.selected || r.onTap == nil {
		return
	}
	r.bg.FillColor = theme.Color(theme.ColorNameHover)
	r.bg.Refresh()
}

func (r *optionRow) MouseMoved(*desktop.MouseEvent) {}

func (r *optionRow) MouseOut() {
	if r.selected || r.onTap == nil {
		return
	}
	r.bg.FillColor = color.Transparent
	r.bg.Refresh()
}
