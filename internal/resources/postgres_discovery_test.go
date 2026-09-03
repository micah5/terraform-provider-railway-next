// SPDX-License-Identifier: MPL-2.0

package resources

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestPostgresDiscoveryUsesRealizedServiceAndVolumeIDs(t *testing.T) {
	t.Parallel()

	doer := fixtureDoer(func(request graphqlFixtureRequest) (map[string]any, error) {
		switch request.OperationName {
		case "ListProjectServices":
			return map[string]any{"data": map[string]any{
				"project": map[string]any{"services": map[string]any{"edges": []any{
					map[string]any{"node": map[string]any{
						"id": "service-id", "name": "Postgres", "projectId": "project",
						"icon": nil, "deletedAt": nil,
					}},
				}}},
			}}, nil
		case "GetProjectVolumes":
			return map[string]any{"data": map[string]any{
				"project": map[string]any{"volumes": map[string]any{"edges": []any{
					map[string]any{"node": map[string]any{
						"id": "volume-id", "name": "data", "projectId": "project",
					}},
				}}},
				"environment": map[string]any{"volumeInstances": map[string]any{"edges": []any{
					map[string]any{"node": map[string]any{
						"id": "volume-instance-id", "volumeId": "volume-id",
						"environmentId": "environment", "serviceId": "service-id",
						"mountPath": postgresMountPath, "region": "ams", "sizeMB": 1024,
						"currentSizeMB": 0, "isPendingDeletion": false,
						"deletedAt": nil, "state": "READY",
					}},
				}}},
			}}, nil
		default:
			return nil, errors.New("unexpected operation " + request.OperationName)
		}
	})
	resource := &Postgres{client: testClient(t, "https://fixture.invalid/graphql", doer)}

	ids, err := resource.findPostgres(context.Background(), "project", "environment", "Postgres")
	if err != nil {
		t.Fatal(err)
	}
	if ids == nil ||
		ids.ServiceID != "service-id" ||
		ids.VolumeID != "volume-id" ||
		ids.VolumeInstanceID != "volume-instance-id" {
		t.Fatalf("discovered IDs = %#v", ids)
	}
}

func TestPendingPostgresAdoptsRemoteIdentities(t *testing.T) {
	t.Parallel()

	doer := fixtureDoer(func(request graphqlFixtureRequest) (map[string]any, error) {
		switch request.OperationName {
		case "ListProjectServices":
			return map[string]any{"data": map[string]any{
				"project": map[string]any{"services": map[string]any{"edges": []any{
					map[string]any{"node": map[string]any{
						"id": "service-id", "name": "Postgres", "projectId": "project",
						"icon": nil, "deletedAt": nil,
					}},
				}}},
			}}, nil
		case "GetProjectVolumes":
			return map[string]any{"data": map[string]any{
				"project": map[string]any{"volumes": map[string]any{"edges": []any{
					map[string]any{"node": map[string]any{
						"id": "volume-id", "name": "data", "projectId": "project",
					}},
				}}},
				"environment": map[string]any{"volumeInstances": map[string]any{"edges": []any{
					map[string]any{"node": map[string]any{
						"id": "volume-instance-id", "volumeId": "volume-id",
						"environmentId": "environment", "serviceId": "service-id",
						"mountPath": postgresMountPath, "region": "ams", "sizeMB": 1024,
						"currentSizeMB": 0, "isPendingDeletion": false,
						"deletedAt": nil, "state": "READY",
					}},
				}}},
			}}, nil
		default:
			return nil, errors.New("unexpected operation " + request.OperationName)
		}
	})
	resource := &Postgres{client: testClient(t, "https://fixture.invalid/graphql", doer)}
	state := postgresModel{
		ID: pendingResourceID("postgres"), ProjectID: types.StringValue("project"),
		EnvironmentID: types.StringValue("environment"), Name: types.StringValue("Postgres"),
	}

	ready, err := resource.adoptPending(context.Background(), &state)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || state.ID.ValueString() != "service-id" || state.VolumeID.ValueString() != "volume-id" {
		t.Fatalf("ready = %t, state = %#v", ready, state)
	}
}
