package voice

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeBackend struct {
	availability    Availability
	availabilityErr error
	listenFn        func(context.Context, []string, chan<- Result, func(error)) error
}

func (f *fakeBackend) checkAvailability(context.Context) (Availability, error) {
	return f.availability, f.availabilityErr
}

func (f *fakeBackend) listen(ctx context.Context, phrases []string, results chan<- Result, ready func(error)) error {
	return f.listenFn(ctx, phrases, results, ready)
}

func newTestRecognizer(b backend) *Recognizer {
	return &Recognizer{language: "en-US", backend: b}
}

func TestNormalizePhrases(t *testing.T) {
	got, err := normalizePhrases([]string{"  Open Settings ", "open settings", "Close Settings"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "Open Settings" || got[1] != "Close Settings" {
		t.Fatalf("normalized phrases = %#v", got)
	}
}

func TestNormalizePhrasesRejectsInvalidInput(t *testing.T) {
	for _, phrases := range [][]string{nil, {""}, {"   "}} {
		if _, err := normalizePhrases(phrases); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("normalizePhrases(%#v) error = %v", phrases, err)
		}
	}
}

func TestNewNormalizesLanguage(t *testing.T) {
	r, err := New(" EN-us ")
	if err != nil {
		t.Fatal(err)
	}
	if r.Language() != "en-US" {
		t.Fatalf("language = %q", r.Language())
	}
}

func TestSessionStateAndResultDelivery(t *testing.T) {
	b := &fakeBackend{
		availability: Availability{Supported: true, Language: "en-US", Microphone: true, Recognizer: true},
		listenFn: func(ctx context.Context, phrases []string, results chan<- Result, ready func(error)) error {
			ready(nil)
			results <- Result{Phrase: phrases[0], Timestamp: time.Now()}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	r := newTestRecognizer(b)
	session, err := r.Start(context.Background(), []string{"Open Settings"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WaitReady(); err != nil {
		t.Fatalf("WaitReady error = %v", err)
	}
	if _, err := r.Start(context.Background(), []string{"Other"}); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("second Start error = %v", err)
	}
	result := <-session.Results()
	if result.Phrase != "Open Settings" || result.Timestamp.IsZero() {
		t.Fatalf("result = %#v", result)
	}
	session.Stop()
	session.Stop()
	if err := session.Wait(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Wait error = %v", err)
	}
	if _, ok := <-session.Results(); ok {
		t.Fatal("results channel is still open")
	}
}

func TestRecognizerStopIsIdempotent(t *testing.T) {
	b := &fakeBackend{
		availability: Availability{Supported: true, Language: "en-US", Microphone: true, Recognizer: true},
		listenFn: func(ctx context.Context, _ []string, _ chan<- Result, ready func(error)) error {
			ready(nil)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	r := newTestRecognizer(b)
	session, err := r.Start(context.Background(), []string{"Stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WaitReady(); err != nil {
		t.Fatalf("WaitReady error = %v", err)
	}
	r.Stop()
	r.Stop()
	if err := session.Wait(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Wait error = %v", err)
	}
	second, err := r.Start(context.Background(), []string{"Again"})
	if err != nil {
		t.Fatalf("Start after stop error = %v", err)
	}
	second.Stop()
	if err := second.Wait(); !errors.Is(err, ErrStopped) {
		t.Fatalf("second Wait error = %v", err)
	}
}

func TestSessionReadinessReportsStartupError(t *testing.T) {
	expected := errors.New("fake startup failure")
	b := &fakeBackend{
		availability: Availability{Supported: true, Language: "en-US", Microphone: true, Recognizer: true},
		listenFn: func(context.Context, []string, chan<- Result, func(error)) error {
			return expected
		},
	}
	r := newTestRecognizer(b)
	session, err := r.Start(context.Background(), []string{"Start"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WaitReady(); !errors.Is(err, expected) {
		t.Fatalf("WaitReady error = %v", err)
	}
	if err := session.Wait(); !errors.Is(err, expected) {
		t.Fatalf("Wait error = %v", err)
	}
}
