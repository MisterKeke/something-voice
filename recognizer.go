// Package voice provides continuous, local speech transcription.
//
// The package feeds microphone audio to a sherpa-onnx streaming recognizer.
// It prints finalized utterances itself and also exposes those utterances to
// the importing application. It never interprets spoken text as a command.
package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultSampleRate       = 16000
	defaultFeatureDim       = 80
	defaultAudioQueueSize   = 32
	defaultEventQueueSize   = 16
	defaultRecognizerThread = 1
	defaultModelType        = "zipformer2"
)

var (
	// ErrInvalidConfiguration indicates invalid library configuration.
	ErrInvalidConfiguration = errors.New("voice: invalid configuration")
	// ErrMissingModel indicates that a configured model file is missing or not a regular file.
	ErrMissingModel = errors.New("voice: missing model file")
	// ErrUnsupported indicates that the current platform or build cannot capture audio.
	ErrUnsupported = errors.New("voice: speech recognition is unsupported")
	// ErrNoMicrophone indicates that no capture device is available.
	ErrNoMicrophone = errors.New("voice: no microphone is available")
	// ErrDeviceUnavailable indicates that the requested microphone cannot be found.
	ErrDeviceUnavailable = errors.New("voice: requested microphone is unavailable")
	// ErrMicrophonePermission indicates that the audio backend could not open the microphone.
	ErrMicrophonePermission = errors.New("voice: microphone permission or device-open failure")
	// ErrMicrophoneDisconnected indicates that the active microphone stopped unexpectedly.
	ErrMicrophoneDisconnected = errors.New("voice: microphone disconnected")
	// ErrRecognizerInit indicates that sherpa-onnx could not create its native recognizer.
	ErrRecognizerInit = errors.New("voice: recognizer initialization failed")
	// ErrRecognition indicates a failure while processing audio.
	ErrRecognition = errors.New("voice: recognition failed")
	// ErrAudioOverload indicates that the bounded audio queue overflowed.
	ErrAudioOverload = errors.New("voice: audio processing queue overflowed")
	// ErrOutput indicates that the configured output writer failed.
	ErrOutput = errors.New("voice: output writer failed")
	// ErrSessionActive indicates that a recognizer already has a listening session.
	ErrSessionActive = errors.New("voice: a listening session is already active")
	// ErrStopped indicates that a session was explicitly stopped.
	ErrStopped = errors.New("voice: listening session stopped")
)

// ModelConfig identifies one sherpa-onnx online transducer model.
//
// A general-purpose streaming Zipformer model needs encoder, decoder, joiner,
// and tokens files. The files are read by the native runtime; this library
// never downloads or copies them.
type ModelConfig struct {
	Encoder string
	Decoder string
	Joiner  string
	Tokens  string

	// ModelType is optional for most models. It defaults to zipformer2, which
	// is the type used by the documented English Zipformer example.
	ModelType string
	// Provider is a sherpa-onnx provider such as cpu. It defaults to cpu.
	Provider string
	// NumThreads controls native model computation and defaults to one.
	NumThreads int
	// Language is descriptive metadata for the selected model (for example,
	// en-US). The online transducer API infers language from the model files.
	Language string
}

// Config controls a Recognizer.
type Config struct {
	Model ModelConfig

	// Language is descriptive metadata for the selected model. Model.Language
	// is used when Language is empty.
	Language string
	// Microphone selects a device by malgo ID or device name. Empty selects the
	// operating system's default capture device.
	Microphone string
	// Output receives exactly one newline-terminated line per finalized
	// utterance. Nil uses os.Stdout. The library never opens a transcript file.
	Output io.Writer
	// OnFinal receives finalized utterances after the library writes them.
	OnFinal func(Utterance)
	// OnPartial receives changing partial recognition text. Partials are never
	// written to Output and are never delivered through Finals.
	OnPartial func(Partial)

	// SampleRate and FeatureDim default to 16000 and 80, respectively.
	SampleRate int
	FeatureDim int
	// AudioQueueSize bounds microphone audio retained in memory. When full,
	// the session stops with ErrAudioOverload rather than growing memory.
	AudioQueueSize int
	// EventQueueSize bounds the exported final and partial event channels.
	EventQueueSize int
}

// Utterance is one finalized speech segment. It is the only recognition result
// intended for command matching by an importing application.
type Utterance struct {
	Text      string
	Timestamp time.Time
}

// Partial is an interim recognition result. It is informational only.
type Partial struct {
	Text      string
	Timestamp time.Time
}

