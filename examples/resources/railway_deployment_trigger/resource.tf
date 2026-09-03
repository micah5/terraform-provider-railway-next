# `railway_service` manages one deployment trigger automatically by default.
# Set `auto_deploy = false` when the standalone resource should own the trigger
# instead — for example to control check-suite gating or manage several
# triggers explicitly.
resource "railway_service" "web" {
  project_id     = railway_environment.uat.project_id
  environment_id = railway_environment.uat.id
  name           = "web"

  source_type = "github"
  repository  = "example/app"
  branch      = "uat"
  auto_deploy = false
}

resource "railway_deployment_trigger" "web" {
  project_id     = railway_service.web.project_id
  environment_id = railway_service.web.environment_id
  service_id     = railway_service.web.id

  # MUST MATCH THE SERVICE'S SOURCE. A trigger for a different repository
  # deploys commits the service was not built from.
  repository = railway_service.web.repository
  branch     = railway_service.web.branch

  # DEFAULTS TO TRUE, which is the safer direction: a red build should not
  # reach an environment. Set false where the repository has no checks at all,
  # since a trigger waiting on check suites that never run never deploys.
  check_suites = true
}

# With `auto_deploy = false`, omitting this resource is also valid for a service
# deployed only by CI or by hand.
