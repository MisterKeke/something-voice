// Package voice recognizes short, caller-supplied spoken phrases locally.
package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxPhrases      = 64
	maxPhraseRunes  = 128
	maxLanguageSize = 32
)

var (
	// ErrInvalidConfiguration indicates an invalid language, context, or phrase list.
	ErrInvalidConfiguration = errors.New("voice: invalid configuration")
	// ErrUnsupported indicates that the current operating system is unsupported.
	ErrUnsupported = errors.New("voice: speech recognition is unsupported")
	// ErrNoMicrophone indicates that no SAPI audio-input device is available.
	ErrNoMicrophone = errors.New("voice: no microphone is available")
	// ErrNoRecognizer indicates that no compatible local speech recognizer is available.
	ErrNoRecognizer = errors.New("voice: no compatible speech recognizer is available")
	// ErrLanguageUnavailable indicates that the requested language is not installed.
	ErrLanguageUnavailable = errors.New("voice: requested recognition language is unavailable")
	// ErrSessionActive indicates that this recognizer already has a listening session.
	ErrSessionActive = errors.New("voice: a listening session is already active")
	// ErrStartup indicates that SAPI could not start a listening session.
	ErrStartup = errors.New("voice: listening session startup failed")
	// ErrStopped indicates that a listening session ended because it was stopped or canceled.
	ErrStopped = errors.New("voice: listening session stopped")
)

// Availability describes whether local recognition can be started for a language.
type Availability struct {
	// Supported is true only when the operating system and all required capabilities exist.
	Supported bool
	// Language is the normalized language requested when the recognizer was created.
	Language string
	// Microphone reports whether SAPI exposes at least one audio-input device.
	Microphone bool
	// Recognizer reports whether a local SAPI recognizer for Language is installed.
	Recognizer bool
	// Reason contains a human-readable explanation when Supported is false.
	Reason string
}

// Result is a phrase recognized by the caller's command grammar.
type Result struct {
	// Phrase is one of the normalized phrases supplied to Start.
	Phrase string
	// Timestamp is the local time at which the result was delivered by SAPI.
	Timestamp time.Time
}

type backend interface {
	checkAvailability(context.Context) (Availability, error)
	listen(context.Context, []string, chan<- Result) error
}

// Recognizer owns one local speech-recognition configuration and permits one session at a time.
type Recognizer struct {
	language string
	backend  backend

	mu     sync.Mutex
	active *Session
}

// New creates a recognizer configured for a BCP-47-style language tag such as en-US.
// New does not access the microphone; call CheckAvailability before listening.
func New(language string) (*Recognizer, error) {
	normalized, err := normalizeLanguage(language)
	if err != nil {
		return nil, err
	}
	b, err := newBackend(normalized)
	if err != nil {
		return nil, err
	}
	return &Recognizer{language: normalized, backend: b}, nil
}

// Language returns the normalized language selected for this recognizer.
func (r *Recognizer) Language() string {
	if r == nil {
		return ""
	}
	return r.language
}

// CheckAvailability checks the local SAPI capabilities without starting capture.
func (r *Recognizer) CheckAvailability(ctx context.Context) (Availability, error) {
	if r == nil || r.backend == nil {
		return Availability{}, fmt.Errorf("%w: nil recognizer", ErrInvalidConfiguration)
	}
	if ctx == nil {
		return Availability{Language: r.language}, fmt.Errorf("%w: nil context", ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return Availability{Language: r.language}, err
	}
	return r.backend.checkAvailability(ctx)
}

// Start starts one bounded-result listening session for phrases.
// Recognition is limited to the supplied phrases; this method never enables dictation.
func (r *Recognizer) Start(ctx context.Context, phrases []string) (*Session, error) {
	if r == nil || r.backend == nil {
		return nil, fmt.Errorf("%w: nil recognizer", ErrInvalidConfiguration)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStopped, err)
	}
	normalized, err := normalizePhrases(phrases)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.active != nil {
		r.mu.Unlock()
		return nil, ErrSessionActive
	}
	r.mu.Unlock()

	availability, err := r.CheckAvailability(ctx)
	if err != nil {
		return nil, err
	}
	if !availability.Supported {
		return nil, unavailableError(availability)
	}

	listenCtx, cancel := context.WithCancel(ctx)
	session := newSession(cancel)
	r.mu.Lock()
	if r.active != nil {
		r.mu.Unlock()
		cancel()
		return nil, ErrSessionActive
	}
	r.active = session
	r.mu.Unlock()

	go func() {
		err := r.backend.listen(listenCtx, normalized, session.results)
		if session.wasStopped() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = ErrStopped
		}
		session.finish(err)
		r.finish(session)
	}()

	return session, nil
}

