// Package fynegui provides a Fyne-based Display implementation. The
// window layout mirrors the ESP32 board's UI: a centered status
// label, a face/emoji, and a scrolling chat list. All widget updates
// are routed through fyne.Do so the methods are safe to call from any
// goroutine (the protocol dispatcher or audio pipeline).
package fynegui

import (
	"image/color"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"xiaoli/mac/display"
)

// Display wraps a Fyne window and implements display.Display.
type Display struct {
	fyneApp fyne.App
	window  fyne.Window

	mu sync.Mutex

	statusText  *widget.Label
	faceEmoji   *canvas.Text
	emotionName *widget.Label
	chatBox     *fyne.Container
	chatScroll  *container.Scroll
	toastLabel  *widget.Label
	toastHideID int
	bubbles     []display.ChatMessage
	themeName   string

	listenBtn     *widget.Button
	onPressListen func()
}

// New builds the window. The returned Display is ready to use; call
// Show to make the window visible.
func New() *Display {
	a := app.New()
	a.SetIcon(nil)
	w := a.NewWindow("小李")
	w.Resize(fyne.NewSize(420, 640))

	d := &Display{
		fyneApp:     a,
		window:      w,
		statusText:  widget.NewLabel("待命"),
		faceEmoji:   canvas.NewText("😐", color.Black),
		emotionName: widget.NewLabel("neutral"),
		chatBox:     container.New(layout.NewCustomPaddedVBoxLayout(8)),
		toastLabel:  widget.NewLabel(""),
		bubbles:     nil,
		themeName:   "light",
	}

	// Big emoji in the center. 2cm wide ≈ 76 DIPs (1 DIP = 1/96 inch).
	const faceEmojiSize float32 = 76
	d.faceEmoji.Alignment = fyne.TextAlignCenter
	d.faceEmoji.TextStyle = fyne.TextStyle{Bold: true}
	d.faceEmoji.TextSize = faceEmojiSize
	d.faceEmoji.Resize(fyne.NewSize(faceEmojiSize, faceEmojiSize))

	// Status bar — text only, matching the ESP32 board which just
	// does lv_label_set_text() in LvglDisplay::SetStatus().
	d.statusText.Alignment = fyne.TextAlignCenter
	d.statusText.TextStyle = fyne.TextStyle{Bold: true}

	// Wrap face + emotion in a fixed-height stage.
	stage := container.NewVBox(
		container.NewCenter(d.faceEmoji),
		container.NewCenter(d.emotionName),
	)

	// Chat scroll.
	d.chatScroll = container.NewVScroll(d.chatBox)

	// "Press to talk" button (Mac-only — ESP32 has no equivalent in
	// the display). Pressing it forwards to the onPressListen
	// callback, which the app wires up to a state transition.
	d.listenBtn = widget.NewButton("按住说话", func() {
		if d.onPressListen != nil {
			d.onPressListen()
		}
	})
	d.listenBtn.Importance = widget.HighImportance
	d.listenBtn.Resize(fyne.NewSize(0, 56))

	// Toast overlay: hidden by default.
	d.toastLabel.Hide()

	// listenBtn is pinned to the bottom of the window so the chat
	// scroll gets all the middle space. The toast is shown just
	// Inner Border gives the chat scroll whatever vertical space is left
	// after the face stage. Using a VBox here would size chatScroll to its
	// content (a single bubble ≈ 50 DIP) and waste the rest.
	content := container.NewBorder(
		d.statusText, // top — status text only, mirrors the ESP32 board
		d.listenBtn,  // bottom (fixed)
		nil, nil,
		container.NewBorder(
			container.NewVBox(
				d.toastLabel,
				stage,
				widget.NewSeparator(),
			),
			nil, // no inner bottom — chat fills the gap above the listen button
			nil, nil,
			d.chatScroll, // center: expands to fill remaining height
		),
	)
	w.SetContent(content)
	return d
}

// Show makes the window visible. The Fyne main loop is started by Run.
func (d *Display) Show() {
	d.window.Show()
}

