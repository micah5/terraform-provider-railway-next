package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type railwayDoer struct {
	config Config
	doer   HTTPDoer
}

type requestEnvelope struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func newRailwayDoer(config Config) HTTPDoer {
	doer := config.HTTPDoer
	if doer == nil {
		doer = &http.Client{
			Transport: http.DefaultTransport,
			Timeout:   config.Timeout,
		}
	}
	return &railwayDoer{config: config, doer: doer}
}

func (d *railwayDoer) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read GraphQL request body: %w", err)
	}
	_ = req.Body.Close()

	var envelope requestEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New("encode Railway GraphQL request")
	}

	logRequest(req.Context(), envelope)
	safeRead := strings.HasPrefix(strings.TrimSpace(envelope.Query), "query ")
	attempts := 1
	if safeRead {
		attempts += d.config.MaxRetries
	}

	for attempt := 0; attempt < attempts; attempt++ {
		attemptReq := req.Clone(req.Context())
		attemptReq.Body = io.NopCloser(bytes.NewReader(body))
		attemptReq.ContentLength = int64(len(body))
		d.setHeaders(attemptReq)

		response, requestErr := d.doer.Do(attemptReq)
		if requestErr != nil {
			if !safeRead || attempt == attempts-1 || !retryableNetworkError(req.Context(), requestErr) {
				return nil, fmt.Errorf("Railway GraphQL request failed: %w", requestErr)
			}
			if err := wait(req.Context(), backoff(attempt, 0)); err != nil {
				return nil, err
			}
			continue
		}

		traceID := traceIDFromHeader(response.Header)
		if response.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
			if safeRead && attempt < attempts-1 {
				drainAndClose(response.Body)
				if err := wait(req.Context(), backoff(attempt, retryAfter)); err != nil {
					return nil, err
				}
				continue
			}
			drainAndClose(response.Body)
			return nil, &RateLimitError{RetryAfter: retryAfter, TraceID: traceID}
		}

		if safeRead && retryableStatus(response.StatusCode) && attempt < attempts-1 {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
			drainAndClose(response.Body)
			if err := wait(req.Context(), backoff(attempt, retryAfter)); err != nil {
				return nil, err
			}
			continue
		}

		if err := injectTraceID(response, traceID); err != nil {
			drainAndClose(response.Body)
			return nil, fmt.Errorf("decode Railway GraphQL response: %w", err)
		}
		return response, nil
	}
	return nil, errors.New("Railway GraphQL request exhausted retries")
}

func (d *railwayDoer) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "terraform-provider-railway-next/"+d.config.Version)
	req.Header.Del("Authorization")
	req.Header.Del("Project-Access-Token")
	if d.config.TokenType == TokenTypeProject {
		req.Header.Set("Project-Access-Token", d.config.Token)
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.config.Token)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryableNetworkError(ctx context.Context, err error) bool {
	// An HTTP-client timeout is retryable while the enclosing request context
	// remains live. Cancellation of that enclosing context is the caller's
	// resource deadline and must stop immediately.
	return ctx.Err() == nil && !errors.Is(err, context.Canceled)
}

func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	base := 200 * time.Millisecond * time.Duration(1<<min(attempt, 4))
	jitter := time.Duration(rand.Int64N(int64(base/2) + 1))
	return base + jitter
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func traceIDFromHeader(header http.Header) string {
	for _, key := range []string{"X-Trace-Id", "X-Request-Id", "Railway-Trace-Id", "Cf-Ray"} {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func injectTraceID(response *http.Response, traceID string) error {
	if response.Body == nil || traceID == "" ||
		!strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		return nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	_ = response.Body.Close()

	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if rawErrors, ok := payload["errors"].([]any); ok {
			for _, rawError := range rawErrors {
				item, ok := rawError.(map[string]any)
				if !ok {
					continue
				}
				extensions, _ := item["extensions"].(map[string]any)
				if extensions == nil {
					extensions = map[string]any{}
					item["extensions"] = extensions
				}
				if _, exists := extensions["traceId"]; !exists {
					extensions["traceId"] = traceID
				}
			}
			body, err = json.Marshal(payload)
			if err != nil {
				return err
			}
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 32<<10))
	_ = body.Close()
}
