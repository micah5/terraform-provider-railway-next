package resources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	railway "github.com/micah5/terraform-provider-railway-next/graphql"
	"github.com/micah5/terraform-provider-railway-next/internal/changeset"
	"github.com/micah5/terraform-provider-railway-next/internal/client"
	"github.com/micah5/terraform-provider-railway-next/internal/references"
)

const postgresMountPath = "/var/lib/postgresql/data"

type postgresRemoteIDs struct {
	ServiceID        string
	VolumeID         string
	VolumeInstanceID string
}

var (
	_ resource.Resource                = (*Postgres)(nil)
	_ resource.ResourceWithConfigure   = (*Postgres)(nil)
	_ resource.ResourceWithImportState = (*Postgres)(nil)
)

type Postgres struct {
	client *client.Client
}

type postgresModel struct {
	ID                types.String   `tfsdk:"id"`
	ProjectID         types.String   `tfsdk:"project_id"`
	EnvironmentID     types.String   `tfsdk:"environment_id"`
	Name              types.String   `tfsdk:"name"`
	Version           types.String   `tfsdk:"version"`
	Region            types.String   `tfsdk:"region"`
	ServiceID         types.String   `tfsdk:"service_id"`
	VolumeID          types.String   `tfsdk:"volume_id"`
	VolumeInstanceID  types.String   `tfsdk:"volume_instance_id"`
	ServiceInstanceID types.String   `tfsdk:"service_instance_id"`
	DeploymentID      types.String   `tfsdk:"deployment_id"`
	References        types.Map      `tfsdk:"references"`
	Timeouts          timeouts.Value `tfsdk:"timeouts"`
}

func NewPostgres() resource.Resource {
	return &Postgres{}
}

func (r *Postgres) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_postgres"
}

func (r *Postgres) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A composite Railway PostgreSQL service using the official `ghcr.io/railwayapp-templates/postgres-ssl` image and a persistent data volume. Destroying this resource permanently deletes database data.",
		Attributes: map[string]schema.Attribute{
			"id": idAttribute("Railway Postgres service ID."),
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
			"name": schema.StringAttribute{Required: true},
			"version": schema.StringAttribute{
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"service_id":          schema.StringAttribute{Computed: true},
			"volume_id":           schema.StringAttribute{Computed: true},
			"volume_instance_id":  schema.StringAttribute{Computed: true},
			"service_instance_id": schema.StringAttribute{Computed: true},
			"deployment_id":       schema.StringAttribute{Computed: true},
			"references": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Safe Railway reference expressions. Database credentials are never returned to Terraform.",
			},
			"timeouts": timeouts.AttributesAll(ctx),
		},
	}
}

func (r *Postgres) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureClient(req, resp)
}

