package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	railway "github.com/micah5/terraform-provider-railway-next/graphql"
	"github.com/micah5/terraform-provider-railway-next/internal/changeset"
	"github.com/micah5/terraform-provider-railway-next/internal/client"
	"github.com/micah5/terraform-provider-railway-next/internal/references"
)

var (
	_ resource.Resource                = (*Bucket)(nil)
	_ resource.ResourceWithConfigure   = (*Bucket)(nil)
	_ resource.ResourceWithImportState = (*Bucket)(nil)
)

type Bucket struct {
	client *client.Client
}

type bucketModel struct {
	ID            types.String   `tfsdk:"id"`
	ProjectID     types.String   `tfsdk:"project_id"`
	EnvironmentID types.String   `tfsdk:"environment_id"`
	Name          types.String   `tfsdk:"name"`
	Region        types.String   `tfsdk:"region"`
	References    types.Map      `tfsdk:"references"`
	Timeouts      timeouts.Value `tfsdk:"timeouts"`
}

type environmentConfig struct {
	Buckets map[string]*environmentBucket `json:"buckets"`
}

type environmentBucket struct {
	Region    string `json:"region"`
	IsDeleted bool   `json:"isDeleted"`
	IsCreated bool   `json:"isCreated"`
}

func NewBucket() resource.Resource {
	return &Bucket{}
}

func (r *Bucket) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bucket"
}

func (r *Bucket) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Railway storage bucket registered in an environment. Region changes replace the bucket. Railway may retain a deleted bucket internally for a delayed permanent-deletion period.",
		Attributes: map[string]schema.Attribute{
			"id": idAttribute("Railway bucket ID."),
			"project_id": schema.StringAttribute{
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"environment_id": schema.StringAttribute{
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Bucket display name and Railway reference namespace.",
			},
			"region": schema.StringAttribute{
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf("sjc", "iad", "ams", "sin"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"references": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Safe Railway reference expressions. These are references, never bucket credentials.",
			},
			"timeouts": timeouts.AttributesAll(ctx),
		},
	}
}

func (r *Bucket) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureClient(req, resp)
}

