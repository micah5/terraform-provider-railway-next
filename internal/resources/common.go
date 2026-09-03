package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	genqlient "github.com/Khan/genqlient/graphql"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	railway "github.com/micah5/terraform-provider-railway-next/graphql"
	"github.com/micah5/terraform-provider-railway-next/internal/client"
)

const (
	defaultCreateTimeout = 15 * time.Minute
	defaultReadTimeout   = 2 * time.Minute
	defaultUpdateTimeout = 15 * time.Minute
	defaultDeleteTimeout = 15 * time.Minute
	changeSetMaxAttempts = 4
	pendingIDPrefix      = "terraform-pending:"
)

var environmentChangeSetLocks sync.Map

func pendingResourceID(kind string) types.String {
	return types.StringValue(pendingIDPrefix + kind)
}

func isPendingResourceID(value types.String, kind string) bool {
	return !value.IsNull() && !value.IsUnknown() && value.ValueString() == pendingIDPrefix+kind
}

func lockEnvironmentChangeSet(environmentID string) func() {
	value, _ := environmentChangeSetLocks.LoadOrStore(environmentID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func previewEnvironmentChangeSet(
	ctx context.Context,
	graphqlClient genqlient.Client,
	environmentID string,
	input json.RawMessage,
) (json.RawMessage, error) {
	preview, err := railway.PreviewEnvironmentChangeSet(ctx, graphqlClient, environmentID, input)
	if err != nil {
		return nil, err
	}
	return preview.EnvironmentPreviewChangeSet.ChangeSet, nil
}

// applyEnvironmentChangeSet applies an intent-level change set using Railway's
// optimistic-concurrency token. STALE_ENVIRONMENT_BASE means Railway rejected
// the mutation before applying it, so it is safe to fetch a new token,
// re-preview the intent, and retry. Ambiguous transport failures are returned
// immediately for resource-specific read-after-error reconciliation.
func applyEnvironmentChangeSet(
	ctx context.Context,
	graphqlClient genqlient.Client,
	environmentID string,
	intent json.RawMessage,
	message string,
) (*railway.ApplyEnvironmentChangeSetResponse, error) {
	unlock := lockEnvironmentChangeSet(environmentID)
	defer unlock()

	var lastErr error
	for attempt := 0; attempt < changeSetMaxAttempts; attempt++ {
		environment, err := railway.GetEnvironmentConfiguration(ctx, graphqlClient, environmentID)
		if err != nil {
			return nil, err
		}
		payload, err := previewEnvironmentChangeSet(ctx, graphqlClient, environmentID, intent)
		if err != nil {
			return nil, err
		}
		etag := environment.Environment.ConfigEtag
		// **WAIT FOR THE CHANGE SET TO BE APPLIED, NOT MERELY QUEUED.**
		//
		// Railway returns from this mutation as soon as the change set is
		// accepted unless it is told otherwise, and every resource built on
		// change sets was silently relying on that acceptance meaning
		// completion. It does not. A bucket delete returned success and left
		// the bucket registered with `deletedAt` null: Terraform recorded the
		// resource as destroyed while Railway still had it, and the next apply
		// failed with `Railway bucket name already exists` against a bucket no
		// configuration owned.
		//
		// The ambient context still bounds this, so the practitioner's
		// `timeouts` block remains the ceiling.
		waitForCompletion := true
		applied, err := railway.ApplyEnvironmentChangeSet(
			ctx,
			graphqlClient,
			environmentID,
			payload,
			&message,
			&etag,
			&waitForCompletion,
		)
		if err == nil {
			return applied, nil
		}
		if !client.IsStaleEnvironment(err) {
			return nil, err
		}
		lastErr = err
		if attempt+1 < changeSetMaxAttempts {
			if err := waitForChangeSetRetry(ctx, attempt); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf(
		"Railway environment remained stale after %d attempts: %w",
		changeSetMaxAttempts,
		lastErr,
	)
}

func waitForChangeSetRetry(ctx context.Context, attempt int) error {
	duration := 100 * time.Millisecond * time.Duration(1<<attempt)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type timeoutOperation string

const (
	timeoutCreate timeoutOperation = "create"
	timeoutRead   timeoutOperation = "read"
	timeoutUpdate timeoutOperation = "update"
	timeoutDelete timeoutOperation = "delete"
)

func operationContext(
	ctx context.Context,
	value timeouts.Value,
	operation timeoutOperation,
	diagnostics *diag.Diagnostics,
) (context.Context, context.CancelFunc, bool) {
	var (
		duration time.Duration
		result   diag.Diagnostics
	)
	switch operation {
	case timeoutCreate:
		duration, result = value.Create(ctx, defaultCreateTimeout)
	case timeoutRead:
		duration, result = value.Read(ctx, defaultReadTimeout)
	case timeoutUpdate:
		duration, result = value.Update(ctx, defaultUpdateTimeout)
	case timeoutDelete:
		duration, result = value.Delete(ctx, defaultDeleteTimeout)
	default:
		diagnostics.AddError("Unsupported resource timeout operation", string(operation))
		return ctx, func() {}, false
	}
	diagnostics.Append(result...)
	if diagnostics.HasError() {
		return ctx, func() {}, false
	}
	timed, cancel := context.WithTimeout(ctx, duration)
	return timed, cancel, true
}

func configureClient(
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) *client.Client {
	if req.ProviderData == nil {
		return nil
	}
	configured, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *client.Client, got %T. Please report this provider bug.", req.ProviderData),
		)
		return nil
	}
	return configured
}

func removeIfNotFound(
	ctx context.Context,
	err error,
	state *resource.ReadResponse,
	summary string,
) bool {
	if client.IsNotFound(err) {
		state.State.RemoveResource(ctx)
		return true
	}
	if err != nil {
		state.Diagnostics.AddError(summary, client.DecodeAPIError(err).Error())
		return true
	}
	return false
}

func importID(
	ctx context.Context,
	id string,
	state *resource.ImportStateResponse,
) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), resource.ImportStateRequest{ID: id}, state)
}

func splitImportID(id string, count int) ([]string, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	parts := strings.Split(id, "/")
	if len(parts) != count {
		diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected %d slash-separated fields, got %d.", count, len(parts)),
		)
		return nil, diagnostics
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			diagnostics.AddError("Invalid import ID", "Import ID fields must not be empty.")
			return nil, diagnostics
		}
	}
	return parts, diagnostics
}

func valueString(value *string) types.String {
	if value == nil {
		return types.StringNull()
	}
	return types.StringValue(*value)
}

func stringPointer(value types.String) *string {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	result := value.ValueString()
	return &result
}

func boolPointer(value types.Bool) *bool {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	result := value.ValueBool()
	return &result
}

func intPointer(value types.Int64) *int {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	result := int(value.ValueInt64())
	return &result
}
