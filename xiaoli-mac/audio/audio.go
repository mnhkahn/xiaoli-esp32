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
