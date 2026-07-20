package main

import (
	_ "embed"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"

	"github.com/fyshos/ferry/internal/ui"
)

//go:embed img/Icon.png
var iconPNG []byte

func main() {
	a := app.NewWithID("com.fyshos.ferry")
	icon := fyne.NewStaticResource("FyshOS.png", iconPNG)
	a.SetIcon(icon)

	w := a.NewWindow("Ferry — FyshOS USB creator")
	wiz := ui.NewWizard(a, w, icon)
	w.SetContent(wiz.BuildUI())
	w.Resize(fyne.NewSize(480, 500))
	wiz.Start()

	w.ShowAndRun()
}
