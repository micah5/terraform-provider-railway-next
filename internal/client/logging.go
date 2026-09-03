// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// **THE PROVIDER USED TO SEND ITS REQUESTS INTO THE DARK, AND THAT IS WHY THE
// EMPTY-ID BUG TOOK SO LONG TO FIND.**
//
// Railway answered a malformed mutation with:
//
//	Not Authorized
//
// The mutation named a service id of `""`, because `id` was unknown in the plan
// — but nothing logged the variables, so the only visible fact was an
// authorisation error against a token that demonstrably worked by hand. The
// diagnosis needed a temporary patch to this package to dump request bodies to
// a file, which is not a thing an operator hitting this in the wild can do.
//
// `tflog` is the framework's answer, and using it means this provider behaves
// like every other one: `TF_LOG=DEBUG` (or `TF_LOG_PROVIDER=DEBUG`) turns
// request logging on, `TF_LOG_PATH` sends it to a file, and the output is
// structured rather than a raw body dump. There is no bespoke env var to know
// about, which is the point — an operator already knows how to debug a
// Terraform provider.
//
// **VARIABLE VALUES ARE NEVER LOGGED.** They carry whatever a practitioner put
// in their configuration, including write-only secret values. `DEBUG` and
// `TRACE` give the operation name, variable KEYS, and the names of empty string
// variables — enough to diagnose a missing id without copying credentials into
// a terminal, CI log, or TF_LOG_PATH file.
//
// The auth token never reaches here: it is applied as a header in `setHeaders`,
// and headers are not logged. `tfsdklog`'s masking is a second line of defence
// rather than the mechanism relied on.

func logRequest(ctx context.Context, envelope requestEnvelope) {
	operation := envelope.OperationName
	if operation == "" {
		operation = "(anonymous)"
	}

	// The KEYS at debug. A variable that is present but empty is invisible in a
	// key list, which is exactly the failure this whole file exists because of
	// — so the empty ones are named explicitly.
	keys := make([]string, 0, len(envelope.Variables))
	empty := make([]string, 0)
	for key, value := range envelope.Variables {
		keys = append(keys, key)
		if text, ok := value.(string); ok && text == "" {
			empty = append(empty, key)
		}
	}

	fields := map[string]any{
		"railway_operation": operation,
		"railway_variables": keys,
	}
	if len(empty) > 0 {
		// **THIS LINE IS THE WHOLE POINT.** An id that never made it out of
		// state arrives as `""`, and Railway reports it as an authorisation
		// failure that names nothing. Saying so plainly turns a multi-hour
		// investigation into a log line.
		fields["railway_empty_variables"] = empty
	}

	tflog.Debug(ctx, "Railway GraphQL request", fields)
	tflog.Trace(ctx, "Railway GraphQL request details", fields)
}
