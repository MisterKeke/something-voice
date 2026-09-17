package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	voice "github.com/MisterKeke/something-voice"
)

func main() {
	var config voice.Config
	flag.StringVar(&config.Model.Encoder, "encoder", "", "path to the streaming encoder ONNX file")
	flag.StringVar(&config.Model.Decoder, "decoder", "", "path to the streaming decoder ONNX file")
	flag.StringVar(&config.Model.Joiner, "joiner", "", "path to the streaming joiner ONNX file")
	flag.StringVar(&config.Model.Tokens, "tokens", "", "path to tokens.txt")
	flag.StringVar(&config.Model.ModelType, "model-type", "zipformer2", "sherpa-onnx model type")
	flag.StringVar(&config.Model.Provider, "provider", "cpu", "sherpa-onnx provider")
	flag.StringVar(&config.Language, "language", "en-US", "descriptive model language")
	flag.StringVar(&config.Microphone, "microphone", "", "microphone name or malgo device ID; empty uses the default")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	recognizer, err := voice.New(config)
	if err != nil {
		fatal(err)
	}
	session, err := recognizer.Start(ctx)
	if err != nil {
		fatal(err)
	}
	if err := session.WaitReady(); err != nil {
		_ = session.Stop()
		fatal(err)
	}

	fmt.Fprintln(os.Stderr, "Listening. Press Ctrl+C to stop.")
	err = session.Wait()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, voice.ErrStopped) {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "voice-demo:", err)
	os.Exit(1)
}
