package admin

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
	opus "gopkg.in/hraban/opus.v2"
)

//go:embed silero_vad.onnx
var sileroONNXBytes []byte

// Constants mirroring the Python reference (xiaozhi-esp32-server SileroVAD):
//   threshold=0.5, threshold_low=0.3, frame_window=5, frame_window_threshold=3
//   sample size = 512 samples @ 16kHz, context = 64 samples.
const (
	sileroSampleRate    = 16000
	sileroChunkSamples  = 512
	sileroContextSize   = 64
	sileroInputSize     = sileroChunkSamples + sileroContextSize // 576
	sileroStateSize     = 2 * 1 * 128                            // (2,1,128)
	sileroVoiceThresh   = 0.5
	sileroSilenceThresh = 0.3
	sileroWindowMax     = 5
	sileroWindowVote    = 3
)

var (
	sileroInitOnce sync.Once
	sileroInitErr  error
)

// initSileroRuntime initializes the global ONNX runtime exactly once.
// It looks for the shared library at:
//   1. XIAOLI_ONNXRUNTIME_PATH env var
//   2. /usr/local/lib/libonnxruntime.so (Linux default)
//   3. /opt/homebrew/lib/libonnxruntime.dylib or /usr/local/lib/libonnxruntime.dylib (macOS)
func initSileroRuntime() error {
	sileroInitOnce.Do(func() {
		path := os.Getenv("XIAOLI_ONNXRUNTIME_PATH")
		if path == "" {
			candidates := []string{
				"/usr/local/lib/libonnxruntime.so",
				"/usr/lib/x86_64-linux-gnu/libonnxruntime.so",
			}
			if runtime.GOOS == "darwin" {
				candidates = []string{
					"/opt/homebrew/lib/libonnxruntime.dylib",
					"/usr/local/lib/libonnxruntime.dylib",
				}
			}
			for _, c := range candidates {
				if _, err := os.Stat(c); err == nil {
					path = c
					break
				}
			}
		}
		if path == "" {
			sileroInitErr = fmt.Errorf("onnxruntime shared library not found; set XIAOLI_ONNXRUNTIME_PATH")
			return
		}
		ort.SetSharedLibraryPath(path)
		if err := ort.InitializeEnvironment(); err != nil {
			sileroInitErr = fmt.Errorf("onnxruntime init: %w", err)
			return
		}
		log.Printf("silero vad: onnxruntime loaded from %s", path)
	})
	return sileroInitErr
}

// SileroVAD is a per-connection voice-activity detector. Not goroutine-safe;
// the caller (deviceSession) must hold its own mutex.
type SileroVAD struct {
	session *ort.AdvancedSession
	inputT  *ort.Tensor[float32] // shape (1, 576)
	stateT  *ort.Tensor[float32] // shape (2, 1, 128)
	srT     *ort.Tensor[int64]   // scalar
	outProb *ort.Tensor[float32] // shape (1, 1)
	outStat *ort.Tensor[float32] // shape (2, 1, 128)

	decoder *opus.Decoder
	pcmBuf  []int16   // decoded PCM scratch (one Opus frame)
	pending []float32 // accumulated PCM samples waiting for next 512-sample chunk

	window      []bool
	lastIsVoice bool
}

// NewSileroVAD creates a per-session VAD instance.
func NewSileroVAD() (*SileroVAD, error) {
	if err := initSileroRuntime(); err != nil {
		return nil, err
	}
	v := &SileroVAD{
		pcmBuf:  make([]int16, sileroSampleRate*60/1000), // up to 960 samples per 60ms Opus frame
		pending: make([]float32, 0, sileroChunkSamples*4),
		window:  make([]bool, 0, sileroWindowMax),
	}
	dec, err := opus.NewDecoder(sileroSampleRate, 1)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}
	v.decoder = dec

	v.inputT, err = ort.NewEmptyTensor[float32](ort.NewShape(1, int64(sileroInputSize)))
	if err != nil {
		return nil, err
	}
	v.stateT, err = ort.NewEmptyTensor[float32](ort.NewShape(2, 1, 128))
	if err != nil {
		v.inputT.Destroy()
		return nil, err
	}
	v.srT, err = ort.NewTensor[int64](ort.NewShape(1), []int64{sileroSampleRate})
	if err != nil {
		v.inputT.Destroy()
		v.stateT.Destroy()
		return nil, err
	}
	v.outProb, err = ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
	if err != nil {
		v.inputT.Destroy()
		v.stateT.Destroy()
		v.srT.Destroy()
		return nil, err
	}
	v.outStat, err = ort.NewEmptyTensor[float32](ort.NewShape(2, 1, 128))
	if err != nil {
		v.inputT.Destroy()
		v.stateT.Destroy()
		v.srT.Destroy()
		v.outProb.Destroy()
		return nil, err
	}

	v.session, err = ort.NewAdvancedSessionWithONNXData(
		sileroONNXBytes,
		[]string{"input", "state", "sr"},
		[]string{"output", "stateN"},
		[]ort.Value{v.inputT, v.stateT, v.srT},
		[]ort.Value{v.outProb, v.outStat},
		nil,
	)
	if err != nil {
		v.inputT.Destroy()
		v.stateT.Destroy()
		v.srT.Destroy()
		v.outProb.Destroy()
		v.outStat.Destroy()
		return nil, fmt.Errorf("silero session: %w", err)
	}
	return v, nil
}

