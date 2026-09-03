// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestBucketProtocolTimeoutAdoptsWithoutDuplicate proves the full recovery
// path through protocol v6. The create change set is accepted, its configured
// timeout expires before Railway lists the bucket, and Terraform receives a
// provisional state entry. On the next refresh Railway exposes the same bucket;
// the provider adopts its real id and must not submit another create change set.
func TestBucketProtocolTimeoutAdoptsWithoutDuplicate(t *testing.T) {
	var fixture pendingBucketFixture
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()

	config := fmt.Sprintf(`
provider "railway" {
  token            = "fixture-token"
  token_type       = "account"
  graphql_endpoint = %q
}

resource "railway_bucket" "cache" {
  project_id     = "project-fixture"
  environment_id = "environment-fixture"
  name           = "cache"
  region         = "ams"

  timeouts = {
    create = "100ms"
  }
}
`, server.URL)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"railway": providerserver.NewProtocol6WithError(New("test")()),
		},
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_bucket.cache", "id", "terraform-pending:bucket"),
				),
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_bucket.cache", "id", "bucket-fixture"),
					resource.TestCheckResourceAttr("railway_bucket.cache", "name", "cache"),
				),
			},
		},
	})

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	// One registration and one test-cleanup deletion. A duplicate create on the
	// recovery step would make this three.
	if fixture.applies != 2 {
		t.Fatalf("change-set applies = %d, want one create and one cleanup delete", fixture.applies)
	}
}

func TestPostgresProtocolTimeoutAdoptsWithoutDuplicate(t *testing.T) {
	var fixture pendingPostgresFixture
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()

	config := fmt.Sprintf(`
provider "railway" {
  token            = "fixture-token"
  token_type       = "account"
  graphql_endpoint = %q
}

resource "railway_postgres" "main" {
  project_id     = "project-fixture"
  environment_id = "environment-fixture"
  name           = "Postgres"
  version        = "18"
  region         = "ams"

  timeouts = {
    create = "100ms"
  }
}
`, server.URL)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"railway": providerserver.NewProtocol6WithError(New("test")()),
		},
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_postgres.main", "id", "terraform-pending:postgres"),
				),
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_postgres.main", "id", "postgres-service-fixture"),
					resource.TestCheckResourceAttr("railway_postgres.main", "volume_id", "postgres-volume-fixture"),
				),
			},
		},
	})

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.applies != 2 {
		t.Fatalf("change-set applies = %d, want one create and one cleanup delete", fixture.applies)
	}
}

type pendingBucketFixture struct {
	mu      sync.Mutex
	visible bool
	deleted bool
	applies int
	lists   int
}

type pendingPostgresFixture struct {
	mu      sync.Mutex
	visible bool
	applies int
	lists   int
}

