package voice

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	runFn func(context.Context, Config, func(recognitionEvent) error, func(error)) error
}

func (f *fakeBackend) run(ctx context.Context, cfg Config, emit func(recognitionEvent) error, ready func(error)) error {
	return f.runFn(ctx, cfg, emit, ready)
}

func newTestRecognizer(b backend, output *strings.Builder) *Recognizer {
	return &Recognizer{
		config: Config{
			Output:         output,
			EventQueueSize: 4,
			AudioQueueSize: 4,
			SampleRate:     defaultSampleRate,
			FeatureDim:     defaultFeatureDim,
			Model:          ModelConfig{NumThreads: 1},
		},
		backend: b,
	}
}

func TestFinalResultIsPrintedAndDeliveredOnce(t *testing.T) {
	var output strings.Builder
	var callbackCount atomic.Int32
	b := &fakeBackend{runFn: func(ctx context.Context, _ Config, emit func(recognitionEvent) error, ready func(error)) error {
		ready(nil)
		event := recognitionEvent{kind: finalEvent, text: "open settings", finalID: 1}
		if err := emit(event); err != nil {
			return err
		}
		if err := emit(event); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}

	r := newTestRecognizer(b, &output)
	r.config.OnFinal = func(Utterance) { callbackCount.Add(1) }
	session, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WaitReady(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-session.Finals():
		if result.Text != "open settings" {
			t.Fatalf("final text = %q", result.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final result")
	}
	if got := output.String(); got != "open settings\n" {
		t.Fatalf("output = %q", got)
	}
	if got := callbackCount.Load(); got != 1 {
		t.Fatalf("callback count = %d", got)
	}
	if err := session.Stop(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Stop error = %v", err)
	}
}

func TestPartialIsNotPrintedOrDeliveredAsFinal(t *testing.T) {
	var output strings.Builder
	b := &fakeBackend{runFn: func(ctx context.Context, _ Config, emit func(recognitionEvent) error, ready func(error)) error {
		ready(nil)
		if err := emit(recognitionEvent{kind: partialEvent, text: "open set"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	r := newTestRecognizer(b, &output)
	session, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WaitReady(); err != nil {
		t.Fatal(err)
	}
	select {
	case partial := <-session.Partials():
		if partial.Text != "open set" {
			t.Fatalf("partial text = %q", partial.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for partial result")
	}
	if output.Len() != 0 {
		t.Fatalf("partial was printed: %q", output.String())
	}
	select {
	case final := <-session.Finals():
		t.Fatalf("partial became final: %#v", final)
	default:
	}
	if err := session.Stop(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Stop error = %v", err)
	}
}

func TestCancellationClosesChannelsAndReleasesSession(t *testing.T) {
	var output strings.Builder
	started := make(chan struct{})
	b := &fakeBackend{runFn: func(ctx context.Context, _ Config, _ func(recognitionEvent) error, ready func(error)) error {
		ready(nil)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	r := newTestRecognizer(b, &output)
	ctx, cancel := context.WithCancel(context.Background())
	session, err := r.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	if err := session.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v", err)
	}
	if _, ok := <-session.Finals(); ok {
		t.Fatal("finals channel is still open")
	}
	if _, ok := <-session.Partials(); ok {
		t.Fatal("partials channel is still open")
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop after cancellation = %v", err)
	}
	second, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Stop(); !errors.Is(err, ErrStopped) {
		t.Fatalf("second Stop error = %v", err)
	}
}

func TestStartRejectsConcurrentSessionAndWaitsOnStop(t *testing.T) {
	var output strings.Builder
	b := &fakeBackend{runFn: func(ctx context.Context, _ Config, _ func(recognitionEvent) error, ready func(error)) error {
		ready(nil)
		<-ctx.Done()
		return ctx.Err()
	}}
	r := newTestRecognizer(b, &output)
	session, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background()); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("second Start error = %v", err)
	}
	if err := r.Stop(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Stop error = %v", err)
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("Stop returned before session cleanup")
	}
}

func TestStartupErrorIsAvailableThroughReadinessAndWait(t *testing.T) {
	var output strings.Builder
	expected := errors.New("recognizer unavailable")
	b := &fakeBackend{runFn: func(context.Context, Config, func(recognitionEvent) error, func(error)) error {
		return expected
	}}
	r := newTestRecognizer(b, &output)
	session, err := r.Start(context.Background())
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

func TestNormalizeConfigDefaultsAndMissingModel(t *testing.T) {
	config, err := normalizeConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config.SampleRate != 16000 || config.FeatureDim != 80 || config.Model.Provider != "cpu" || config.Model.ModelType != "zipformer2" {
		t.Fatalf("defaults = %#v", config)
	}
	if _, err := New(Config{}); !errors.Is(err, ErrMissingModel) {
		t.Fatalf("New empty config error = %v", err)
	}
}