func (r *Bucket) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	stateCtx := ctx
	var plan bucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, plan.Timeouts, timeoutCreate, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	existing, err := r.findBucketByName(ctx, plan.ProjectID.ValueString(), plan.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to check for an existing Railway bucket", client.DecodeAPIError(err).Error())
		return
	}
	if existing != nil {
		resp.Diagnostics.AddError(
			"Railway bucket name already exists",
			"A bucket named "+plan.Name.ValueString()+" already exists in the project. Import it instead of creating a second bucket with the same reference namespace.",
		)
		return
	}

	payload, err := changeset.RegisterBucket(plan.Name.ValueString(), plan.Region.ValueString()).JSON()
	if err != nil {
		resp.Diagnostics.AddError("Unable to build Railway bucket registration", err.Error())
		return
	}
	message := "Terraform: register bucket " + plan.Name.ValueString()
	_, err = applyEnvironmentChangeSet(
		ctx,
		r.client.GraphQL(),
		plan.EnvironmentID.ValueString(),
		payload,
		message,
	)
	if err != nil && !client.IsAmbiguousMutationError(err) {
		resp.Diagnostics.AddError("Unable to create Railway bucket", client.DecodeAPIError(err).Error())
		return
	}
	bucket, reconcileErr := r.waitForBucketRegistration(
		ctx,
		plan.ProjectID.ValueString(),
		plan.EnvironmentID.ValueString(),
		plan.Name.ValueString(),
		consistencyPollInterval,
	)
	if reconcileErr != nil || bucket == nil {
		detail := "Railway did not expose exactly one registered bucket with the requested name before the create timeout expired."
		if err != nil {
			detail += " The apply request also returned: " + client.DecodeAPIError(err).Error()
		}
		if reconcileErr != nil {
			detail += " Reconciliation returned: " + client.DecodeAPIError(reconcileErr).Error()
		}

		// Never issue a final request on the expired operation context. Persist a
		// provisional identity instead: the next Read resolves the project/name
		// pair to Railway's real id once asynchronous registration becomes
		// visible. This prevents a retry from creating a duplicate orphan.
		plan.ID = pendingResourceID("bucket")
		r.setReferences(stateCtx, &plan, &resp.Diagnostics)
		ResolveUnknowns(&plan)
		resp.Diagnostics.Append(resp.State.Set(stateCtx, &plan)...)
		resp.Diagnostics.AddWarning(
			"Railway bucket creation is still pending",
			detail+" Terraform saved a pending identity and will adopt the bucket by project and name on a later refresh instead of creating a duplicate.",
		)
		return
	}
	plan.ID = types.StringValue(bucket.Id)
	plan.Name = types.StringValue(bucket.Name)

	// The bucket exists in Railway now, so it belongs in state even if reading
	// its references fails. Without this, a reference lookup error leaves a
	// registered bucket Terraform has no record of — and the next apply fails
	// with `Railway bucket name already exists`, which Railway holds through
	// its delayed deletion window.
	r.setReferences(ctx, &plan, &resp.Diagnostics)
	ResolveUnknowns(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *Bucket) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	stateCtx := ctx
	var state bucketModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, state.Timeouts, timeoutRead, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	if isPendingResourceID(state.ID, "bucket") {
		ready, err := r.adoptPending(ctx, &state)
		if err != nil {
			resp.Diagnostics.AddError("Unable to reconcile pending Railway bucket", client.DecodeAPIError(err).Error())
			return
		}
		if !ready {
			resp.Diagnostics.AddWarning(
				"Railway bucket creation is still pending",
				"The earlier change set may still be applying. Terraform retained the provisional state and will check again on the next refresh.",
			)
			resp.Diagnostics.Append(resp.State.Set(stateCtx, &state)...)
			return
		}
	}

	buckets, err := railway.ListProjectBuckets(ctx, r.client.GraphQL(), state.ProjectID.ValueString())
	if removeIfNotFound(ctx, err, resp, "Unable to read Railway bucket") {
		return
	}
	found := false
	for _, edge := range buckets.Project.Buckets.Edges {
		if edge.Node.Id == state.ID.ValueString() {
			state.Name = types.StringValue(edge.Node.Name)
			found = true
			break
		}
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	config, err := r.readEnvironmentConfig(ctx, state.EnvironmentID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Railway bucket registration", client.DecodeAPIError(err).Error())
		return
	}
	registered, ok := config.Buckets[state.ID.ValueString()]
	if !ok || registered == nil || registered.IsDeleted {
		resp.State.RemoveResource(ctx)
		return
	}
	if registered.Region != "" {
		state.Region = types.StringValue(registered.Region)
	}
	r.setReferences(ctx, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *Bucket) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan bucketModel
	var prior bucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, plan.Timeouts, timeoutUpdate, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	if isPendingResourceID(prior.ID, "bucket") {
		ready, err := r.adoptPending(ctx, &prior)
		if err != nil {
			resp.Diagnostics.AddError("Unable to reconcile pending Railway bucket", client.DecodeAPIError(err).Error())
			return
		}
		if !ready {
			resp.Diagnostics.AddError("Railway bucket creation is still pending", "Terraform will not update or recreate the bucket until Railway exposes the result of the earlier change set.")
			return
		}
		plan.ID = prior.ID
	}
	unlockChangeSet := lockEnvironmentChangeSet(plan.EnvironmentID.ValueString())
	defer unlockChangeSet()
	result, err := railway.UpdateBucket(ctx, r.client.GraphQL(), plan.ID.ValueString(), railway.BucketUpdateInput{
		Name: plan.Name.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to update Railway bucket", client.DecodeAPIError(err).Error())
		return
	}
	plan.Name = types.StringValue(result.BucketUpdate.Name)
	r.setReferences(ctx, &plan, &resp.Diagnostics)
	ResolveUnknowns(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *Bucket) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state bucketModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, state.Timeouts, timeoutDelete, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	if isPendingResourceID(state.ID, "bucket") {
		ready, err := r.adoptPending(ctx, &state)
		if err != nil {
			resp.Diagnostics.AddError("Unable to reconcile pending Railway bucket", client.DecodeAPIError(err).Error())
			return
		}
		if !ready {
			resp.Diagnostics.AddError("Railway bucket creation is still pending", "Terraform cannot safely forget this provisional state because the bucket may still appear after destroy. Retry after Railway finishes applying the earlier change set.")
			return
		}
	}
	payload, err := changeset.DeleteBucket(state.Name.ValueString(), state.Region.ValueString()).JSON()
	if err != nil {
		resp.Diagnostics.AddError("Unable to build Railway bucket deletion", err.Error())
		return
	}
	message := "Terraform: delete bucket " + state.Name.ValueString()
	_, err = applyEnvironmentChangeSet(
		ctx,
		r.client.GraphQL(),
		state.EnvironmentID.ValueString(),
		payload,
		message,
	)
	if err != nil {
		registered, readErr := r.isRegistered(ctx, state.EnvironmentID.ValueString(), state.ID.ValueString())
		if readErr != nil || registered {
			resp.Diagnostics.AddError("Unable to delete Railway bucket", client.DecodeAPIError(err).Error())
			return
		}
		// The mutation errored but the bucket is gone, so the delete stands.
		return
	}

	// **A SUCCESSFUL CHANGE SET IS NOT A COMPLETED DELETE, AND THIS IS WHERE
	// THAT COST A LIVE BUCKET.**
	//
	// `environmentStagedChangesApply` returns when Railway has ACCEPTED the
	// change set. The bucket is deregistered afterwards, asynchronously. The
	// old code returned here on success, so Terraform recorded the resource as
	// destroyed and dropped it from state — and Railway then left it
	// registered, with `deletedAt` null. The next apply planned to create it,
	// hit the name that was still taken, and the operator was left with a live
	// bucket no configuration owned.
	//
	// Returning an error here would be no better: the delete was requested and
	// may yet land. So this WAITS for the deregistration it just asked for, and
	// only reports failure if the bucket is still registered when the
	// practitioner's own `timeouts { delete = ... }` runs out.
	waitErr := awaitConsistency(ctx, consistencyPollInterval, func(ctx context.Context) error {
		registered, readErr := r.isRegistered(ctx, state.EnvironmentID.ValueString(), state.ID.ValueString())
		if readErr != nil {
			return readErr
		}
		if registered {
			return errNotReady
		}
		return nil
	})
	if waitErr != nil {
		resp.Diagnostics.AddError(
			"Unable to confirm Railway bucket deletion",
			"Railway accepted the change set deleting bucket "+state.Name.ValueString()+
				", but the bucket was still registered to the environment when the delete timeout expired. "+
				"Railway retains a deleted bucket through a delayed permanent-deletion window; if the name is "+
				"still taken on the next apply, the deletion has not finished. Underlying error: "+waitErr.Error(),
		)
	}
}

func (r *Bucket) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts, diagnostics := splitImportID(req.ID, 3)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("project_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("environment_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), parts[2])...)
}

