package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vektah/gqlparser/v2/gqlerror"
)

type RateLimitError struct {
	RetryAfter time.Duration
	TraceID    string
}

func (e *RateLimitError) Error() string {
	message := "Railway API rate limit exceeded"
	if e.RetryAfter > 0 {
		message += fmt.Sprintf("; retry after %s", e.RetryAfter.Round(time.Second))
	}
	if e.TraceID != "" {
		message += "; trace ID " + e.TraceID
	}
	return message
}

type APIError struct {
	Message string
	Code    string
	TraceID string
}

func (e *APIError) Error() string {
	message := e.Message
	if e.Code != "" {
		message += " (Railway code " + e.Code + ")"
	}
	if e.TraceID != "" {
		message += " (trace ID " + e.TraceID + ")"
	}
	return message
}

func DecodeAPIError(err error) *APIError {
	if err == nil {
		return nil
	}
	var list gqlerror.List
	if errors.As(err, &list) && len(list) > 0 {
		item := list[0]
		apiError := &APIError{Message: item.Message}
		if value, ok := item.Extensions["code"].(string); ok {
			apiError.Code = value
		}
		if value, ok := item.Extensions["traceId"].(string); ok {
			apiError.TraceID = value
		}
		return apiError
	}
	return &APIError{Message: err.Error()}
}

func IsNotFound(err error) bool {
	apiError := DecodeAPIError(err)
	if apiError == nil {
		return false
	}
	switch apiError.Code {
	case "NOT_FOUND", "RESOURCE_NOT_FOUND":
		return true
	default:
		return false
	}
}

func IsStaleEnvironment(err error) bool {
	apiError := DecodeAPIError(err)
	return apiError != nil && apiError.Code == "STALE_ENVIRONMENT_BASE"
}

func IsAmbiguousMutationError(err error) bool {
	return err != nil &&
		(errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) ||
			isTransportError(err))
}

// IsRetryableReadError reports transport conditions that can safely be retried
// because a GraphQL query has no side effects. The caller must still check its
// own operation context so a configured resource timeout remains the ceiling.
func IsRetryableReadError(err error) bool {
	if err == nil {
		return false
	}
	var rateLimit *RateLimitError
	return errors.As(err, &rateLimit) ||
		errors.Is(err, context.DeadlineExceeded) ||
		isTransportError(err)
}

func isTransportError(err error) bool {
	var rateLimit *RateLimitError
	return !errors.As(err, &rateLimit) &&
		(contains(err.Error(), "request failed") || contains(err.Error(), "connection"))
}

func contains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
