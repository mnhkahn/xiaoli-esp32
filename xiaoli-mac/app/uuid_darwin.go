// Package app wires every component together and runs the single
// main goroutine. The pattern matches the ESP32 application task:
// every event (network, state, audio) is funneled through one
// goroutine that calls Display methods serially.
package app

import (
	"bytes"
	"log"
	"os/exec"
	"strings"
)

// hardwareUUID returns a stable per-Mac identifier from
// `ioreg -rd1 -c IOPlatformExpertDevice`. Falls back to hostname.
func hardwareUUID() string {
	out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		// Fall back to hostname; the server treats unknown ids
		// as unauthorized, which is the safe default.
		if h, herr := exec.Command("hostname").Output(); herr == nil {
			return "mac-" + strings.TrimSpace(string(h))
		}
		return ""
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if bytes.Contains(line, []byte("IOPlatformUUID")) {
			parts := strings.SplitN(string(line), "=", 2)
			if len(parts) != 2 {
				continue
			}
			id := strings.Trim(strings.TrimSpace(parts[1]), `"`)
			if id != "" {
				return id
			}
		}
	}
	if h, err := exec.Command("hostname").Output(); err == nil {
		return "mac-" + strings.TrimSpace(string(h))
	}
	log.Printf("[app] could not determine hardware UUID")
	return ""
}
