// SPDX-License-Identifier: MPL-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportAuthenticationHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tokenType TokenType
		header    string
		value     string
		absent    string
	}{
		{
			name:      "account",
			tokenType: TokenTypeAccount,
			header:    "Authorization",
			value:     "Bearer account-token",
			absent:    "Project-Access-Token",
		},
		{
			name:      "workspace",
			tokenType: TokenTypeWorkspace,
			header:    "Authorization",
			value:     "Bearer account-token",
			absent:    "Project-Access-Token",
		},
		{
			name:      "project",
			tokenType: TokenTypeProject,
			header:    "Project-Access-Token",
			value:     "account-token",
			absent:    "Authorization",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(test.header); got != test.value {
					t.Errorf("%s = %q, want %q", test.header, got, test.value)
				}
				if got := r.Header.Get(test.absent); got != "" {
					t.Errorf("%s must be absent, got %q", test.absent, got)
				}
				if got := r.Header.Get("User-Agent"); got != "terraform-provider-railway-next/test" {
					t.Errorf("User-Agent = %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"ok":true}}`)
			}))
			defer server.Close()

			doer := newRailwayDoer(Config{
				Token:      "account-token",
				TokenType:  test.tokenType,
				Version:    "test",
				Timeout:    time.Second,
				MaxRetries: 1,
			})
			req := graphqlRequest(t, server.URL, "query Test { ok }")
			response, err := doer.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			drainAndClose(response.Body)
		})
	}
}

func TestTransportRetriesSafeReadAndHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"ok":true}}`)
	}))
	defer server.Close()

	doer := newRailwayDoer(Config{
		Token: "token", TokenType: TokenTypeAccount, Version: "test",
		Timeout: time.Second, MaxRetries: 1,
	})
	response, err := doer.Do(graphqlRequest(t, server.URL, "query Test { ok }"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drainAndClose(response.Body)
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestTransportRetriesHTTPClientDeadlineOnSafeRead(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	doer := newRailwayDoer(Config{
		Token: "token", TokenType: TokenTypeAccount, Version: "test", MaxRetries: 1,
		HTTPDoer: roundTripFunc(func(*http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, context.DeadlineExceeded
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"data":{"ok":true}}`)),
			}, nil
		}),
	})
	response, err := doer.Do(graphqlRequest(t, "https://example.test/graphql", "query Test { ok }"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drainAndClose(response.Body)
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestTransportDoesNotRetryMutation(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	doer := newRailwayDoer(Config{
		Token: "token", TokenType: TokenTypeAccount, Version: "test",
		Timeout: time.Second, MaxRetries: 3,
	})
	response, err := doer.Do(graphqlRequest(t, server.URL, "mutation Create { create }"))
	if err != nil {
		t.Fatalf("Do should return the HTTP response for GraphQL decoding: %v", err)
	}
	drainAndClose(response.Body)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestTransportPreservesTraceIDInGraphQLErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Trace-Id", "trace-123")
		_, _ = io.WriteString(w, `{"errors":[{"message":"denied","extensions":{"code":"FORBIDDEN"}}]}`)
	}))
	defer server.Close()

	doer := newRailwayDoer(Config{
		Token: "token", TokenType: TokenTypeAccount, Version: "test",
		Timeout: time.Second,
	})
	response, err := doer.Do(graphqlRequest(t, server.URL, "query Test { denied }"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"traceId":"trace-123"`)) {
		t.Fatalf("response does not contain trace ID: %s", body)
	}
}

func TestRequestContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := newRailwayDoer(Config{
		Token: "token", TokenType: TokenTypeAccount, Version: "test",
		Timeout: time.Second, MaxRetries: 1,
		HTTPDoer: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled
		}),
	})
	_, err := doer.Do(graphqlRequestWithContext(t, ctx, "http://127.0.0.1/graphql", "query Test { ok }"))
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRedactVariables(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"token": "do-not-log",
		"input": map[string]any{
			"name":      "also-redacted-by-default",
			"variables": map[string]any{"API_KEY": "secret"},
		},
	}
	encoded, err := json.Marshal(RedactVariables(input))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"do-not-log", "also-redacted-by-default", "secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("redacted output contains %q: %s", forbidden, text)
		}
	}
}

func graphqlRequest(t *testing.T, endpoint, operation string) *http.Request {
	t.Helper()
	return graphqlRequestWithContext(t, context.Background(), endpoint, operation)
}

func graphqlRequestWithContext(t *testing.T, ctx context.Context, endpoint, operation string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"query":     operation,
		"variables": map[string]any{"secret": "must-not-appear"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