func (r *Postgres) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	stateCtx := ctx
	var plan postgresModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, plan.Timeouts, timeoutCreate, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	existingServiceID, err := r.findServiceIDByName(ctx, plan.ProjectID.ValueString(), plan.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to check for an existing Railway PostgreSQL service", client.DecodeAPIError(err).Error())
		return
	}
	if existingServiceID != "" {
		resp.Diagnostics.AddError(
			"Railway service name already exists",
			"A service named "+plan.Name.ValueString()+" already exists in the project. Import it instead of creating a second PostgreSQL service with the same reference namespace.",
		)
		return
	}
	set := changeset.CreatePostgres(plan.Name.ValueString(), plan.Version.ValueString(), plan.Region.ValueString())
	payload, err := set.JSON()
	if err != nil {
		resp.Diagnostics.AddError("Unable to build Railway PostgreSQL change set", err.Error())
		return
	}
	message := "Terraform: create PostgreSQL " + plan.Name.ValueString()
	applied, err := applyEnvironmentChangeSet(
		ctx,
		r.client.GraphQL(),
		plan.EnvironmentID.ValueString(),
		payload,
		message,
	)
	if err != nil && !client.IsAmbiguousMutationError(err) {
		resp.Diagnostics.AddError("Unable to create Railway PostgreSQL", client.DecodeAPIError(err).Error())
		return
	}
	ids, reconcileErr := r.waitForPostgres(
		ctx,
		plan.ProjectID.ValueString(),
		plan.EnvironmentID.ValueString(),
		plan.Name.ValueString(),
		time.Second,
	)
	if reconcileErr != nil || ids == nil {
		detail := "Railway did not expose exactly one PostgreSQL service and attached data volume with the requested name before the create timeout expired."
		if err != nil {
			detail += " The apply request also returned: " + client.DecodeAPIError(err).Error()
		}
		if reconcileErr != nil {
			detail += " Reconciliation returned: " + client.DecodeAPIError(reconcileErr).Error()
		}
		plan.ID = pendingResourceID("postgres")
		plan.ServiceID = types.StringNull()
		plan.VolumeID = types.StringNull()
		plan.VolumeInstanceID = types.StringNull()
		value, converted := types.MapValueFrom(stateCtx, types.StringType, references.Postgres(plan.Name.ValueString()))
		resp.Diagnostics.Append(converted...)
		plan.References = value
		ResolveUnknowns(&plan)
		resp.Diagnostics.Append(resp.State.Set(stateCtx, &plan)...)
		resp.Diagnostics.AddWarning(
			"Railway PostgreSQL creation is still pending",
			detail+" Terraform saved a pending identity and will adopt the service and volume by project, environment, and name on a later refresh instead of creating a duplicate.",
		)
		return
	}
	plan.ServiceID = types.StringValue(ids.ServiceID)
	plan.ID = plan.ServiceID
	plan.VolumeID = types.StringValue(ids.VolumeID)
	plan.VolumeInstanceID = types.StringValue(ids.VolumeInstanceID)
	if err == nil && applied.EnvironmentApplyChangeSet.DeploymentId != nil {
		plan.DeploymentID = types.StringValue(*applied.EnvironmentApplyChangeSet.DeploymentId)
	}
	// The database exists now. A failed refresh must not discard it — a
	// PostgreSQL service Terraform has no record of is one that cannot be
	// renamed, updated or destroyed through the provider, and the next apply
	// collides with it by name.
	refreshed := r.refresh(ctx, &plan, &resp.Diagnostics)
	ResolveUnknowns(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if !refreshed {
		return
	}
}

func (r *Postgres) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	stateCtx := ctx
	var state postgresModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, state.Timeouts, timeoutRead, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	if isPendingResourceID(state.ID, "postgres") {
		ready, err := r.adoptPending(ctx, &state)
		if err != nil {
			resp.Diagnostics.AddError("Unable to reconcile pending Railway PostgreSQL", client.DecodeAPIError(err).Error())
			return
		}
		if !ready {
			resp.Diagnostics.AddWarning(
				"Railway PostgreSQL creation is still pending",
				"The earlier change set may still be applying. Terraform retained the provisional state and will check again on the next refresh.",
			)
			resp.Diagnostics.Append(resp.State.Set(stateCtx, &state)...)
			return
		}
	}
	if !r.refresh(ctx, &state, &resp.Diagnostics) {
		if !resp.Diagnostics.HasError() {
			resp.State.RemoveResource(ctx)
		}
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *Postgres) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan postgresModel
	var prior postgresModel
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
	// These component ids are computed and therefore unknown in an update plan.
	// They identify the existing composite resource, so carry them from prior
	// state rather than sending empty ids to Railway.
	plan.ID = prior.ID
	plan.ServiceID = prior.ServiceID
	plan.VolumeID = prior.VolumeID
	plan.VolumeInstanceID = prior.VolumeInstanceID
	if isPendingResourceID(prior.ID, "postgres") {
		ready, err := r.adoptPending(ctx, &prior)
		if err != nil {
			resp.Diagnostics.AddError("Unable to reconcile pending Railway PostgreSQL", client.DecodeAPIError(err).Error())
			return
		}
		if !ready {
			resp.Diagnostics.AddError("Railway PostgreSQL creation is still pending", "Terraform will not update or recreate the database until Railway exposes the result of the earlier change set.")
			return
		}
		plan.ID = prior.ID
		plan.ServiceID = prior.ServiceID
		plan.VolumeID = prior.VolumeID
		plan.VolumeInstanceID = prior.VolumeInstanceID
	}
	unlockEnvironment := lockEnvironmentChangeSet(plan.EnvironmentID.ValueString())
	defer unlockEnvironment()
	_, err := railway.UpdateService(ctx, r.client.GraphQL(), plan.ServiceID.ValueString(), railway.ServiceUpdateInput{
		Name: stringPointer(plan.Name),
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to rename Railway PostgreSQL service", client.DecodeAPIError(err).Error())
		return
	}
	if !r.refresh(ctx, &plan, &resp.Diagnostics) {
		return
	}
	ResolveUnknowns(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *Postgres) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state postgresModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel, ok := operationContext(ctx, state.Timeouts, timeoutDelete, &resp.Diagnostics)
	if !ok {
		return
	}
	defer cancel()
	if isPendingResourceID(state.ID, "postgres") {
		ready, err := r.adoptPending(ctx, &state)
		if err != nil {
			resp.Diagnostics.AddError("Unable to reconcile pending Railway PostgreSQL", client.DecodeAPIError(err).Error())
			return
		}
		if !ready {
			resp.Diagnostics.AddError("Railway PostgreSQL creation is still pending", "Terraform cannot safely forget this provisional state because the service and volume may still appear after destroy. Retry after Railway finishes applying the earlier change set.")
			return
		}
	}
	serviceDeletedByChangeSet := false
	payload, err := changeset.DeletePostgres(state.Name.ValueString(), state.Version.ValueString(), state.Region.ValueString()).JSON()
	if err == nil {
		message := "Terraform: delete PostgreSQL " + state.Name.ValueString()
		applied, applyErr := applyEnvironmentChangeSet(
			ctx,
			r.client.GraphQL(),
			state.EnvironmentID.ValueString(),
			payload,
			message,
		)
		if applyErr != nil && !client.IsNotFound(applyErr) {
			resp.Diagnostics.AddWarning("Railway PostgreSQL change-set deletion was not confirmed", client.DecodeAPIError(applyErr).Error())
		} else if applyErr == nil {
			serviceDeletedByChangeSet = strings.EqualFold(applied.EnvironmentApplyChangeSet.Status, "applied")
		}
	}
	unlockEnvironment := lockEnvironmentChangeSet(state.EnvironmentID.ValueString())
	defer unlockEnvironment()
	if !state.VolumeID.IsNull() {
		if _, err := railway.DeleteVolume(ctx, r.client.GraphQL(), state.VolumeID.ValueString()); err != nil && !client.IsNotFound(err) {
			resp.Diagnostics.AddError("Unable to delete Railway PostgreSQL volume", client.DecodeAPIError(err).Error())
			return
		}
	}
	if !serviceDeletedByChangeSet && !state.ServiceID.IsNull() {
		if _, err := railway.DeleteService(ctx, r.client.GraphQL(), state.ServiceID.ValueString(), nil); err != nil && !client.IsNotFound(err) {
			resp.Diagnostics.AddError("Unable to delete Railway PostgreSQL service", client.DecodeAPIError(err).Error())
		}
	}
}

