// Package state mirrors xiaozhi-esp32/main/device_state.h and
// device_state_machine.{h,cc}.
//
// State values, transition rules and observer API are deliberately kept
// identical to the ESP32 firmware so the Go device can be cross-checked
// against the C++ reference.
package state

// State is the device state. Values match the C enum DeviceState exactly.
type State int

const (
	Unknown        State = iota
	Starting                // kDeviceStateStarting
	WifiConfiguring         // kDeviceStateWifiConfiguring (kept for parity; unused on Mac)
	Idle                    // kDeviceStateIdle
	Connecting              // kDeviceStateConnecting
	Listening               // kDeviceStateListening
	Speaking                // kDeviceStateSpeaking
	Upgrading               // kDeviceStateUpgrading
	Activating              // kDeviceStateActivating
	AudioTesting            // kDeviceStateAudioTesting
	FatalError              // kDeviceStateFatalError
)

var stateNames = [...]string{
	"unknown",
	"starting",
	"wifi_configuring",
	"idle",
	"connecting",
	"listening",
	"speaking",
	"upgrading",
	"activating",
	"audio_testing",
	"fatal_error",
	"invalid_state",
}

func (s State) String() string {
	if s < 0 || int(s) >= len(stateNames)-1 {
		return stateNames[len(stateNames)-1]
	}
	return stateNames[s]
}

// IsValid reports whether s is a known non-fatal state.
func (s State) IsValid() bool {
	return s >= Starting && s <= AudioTesting
}