// Run blocks until the window is closed. The fyneApp's internal loop
// drives the UI; the protocol dispatcher and audio pipeline can call
// Display methods from any goroutine — they are marshalled onto the
// Fyne thread via fyne.Do.
func (d *Display) Run() {
	d.fyneApp.Run()
}

// ----- display.Display implementation -----

func (d *Display) SetStatus(status string) {
	fyne.Do(func() {
		d.statusText.SetText(status)
	})
}

func (d *Display) ShowNotification(text string, durationMs int) {
	if durationMs <= 0 {
		durationMs = 3000
	}
	fyne.Do(func() {
		d.toastLabel.SetText(text)
		d.toastLabel.Show()
		d.cancelPendingToast()
		d.toastHideID = scheduleAfter(durationMs, func() {
			d.toastLabel.Hide()
		})
	})
}

func (d *Display) SetEmotion(emotion string) {
	fyne.Do(func() {
		d.faceEmoji.Text = emojiFor(emotion)
		d.faceEmoji.Refresh()
		d.emotionName.SetText(emotion)
	})
}

func (d *Display) SetChatMessage(role display.Role, content string) {
	fyne.Do(func() {
		d.bubbles = append(d.bubbles, display.ChatMessage{
			Role:      role,
			Content:   content,
			Timestamp: time.Now(),
		})
		d.renderBubbles()
	})
}

func (d *Display) SetChatMessages(messages []display.ChatMessage) {
	fyne.Do(func() {
		d.bubbles = append(d.bubbles[:0:0], messages...)
		d.renderBubbles()
	})
}

func (d *Display) ClearChatMessages() {
	fyne.Do(func() {
		d.bubbles = nil
		d.renderBubbles()
	})
}

func (d *Display) SetTheme(theme *display.Theme) {
	if theme == nil {
		return
	}
	fyne.Do(func() {
		d.themeName = theme.Name
		// Fyne 2.x has no runtime theme switch, but we record the
		// preference for persistence and a future theme selector.
	})
}

func (d *Display) SetPowerSaveMode(on bool) {
	fyne.Do(func() {
		if on {
			d.window.Canvas().SetOnTypedRune(nil) // placeholder
		}
	})
}

func (d *Display) SetOnPressListen(fn func()) {
	d.mu.Lock()
	d.onPressListen = fn
	d.mu.Unlock()
}

func (d *Display) SetListenButtonState(label string, enabled bool) {
	fyne.Do(func() {
		d.listenBtn.SetText(label)
		if enabled {
			d.listenBtn.Enable()
		} else {
			d.listenBtn.Disable()
		}
	})
}

// ----- helpers -----

func (d *Display) renderBubbles() {
	d.chatBox.RemoveAll()
	// Cap history to avoid unbounded growth.
	if len(d.bubbles) > 200 {
		d.bubbles = d.bubbles[len(d.bubbles)-200:]
	}
	for _, m := range d.bubbles {
		d.chatBox.Add(bubbleFor(m.Role, m.Content, m.Timestamp))
	}
	d.chatBox.Refresh()
	// Refresh the scroll container so it re-measures the new content
	// height; without this the bubbles are added but the visible
	// area stays unchanged.
	d.chatScroll.Refresh()
	// Scroll to bottom.
	d.chatScroll.ScrollToBottom()
}

func (d *Display) cancelPendingToast() {
	if d.toastHideID != 0 {
		cancelTimer(d.toastHideID)
		d.toastHideID = 0
	}
}