func (r *Postgres) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts, diagnostics := splitImportID(req.ID, 4)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("service_id"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("volume_id"), parts[3])...)
}

func (r *Postgres) refresh(ctx context.Context, state *postgresModel, diagnostics *diag.Diagnostics) bool {
	// Computed values arrive as unknown during Create and Import. Railway can
	// register service/volume instances asynchronously, so absence is a known
	// null rather than an unknown value after the operation completes.
	resetPostgresComputedIdentifiers(state)

	serviceID := state.ServiceID.ValueString()
	if serviceID == "" {
		serviceID = state.ID.ValueString()
		state.ServiceID = types.StringValue(serviceID)
	}
	service, err := railway.GetService(ctx, r.client.GraphQL(), serviceID, state.EnvironmentID.ValueString())
	if client.IsNotFound(err) {
		return false
	}
	if err != nil {
		diagnostics.AddError("Unable to read Railway PostgreSQL service", client.DecodeAPIError(err).Error())
		return false
	}
	state.ID = types.StringValue(service.Service.Id)
	state.ServiceID = state.ID
	state.ProjectID = types.StringValue(service.Service.ProjectId)
	state.Name = types.StringValue(service.Service.Name)
	for _, edge := range service.Environment.ServiceInstances.Edges {
		if edge.Node.ServiceId != serviceID {
			continue
		}
		state.ServiceInstanceID = types.StringValue(edge.Node.Id)
		if edge.Node.Source != nil && edge.Node.Source.Image != nil {
			state.Version = types.StringValue(strings.TrimPrefix(*edge.Node.Source.Image, "ghcr.io/railwayapp-templates/postgres-ssl:"))
		}
		if edge.Node.Region != nil {
			state.Region = types.StringValue(normalizePostgresRegion(*edge.Node.Region))
		}
		if edge.Node.LatestDeployment != nil {
			state.DeploymentID = types.StringValue(edge.Node.LatestDeployment.Id)
		}
		break
	}

	volumes, err := railway.GetProjectVolumes(ctx, r.client.GraphQL(), state.ProjectID.ValueString(), state.EnvironmentID.ValueString())
	if err != nil {
		diagnostics.AddError("Unable to read Railway PostgreSQL volume", client.DecodeAPIError(err).Error())
		return false
	}
	volumeFound := false
	for _, edge := range volumes.Project.Volumes.Edges {
		if edge.Node.Id == state.VolumeID.ValueString() {
			volumeFound = true
			break
		}
	}
	if !volumeFound && !state.VolumeID.IsNull() {
		return false
	}
	for _, edge := range volumes.Environment.VolumeInstances.Edges {
		if edge.Node.VolumeId == state.VolumeID.ValueString() {
			state.VolumeInstanceID = types.StringValue(edge.Node.Id)
			if edge.Node.Region != nil {
				state.Region = types.StringValue(normalizePostgresRegion(*edge.Node.Region))
			}
			break
		}
	}
	value, converted := types.MapValueFrom(ctx, types.StringType, references.Postgres(state.Name.ValueString()))
	diagnostics.Append(converted...)
	state.References = value
	return !diagnostics.HasError()
}

