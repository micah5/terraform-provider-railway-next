resource "railway_service" "api" {
  project_id     = railway_project.example.id
  environment_id = railway_project.example.default_environment_id
  name           = "api"
  source_type    = "github"
  repository     = "acme/example"
  branch         = "main"
  # Defaults to true: pushes to the branch above build and deploy without a
  # separate railway_deployment_trigger resource.
  auto_deploy      = true
  root_directory   = "api"
  healthcheck_path = "/healthz"
  start_command    = "./api"
  regions          = { ams = 1 }
}
