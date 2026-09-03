// SPDX-License-Identifier: MPL-2.0

package resources

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	railway "github.com/micah5/terraform-provider-railway-next/graphql"
	"github.com/micah5/terraform-provider-railway-next/internal/client"
)

var (
	_ resource.Resource                = (*DeploymentTrigger)(nil)
	_ resource.ResourceWithConfigure   = (*DeploymentTrigger)(nil)
	_ resource.ResourceWithImportState = (*DeploymentTrigger)(nil)
)

// DeploymentTrigger connects a branch to a service, which is what actually
// makes a GitHub-sourced service deploy.
//
// railway_service manages one trigger by default for backward compatibility.
// This resource is the explicit ownership path when that service sets
// auto_deploy=false, including check-suite gating and multiple triggers.
//
// That is not hypothetical. A four-service environment applied cleanly, showed
// the right source on every service, and had zero `repoTriggers` and zero
// deployments; the only visible symptom was that nothing was reachable, with
// no error anywhere to explain it.
//
// So this is a separate resource rather than a field on `railway_service`, for
// the reason the two things genuinely are separate: a service may be built from
// an image (no trigger is possible), from a repository with continuous
// deployment (one trigger), or from a repository deployed only by hand or by
// CI (a source, deliberately no trigger). Folding it into the service would
// make the third case unexpressible.
type DeploymentTrigger struct {
	client *client.Client
}

type deploymentTriggerModel struct {
	ID            types.String   `tfsdk:"id"`
	ProjectID     types.String   `tfsdk:"project_id"`
	EnvironmentID types.String   `tfsdk:"environment_id"`
	ServiceID     types.String   `tfsdk:"service_id"`
	Repository    types.String   `tfsdk:"repository"`
	Branch        types.String   `tfsdk:"branch"`
	Provider      types.String   `tfsdk:"provider_name"`
	CheckSuites   types.Bool     `tfsdk:"check_suites"`
	Timeouts      timeouts.Value `tfsdk:"timeouts"`
}

func NewDeploymentTrigger() resource.Resource { return &DeploymentTrigger{} }

func (r *DeploymentTrigger) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_deployment_trigger"
}

func (r *DeploymentTrigger) Schema(
	ctx context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Explicitly subscribes a service to a branch, so a push deploys it. " +
			"Set auto_deploy=false on railway_service when this resource owns its trigger.",
		Attributes: map[string]schema.Attribute{
			"id": idAttribute("Railway deployment trigger ID."),
			// EVERY IDENTIFYING FIELD REPLACES. Railway's update mutation
			// covers the branch and check-suite settings; moving a trigger to
			// another service or project is a different trigger.
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Railway project ID.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"environment_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Railway environment ID this trigger deploys into.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"service_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Railway service ID to deploy.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"repository": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Repository in `owner/name` form. Must match the service's source.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"branch": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Branch whose pushes deploy this service.",
			},
			"provider_name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				// `provider` IS RESERVED IN HCL, hence `provider_name`. Naming
				// the attribute `provider` makes the block unparseable.
				MarkdownDescription: "Source control provider. Defaults to `github`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"check_suites": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Wait for the commit's check suites to pass before deploying. " +
					"Defaults to true, which is the safer direction: a red build should not reach an environment.",
			},
			"timeouts": timeouts.AttributesAll(ctx),
		},
	}
}

func (r *DeploymentTrigger) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	r.client = configureClient(req, resp)
}

