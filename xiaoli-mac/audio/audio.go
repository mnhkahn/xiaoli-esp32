// Package audio is the Mac analog of xiaozhi-esp32/main/audio.
// It runs three concurrent goroutines:
//
//   1. capture:   PortAudio mic  → pcmIn   (16kHz, mono, int16)
//   2. encode:    pcmIn          → opusOut (60ms OPUS frames)
//   3. decode:    opusIn         → pcmOut  (server TTS audio)
//   4. playback:  pcmOut         → PortAudio speaker
//
// The encode side is gated on the device being in Listening mode.
// The decode side is gated on the server sending a tts.start frame
// and stops at tts.stop. This matches audio_service.cc.
package audio

import (
	"log"
	"sync"

	"github.com/gordonklaus/portaudio"
)

// FrameDurationMS is the OPUS frame size. The server expects 60ms
// frames (see directDeviceAudioFrameDurationMS in direct_device.go).
const FrameDurationMS = 60

// SampleRate is the PCM sample rate for both capture and playback.
const SampleRate = 16000

// Channels is fixed to mono to match the server.
const Channels = 1

// samplesPerFrame is the number of int16 samples that make up one
// OPUS frame at the configured rate and duration.
const samplesPerFrame = SampleRate * FrameDurationMS / 1000 // 960

// PortAudio is a process-wide singleton library, not a per-stream
// resource. Per the upstream docs, Pa_Terminate() deallocates "all
// resources allocated by PortAudio since it was initialized" and
// "will automatically close any PortAudio streams that are still
// open". The previous code called Terminate() from every stream's
// cleanup goroutine; that races with the next OpenStream() and
// leaves the new stream in a half-initialized state, which is the
// root cause of the garbled TTS the user reported on the Mac client
// (the ESP32 board uses hardware I2S and has no equivalent global
// state). We now initialize once at first use and terminate exactly
// once at process exit.
var (
	paInitOnce sync.Once
	paTermOnce sync.Once
)

// Initialize is the process-wide PortAudio initializer. Safe to
// call from any goroutine; only the first call has any effect.
func Initialize() {
	paInitOnce.Do(func() {
		if err := portaudio.Initialize(); err != nil {
			log.Printf("[audio] portaudio init: %v", err)
		}
	})
}

// Terminate is the process-wide PortAudio finalizer. Must be
// called once at process exit (see main.go's defer) so the OS
// doesn't have to wait for a reboot to free the audio device.
// Never call it from a stream's cleanup goroutine — see the
// comment on the package-level paTermOnce for why.
func Terminate() {
	paTermOnce.Do(func() {
		_ = portaudio.Terminate()
	})
}
