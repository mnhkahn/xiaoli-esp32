//go:build darwin

package fynegui

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit

#include <AppKit/AppKit.h>

// NSFloatingWindowLevel = 3. Keeps the window above normal windows
// without forcing it above the dock or modal alerts.
static void setWindowLevel(void *window, int level) {
	NSWindow *win = (__bridge NSWindow *)window;
	[win setLevel:level];
}
*/
import "C"

import (
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
)

// setAlwaysOnTop raises the window to NSFloatingWindowLevel so it
// stays above the rest of the desktop. The user kept losing the
// window behind other apps; this is a no-op on non-macOS.
func setAlwaysOnTop(w fyne.Window) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return
	}
	nw.RunNative(func(ctx any) {
		mc, ok := ctx.(driver.MacWindowContext)
		if !ok {
			return
		}
		C.setWindowLevel(unsafe.Pointer(mc.NSWindow), C.int(3))
	})
}
