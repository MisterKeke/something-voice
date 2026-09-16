//go:build !windows

package voice

import "context"

type unsupportedBackend struct{ language string }

func newBackend(language string) (backend, error) {
	return unsupportedBackend{language: language}, nil
}

func (b unsupportedBackend) checkAvailability(context.Context) (Availability, error) {
	return Availability{
		Language: b.language,
		Reason:   "Windows SAPI is required",
	}, ErrUnsupported
}

func (b unsupportedBackend) listen(context.Context, []string, chan<- Result) error {
	return ErrUnsupported
}
