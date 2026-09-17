# something-voice

`something-voice` is a standalone Go library for continuous, local speech transcription. It captures microphone audio with [malgo](https://github.com/gen2brain/malgo), recognizes general speech with the streaming [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) Go API, prints each finalized utterance once, and delivers the same finalized utterance to the caller.

It does not use Windows Speech Recognition, SAPI grammars, the Web Speech API, a cloud service, or a command-line speech process. It does not execute commands or attach meanings to speech.

## Setup

The module path is `github.com/MisterKeke/something-voice`.

Pinned dependencies:

```text
github.com/k2-fsa/sherpa-onnx-go v1.13.8
github.com/gen2brain/malgo v0.11.25
```

The native packages require cgo and a working C toolchain. Supported build targets are Windows, Linux, and macOS on architectures supported by the pinned sherpa-onnx Go packages. Other operating systems, and builds with cgo disabled, return `ErrUnsupported` at startup.

Download a general-purpose streaming model before creating a recognizer. The documented English example is `sherpa-onnx-streaming-zipformer-en-2023-06-26`, available from the [sherpa-onnx ASR model release](https://github.com/k2-fsa/sherpa-onnx/releases/tag/asr-models). Its directory contains four files used by this package:

```text
encoder-epoch-99-avg-1-chunk-16-left-128.onnx
decoder-epoch-99-avg-1-chunk-16-left-128.onnx
joiner-epoch-99-avg-1-chunk-16-left-128.onnx
tokens.txt
```

The model documentation lists other online transducer models and languages. Set `ModelConfig.Language` or `Config.Language` as descriptive metadata and provide the corresponding model files. Language selection is determined by the model; the online transducer API does not translate a language tag into a model.

The sherpa-onnx Go API is a cgo wrapper around prebuilt platform native libraries. At runtime, ship the native files for the selected platform and architecture with the application. In particular, the upstream Go documentation says Windows users may need to copy DLLs from the pinned `sherpa-onnx-go-windows` package's `lib/x86_64-pc-windows-gnu` (Win64), `lib/i686-pc-windows-gnu` (Win32), or corresponding arm64 directory beside the executable. See the [upstream Go API installation notes](https://k2-fsa.github.io/sherpa/onnx/go-api/index.html) for platform-specific details. malgo requires cgo; it does not require an extra library on Windows/macOS and links `-ldl` on Linux/BSD systems.

## Library API

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

recognizer, err := voice.New(voice.Config{
    Language: "en-US",
    Model: voice.ModelConfig{
        Encoder: "/models/encoder-epoch-99-avg-1-chunk-16-left-128.onnx",
        Decoder: "/models/decoder-epoch-99-avg-1-chunk-16-left-128.onnx",
        Joiner:  "/models/joiner-epoch-99-avg-1-chunk-16-left-128.onnx",
        Tokens:  "/models/tokens.txt",
        ModelType: "zipformer2",
    },
    OnFinal: func(utterance voice.Utterance) {
        // Match utterance.Text in the importing application.
    },
})
if err != nil {
    return err
}

session, err := recognizer.Start(ctx)
if err != nil {
    return err
}
if err := session.WaitReady(); err != nil {
    return err
}

for utterance := range session.Finals() {
    _ = utterance // The library has already printed this line to Output.
}
return session.Wait()
```

`Config.Output` defaults to `os.Stdout`; inject a `bytes.Buffer` or another in-memory `io.Writer` in tests. The library never opens a transcript file. `OnFinal` and `Session.Finals()` are alternative ways to receive actionable finalized text; do not print it again in the caller if you want one console line per utterance. `Session.Partials()` and `OnPartial` are informational and are never printed or delivered as final events.

Call `session.Stop()` or `recognizer.Stop()` to cancel capture and wait until the microphone, sherpa stream/recognizer, malgo device/context, and worker have been released. Context cancellation has the same cleanup guarantee. Start and Stop are serialized so a session cannot be replaced while its resources are still being released.

## Runtime behavior and privacy

The malgo callback only copies one bounded PCM chunk into an in-memory queue. Recognition runs on the session worker, away from the callback. If the queue fills, the newest chunk is dropped, the session is canceled, and `ErrAudioOverload` is reported; the queue never grows without bound. A malgo stop notification ends the session with `ErrMicrophoneDisconnected`.

The library retains microphone samples and event text in memory only. It never writes audio, transcripts, temporary recordings, or transcript logs to a file or database, and it never sends audio or transcripts over the network. Model and native runtime files are read locally. Microphone permission is controlled by the operating system and must be granted by the host application/user.

Final text is emitted only when sherpa-onnx reports an endpoint. A partial may change several times while the user is speaking and is not a command. Only speech recognized by the selected model can be printed; this library does not claim word-perfect transcription.

Errors include `ErrMissingModel`, `ErrNoMicrophone`, `ErrDeviceUnavailable`, `ErrMicrophonePermission`, `ErrMicrophoneDisconnected`, `ErrRecognizerInit`, `ErrRecognition`, `ErrAudioOverload`, `ErrUnsupported`, and `ErrOutput`. Use `errors.Is` when handling them.

## Demo

`cmd/voice-demo` is a small terminal listener. It uses the library's own stdout printing and waits for Ctrl+C:

```text
go run ./cmd/voice-demo -encoder /models/encoder.onnx -decoder /models/decoder.onnx -joiner /models/joiner.onnx -tokens /models/tokens.txt
```

Use `-microphone` with a malgo device ID or name; leave it empty for the OS default. The demo does not match phrases or perform actions.

## Licensing and attribution

The library source in this repository is MIT licensed; see [LICENSE](LICENSE).

This project includes or links to these external components:

* [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx), including its Go bindings and native runtime, is Apache-2.0 licensed. Preserve upstream notices when redistributing native files.
* [malgo](https://github.com/gen2brain/malgo) is released into the public domain under its included Unlicense/public-domain dedication.
* The suggested `sherpa-onnx-streaming-zipformer-en-2023-06-26` model is a separate model artifact. Review the license and attribution included with the exact model archive before redistribution or commercial deployment; this repository does not redistribute model weights and does not assume that the sherpa-onnx source license covers them.

## Manual verification

Per the project request, tests, builds, the demo, and other development commands were not run. Run these manually from the repository root after installing native prerequisites:

```text
go mod tidy
go test ./...
go vet ./...
go build ./...
go run ./cmd/voice-demo -encoder <encoder.onnx> -decoder <decoder.onnx> -joiner <joiner.onnx> -tokens <tokens.txt>
```

For Something-v2, tag this breaking rewrite as `v0.2.0` and import `github.com/MisterKeke/something-voice@v0.2.0`.
