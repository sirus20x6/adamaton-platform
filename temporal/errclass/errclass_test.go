package errclass

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
)

func TestClassifyNil(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Fatalf("Classify(nil) = %q, want empty", got)
	}
}

func TestClassifyTimeout(t *testing.T) {
	cases := []error{
		temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil),
		context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		&url.Error{Op: "Get", URL: "http://gitea.local", Err: timeoutNetErr{}},
	}
	for i, err := range cases {
		if got := Classify(err); got != ClassTimeout {
			t.Errorf("case %d: Classify(%v) = %q, want %q", i, err, got, ClassTimeout)
		}
	}
}

func TestClassifyPanic(t *testing.T) {
	err := temporal.NewApplicationErrorWithCause("activity panicked", "PanicError", nil)
	// NewPanicError is internal; simulate via the SDK's exported check by
	// wrapping a real PanicError from a recovered panic path is not
	// constructible here, so assert the ApplicationError fallback instead:
	if got := Classify(err); got != ClassActivityError {
		t.Errorf("ApplicationError(PanicError type) = %q, want %q (PanicError type string is not special-cased)", got, ClassActivityError)
	}
}

func TestClassifyValidationTypes(t *testing.T) {
	for typ := range validationTypes {
		err := temporal.NewNonRetryableApplicationError("boom", typ, nil)
		if got := Classify(err); got != ClassValidation {
			t.Errorf("type %q = %q, want %q", typ, got, ClassValidation)
		}
	}
}

func TestClassifyExternalAPITypes(t *testing.T) {
	err := temporal.NewApplicationError("upstream 502", "gitea")
	if got := Classify(err); got != ClassExternalAPI {
		t.Errorf("gitea app error = %q, want %q", got, ClassExternalAPI)
	}
	// Case-insensitive match.
	err = temporal.NewApplicationError("upstream 502", "Gitea")
	if got := Classify(err); got != ClassExternalAPI {
		t.Errorf("Gitea app error = %q, want %q", got, ClassExternalAPI)
	}
}

func TestClassifyTransportErrors(t *testing.T) {
	cases := []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		&url.Error{Op: "Get", URL: "http://x", Err: errors.New("EOF")},
		fmt.Errorf("call upstream: %w", syscall.ECONNREFUSED),
		fmt.Errorf("stream: %w", syscall.ECONNRESET),
	}
	for i, err := range cases {
		if got := Classify(err); got != ClassExternalAPI {
			t.Errorf("case %d: Classify(%v) = %q, want %q", i, err, got, ClassExternalAPI)
		}
	}
}

func TestClassifyDefault(t *testing.T) {
	cases := []error{
		errors.New("some business failure"),
		temporal.NewApplicationError("delegation exited 1", "DelegationFailed"),
	}
	for i, err := range cases {
		if got := Classify(err); got != ClassActivityError {
			t.Errorf("case %d: Classify(%v) = %q, want %q", i, err, got, ClassActivityError)
		}
	}
}

// timeoutNetErr implements net.Error with Timeout()=true.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }
