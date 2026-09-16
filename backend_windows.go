//go:build windows

package voice

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
)

const (
	clsidSpSharedRecognizer    = "{3BEE4890-4FE9-4A37-8C1E-5E7E1273D9F3}"
	clsidSpObjectTokenCategory = "{A910187F-0C7A-45AC-92CC-59EDAFB77B53}"
	iidISpRecognizer           = "{C2B5F241-DAA0-4507-9E16-5A1C6E6B2C49}"
	iidISpObjectTokenCategory  = "{2D3D3845-39AF-4850-BBF9-40B49780011D}"
	sprafTopLevel   = 0x1
	sprsActive      = 0x1
	sprsInactive    = 0x0
	spwtLexical     = 0
	speiRecognition = 38
	waitObject0     = 0
	waitTimeout     = 258
)

// SAPI stores category identifiers as registry paths.
var (
	spcatAudioInID     = "HKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Speech\\AudioInput"
	spcatRecognizersID = "HKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Speech\\Recognizers"
)

type sapiBackend struct{ language string }

type sapiEvent struct {
	eventID     uint16
	paramType   uint16
	streamNum   uint32
	audioOffset uint64
	wParam      uintptr
	lParam      uintptr
}

var procWaitForSingleObject = syscall.NewLazyDLL("kernel32.dll").NewProc("WaitForSingleObject")
var procCloseHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")

func newBackend(language string) (backend, error) {
	return sapiBackend{language: language}, nil
}

func (b sapiBackend) checkAvailability(ctx context.Context) (Availability, error) {
	availability := Availability{Language: b.language}
	if ctx.Err() != nil {
		return availability, ctx.Err()
	}
	err := runCOM(func() error {
		var token *ole.IUnknown
		var err error
		token, err = b.findRecognizerToken()
		if err == nil {
			availability.Recognizer = true
			token.Release()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		micErr := hasAudioInput()
		if micErr == nil {
			availability.Microphone = true
		}
		if err != nil {
			availability.Reason = err.Error()
			return err
		}
		if micErr != nil {
			availability.Reason = micErr.Error()
			return micErr
		}
		availability.Supported = true
		return nil
	})
	return availability, err
}

func (b sapiBackend) listen(ctx context.Context, phrases []string, results chan<- Result) error {
	return runCOM(func() error { return b.listenOnCOMThread(ctx, phrases, results) })
}

func runCOM(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
			done <- fmt.Errorf("%w: COM initialization: %v", ErrStartup, err)
			return
		}
		err := fn()
		ole.CoUninitialize()
		done <- err
	}()
	return <-done
}

func (b sapiBackend) findRecognizerToken() (*ole.IUnknown, error) {
	locale, ok := sapiLocale(b.language)
	if !ok {
		return nil, fmt.Errorf("%w: SAPI token mapping is unavailable for %s", ErrLanguageUnavailable, b.language)
	}
	category, enum, err := enumCategory(spcatRecognizersID, fmt.Sprintf("Language=%s", locale))
	if err != nil {
		return nil, fmt.Errorf("%w: enumerate recognizers: %v", ErrNoRecognizer, err)
	}
	defer category.Release()
	defer enum.Release()

	var token *ole.IUnknown
	var fetched uint32
	hr := comCall(enum, 3, 1, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&fetched)))
	if hr == 0 && fetched == 1 && token != nil {
		return token, nil
	}
	if hr != 0 && hr != 1 {
		return nil, fmt.Errorf("%w: enumerate recognizers: %v", ErrNoRecognizer, ole.NewError(hr))
	}
	return nil, fmt.Errorf("%w: no recognizer is installed for %s", ErrLanguageUnavailable, b.language)
}

func hasAudioInput() error {
	category, enum, err := enumCategory(spcatAudioInID, "")
	if err != nil {
		return fmt.Errorf("%w: enumerate audio inputs: %v", ErrNoMicrophone, err)
	}
	defer category.Release()
	defer enum.Release()
	var token *ole.IUnknown
	var fetched uint32
	hr := comCall(enum, 3, 1, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&fetched)))
	if hr == 0 && fetched == 1 && token != nil {
		token.Release()
		return nil
	}
	if hr != 0 && hr != 1 {
		return fmt.Errorf("%w: enumerate audio inputs: %v", ErrNoMicrophone, ole.NewError(hr))
	}
	return ErrNoMicrophone
}

