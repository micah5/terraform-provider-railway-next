// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestServiceProtocolCreateNormalizesOptionalComputedValues exercises Create
// through Terraform Plugin Protocol v6. In particular, it omits every
// Optional+Computed service setting whose Railway response may be null and
// verifies that no unknown planned value survives into post-apply state.
func TestServiceProtocolCreateNormalizesOptionalComputedValues(t *testing.T) {
	fixture := serviceFixture{sourceVisibleAfter: 4}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()

	config := fmt.Sprintf(`
provider "railway" {
  token            = "fixture-token"
  token_type       = "account"
  graphql_endpoint = %q
}

resource "railway_service" "api" {
  project_id     = "project-fixture"
  environment_id = "environment-fixture"
  name           = "api"
  source_type    = "github"
  repository     = "owner/repository"
  branch         = "master"
  config_path    = "railway.json"
  regions        = { ams = 1 }
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
					resource.TestCheckResourceAttr("railway_service.api", "id", "service-fixture"),
					resource.TestCheckResourceAttr("railway_service.api", "repository", "owner/repository"),
					resource.TestCheckResourceAttr("railway_service.api", "branch", "master"),
					resource.TestCheckResourceAttr("railway_service.api", "config_path", "railway.json"),
					resource.TestCheckResourceAttr("railway_service.api", "regions.ams", "1"),
					resource.TestCheckNoResourceAttr("railway_service.api", "image"),
					resource.TestCheckNoResourceAttr("railway_service.api", "memory_gb"),
					resource.TestCheckNoResourceAttr("railway_service.api", "vcpus"),
					resource.TestCheckNoResourceAttr("railway_service.api", "pre_deploy_command"),
					checkNoUnknownState("railway_service.api"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.getServiceCalls < fixture.sourceVisibleAfter {
		t.Fatalf("provider returned before the Railway source converged: got %d reads, want at least %d", fixture.getServiceCalls, fixture.sourceVisibleAfter)
	}
}

// TestServiceProtocolBranchUpdateUpdatesDeploymentTrigger guards the v0.2
// ownership boundary: railway_service.branch is the branch watched by the
// service-managed trigger unless auto_deploy is disabled. Updating it must
// change Railway before the provider refreshes state, otherwise Terraform sees
// the old remote branch and reports an inconsistent result after apply.
func TestServiceProtocolBranchUpdateUpdatesDeploymentTrigger(t *testing.T) {
	var fixture serviceFixture
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()

	config := func(branch string) string {
		return fmt.Sprintf(`
provider "railway" {
  token            = "fixture-token"
  token_type       = "account"
  graphql_endpoint = %q
}

resource "railway_service" "api" {
  project_id     = "project-fixture"
  environment_id = "environment-fixture"
  name           = "api"
  source_type    = "github"
  repository     = "owner/repository"
  branch         = %q
}
`, server.URL, branch)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"railway": providerserver.NewProtocol6WithError(New("test")()),
		},
		Steps: []resource.TestStep{
			{
				Config: config("master"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_service.api", "branch", "master"),
					resource.TestCheckResourceAttr("railway_service.api", "deployment_trigger_id", "trigger-fixture"),
				),
			},
			{
				Config: config("develop"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_service.api", "branch", "develop"),
					resource.TestCheckResourceAttr("railway_service.api", "deployment_trigger_id", "trigger-fixture"),
				),
			},
		},
	})

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.triggerUpdates == 0 {
		t.Fatal("branch change never called deploymentTriggerUpdate")
	}
}

func TestServiceProtocolUsesRepositoryDefaultBranchWhenOmitted(t *testing.T) {
	var fixture serviceFixture
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()

	config := fmt.Sprintf(`
provider "railway" {
  token            = "fixture-token"
  token_type       = "account"
  graphql_endpoint = %q
}

resource "railway_service" "api" {
  project_id     = "project-fixture"
  environment_id = "environment-fixture"
  name           = "api"
  source_type    = "github"
  repository     = "owner/repository"
}
`, server.URL)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"railway": providerserver.NewProtocol6WithError(New("test")()),
		},
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("railway_service.api", "branch", "main"),
			),
		}},
	})
}

func checkNoUnknownState(name string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		instance, ok := state.RootModule().Resources[name]
		if !ok || instance.Primary == nil {
			return fmt.Errorf("missing state for %s", name)
		}
		for attribute, value := range instance.Primary.Attributes {
			if strings.Contains(strings.ToLower(value), "unknown") {
				return fmt.Errorf("%s.%s remained unknown after apply", name, attribute)
			}
		}
		return nil
	}
}

type serviceFixture struct {
	mu                 sync.Mutex
	exists             bool
	connected          bool
	triggerExists      bool
	repository         string
	branch             string
	triggerUpdates     int
	getServiceCalls    int
	sourceVisibleAfter int
}

func (f *serviceFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
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
	case "GetGitHubRepository":
		_, _ = io.WriteString(w, `{"data":{"githubRepo":{"defaultBranch":"main"}}}`)
	case "CreateService":
		// Railway can accept source in ServiceCreateInput while the resulting
		// instance still reports source=null. Model that live behavior so the
		// environment-aware serviceInstanceUpdate fallback is required.
		f.exists = true
		if variables, ok := request.Variables["input"].(map[string]any); ok {
			f.branch, _ = variables["branch"].(string)
			if source, ok := variables["source"].(map[string]any); ok {
				f.repository, _ = source["repo"].(string)
			}
		}
		writeServiceMutation(w, "serviceCreate")
	case "ConnectService":
		f.connected = true
		writeServiceMutation(w, "serviceConnect")
	case "UpdateServiceInstance":
		if input, ok := request.Variables["input"].(map[string]any); ok {
			if source, ok := input["source"].(map[string]any); ok {
				if repository, ok := source["repo"].(string); ok && repository != "" {
					f.connected = true
					f.repository = repository
				}
				if image, ok := source["image"].(string); ok && image != "" {
					f.connected = true
				}
			} else {
				f.connected = false
			}
		}
		_, _ = io.WriteString(w, `{"data":{"serviceInstanceUpdate":true}}`)
	case "CreateDeploymentTrigger":
		input, _ := request.Variables["input"].(map[string]any)
		f.triggerExists = true
		f.branch, _ = input["branch"].(string)
		f.repository, _ = input["repository"].(string)
		writeServiceDeploymentTriggerMutation(w, "deploymentTriggerCreate", f.repository, f.branch)
	case "UpdateDeploymentTrigger":
		input, _ := request.Variables["input"].(map[string]any)
		if branch, ok := input["branch"].(string); ok {
			f.branch = branch
		}
		if repository, ok := input["repository"].(string); ok {
			f.repository = repository
		}
		f.triggerUpdates++
		writeServiceDeploymentTriggerMutation(w, "deploymentTriggerUpdate", f.repository, f.branch)
	case "DeleteDeploymentTrigger":
		f.triggerExists = false
		_, _ = io.WriteString(w, `{"data":{"deploymentTriggerDelete":true}}`)
	case "GetEnvironmentPrivateNetworks":
		// **THE FIXTURE REPORTS NO PRIVATE NETWORK**, which is a real state:
		// private networking can be disabled. `privatenet.Read` treats
		// anything other than exactly one network as "no address to report"
		// rather than an error, so this exercises that path.
		_, _ = io.WriteString(w, `{"data":{"privateNetworks":[]}}`)

	case "GetService":
		if !f.exists {
			_, _ = io.WriteString(w, `{"errors":[{"message":"not found","extensions":{"code":"NOT_FOUND"}}]}`)
			return
		}
		f.getServiceCalls++
		repoTriggers := []any{}
		var source any
		if f.triggerExists {
			repoTriggers = []any{map[string]any{
				"node": map[string]any{
					"id": "trigger-fixture", "environmentId": "environment-fixture",
					"branch": f.branch, "repository": f.repository,
					"provider": "github", "projectId": "project-fixture",
					"serviceId": "service-fixture", "checkSuites": false,
				},
			}}
		}
		if f.connected && f.getServiceCalls >= f.sourceVisibleAfter {
			source = map[string]any{"image": nil, "repo": "owner/repository"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"service": map[string]any{
				"id": "service-fixture", "name": "api", "projectId": "project-fixture",
				"icon": nil, "deletedAt": nil,
				"repoTriggers": map[string]any{"edges": repoTriggers},
			},
			"environment": map[string]any{
				"config": map[string]any{"services": map[string]any{
					"service-fixture": map[string]any{"deploy": map[string]any{
						"multiRegionConfig": map[string]any{
							"ams": map[string]any{"numReplicas": 1},
						},
					}},
				}},
				"serviceInstances": map[string]any{"edges": []any{map[string]any{
					"node": map[string]any{
						"id": "instance-fixture", "environmentId": "environment-fixture",
						"serviceId": "service-fixture", "serviceName": "api",
						"buildCommand": nil, "builder": "RAILPACK", "dockerfilePath": nil,
						"drainingSeconds": nil, "healthcheckPath": nil, "healthcheckTimeout": nil,
						"ipv6EgressEnabled": nil, "numReplicas": nil, "overlapSeconds": nil,
						"preDeployCommand": nil, "railwayConfigFile": "railway.json", "region": nil,
						"restartPolicyMaxRetries": 0, "restartPolicyType": "ON_FAILURE",
						"rootDirectory": nil, "sleepApplication": nil,
						"source":       source,
						"startCommand": nil, "watchPatterns": []any{}, "latestDeployment": nil,
					},
				}}},
			},
			"limitOverride": nil,
		}})
	case "DeleteService":
		f.exists = false
		f.triggerExists = false
		_, _ = io.WriteString(w, `{"data":{"serviceDelete":true}}`)
	default:
		http.Error(w, "unexpected operation "+request.OperationName, http.StatusBadRequest)
	}
}

func writeServiceDeploymentTriggerMutation(w io.Writer, field, repository, branch string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
		field: map[string]any{
			"id": "trigger-fixture", "branch": branch, "repository": repository,
			"provider": "github", "projectId": "project-fixture",
			"environmentId": "environment-fixture", "serviceId": "service-fixture",
			"checkSuites": false,
		},
	}})
}

func writeServiceMutation(w io.Writer, field string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
		field: map[string]any{
			"id": "service-fixture", "name": "api", "projectId": "project-fixture",
			"icon": nil, "deletedAt": nil,
		},
	}})
}