func bubbleFor(role display.Role, content string, ts time.Time) fyne.CanvasObject {
	bg := bubbleColor(role)
	label := widget.NewLabel(content)
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignLeading

	// Fyne's bundled Latin font can't measure CJK glyphs, so the
	// label reports MinSize.Width=0 and the bubble collapses to a
	// thin sliver. Estimate the rendered width and pin it onto the
	// rectangle (which has SetMinSize); the label inside NewPadded
	// gets (rect width − 8 DIPs of padding) and renders correctly.
	// Capped at 280 so long sentences wrap onto multiple lines
	// instead of blowing past the chat area width.
	const maxBubbleWidth float32 = 280
	est := estimatedTextWidth(content)
	if est > maxBubbleWidth {
		est = maxBubbleWidth
	}
	rect := canvas.NewRectangle(bg)
	rect.CornerRadius = 8
	rect.SetMinSize(fyne.NewSize(est, 1))
	bubble := container.NewStack(
		rect,
		container.NewPadded(label),
	)

	// Small direction + timestamp tag above the bubble: user STT
	// is the local mic transcript (server's ASR of what the user
	// just said), assistant TTS and `custom` payloads both come
	// from the server. The arrow + label + clock make the source
	// and timing unambiguous so mixed-direction transcripts don't
	// read as one continuous conversation.
	row := container.NewVBox(directionLabel(role, ts), bubble)

	if role == display.RoleUser {
		return container.NewHBox(layout.NewSpacer(), row)
	}
	if role == display.RoleAssistant {
		return container.NewHBox(row, layout.NewSpacer())
	}
	return row // system: center
}

// directionLabel returns the small "↑ 本地 10:16" / "↓ 服务端 10:16"
// tag shown above each chat bubble. Alignment follows the role so
// the tag sits directly over the bubble rather than at the row's
// left edge. ts is rendered in local 24h HH:MM.
func directionLabel(role display.Role, ts time.Time) *widget.Label {
	var text string
	var align fyne.TextAlign
	switch role {
	case display.RoleUser:
		text = "↑ 本地 " + ts.Format("15:04")
		align = fyne.TextAlignTrailing
	case display.RoleAssistant:
		text = "↓ 服务端 " + ts.Format("15:04")
		align = fyne.TextAlignLeading
	default: // RoleSystem
		text = "↓ 服务端 " + ts.Format("15:04")
		align = fyne.TextAlignCenter
	}
	l := widget.NewLabel(text)
	l.Alignment = align
	l.TextStyle = fyne.TextStyle{Italic: true}
	return l
}

// estimatedTextWidth approximates the rendered width of a string in
// the Fyne default font (14pt). The real measurement goes through
// font.ParseTTF, which falls back to a zero-width placeholder for
// any glyph the bundled font can't shape (i.e. CJK), so we count
// characters by hand: CJK rune ≈ 22 DIPs, ASCII ≈ 8 DIPs. Empirically
// 22 is just over the actual rendered CJK glyph width on macOS so a
// short line like "今天北京天气。" stays on one line; lower values
// (15) caused the label to wrap inside the bubble.
func estimatedTextWidth(s string) float32 {
	var w float32
	for _, r := range s {
		if r > 127 {
			w += 22
		} else {
			w += 8
		}
	}
	return w
}

func bubbleColor(role display.Role) color.Color {
	switch role {
	case display.RoleUser:
		return color.NRGBA{R: 0x4F, G: 0x8C, B: 0xFF, A: 0xFF}
	case display.RoleAssistant:
		return color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	case display.RoleSystem:
		return color.NRGBA{R: 0xFF, G: 0xF7, B: 0xD6, A: 0xFF}
	}
	return color.Black
}

// emojiFor mirrors the C++ Twemoji emotion collection. The macOS
// system font renders these natively.
func emojiFor(name string) string {
	switch name {
	case "happy":
		return "🙂"
	case "sad":
		return "🙁"
	case "laughing":
		return "😆"
	case "angry":
		return "😠"
	case "crying":
		return "😢"
	case "loving":
		return "😍"
	case "embarrassed":
		return "😳"
	case "surprised":
		return "😮"
	case "shocked":
		return "😱"
	case "thinking":
		return "🤔"
	case "winking":
		return "😉"
	case "cool":
		return "😎"
	case "relaxed":
		return "😌"
	case "delicious":
		return "😋"
	case "kissy":
		return "😘"
	case "confident":
		return "😏"
	case "sleepy":
		return "😴"
	case "silly":
		return "🤪"
	case "confused":
		return "😕"
	default:
		return "😐"
	}
}
