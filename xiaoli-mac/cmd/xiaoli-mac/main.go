// xiaoli-mac is the Mac port of the ESP32 board firmware. It is a
// Go rewrite that runs the same protocol, state machine and display
// pipeline, but with PortAudio in place of the ESP32 codec and a
// Fyne-based UI in place of LVGL.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"xiaoli/mac/app"
	"xiaoli/mac/config"
)

func main() {
	cfgPath := flag.String("config", envOr("XIAOLI_MAC_CONFIG", "xiaoli-mac.json"),
		"path to the device config JSON file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("server=%s wake=%s", cfg.Server, cfg.WakeWord.Engine)

	a := app.New(cfg)
	if cfg.RecordTTS != "" {
		path := expandRecordPath(cfg.RecordTTS)
		if err := a.SetTTSRecorder(path); err != nil {
			log.Fatalf("record-tts: %v", err)
		}
		log.Printf("record-tts=%s (raw OPUS frames, append mode)", path)
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := a.Run(ctx); err != nil && err != context.Canceled {
		log.Printf("exit: %v", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// expandRecordPath substitutes the literal token "{ts}" in a
// configured record-tts path with a session-start timestamp, so
// every run lands in a fresh file. Paths without the token are
// returned unchanged (and will be appended-to on next run, which
// is the caller's choice).
func expandRecordPath(p string) string {
	if !strings.Contains(p, "{ts}") {
		return p
	}
	ts := time.Now().Format("20060102-150405")
	out := strings.ReplaceAll(p, "{ts}", ts)
	_ = os.MkdirAll(filepath.Dir(out), 0o755)
	return out
}