func enumCategory(categoryID, requiredAttributes string) (*ole.IUnknown, *ole.IUnknown, error) {
	categoryCLSID := ole.NewGUID(clsidSpObjectTokenCategory)
	categoryIID := ole.NewGUID(iidISpObjectTokenCategory)
	category, err := ole.CreateInstance(categoryCLSID, categoryIID)
	if err != nil {
		return nil, nil, err
	}
	categoryID16 := syscall.StringToUTF16(categoryID)
	hr := comCall(category, 15, uintptr(unsafe.Pointer(&categoryID16[0])), 0)
	if hr != 0 {
		category.Release()
		return nil, nil, ole.NewError(hr)
	}

	var enum *ole.IUnknown
	required16 := syscall.StringToUTF16(requiredAttributes)
	var requiredPtr uintptr
	if requiredAttributes != "" {
		requiredPtr = uintptr(unsafe.Pointer(&required16[0]))
	}
	hr = comCall(category, 18, requiredPtr, 0, uintptr(unsafe.Pointer(&enum)))
	if hr != 0 {
		category.Release()
		return nil, nil, ole.NewError(hr)
	}
	return category, enum, nil
}

func (b sapiBackend) listenOnCOMThread(ctx context.Context, phrases []string, results chan<- Result) error {
	token, err := b.findRecognizerToken()
	if err != nil {
		return err
	}
	defer token.Release()

	recognizer, err := ole.CreateInstance(ole.NewGUID(clsidSpSharedRecognizer), ole.NewGUID(iidISpRecognizer))
	if err != nil {
		return fmt.Errorf("%w: create shared recognizer: %v", ErrStartup, err)
	}
	defer recognizer.Release()
	if err := requireHR("select recognizer", comCall(recognizer, 7, uintptr(unsafe.Pointer(token)))); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}

	var contextObject *ole.IUnknown
	if err := requireHR("create recognition context", comCall(recognizer, 12, uintptr(unsafe.Pointer(&contextObject)))); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}
	defer contextObject.Release()

	var grammar *ole.IUnknown
	if err := requireHR("create command grammar", comCall(contextObject, 14, 1, uintptr(unsafe.Pointer(&grammar)))); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}
	defer grammar.Release()
	if err := buildCommandGrammar(grammar, phrases); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}
	interest := uintptr(uint64(1) << spEiRecognition)
	if err := requireHR("set recognition interest", comCall(contextObject, 10, interest, interest)); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}
	if err := requireHR("set Win32 event notification", comCall(contextObject, 7)); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}
	eventHandle := comCall(contextObject, 8)
	if eventHandle == 0 {
		return fmt.Errorf("%w: SAPI returned a null event handle", ErrStartup)
	}
	defer procCloseHandle.Call(eventHandle)
	commandRule := syscall.StringToUTF16("Commands")
	if err := requireHR("activate command grammar", comCall(grammar, 20, uintptr(unsafe.Pointer(&commandRule[0])), 0, sprsActive)); err != nil {
		return fmt.Errorf("%w: %v", ErrStartup, err)
	}

	lookup := make([]string, len(phrases))
	for i, phrase := range phrases {
		lookup[i] = phrase
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = comCall(grammar, 20, uintptr(unsafe.Pointer(&commandRule[0])), 0, sprsInactive)
			return err
		}
		waitResult, _, waitErr := procWaitForSingleObject.Call(eventHandle, 100)
		if waitResult == uintptr(^uint32(0)) && waitErr != nil {
			return fmt.Errorf("%w: wait for recognition event: %v", ErrStartup, waitErr)
		}
		if waitResult == waitTimeout {
			continue
		}
		if waitResult != waitObject0 {
			return fmt.Errorf("%w: wait for recognition event returned %d", ErrStartup, waitResult)
		}
		if err := drainRecognitionEvents(ctx, contextObject, lookup, results); err != nil {
			return err
		}
	}
}

