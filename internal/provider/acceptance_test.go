// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccParallelServiceAndPostgresLifecycle is intentionally one
// non-parallel test process, while Terraform itself uses its normal parallelism
// to exercise concurrent service, volume, and PostgreSQL environment changes.
// It creates billable resources only when TF_ACC=1 and the explicit
// disposable-project guard passes.
//
// Buckets are excluded because Railway currently accepts and ignores their
// public-API deletion change set. Keeping one here makes post-test destroy fail
// by design; bucket create/timeout/adoption remains covered at protocol level.
func TestAccParallelServiceAndPostgresLifecycle(t *testing.T) {
	prefix := os.Getenv("RAILWAY_ACC_PROJECT_PREFIX")
	name := fmt.Sprintf("%s%d", prefix, time.Now().Unix())
	config := acceptanceParallelConfig(
		name,
		os.Getenv("RAILWAY_ACC_GITHUB_REPOSITORY"),
		os.Getenv("RAILWAY_ACC_GITHUB_BRANCH"),
	)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			acceptancePreCheck(t)
		},
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"railway": providerserver.NewProtocol6WithError(New("acceptance")()),
		},
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("railway_postgres.main", "service_id"),
					resource.TestCheckResourceAttrSet("railway_postgres.main", "volume_id"),
					resource.TestCheckResourceAttr("railway_postgres.main", "version", "18"),
					resource.TestCheckResourceAttrSet("railway_service.api", "id"),
					resource.TestCheckResourceAttr("railway_service.api", "repository", os.Getenv("RAILWAY_ACC_GITHUB_REPOSITORY")),
					resource.TestCheckResourceAttrSet("railway_service.ui", "id"),
					resource.TestCheckResourceAttrSet("railway_volume.api_data", "id"),
					resource.TestCheckResourceAttrSet("railway_volume.api_data", "volume_instance_id"),
					resource.TestCheckResourceAttr("railway_volume.api_data", "mount_path", "/data"),
					checkNoUnknownState("railway_service.api"),
					checkNoUnknownState("railway_service.ui"),
					checkNoUnknownState("railway_volume.api_data"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				ResourceName:      "railway_postgres.main",
				ImportState:       true,
				ImportStateIdFunc: compositeImportID("railway_postgres.main", "project_id", "environment_id", "service_id", "volume_id"),
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"deployment_id",
					"service_instance_id",
					"volume_instance_id",
				},
			},
			{
				ResourceName:      "railway_volume.api_data",
				ImportState:       true,
				ImportStateIdFunc: compositeImportID("railway_volume.api_data", "project_id", "environment_id", "id"),
				ImportStateVerify: true,
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func acceptancePreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("RAILWAY_API_TOKEN") == "" && os.Getenv("RAILWAY_TOKEN") == "" {
		t.Fatal("acceptance tests require RAILWAY_API_TOKEN or RAILWAY_TOKEN")
	}
	prefix := os.Getenv("RAILWAY_ACC_PROJECT_PREFIX")
	if !strings.HasPrefix(prefix, "tfacc-") || len(prefix) < len("tfacc-x") {
		t.Fatal("RAILWAY_ACC_PROJECT_PREFIX must begin with tfacc- and identify disposable projects")
	}
	repository := os.Getenv("RAILWAY_ACC_GITHUB_REPOSITORY")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatal("RAILWAY_ACC_GITHUB_REPOSITORY must be an explicit GitHub owner/repository")
	}
	if strings.TrimSpace(os.Getenv("RAILWAY_ACC_GITHUB_BRANCH")) == "" {
		t.Fatal("RAILWAY_ACC_GITHUB_BRANCH must name the branch used by the disposable services")
	}
}

func compositeImportID(resourceName string, attributes ...string) resource.ImportStateIdFunc {
	return func(state *terraform.State) (string, error) {
		module := state.RootModule()
		if module == nil {
			return "", errors.New("missing Terraform root module")
		}
		instance, ok := module.Resources[resourceName]
		if !ok || instance.Primary == nil {
			return "", fmt.Errorf("missing state for %s", resourceName)
		}
		parts := make([]string, 0, len(attributes))
		for _, attribute := range attributes {
			value := instance.Primary.Attributes[attribute]
			if value == "" {
				return "", fmt.Errorf("%s.%s is empty", resourceName, attribute)
			}
			parts = append(parts, value)
		}
		return strings.Join(parts, "/"), nil
	}
}

func acceptanceParallelConfig(projectName, repository, branch string) string {
	return fmt.Sprintf(`
provider "railway" {
  token_type = "account"
}

resource "railway_project" "test" {
  name                         = %q
  description                  = "Disposable terraform-provider-railway-next acceptance project"
  is_public                    = false
  default_environment_name     = "production"
  pr_deploys                   = false
  bot_pr_environments          = false
  focused_pr_environments      = false
}

resource "railway_service" "api" {
  project_id     = railway_project.test.id
  environment_id = railway_project.test.default_environment_id
  name           = "tfacc-api"
  source_type    = "github"
  repository     = %q
  branch         = %q
  regions        = { ams = 1 }
}

resource "railway_service" "ui" {
  project_id     = railway_project.test.id
  environment_id = railway_project.test.default_environment_id
  name           = "tfacc-ui"
  source_type    = "github"
  repository     = %q
  branch         = %q
  regions        = { ams = 1 }
}

resource "railway_volume" "api_data" {
  project_id     = railway_project.test.id
  environment_id = railway_project.test.default_environment_id
  service_id     = railway_service.api.id
  name           = "tfacc-api-data"
  mount_path     = "/data"
}

resource "railway_postgres" "main" {
  project_id     = railway_project.test.id
  environment_id = railway_project.test.default_environment_id
  name           = "Postgres"
  version        = "18"
  region         = "ams"
}
`, projectName, repository, branch, repository, branch)
}
