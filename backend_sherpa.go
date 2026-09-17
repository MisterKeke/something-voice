//go:build cgo && (windows || linux || darwin)

package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

type localBackend struct{}

func newBackend() (backend, error) {
	return localBackend{}, nil
}

func (localBackend) run(ctx context.Context, config Config, emit func(recognitionEvent) error, ready func(error)) error {
	if err := validateModelFiles(config.Model); err != nil {
		return err
	}

	sherpaConfig := sherpa.OnlineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{
			SampleRate: config.SampleRate,
			FeatureDim: config.FeatureDim,
		},
		ModelConfig: sherpa.OnlineModelConfig{
			Transducer: sherpa.OnlineTransducerModelConfig{
				Encoder: config.Model.Encoder,
				Decoder: config.Model.Decoder,
				Joiner:  config.Model.Joiner,
			},
			Tokens:     config.Model.Tokens,
			NumThreads: config.Model.NumThreads,
			Provider:   config.Model.Provider,
			ModelType:  config.Model.ModelType,
		},
		DecodingMethod:          "greedy_search",
		MaxActivePaths:          4,
		EnableEndpoint:          1,
		Rule1MinTrailingSilence: 2.4,
		Rule2MinTrailingSilence: 1.2,
		Rule3MinUtteranceLength: 20,
	}

	recognizer := sherpa.NewOnlineRecognizer(&sherpaConfig)
	if recognizer == nil {
		return fmt.Errorf("%w: sherpa-onnx returned a nil recognizer", ErrRecognizerInit)
	}
	defer sherpa.DeleteOnlineRecognizer(recognizer)
	stream := sherpa.NewOnlineStream(recognizer)
	if stream == nil {
		return fmt.Errorf("%w: sherpa-onnx returned a nil stream", ErrRecognizerInit)
	}
	defer sherpa.DeleteOnlineStream(stream)

	audioContext, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("%w: initialize audio context: %v", ErrMicrophonePermission, err)
	}
	defer func() {
		_ = audioContext.Uninit()
		audioContext.Free()
	}()

	selectedID, err := selectMicrophone(audioContext.Context, config.Microphone)
	if err != nil {
		return err
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.SampleRate = uint32(config.SampleRate)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = 1
	if selectedID != nil {
		deviceConfig.Capture.DeviceID = selectedID.Pointer()
	}

	audioQueue := make(chan []byte, config.AudioQueueSize)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var overload atomic.Bool
	var disconnected atomic.Bool
	var stopping atomic.Bool

	callbacks := malgo.DeviceCallbacks{
		Data: func(_, input []byte, _ uint32) {
			if len(input) == 0 || runCtx.Err() != nil {
				return
			}
			chunk := make([]byte, len(input))
			copy(chunk, input)
			select {
			case audioQueue <- chunk:
			default:
				overload.Store(true)
				cancel()
			}
		},
		Stop: func() {
			if !stopping.Load() {
				disconnected.Store(true)
				cancel()
			}
		},
	}

	device, err := malgo.InitDevice(audioContext.Context, deviceConfig, callbacks)
	if err != nil {
		return wrapDeviceOpenError(err)
	}
	started := false
	defer func() {
		stopping.Store(true)
		if started {
			_ = device.Stop()
		}
		device.Uninit()
	}()

	if err := device.Start(); err != nil {
		return wrapDeviceOpenError(err)
	}
	started = true
	if err := ctx.Err(); err != nil {
		return err
	}
	ready(nil)

	var lastPartial string
	var finalID uint64
	for {
		select {
		case <-runCtx.Done():
			if overload.Load() {
				return ErrAudioOverload
			}
			if disconnected.Load() {
				return ErrMicrophoneDisconnected
			}
			return runCtx.Err()
		case input := <-audioQueue:
			if overload.Load() {
				return ErrAudioOverload
			}
			samples := s16ToFloat32(input)
			if len(samples) == 0 {
				continue
			}
			stream.AcceptWaveform(config.SampleRate, samples)
			for recognizer.IsReady(stream) {
				recognizer.Decode(stream)
			}

			result := recognizer.GetResult(stream)
			text := ""
			if result != nil {
				text = strings.TrimSpace(result.Text)
			}
			if text != "" && text != lastPartial {
				if err := emit(recognitionEvent{kind: partialEvent, text: text, timestamp: time.Now()}); err != nil {
					return err
				}
				lastPartial = text
			}
			if recognizer.IsEndpoint(stream) {
				if text != "" {
					finalID++
					if err := emit(recognitionEvent{kind: finalEvent, text: text, timestamp: time.Now(), finalID: finalID}); err != nil {
						return err
					}
				}
				recognizer.Reset(stream)
				lastPartial = ""
			}
		}
	}
}

func selectMicrophone(ctx malgo.Context, selection string) (*malgo.DeviceID, error) {
	devices, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, fmt.Errorf("%w: enumerate capture devices: %v", ErrMicrophonePermission, err)
	}
	if len(devices) == 0 {
		return nil, ErrNoMicrophone
	}
	selection = strings.TrimSpace(selection)
	if selection == "" || strings.EqualFold(selection, "default") {
		return nil, nil
	}
	for i := range devices {
		if strings.EqualFold(selection, devices[i].ID.String()) || strings.EqualFold(selection, devices[i].Name()) {
			id := devices[i].ID
			return &id, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrDeviceUnavailable, selection)
}

func wrapDeviceOpenError(err error) error {
	if errors.Is(err, malgo.ErrNoDevice) {
		return fmt.Errorf("%w: %v", ErrDeviceUnavailable, err)
	}
	return fmt.Errorf("%w: %v", ErrMicrophonePermission, err)
}

func s16ToFloat32(input []byte) []float32 {
	if len(input) < 2 {
		return nil
	}
	input = input[:len(input)-len(input)%2]
	samples := make([]float32, len(input)/2)
	for i := range samples {
		value := int16(uint16(input[2*i]) | uint16(input[2*i+1])<<8)
		samples[i] = float32(value) / 32768
	}
	return samples
}
