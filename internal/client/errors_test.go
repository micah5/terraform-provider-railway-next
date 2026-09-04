// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"testing"

	"github.com/vektah/gqlparser/v2/gqlerror"
)

func TestDecodeAPIError(t *testing.T) {
	t.Parallel()

	err := gqlerror.List{&gqlerror.Error{
		Message: "missing",
		Extensions: map[string]any{
			"code":    "NOT_FOUND",
			"traceId": "trace-42",
		},
	}}
	decoded := DecodeAPIError(err)
	if decoded.Code != "NOT_FOUND" || decoded.TraceID != "trace-42" {
		t.Fatalf("decoded = %#v", decoded)
	}
	if !IsNotFound(err) {
		t.Fatal("expected IsNotFound")
	}
}

func TestAmbiguousMutationError(t *testing.T) {
	t.Parallel()

	if !IsAmbiguousMutationError(context.DeadlineExceeded) {
		t.Fatal("deadline must be ambiguous")
	}
	if IsAmbiguousMutationError(&RateLimitError{}) {
		t.Fatal("rate limit is explicit, not ambiguous")
	}
}

func TestRetryableReadError(t *testing.T) {
	t.Parallel()

	if !IsRetryableReadError(context.DeadlineExceeded) {
		t.Fatal("HTTP-client deadline must be retryable for a safe read")
	}
	if !IsRetryableReadError(&RateLimitError{}) {
		t.Fatal("rate limiting must be retryable for a safe read")
	}
	if IsRetryableReadError(context.Canceled) {
		t.Fatal("explicit cancellation must not be retried")
	}
}
