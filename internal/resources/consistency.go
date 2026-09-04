// SPDX-License-Identifier: MPL-2.0

package resources

import (
	"context"
	"errors"
	"time"

	"github.com/micah5/terraform-provider-railway-next/internal/client"
)

// **RAILWAY IS EVENTUALLY CONSISTENT, AND THAT IS THE THROUGH-LINE OF EVERY
// TIMING BUG IN THIS PROVIDER.**
//
// A mutation returns when the API has accepted it, not when the object is
// readable. So a create is followed by a window in which:
//
//   - a service exists but its per-environment INSTANCE does not, and
//     `serviceConnect` fails with `ServiceInstance not found`
//   - a bucket's change set is applied but the bucket is not yet listed
//   - a volume exists but its environment attachment is not visible
//   - a Postgres service exists but its deployment has not started
//
// Each of those grew its own bespoke wait — `waitForBucketRegistration`,
// `waitForVolumeInstance`, `waitForPostgres` — with different intervals and
// different notions of "ready". `service` grew none, which is exactly why the
// connect race survived until it broke a real apply.
//
// **THE HARDCODED 30 SECONDS WAS A BUG IN ALL THREE.** A practitioner writing
// `timeouts { create = "5m" }` still got thirty seconds, because the wait built
// its own context instead of using the one `operationContext` already derived
// from their configuration. `awaitConsistency` uses the ambient context, so the
// configured timeout is the ceiling and the caller's intent is honoured.
//
// The remaining parameter is the POLL INTERVAL, which is a property of how fast
// the thing being waited on settles rather than of how long anybody is willing
// to wait.

// consistencyPollInterval is how often a consistency wait re-reads Railway.
//
// It is the POLL RATE, not a deadline. Every wait in this package used to build
// its own deadline — thirty seconds in `service`, `volume` and `bucket`, a
// minute in `postgres`, with no recorded reason for the difference — which
// silently overrode the practitioner's `timeouts` block. The ceiling now comes
// from the ambient context, and this is the only knob left.
const consistencyPollInterval = time.Second

// errNotReady is returned by a probe that has not seen what it is waiting for.
// It is not a failure — it is the normal answer during the consistency window.
var errNotReady = errors.New("not ready")

// awaitConsistency polls until probe returns nil, the context expires, or probe
// returns a non-retryable error.
//
// A probe that returns a REAL error stops immediately: an authorisation failure
// or a malformed request will not fix itself, and retrying it for the whole
// timeout turns a clear error into a slow one. `errNotReady` and transient
// read transport failures mean "look again"; authentication and schema errors
// still stop immediately.
//
// The first probe runs BEFORE any sleep, because the object is often already
// there — waiting a second to discover that is a second added to every apply.
func awaitConsistency(ctx context.Context, interval time.Duration, probe func(context.Context) error) error {
	retryable := func(err error) bool {
		return errors.Is(err, errNotReady) ||
			(ctx.Err() == nil && client.IsRetryableReadError(err))
	}
	if err := probe(ctx); !retryable(err) {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			// REPORT WHAT THE LAST ATTEMPT SAW, not just that time ran out.
			// "deadline exceeded" alone sends somebody looking at their timeout
			// when the real answer is in the error the probe kept returning.
			if lastErr != nil && !errors.Is(lastErr, errNotReady) {
				return lastErr
			}
			return ctx.Err()

		case <-ticker.C:
			err := probe(ctx)
			if !retryable(err) {
				return err
			}
			lastErr = err
		}
	}
}