func (f *pendingPostgresFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch request.OperationName {
	case "ListProjectServices":
		f.lists++
		if f.applies == 1 && f.lists >= 3 {
			f.visible = true
		}
		edges := []any{}
		if f.visible {
			edges = append(edges, map[string]any{"node": map[string]any{
				"id": "postgres-service-fixture", "name": "Postgres",
				"projectId": "project-fixture", "icon": nil, "deletedAt": nil,
			}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"project": map[string]any{"services": map[string]any{"edges": edges}},
		}})

	case "GetProjectVolumes":
		volumes := []any{}
		instances := []any{}
		if f.visible {
			volumes = append(volumes, map[string]any{"node": map[string]any{
				"id": "postgres-volume-fixture", "name": "Postgres-data", "projectId": "project-fixture",
			}})
			instances = append(instances, map[string]any{"node": map[string]any{
				"id": "postgres-volume-instance-fixture", "volumeId": "postgres-volume-fixture",
				"environmentId": "environment-fixture", "serviceId": "postgres-service-fixture",
				"mountPath": "/var/lib/postgresql/data", "region": "europe-west4-drams3a",
				"sizeMB": 5000, "currentSizeMB": 0, "isPendingDeletion": false,
				"deletedAt": nil, "state": "READY",
			}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"project":     map[string]any{"volumes": map[string]any{"edges": volumes}},
			"environment": map[string]any{"volumeInstances": map[string]any{"edges": instances}},
		}})

	case "GetService":
		if !f.visible {
			_, _ = io.WriteString(w, `{"errors":[{"message":"not found","extensions":{"code":"NOT_FOUND"}}]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"service": map[string]any{
				"id": "postgres-service-fixture", "name": "Postgres", "projectId": "project-fixture",
				"icon": nil, "deletedAt": nil, "repoTriggers": map[string]any{"edges": []any{}},
			},
			"environment": map[string]any{
				"config": map[string]any{"services": map[string]any{}},
				"serviceInstances": map[string]any{"edges": []any{map[string]any{"node": map[string]any{
					"id": "postgres-service-instance-fixture", "environmentId": "environment-fixture",
					"serviceId": "postgres-service-fixture", "serviceName": "Postgres",
					"hasEverDeployed": true, "buildCommand": nil, "builder": "RAILPACK",
					"dockerfilePath": nil, "drainingSeconds": nil, "healthcheckPath": nil,
					"healthcheckTimeout": nil, "ipv6EgressEnabled": nil, "numReplicas": nil,
					"overlapSeconds": nil, "preDeployCommand": nil, "railwayConfigFile": nil,
					"region": "europe-west4-drams3a", "restartPolicyMaxRetries": 0,
					"restartPolicyType": "ALWAYS", "rootDirectory": nil, "sleepApplication": nil,
					"source":       map[string]any{"image": "ghcr.io/railwayapp-templates/postgres-ssl:18", "repo": nil},
					"startCommand": nil, "watchPatterns": []any{}, "latestDeployment": nil,
				}}}},
			},
			"limitOverride": nil,
		}})

	case "GetEnvironmentConfiguration":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"environment": map[string]any{
				"id": "environment-fixture", "projectId": "project-fixture",
				"configEtag": "etag-fixture", "config": map[string]any{},
			},
		}})

	case "PreviewEnvironmentChangeSet":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"environmentPreviewChangeSet": map[string]any{
				"changeSet": request.Variables["input"], "diagnostics": []any{}, "effects": []any{},
			},
		}})

	case "ApplyEnvironmentChangeSet":
		f.applies++
		if f.applies > 1 {
			f.visible = false
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"environmentApplyChangeSet": map[string]any{
				"id": "operation-fixture", "status": "applied", "deploymentId": nil,
				"stagedPatchId": nil, "diagnostics": []any{}, "changes": []any{},
			},
		}})

	case "DeleteVolume":
		_, _ = io.WriteString(w, `{"data":{"volumeDelete":true}}`)

	default:
		http.Error(w, "unexpected operation "+request.OperationName, http.StatusBadRequest)
	}
}

func (f *pendingBucketFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch request.OperationName {
	case "ListProjectBuckets":
		f.lists++
		// Call one is Create's duplicate-name check and call two is its first
		// reconciliation probe. The short create timeout expires before the next
		// poll. Call three is therefore the following Terraform refresh, where
		// Railway finally exposes the accepted change set.
		if f.applies == 1 && f.lists >= 3 {
			f.visible = true
		}
		edges := []any{}
		if f.visible {
			var deletedAt any
			if f.deleted {
				deletedAt = "2026-09-03T00:00:00Z"
			}
			edges = append(edges, map[string]any{"node": map[string]any{
				"id": "bucket-fixture", "name": "cache", "projectId": "project-fixture",
				"groupId": nil, "createdAt": "2026-09-03T00:00:00Z",
				"updatedAt": "2026-09-03T00:00:00Z", "deletedAt": deletedAt,
			}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"project": map[string]any{"buckets": map[string]any{"edges": edges}},
		}})

	case "GetEnvironmentConfiguration":
		buckets := map[string]any{}
		if f.visible {
			buckets["bucket-fixture"] = map[string]any{
				"region": "ams", "isCreated": true, "isDeleted": f.deleted,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"environment": map[string]any{
				"id": "environment-fixture", "projectId": "project-fixture",
				"configEtag": "etag-fixture", "config": map[string]any{"buckets": buckets},
			},
		}})

	case "PreviewEnvironmentChangeSet":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"environmentPreviewChangeSet": map[string]any{
				"changeSet": request.Variables["input"], "diagnostics": []any{}, "effects": []any{},
			},
		}})

	case "ApplyEnvironmentChangeSet":
		f.applies++
		if f.applies > 1 {
			f.deleted = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"environmentApplyChangeSet": map[string]any{
				"id": "operation-fixture", "status": "applied", "deploymentId": nil,
				"stagedPatchId": nil, "diagnostics": []any{}, "changes": []any{},
			},
		}})

	default:
		_, _ = io.WriteString(w, `{"data":{}}`)
	}
}