func buildCommandGrammar(grammar *ole.IUnknown, phrases []string) error {
	ruleName := syscall.StringToUTF16("Commands")
	var initialState uintptr
	hr := comCall(grammar, 4, uintptr(unsafe.Pointer(&ruleName[0])), 0, sprafTopLevel, 1, uintptr(unsafe.Pointer(&initialState)))
	if err := requireHR("create grammar rule", hr); err != nil {
		return err
	}
	separators := syscall.StringToUTF16(" ")
	for _, phrase := range phrases {
		words := syscall.StringToUTF16(phrase)
		hr = comCall(grammar, 7, initialState, 0, uintptr(unsafe.Pointer(&words[0])), uintptr(unsafe.Pointer(&separators[0])), spwtLexical, uintptr(mathFloat32Bits(1)), 0)
		if err := requireHR("add phrase to grammar", hr); err != nil {
			return err
		}
	}
	return requireHR("commit grammar", comCall(grammar, 10, 0))
}

func drainRecognitionEvents(ctx context.Context, contextObject *ole.IUnknown, phrases []string, results chan<- Result) error {
	var events [8]sapiEvent
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var fetched uint32
		hr := comCall(contextObject, 11, uint32(len(events)), uintptr(unsafe.Pointer(&events[0])), uintptr(unsafe.Pointer(&fetched)))
		if hr == 1 || fetched == 0 {
			return nil
		}
		if hr != 0 {
			return fmt.Errorf("%w: retrieve recognition event: %v", ErrStartup, ole.NewError(hr))
		}
		for i := uint32(0); i < fetched; i++ {
			if events[i].eventID != speiRecognition || events[i].lParam == 0 {
				continue
			}
			resultObject := (*ole.IUnknown)(unsafe.Pointer(events[i].lParam))
			text, err := resultText(resultObject)
			resultObject.Release()
			if err != nil {
				return fmt.Errorf("%w: read recognition result: %v", ErrStartup, err)
			}
			for _, phrase := range phrases {
				if strings.EqualFold(strings.TrimSpace(text), phrase) {
					select {
					case results <- Result{Phrase: phrase, Timestamp: time.Now()}:
					case <-ctx.Done():
						return ctx.Err()
					}
					break
				}
			}
		}
	}
}

func resultText(result *ole.IUnknown) (string, error) {
	var textPtr *uint16
	hr := comCall(result, 5, 0, 0, 1, uintptr(unsafe.Pointer(&textPtr)), 0)
	if hr != 0 {
		return "", ole.NewError(hr)
	}
	if textPtr == nil {
		return "", nil
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(textPtr)))
	return ole.UTF16PtrToString(textPtr), nil
}

func requireHR(operation string, hr uintptr) error {
	if hr != 0 {
		return fmt.Errorf("%s: %v", operation, ole.NewError(hr))
	}
	return nil
}

func comCall(object *ole.IUnknown, index int, args ...uintptr) uintptr {
	vtable := (*[64]uintptr)(unsafe.Pointer(object.RawVTable))
	callArgs := make([]uintptr, 0, len(args)+1)
	callArgs = append(callArgs, uintptr(unsafe.Pointer(object)))
	callArgs = append(callArgs, args...)
	return syscall.SyscallN(vtable[index], callArgs...)
}

func mathFloat32Bits(value float32) uint32 {
	return *(*uint32)(unsafe.Pointer(&value))
}

func sapiLocale(language string) (string, bool) {
	locales := map[string]string{
		"en-US": "409",
		"en-GB": "809",
		"en-AU": "C09",
		"en-CA": "1009",
		"de-DE": "407",
		"es-ES": "40A",
		"fr-FR": "40C",
		"it-IT": "410",
		"ja-JP": "411",
		"pt-BR": "416",
		"tr-TR": "41F",
		"zh-CN": "804",
	}
	locale, ok := locales[language]
	return locale, ok
}
