//go:build !cgo || (!windows && !linux && !darwin)

package voice

import (
	"context"
	"fmt"
)

type unsupportedBackend struct{}

func newBackend() (backend, error) {
	return unsupportedBackend{}, nil
}

func (unsupportedBackend) run(context.Context, Config, func(recognitionEvent) error, func(error)) error {
	return fmt.Errorf("%w: cgo with Windows, Linux, or macOS is required", ErrUnsupported)
}