func (r *DeploymentTrigger) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan deploymentTriggerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, plan.Timeouts, timeoutCreate, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()

	provider := plan.Provider.ValueString()
	if plan.Provider.IsNull() || plan.Provider.IsUnknown() || provider == "" {
		provider = "github"
	}

	result, err := railway.CreateDeploymentTrigger(ctx, r.client.GraphQL(), railway.DeploymentTriggerCreateInput{
		ProjectId:     plan.ProjectID.ValueString(),
		EnvironmentId: plan.EnvironmentID.ValueString(),
		ServiceId:     plan.ServiceID.ValueString(),
		Repository:    plan.Repository.ValueString(),
		Branch:        plan.Branch.ValueString(),
		Provider:      provider,
		CheckSuites:   boolPointer(plan.CheckSuites),
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to create Railway deployment trigger",
			client.DecodeAPIError(err).Error(),
		)
		return
	}

	setDeploymentTriggerState(&plan, result.DeploymentTriggerCreate.DeploymentTriggerFields)
	ResolveUnknowns(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *DeploymentTrigger) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state deploymentTriggerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, state.Timeouts, timeoutRead, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()

	// **READ THROUGH THE SERVICE, BECAUSE THERE IS NO TRIGGER-BY-ID QUERY.**
	// A trigger is only visible in its service's `repoTriggers`, so a deleted
	// service takes its triggers with it — which is the right behaviour for
	// state removal too.
	result, err := railway.GetService(
		ctx,
		r.client.GraphQL(),
		state.ServiceID.ValueString(),
		state.EnvironmentID.ValueString(),
	)
	if removeIfNotFound(ctx, err, resp, "Unable to read Railway deployment trigger") {
		return
	}

	for _, edge := range result.Service.RepoTriggers.Edges {
		if edge.Node.Id != state.ID.ValueString() {
			continue
		}
		state.Branch = types.StringValue(edge.Node.Branch)
		state.Repository = types.StringValue(edge.Node.Repository)
		// EVERY FIELD, so an IMPORT round-trips. Leaving these unset made an
		// imported trigger plan as a replacement, because `provider_name`
		// forces replacement and a field the read never fills always looks
		// changed.
		state.Provider = types.StringValue(edge.Node.Provider)
		state.CheckSuites = types.BoolValue(edge.Node.CheckSuites)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	// The service exists and no longer carries this trigger, so it is gone.
	resp.State.RemoveResource(ctx)
}

func (r *DeploymentTrigger) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var plan deploymentTriggerModel
	var prior deploymentTriggerModel
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

	// THE ID COMES FROM PRIOR STATE, NOT THE PLAN — see `idAttribute`. The
	// modifier makes them the same here, and reading it from the value that is
	// known by construction rather than by plan modifier is the safer habit.
	id := prior.ID.ValueString()

	result, err := railway.UpdateDeploymentTrigger(ctx, r.client.GraphQL(), id, railway.DeploymentTriggerUpdateInput{
		Branch:      stringPointer(plan.Branch),
		CheckSuites: boolPointer(plan.CheckSuites),
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to update Railway deployment trigger",
			client.DecodeAPIError(err).Error(),
		)
		return
	}

	setDeploymentTriggerState(&plan, result.DeploymentTriggerUpdate.DeploymentTriggerFields)
	ResolveUnknowns(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *DeploymentTrigger) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state deploymentTriggerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, state.Timeouts, timeoutDelete, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()

	if _, err := railway.DeleteDeploymentTrigger(ctx, r.client.GraphQL(), state.ID.ValueString()); err != nil {
		if client.IsNotFound(err) {
			return
		}
		resp.Diagnostics.AddError(
			"Unable to delete Railway deployment trigger",
			client.DecodeAPIError(err).Error(),
		)
	}
}

func (r *DeploymentTrigger) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	// `project/environment/service/trigger` — the service is part of the
	// address because a trigger can only be read through its service.
	parts, diagnostics := splitImportID(req.ID, 4)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("project_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("environment_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("service_id"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), parts[3])...)
}

func setDeploymentTriggerState(state *deploymentTriggerModel, remote railway.DeploymentTriggerFields) {
	state.ID = types.StringValue(remote.Id)
	state.Branch = types.StringValue(remote.Branch)
	state.Repository = types.StringValue(remote.Repository)
	state.Provider = types.StringValue(remote.Provider)
	state.CheckSuites = types.BoolValue(remote.CheckSuites)
}
