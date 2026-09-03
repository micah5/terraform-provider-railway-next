// SPDX-License-Identifier: MPL-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"

	"github.com/hashicorp/terraform-plugin-log/tfsdklog"
)

// TestLogRequestNamesEmptyVariables asserts the log line that would have made
// the empty-id bug a five-minute diagnosis instead of an afternoon.
//
// Railway reports a mutation carrying `serviceId: ""` as `Not Authorized`,
// naming neither the variable nor the resource. A key list alone does not help,
// because the key IS present — it is the value that is missing. So the empty
// ones are called out by name.
func TestLogRequestNamesEmptyVariables(t *testing.T) {

	logged := captureProviderLog(t, hclog.Trace, func(ctx context.Context) {
		logRequest(ctx, requestEnvelope{
			OperationName: "UpdateServiceInstance",
			Variables: map[string]any{
				"environmentId": "environment-fixture",
				"serviceId":     "",
			},
		})
	})
	if !strings.Contains(logged, "UpdateServiceInstance") {
		t.Errorf("the operation name is missing, so the log cannot say which call failed:\n%s", logged)
	}
	if !strings.Contains(logged, "railway_empty_variables") || !strings.Contains(logged, "serviceId") {
		t.Errorf("serviceId was empty and the log did not say so:\n%s", logged)
	}
}

// TestLogRequestKeepsValuesOutOfTrace is the privacy half of the contract.
//
// Variables carry whatever the practitioner configured, so their values never
// belong in provider logs — including TRACE, which commonly lands in CI logs or
// a TF_LOG_PATH file. Keys and empty-variable names are sufficient to diagnose
// malformed requests.
func TestLogRequestKeepsValuesOutOfTrace(t *testing.T) {

	logged := captureProviderLog(t, hclog.Trace, func(ctx context.Context) {
		logRequest(ctx, requestEnvelope{
			OperationName: "UpsertVariables",
			Variables: map[string]any{
				"serviceId": "service-fixture",
				"input": map[string]any{
					"variables": map[string]any{
						"API_KEY": "super-secret-value",
					},
				},
			},
		})
	})

	for _, secret := range []string{"service-fixture", "super-secret-value", "API_KEY"} {
		if strings.Contains(logged, secret) {
			t.Errorf("variable value %q reached the trace log:\n%s", secret, logged)
		}
	}
	for _, key := range []string{"serviceId", "input"} {
		if !strings.Contains(logged, key) {
			t.Errorf("top-level variable key %q should still be logged:\n%s", key, logged)
		}
	}
}

// TestRequestEnvelopeDecodesOperationAndVariables guards the decode itself.
//
// The envelope originally captured only `query`, which is why the transport
// could not say what it was sending. If these tags drift, every log line above
// silently degrades to `(anonymous)` with no variables and the tests still pass
// unless this one is here.
func TestRequestEnvelopeDecodesOperationAndVariables(t *testing.T) {
	t.Parallel()

	var envelope requestEnvelope
	body := `{"query":"mutation X { a }","operationName":"X","variables":{"serviceId":"s-1"}}`
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}

	if envelope.OperationName != "X" {
		t.Errorf("operationName = %q, want X", envelope.OperationName)
	}
	if envelope.Variables["serviceId"] != "s-1" {
		t.Errorf("variables = %v, want serviceId s-1", envelope.Variables)
	}
}

// captureProviderLog runs body with a real provider root logger at the given
// level and returns what it wrote.
//
// **IT CAPTURES STDERR RATHER THAN INJECTING A BUFFER**, because `tfsdklog` in
// this version exposes no output option — the logger it builds writes to
// stderr, which is how Terraform actually collects provider logs. Capturing the
// real sink tests the path an operator gets from `TF_LOG`, instead of a
// parallel one that could drift from it.
//
// These tests therefore cannot be `t.Parallel()`: os.Stderr is process-wide.
func captureProviderLog(t *testing.T, level hclog.Level, body func(context.Context)) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	original := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = original }()

	ctx := tfsdklog.NewRootProviderLogger(context.Background(),
		tfsdklog.WithLevel(level), tfsdklog.WithoutLocation())
	body(ctx)

	if err := write.Close(); err != nil {
		t.Fatal(err)
	}

	var captured bytes.Buffer
	if _, err := io.Copy(&captured, read); err != nil {
		t.Fatal(err)
	}
	return captured.String()
}
