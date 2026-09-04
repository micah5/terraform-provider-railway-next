// SPDX-License-Identifier: MPL-2.0

package resources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAwaitConsistencyHonoursTheCallerDeadline is the property the four
// hand-rolled waits got wrong: the ceiling belongs to the CALLER.
//
// Each of `service`, `volume`, `bucket` and `postgres` built its own context —
// thirty seconds, or a minute in postgres, with no recorded reason for the
// difference — so a practitioner writing `timeouts { create = "5m" }` still got
// thirty seconds and an error blaming Railway for being slow.
func TestAwaitConsistencyHonoursTheCallerDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := awaitConsistency(ctx, time.Millisecond, func(context.Context) error {
		return errNotReady
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	// A bespoke 30-second deadline would blow straight past the caller's 50ms.
	if elapsed > 5*time.Second {
		t.Errorf("waited %v, so a deadline other than the caller's was in force", elapsed)
	}
}

// TestAwaitConsistencyStopsOnRealErrors distinguishes "not yet" from "no".
//
// An authorisation failure or a malformed request will not fix itself. Retrying
// it for the whole timeout turns a clear error into a slow one — and that is
// precisely how `Not Authorized` on an empty service id would have presented if
// it had been retried: as a timeout rather than as a fact.
func TestAwaitConsistencyStopsOnRealErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("not authorized")
	calls := 0

	err := awaitConsistency(context.Background(), time.Millisecond, func(context.Context) error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the probe's own error", err)
	}
	if calls != 1 {
		t.Errorf("probe ran %d times, want 1 — a real error must not be retried", calls)
	}
}

func TestAwaitConsistencyRetriesTransientReadErrors(t *testing.T) {
	t.Parallel()

	calls := 0
	err := awaitConsistency(context.Background(), time.Millisecond, func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("probe ran %d times, want 2", calls)
	}
}

// TestAwaitConsistencyProbesBeforeSleeping keeps the common case fast.
//
// The object is usually already there, and waiting a full interval to discover
// that adds the interval to every single apply.
func TestAwaitConsistencyProbesBeforeSleeping(t *testing.T) {
	t.Parallel()

	start := time.Now()
	if err := awaitConsistency(context.Background(), time.Hour, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if time.Since(start) > time.Second {
		t.Error("the first probe waited for the ticker instead of running immediately")
	}
}

// TestNoResourceBuildsItsOwnDeadline is the regression guard for the whole
// class, not for the four instances that happened to exist.
//
// Every one of these waits was written the same way — `context.WithTimeout` on
// a literal, a ticker, and a select — and each silently overrode the
// practitioner's `timeouts` block. Fixing four call sites without pinning the
// property just means the fifth arrives later.
//
// `operationContext` is the ONE place allowed to derive a deadline, because
// that is the one that reads the configuration.
func TestNoResourceBuildsItsOwnDeadline(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	deadline := regexp.MustCompile(`context\.WithTimeout\(`)

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// `common.go` holds `operationContext`, which is where a deadline is
		// SUPPOSED to come from — it derives one from the practitioner's own
		// `timeouts` block.
		if name == "common.go" {
			continue
		}

		source, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		if deadline.Match(source) {
			t.Errorf("%s builds its own deadline; use awaitConsistency so the "+
				"practitioner's timeouts block is the ceiling", name)
		}
	}
}
