// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestServiceProtocolPartialCreateKeepsStateAndRecovers exercises the failure
// this fork exists to fix, through the real protocol rather than by inspecting
// the source.
//
// **The bug.** `Create` makes the service, a later step fails, and the old code
// returned without `resp.State.Set`. Terraform discarded the plan and recorded
// nothing. The service existed in Railway and not in state, so the next apply
// failed with `a service named "api" already exists`, and the only escape was
// deleting it by hand.
//
// **The step that fails here has changed, and that is the better fix.** The
// original failure was `serviceConnect` — a second mutation that takes only a
// service id and resolves the instance itself, wrongly, in a multi-environment
// project. The source now attaches during `serviceCreate`, as Railway's own API
// cookbook shows, so that window is gone entirely rather than made recoverable.
//
// What can still fail after the service exists is configuring its instance, so
// that is what this drives.
//
// The framework catches the version of this where the provider returns NO
// error: `fwserver` raises "Missing Resource State After Create", whose own
// detail says *"The resource may have been successfully created, but Terraform
// is not tracking it."* It cannot catch this one, because the provider does
// return an error — which is exactly why it needs a test.
//
// **Two steps, and the second is the point.** Step one applies with the connect
// call failing and expects the error. Step two lets it succeed and expects the
// apply to CONVERGE: only possible if step one left the service in state, since
// otherwise Terraform would try to create it again and Railway would refuse the
// duplicate name.
//
// That is stronger than asserting on state after a failed apply — it tests the
// property the fix exists for, which is recoverability.
func TestServiceProtocolPartialCreateKeepsStateAndRecovers(t *testing.T) {
	fixture := &partialCreateFixture{failConnect: true}
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
}
`, server.URL)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"railway": providerserver.NewProtocol6WithError(New("test")()),
		},
		Steps: []resource.TestStep{
			{
				// The service is created; configuring its instance fails.
				Config:      config,
				ExpectError: regexp.MustCompile("instance limits rejected|could not be applied|Unable to"),
			},
			{
				// THE RECOVERY. With the instance update now succeeding, this
				// apply must converge — which requires step one to have left
				// the service in state. If it did not, the provider would call
				// CreateService again and the fixture would reject the
				// duplicate name, exactly as Railway does.
				PreConfig: func() { fixture.allowConnect() },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("railway_service.api", "id", "service-fixture"),
					resource.TestCheckResourceAttr("railway_service.api", "repository", "owner/repository"),
				),
			},
		},
	})
}

// partialCreateFixture is a Railway that creates a service successfully and then
// fails to configure it — and, like Railway, refuses to create a second service
// with a name it already has.
type partialCreateFixture struct {
	mu          sync.Mutex
	failConnect bool
	exists      bool
	connected   bool
	trigger     bool
	reads       int
}

func (f *partialCreateFixture) allowConnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failConnect = false
}

func (f *partialCreateFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
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
	case "CreateService":
		// The source arrives with the create now, so a successful create means
		// a connected service.
		// **THE DUPLICATE-NAME REFUSAL IS THE ASSERTION.** Railway rejects a
		// second service with the same name, and reproducing that here is what
		// makes step two a real test: without the fix, the retry reaches this
		// branch and fails, which is precisely the loop operators hit.
		if f.exists {
			_, _ = io.WriteString(w, `{"errors":[{"message":"A service named \"api\" already exists in this project","extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}`)
			return
		}
		f.exists = true
		f.connected = true
		writeServiceMutation(w, "serviceCreate")

	case "CreateService2":
		// unreachable; kept so the switch shape matches the real fixture

	case "UpdateServiceInstance":
		// **THE REMAINING PARTIAL-FAILURE WINDOW.** The source now attaches
		// during `serviceCreate`, so the old connect failure cannot happen —
		// which is the better fix. What can still fail after the service
		// exists is configuring its instance, and that is what this exercises.
		if f.failConnect {
			_, _ = io.WriteString(w, `{"errors":[{"message":"instance limits rejected","extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"serviceInstanceUpdate":true}}`)

	case "CreateDeploymentTrigger":
		f.trigger = true
		writeServiceDeploymentTriggerMutation(w, "deploymentTriggerCreate", "owner/repository", "master")

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
		f.reads++
		repoTriggers := []any{}
		var source any
		if f.trigger {
			repoTriggers = []any{map[string]any{
				"node": map[string]any{
					"id": "trigger-fixture", "environmentId": "environment-fixture",
					"branch": "master", "repository": "owner/repository",
					"provider": "github", "projectId": "project-fixture",
					"serviceId": "service-fixture", "checkSuites": false,
				},
			}}
		}
		if f.connected {
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
					"service-fixture": map[string]any{"deploy": map[string]any{}},
				}},
				"serviceInstances": map[string]any{"edges": []any{map[string]any{
					"node": map[string]any{
						"id": "instance-fixture", "environmentId": "environment-fixture",
						"serviceId": "service-fixture", "serviceName": "api",
						"buildCommand": nil, "builder": "RAILPACK", "dockerfilePath": nil,
						"drainingSeconds": nil, "healthcheckPath": nil, "healthcheckTimeout": nil,
						"numReplicas": nil, "overlapSeconds": nil, "preDeployCommand": nil,
						"railwayConfigFile": nil, "region": nil, "restartPolicyType": nil,
						"restartPolicyMaxRetries": nil, "rootDirectory": nil,
						"sleepApplication": nil, "startCommand": nil, "source": source,
						"cronSchedule": nil, "ipv6EgressEnabled": nil,
					},
				}}},
			},
		}})

	case "DeleteService":
		f.exists = false
		f.connected = false
		f.trigger = false
		_, _ = io.WriteString(w, `{"data":{"serviceDelete":true}}`)

	default:
		_, _ = io.WriteString(w, `{"data":{}}`)
	}
}