func (r *Bucket) findBucketByName(ctx context.Context, projectID, name string) (*railway.BucketFields, error) {
	result, err := railway.ListProjectBuckets(ctx, r.client.GraphQL(), projectID)
	if err != nil {
		return nil, err
	}
	var found *railway.BucketFields
	for _, edge := range result.Project.Buckets.Edges {
		if edge.Node.Name != name {
			continue
		}
		// **A DELETED BUCKET IS STILL LISTED, AND IT IS NOT A BUCKET.**
		//
		// Railway keeps a deleted bucket in this listing through its delayed
		// permanent-deletion window. Counting it made `Create`'s
		// already-exists guard fire on the provider's own tombstone, so a
		// REPLACE — destroy then create under the same name, which is what a
		// region change plans — refused the create Terraform had just planned
		// and left the resource out of state.
		if edge.Node.DeletedAt != nil {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple Railway buckets named %q", name)
		}
		copy := edge.Node.BucketFields
		found = &copy
	}
	return found, nil
}

func (r *Bucket) adoptPending(ctx context.Context, state *bucketModel) (bool, error) {
	bucket, err := r.findBucketByName(ctx, state.ProjectID.ValueString(), state.Name.ValueString())
	if err != nil || bucket == nil {
		return false, err
	}
	registered, err := r.isRegistered(ctx, state.EnvironmentID.ValueString(), bucket.Id)
	if err != nil || !registered {
		return false, err
	}
	state.ID = types.StringValue(bucket.Id)
	state.Name = types.StringValue(bucket.Name)
	return true, nil
}

func (r *Bucket) readEnvironmentConfig(ctx context.Context, environmentID string) (*environmentConfig, error) {
	result, err := railway.GetEnvironmentConfiguration(ctx, r.client.GraphQL(), environmentID)
	if err != nil {
		return nil, err
	}
	var config environmentConfig
	if err := json.Unmarshal(result.Environment.Config, &config); err != nil {
		return nil, fmt.Errorf("decode opaque Railway environment config: %w", err)
	}
	return &config, nil
}

func (r *Bucket) isRegistered(ctx context.Context, environmentID, bucketID string) (bool, error) {
	config, err := r.readEnvironmentConfig(ctx, environmentID)
	if err != nil {
		return false, err
	}
	bucket, ok := config.Buckets[bucketID]
	return ok && bucket != nil && !bucket.IsDeleted, nil
}

func (r *Bucket) waitForBucketRegistration(
	ctx context.Context,
	projectID string,
	environmentID string,
	name string,
	interval time.Duration,
) (*railway.BucketFields, error) {
	// **THE TIMEOUT COMES FROM THE CALLER'S CONTEXT, NOT FROM HERE.**
	//
	// This used to wrap `ctx` in its own 30-second deadline, so a practitioner
	// writing `timeouts { create = "5m" }` still got thirty seconds and an
	// error that told them Railway was too slow rather than that the provider
	// had ignored them.
	var found *railway.BucketFields
	err := awaitConsistency(ctx, interval, func(ctx context.Context) error {
		bucket, err := r.findBucketByName(ctx, projectID, name)
		if err != nil {
			return err
		}
		if bucket == nil {
			return errNotReady
		}
		registered, err := r.isRegistered(ctx, environmentID, bucket.Id)
		if err != nil {
			return err
		}
		if !registered {
			return errNotReady
		}
		found = bucket
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (r *Bucket) setReferences(ctx context.Context, state *bucketModel, diagnostics *diag.Diagnostics) {
	value, diagResult := types.MapValueFrom(ctx, types.StringType, references.Bucket(state.Name.ValueString()))
	diagnostics.Append(diagResult...)
	state.References = value
}

func pathRoot(name string) path.Path {
	return path.Root(name)
}
