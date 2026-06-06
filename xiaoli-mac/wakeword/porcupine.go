package wakeword

import (
	"context"
	"errors"
	"fmt"
)

// PorcupineDetector is a stub for Picovoice Porcupine. The full
// implementation requires a paid access key and a `.ppn` keyword
// file; it is left as a stub here so the wiring is in place. To
// enable it, run `go get github.com/Picovoice/porcupine` and replace
// the body of Run.
type PorcupineDetector struct {
	Keyword   string
	AccessKey string
}

// NewPorcupine returns a stub detector. The real implementation
// would call porcupine.NewPorcupine(...).
func NewPorcupine(keyword, accessKey string) (*PorcupineDetector, error) {
	if keyword == "" || accessKey == "" {
		return nil, errors.New("porcupine: keyword and accessKey are required")
	}
	return &PorcupineDetector{Keyword: keyword, AccessKey: accessKey}, nil
}

// Run is a placeholder. It returns an error so the operator notices
// that the real model is not wired up. Replace with the real
// Porcupine binding when ready.
func (p *PorcupineDetector) Run(ctx context.Context, frames <-chan []int16, onWake Callback) error {
	return fmt.Errorf("porcupine detector is a stub: install the Picovoice Go binding to enable it")
}
