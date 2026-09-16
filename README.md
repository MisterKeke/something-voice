# something-voice

`something-voice` is a small Go library for recognizing a caller-supplied list of short spoken phrases. It reports phrases to its caller; it does not interpret or execute commands.

## Support and recognition approach

The first target is Windows. The Windows implementation uses the locally installed Microsoft Speech API (SAPI) shared recognizer and a dynamically built command-and-control grammar. Microphone audio stays on the machine; this package does not use a network recognition service, save audio, save transcripts, or log recognized speech.

The configured language is `en-US` in the initial integration. A matching local SAPI recognizer must be installed, and Windows must expose an audio-input device. Availability is checked before a session starts. The package never falls back to unrestricted dictation or another engine. Recognition confidence is intentionally not exposed because this API does not need to depend on a fragile confidence interpretation across SAPI engines.

Non-Windows builds compile to a clean unsupported-capability implementation.

## Install

```text
go get github.com/MisterKeke/something-voice
```

The Windows implementation depends on the minimal COM interop package `github.com/go-ole/go-ole`.

## Import example

```go
package main

import (
	"context"
	"fmt"

	voice "github.com/MisterKeke/something-voice"
)

func check(ctx context.Context) error {
	recognizer, err := voice.New("en-US")
	if err != nil {
		return err
	}
	availability, err := recognizer.CheckAvailability(ctx)
	if err != nil {
		return fmt.Errorf("voice unavailable: %w", err)
	}
	fmt.Printf("language=%s supported=%t microphone=%t recognizer=%t\n",
		availability.Language, availability.Supported,
		availability.Microphone, availability.Recognizer)
	return nil
}
```

## Listening example

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

recognizer, err := voice.New("en-US")
if err != nil {
	panic(err)
}
session, err := recognizer.Start(ctx, []string{"open settings", "close settings"})
if err != nil {
	panic(err)
}
defer session.Stop()

for {
	select {
	case result, ok := <-session.Results():
		if !ok {
			return
		}
		// The caller decides what result.Phrase means.
		fmt.Println(result.Phrase, result.Timestamp)
	case err, ok := <-session.Errors():
		if ok {
			fmt.Println("listening ended:", err)
		}
		return
	}
}
```

`Session.Stop`, `Recognizer.Stop`, and context cancellation are idempotent ways to stop capture. Results use a bounded channel; a slow consumer applies backpressure, and cancellation still releases the SAPI objects and worker thread.

## Limitations

- Windows SAPI and an installed matching local speech recognizer are required for recognition.
- The installed recognizer determines pronunciation and language quality; the library does not download or install engines.
- Only one session may be active per `Recognizer`.
- The grammar is limited to at most 64 phrases, each at most 128 Unicode code points.
- The package reports canonical forms from the supplied phrase list and never executes an action.

## Manual verification

Tests and builds were intentionally not run while preparing this repository. On a supported Windows machine, run these commands yourself:

```text
go test ./...
go vet ./...
go build ./...
```

Then integrate the listening example in a small host program, confirm `CheckAvailability` reports `en-US`, speak only supplied phrases, cancel the context, and verify that the result channel closes. Also verify Windows microphone privacy permissions and the installed Speech Recognition language in Settings.

## License

No license file was added because no license choice was supplied. Choose and add a license before treating the public repository as generally redistributable.
