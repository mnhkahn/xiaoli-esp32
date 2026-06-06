//go:build !darwin

package fynegui

import "fyne.io/fyne/v2"

// setAlwaysOnTop is a no-op on non-macOS. The macOS build calls
// NSWindow's setLevel:NSFloatingWindowLevel via CGo.
func setAlwaysOnTop(w fyne.Window) {}