// Stop stops the active session, if any. It is safe to call repeatedly.
func (r *Recognizer) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	session := r.active
	r.mu.Unlock()
	if session != nil {
		session.Stop()
	}
}

func (r *Recognizer) finish(session *Session) {
	r.mu.Lock()
	if r.active == session {
		r.active = nil
	}
	r.mu.Unlock()
}

func unavailableError(a Availability) error {
	switch {
	case !a.Microphone:
		return ErrNoMicrophone
	case !a.Recognizer:
		return ErrLanguageUnavailable
	case !a.Supported && a.Reason != "":
		return fmt.Errorf("%w: %s", ErrUnsupported, a.Reason)
	default:
		return ErrUnsupported
	}
}

// Session exposes results and lifecycle errors for one listening session.
type Session struct {
	results chan Result
	errors  chan error
	done    chan struct{}
	cancel  context.CancelFunc

	stopOnce sync.Once
	mu       sync.Mutex
	stopped  bool
	err      error
}

func newSession(cancel context.CancelFunc) *Session {
	return &Session{
		results: make(chan Result, 16),
		errors:  make(chan error, 1),
		done:    make(chan struct{}),
		cancel:  cancel,
	}
}

// Results returns a bounded stream of recognized results. It is closed when the session ends.
func (s *Session) Results() <-chan Result {
	if s == nil {
		return nil
	}
	return s.results
}

// Errors returns zero or one lifecycle error and is closed when the session ends.
func (s *Session) Errors() <-chan error {
	if s == nil {
		return nil
	}
	return s.errors
}

// Done returns a channel closed when the session has released its backend resources.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// Wait blocks until the session ends and returns its lifecycle error, if any.
func (s *Session) Wait() error {
	if s == nil {
		return fmt.Errorf("%w: nil session", ErrInvalidConfiguration)
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Stop stops this session. It is safe to call repeatedly.
func (s *Session) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.cancel()
	})
}

func (s *Session) wasStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *Session) finish(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	if err != nil {
		s.errors <- err
	}
	close(s.results)
	close(s.errors)
	close(s.done)
}

func normalizeLanguage(language string) (string, error) {
	language = strings.TrimSpace(language)
	if language == "" || len(language) > maxLanguageSize {
		return "", fmt.Errorf("%w: language must be a non-empty tag of at most %d bytes", ErrInvalidConfiguration, maxLanguageSize)
	}
	parts := strings.Split(language, "-")
	if len(parts[0]) < 2 || len(parts[0]) > 3 || !asciiLetters(parts[0]) {
		return "", fmt.Errorf("%w: invalid language tag %q", ErrInvalidConfiguration, language)
	}
	parts[0] = strings.ToLower(parts[0])
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) == 0 || len(parts[i]) > 8 || !asciiAlphaNumeric(parts[i]) {
			return "", fmt.Errorf("%w: invalid language tag %q", ErrInvalidConfiguration, language)
		}
		if len(parts[i]) == 2 && asciiLetters(parts[i]) {
			parts[i] = strings.ToUpper(parts[i])
		} else {
			parts[i] = strings.ToLower(parts[i])
		}
	}
	return strings.Join(parts, "-"), nil
}

func normalizePhrases(phrases []string) ([]string, error) {
	if len(phrases) == 0 {
		return nil, fmt.Errorf("%w: at least one phrase is required", ErrInvalidConfiguration)
	}
	if len(phrases) > maxPhrases {
		return nil, fmt.Errorf("%w: at most %d phrases are allowed", ErrInvalidConfiguration, maxPhrases)
	}
	result := make([]string, 0, len(phrases))
	seen := make(map[string]struct{}, len(phrases))
	for _, phrase := range phrases {
		phrase = strings.TrimSpace(phrase)
		if phrase == "" {
			return nil, fmt.Errorf("%w: phrases cannot be empty", ErrInvalidConfiguration)
		}
		if !utf8.ValidString(phrase) || utf8.RuneCountInString(phrase) > maxPhraseRunes {
			return nil, fmt.Errorf("%w: phrase %q is invalid or too long", ErrInvalidConfiguration, phrase)
		}
		key := strings.ToLower(phrase)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, phrase)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: at least one non-empty phrase is required", ErrInvalidConfiguration)
	}
	return result, nil
}

func asciiLetters(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