func resetPostgresComputedIdentifiers(state *postgresModel) {
	state.VolumeInstanceID = types.StringNull()
	state.ServiceInstanceID = types.StringNull()
	state.DeploymentID = types.StringNull()
}

func normalizePostgresRegion(region string) string {
	switch region {
	case "us-west2":
		return "sjc"
	case "us-east4-eqdc4a":
		return "iad"
	case "europe-west4-drams3a":
		return "ams"
	case "asia-southeast1-eqsg3a":
		return "sin"
	default:
		return region
	}
}

func (r *Postgres) findServiceIDByName(ctx context.Context, projectID, name string) (string, error) {
	result, err := railway.ListProjectServices(ctx, r.client.GraphQL(), projectID)
	if err != nil {
		return "", err
	}
	var found string
	for _, edge := range result.Project.Services.Edges {
		if edge.Node.Name != name {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("multiple Railway services named %q", name)
		}
		found = edge.Node.Id
	}
	return found, nil
}

func (r *Postgres) findPostgres(
	ctx context.Context,
	projectID string,
	environmentID string,
	name string,
) (*postgresRemoteIDs, error) {
	serviceID, err := r.findServiceIDByName(ctx, projectID, name)
	if err != nil || serviceID == "" {
		return nil, err
	}
	volumes, err := railway.GetProjectVolumes(ctx, r.client.GraphQL(), projectID, environmentID)
	if err != nil {
		return nil, err
	}
	for _, edge := range volumes.Environment.VolumeInstances.Edges {
		instance := edge.Node
		if instance.DeletedAt != nil || instance.ServiceId == nil || *instance.ServiceId != serviceID {
			continue
		}
		if instance.MountPath != postgresMountPath {
			continue
		}
		return &postgresRemoteIDs{
			ServiceID:        serviceID,
			VolumeID:         instance.VolumeId,
			VolumeInstanceID: instance.Id,
		}, nil
	}
	return nil, nil
}

func (r *Postgres) adoptPending(ctx context.Context, state *postgresModel) (bool, error) {
	ids, err := r.findPostgres(
		ctx,
		state.ProjectID.ValueString(),
		state.EnvironmentID.ValueString(),
		state.Name.ValueString(),
	)
	if err != nil || ids == nil {
		return false, err
	}
	state.ID = types.StringValue(ids.ServiceID)
	state.ServiceID = state.ID
	state.VolumeID = types.StringValue(ids.VolumeID)
	state.VolumeInstanceID = types.StringValue(ids.VolumeInstanceID)
	return true, nil
}

func (r *Postgres) waitForPostgres(
	ctx context.Context,
	projectID string,
	environmentID string,
	name string,
	interval time.Duration,
) (*postgresRemoteIDs, error) {
	// The ceiling is the CALLER'S timeout — see `awaitConsistency`. This one
	// was a minute rather than thirty seconds, for no reason recorded anywhere;
	// a Postgres service that takes longer than its configured create timeout
	// to appear is a timeout, not a different kind of wait.
	var found *postgresRemoteIDs
	err := awaitConsistency(ctx, interval, func(ctx context.Context) error {
		ids, err := r.findPostgres(ctx, projectID, environmentID, name)
		if err != nil {
			return err
		}
		if ids == nil {
			return errNotReady
		}
		found = ids
		return nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, nil
		}
		return nil, err
	}
	return found, nil
}