type eventKind uint8

const (
	partialEvent eventKind = iota + 1
	finalEvent
)

type recognitionEvent struct {
	kind      eventKind
	text      string
	timestamp time.Time
	// finalID is non-zero for backend-produced finalized segments. It lets the
	// session suppress an accidental repeated delivery of one endpoint while
	// still allowing two identical spoken utterances in a row.
	finalID uint64
}

type backend interface {
	run(context.Context, Config, func(recognitionEvent) error, func(error)) error
}

// Recognizer owns one local model configuration and permits one session at a time.
type Recognizer struct {
	config  Config
	backend backend

	mu     sync.Mutex
	active *Session
}

// New validates model paths and creates a local recognizer configuration. It
// does not access the microphone or initialize the native model.
func New(config Config) (*Recognizer, error) {
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if err := validateModelFiles(config.Model); err != nil {
		return nil, err
	}
	b, err := newBackend()
	if err != nil {
		return nil, err
	}
	return &Recognizer{config: config, backend: b}, nil
}

// Language returns the descriptive language metadata configured for the model.
func (r *Recognizer) Language() string {
	if r == nil {
		return ""
	}
	return r.config.Language
}

// Start begins microphone capture and continues until Stop, context
// cancellation, a microphone failure, or a recognition failure. Setup errors
// are reported by Session.WaitReady and Session.Errors.
func (r *Recognizer) Start(ctx context.Context) (*Session, error) {
	if r == nil || r.backend == nil {
		return nil, fmt.Errorf("%w: nil recognizer", ErrInvalidConfiguration)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.active != nil {
		select {
		case <-r.active.done:
			r.active = nil
		default:
			r.mu.Unlock()
			return nil, ErrSessionActive
		}
	}
	listenCtx, cancel := context.WithCancel(ctx)
	session := newSession(listenCtx, cancel, r.config)
	r.active = session
	r.mu.Unlock()

	go func() {
		err := r.backend.run(listenCtx, r.config, session.emit, session.signalReady)
		if err == nil {
			err = listenCtx.Err()
		}
		if session.wasStopped() {
			err = ErrStopped
		}
		session.signalReady(err)
		session.finish(err)
		r.finish(session)
	}()

	return session, nil
}

// Stop stops the active session and waits for its workers and native
// resources to be released. It is safe to call repeatedly.
func (r *Recognizer) Stop() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	session := r.active
	r.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Stop()
}

func (r *Recognizer) finish(session *Session) {
	r.mu.Lock()
	if r.active == session {
		r.active = nil
	}
	r.mu.Unlock()
}

type Session struct {
	finals   chan Utterance
	partials chan Partial
	errors   chan error
	ready    chan struct{}
	done     chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	config Config

	stopOnce   sync.Once
	readyOnce  sync.Once
	finishOnce sync.Once
	mu         sync.Mutex
	stopped    bool
	readyErr   error
	err        error
	lastFinal  uint64
}

func newSession(ctx context.Context, cancel context.CancelFunc, config Config) *Session {
	return &Session{
		finals:   make(chan Utterance, config.EventQueueSize),
		partials: make(chan Partial, config.EventQueueSize),
		errors:   make(chan error, 1),
		ready:    make(chan struct{}),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
		config:   config,
	}
}

// Finals returns a bounded stream of finalized utterances. It is closed when
// the session ends.
func (s *Session) Finals() <-chan Utterance {
	if s == nil {
		return nil
	}
	return s.finals
}

// Partials returns a bounded, informational stream of changing partial text.
// A slow partial consumer may cause partials to be dropped; finalized text is
// never replaced by a partial.
func (s *Session) Partials() <-chan Partial {
	if s == nil {
		return nil
	}
	return s.partials
}

// Errors returns zero or one terminal error and is closed when the session ends.
func (s *Session) Errors() <-chan error {
	if s == nil {
		return nil
	}
	return s.errors
}

// Ready returns a channel closed after native setup succeeds or fails.
func (s *Session) Ready() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.ready
}

// WaitReady waits for microphone and recognizer setup to complete.
func (s *Session) WaitReady() error {
	if s == nil {
		return fmt.Errorf("%w: nil session", ErrInvalidConfiguration)
	}
	<-s.ready
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyErr
}

