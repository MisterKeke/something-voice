//go:build windows

package voice

import (
	"errors"
	"testing"
)

func TestSAPIEnglishUSLocale(t *testing.T) {
	got, ok := sapiLocale("en-US")
	if !ok || got != "409" {
		t.Fatalf("sapiLocale(en-US) = %q, %v", got, ok)
	}
}

func TestSAPIUnknownLocaleIsNotClaimed(t *testing.T) {
	if _, ok := sapiLocale("xx-YY"); ok {
		t.Fatal("unknown locale was reported as supported")
	}
	if !errors.Is(ErrLanguageUnavailable, ErrLanguageUnavailable) {
		t.Fatal("sentinel error is not usable")
	}
}