// Close releases all native resources.
func (v *SileroVAD) Close() {
	if v == nil {
		return
	}
	if v.session != nil {
		_ = v.session.Destroy()
		v.session = nil
	}
	for _, t := range []ort.Value{v.inputT, v.stateT, v.srT, v.outProb, v.outStat} {
		if t != nil {
			_ = t.Destroy()
		}
	}
	v.inputT, v.stateT, v.srT, v.outProb, v.outStat = nil, nil, nil, nil, nil
}

// Detect decodes one Opus packet and runs Silero on every full 512-sample
// chunk it contains. Returns (isVoice, anyChunkProcessed, lastProb).
// isVoice is the sliding-window vote: true iff ≥3 of the last 5 chunks were voiced.
// lastProb is the most recent raw probability (useful for logging); -1 if no chunk ran.
func (v *SileroVAD) Detect(opusPayload []byte) (isVoice bool, ran bool, lastProb float32) {
	lastProb = -1
	if v == nil || v.session == nil {
		return false, false, lastProb
	}
	n, err := v.decoder.Decode(opusPayload, v.pcmBuf)
	if err != nil || n <= 0 {
		if err != nil {
			log.Printf("silero vad: opus decode err: %v (payload=%d)", err, len(opusPayload))
		}
		// On decode failure return the current window state without modification.
		return v.windowVote(), false, lastProb
	}
	// Append decoded samples (normalised float32) to pending buffer.
	for i := 0; i < n; i++ {
		v.pending = append(v.pending, float32(v.pcmBuf[i])/32768.0)
	}

	inputData := v.inputT.GetData()    // length 576
	stateData := v.stateT.GetData()    // length 256
	stateOut := v.outStat.GetData()    // length 256
	probData := v.outProb.GetData()    // length 1

	for len(v.pending) >= sileroChunkSamples {
		// inputData layout: [context (64) | chunk (512)]
		// Context = trailing 64 samples of the *previous* full input (preserved
		// across calls by leaving inputData[sileroChunkSamples:] from last run).
		// First-time call: context starts as zeros (NewEmptyTensor zero-fills).
		// We need to carry context forward from the previous input's tail.
		copy(inputData[:sileroContextSize], inputData[sileroInputSize-sileroContextSize:])
		copy(inputData[sileroContextSize:], v.pending[:sileroChunkSamples])
		v.pending = v.pending[sileroChunkSamples:]

		if err := v.session.Run(); err != nil {
			log.Printf("silero vad: session.Run err: %v", err)
			return v.windowVote(), false, lastProb
		}
		prob := probData[0]
		lastProb = prob
		// Carry state forward.
		copy(stateData, stateOut)

		var thisVoiced bool
		switch {
		case prob >= sileroVoiceThresh:
			thisVoiced = true
		case prob <= sileroSilenceThresh:
			thisVoiced = false
		default:
			thisVoiced = v.lastIsVoice
		}
		v.lastIsVoice = thisVoiced
		v.window = append(v.window, thisVoiced)
		if len(v.window) > sileroWindowMax {
			v.window = v.window[len(v.window)-sileroWindowMax:]
		}
		ran = true
	}
	return v.windowVote(), ran, lastProb
}

func (v *SileroVAD) windowVote() bool {
	count := 0
	for _, b := range v.window {
		if b {
			count++
		}
	}
	return count >= sileroWindowVote
}