// Done returns a channel closed only after all native resources and workers
// have been released.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// Wait blocks until the session ends and returns its terminal error. Explicit
// Stop returns ErrStopped; parent-context cancellation returns that context's
// error. A nil result means the backend ended normally.
func (s *Session) Wait() error {
	if s == nil {
		return fmt.Errorf("%w: nil session", ErrInvalidConfiguration)
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Stop requests cancellation, waits for cleanup, and returns the session's
// terminal error. It is safe to call repeatedly.
func (s *Session) Stop() error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.cancel()
	})
	return s.Wait()
}

func (s *Session) wasStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *Session) signalReady(err error) {
	s.readyOnce.Do(func() {
		s.mu.Lock()
		s.readyErr = err
		s.mu.Unlock()
		close(s.ready)
	})
}

func (s *Session) emit(event recognitionEvent) error {
	text := strings.TrimSpace(event.text)
	if text == "" {
		return nil
	}
	when := event.timestamp
	if when.IsZero() {
		when = time.Now()
	}

	switch event.kind {
	case partialEvent:
		partial := Partial{Text: text, Timestamp: when}
		if s.config.OnPartial != nil {
			s.config.OnPartial(partial)
		}
		select {
		case s.partials <- partial:
		default:
		}
		return nil
	case finalEvent:
		if event.finalID != 0 {
			s.mu.Lock()
			if event.finalID <= s.lastFinal {
				s.mu.Unlock()
				return nil
			}
			s.lastFinal = event.finalID
			s.mu.Unlock()
		}
		utterance := Utterance{Text: text, Timestamp: when}
		if _, err := io.WriteString(s.config.Output, text+"\n"); err != nil {
			return fmt.Errorf("%w: %v", ErrOutput, err)
		}
		if s.config.OnFinal != nil {
			s.config.OnFinal(utterance)
		}
		select {
		case s.finals <- utterance:
			return nil
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	default:
		return fmt.Errorf("%w: unknown recognition event", ErrRecognition)
	}
}

func (s *Session) finish(err error) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		if err != nil {
			s.errors <- err
		}
		close(s.finals)
		close(s.partials)
		close(s.errors)
		close(s.done)
	})
}

func normalizeConfig(config Config) (Config, error) {
	config.Model.Encoder = strings.TrimSpace(config.Model.Encoder)
	config.Model.Decoder = strings.TrimSpace(config.Model.Decoder)
	config.Model.Joiner = strings.TrimSpace(config.Model.Joiner)
	config.Model.Tokens = strings.TrimSpace(config.Model.Tokens)
	config.Model.ModelType = strings.TrimSpace(config.Model.ModelType)
	config.Model.Provider = strings.TrimSpace(config.Model.Provider)
	config.Language = strings.TrimSpace(config.Language)
	if config.Language == "" {
		config.Language = strings.TrimSpace(config.Model.Language)
	}
	if config.Output == nil {
		config.Output = os.Stdout
	}
	if config.SampleRate == 0 {
		config.SampleRate = defaultSampleRate
	}
	if config.FeatureDim == 0 {
		config.FeatureDim = defaultFeatureDim
	}
	if config.AudioQueueSize == 0 {
		config.AudioQueueSize = defaultAudioQueueSize
	}
	if config.EventQueueSize == 0 {
		config.EventQueueSize = defaultEventQueueSize
	}
	if config.Model.NumThreads == 0 {
		config.Model.NumThreads = defaultRecognizerThread
	}
	if config.Model.ModelType == "" {
		config.Model.ModelType = defaultModelType
	}
	if config.Model.Provider == "" {
		config.Model.Provider = "cpu"
	}
	if config.SampleRate <= 0 || config.FeatureDim <= 0 || config.AudioQueueSize <= 0 || config.EventQueueSize <= 0 || config.Model.NumThreads <= 0 {
		return Config{}, fmt.Errorf("%w: numeric settings must be positive", ErrInvalidConfiguration)
	}
	return config, nil
}

func validateModelFiles(model ModelConfig) error {
	paths := []struct {
		name string
		path string
	}{
		{"encoder", model.Encoder},
		{"decoder", model.Decoder},
		{"joiner", model.Joiner},
		{"tokens", model.Tokens},
	}
	for _, item := range paths {
		if item.path == "" {
			return fmt.Errorf("%w: %s path is empty", ErrMissingModel, item.name)
		}
		info, err := os.Stat(item.path)
		if err != nil {
			return fmt.Errorf("%w: %s %q: %v", ErrMissingModel, item.name, item.path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s %q is not a regular file", ErrMissingModel, item.name, item.path)
		}
	}
	return nil
}
