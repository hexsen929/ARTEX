package memory

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// ExtractionResult contains reference material and a body-free outcome. An
// empty result is reserved for a valid, explicit model response of []. A
// nonempty response whose conditions cannot be retained is output_rejected,
// not evidence that the model found nothing worth remembering.
type ExtractionResult struct {
	Drafts  []Draft
	Outcome string
}

type extractionDiagnostic struct {
	code  string
	cause error
}

func (e *extractionDiagnostic) Error() string { return "memory extraction: " + e.code }
func (e *extractionDiagnostic) Unwrap() error { return e.cause }

func diagnostic(code string, cause error) error {
	if cause == nil {
		return nil
	}
	return &extractionDiagnostic{code: code, cause: cause}
}

// DiagnosticCode returns only fixed, non-sensitive categories. It never parses
// or returns an error message. Cancellation belongs to the caller's shutdown
// lifecycle; unknown errors deliberately have no invented category.
func DiagnosticCode(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var diagnosed *extractionDiagnostic
	if errors.As(err, &diagnosed) {
		switch diagnosed.code {
		case "input_invalid", "input_sensitive", "output_invalid", "output_truncated", "output_rejected", "output_sensitive", "provider_error", "timeout":
			return diagnosed.code
		default:
			return ""
		}
	}
	// Raw source validators remain usable by callers before extraction starts.
	if errors.Is(err, ErrSensitive) {
		return "input_sensitive"
	}
	if errors.Is(err, ErrInvalid) {
		return "input_invalid"
	}
	if errors.Is(err, ErrUnavailable) {
		return "provider_error"
	}
	return ""
}

func inputDiagnostic(err error) error {
	if errors.Is(err, ErrSensitive) {
		return diagnostic("input_sensitive", err)
	}
	return diagnostic("input_invalid", err)
}

func outputSafetyDiagnostic(text string, err error) error {
	if errors.Is(err, ErrSensitive) {
		return diagnostic("output_sensitive", err)
	}
	if text != cleanInvisible(text) || strings.ContainsRune(text, utf8.RuneError) {
		return diagnostic("output_invalid", err)
	}
	return diagnostic("output_rejected", err)
}
