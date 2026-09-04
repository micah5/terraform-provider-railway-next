# Terraform Provider Railway Next

`railway-next` is a native Go Terraform provider for Railway's GraphQL API. It
speaks Terraform Plugin Protocol v6 and makes typed GraphQL requests directly;
the provider runtime contains no Node.js, Railway CLI, browser automation,
provisioners, or shell scripts.

This is an independent community provider and is not affiliated with or
endorsed by Railway Corporation.

> Release status: initial community release. The fixture-backed suite and
> guarded disposable-project service/PostgreSQL lifecycle acceptance test passed
> on September 3, 2026. Review destructive plans carefully and begin with
> disposable Railway projects before adopting the provider for production.

## Requirements

- Terraform 1.11 or later. This is enforced because `railway_secret` uses
  write-only attributes.
- Go 1.26.5 for provider development.

The distribution address is
`registry.terraform.io/micah5/railway-next`. The recommended local provider
name is `railway`, which keeps resource names conventional (`railway_project`,
`railway_bucket`, and so on):

```hcl
terraform {
  required_version = ">= 1.11.0"

  required_providers {
    railway = {
      source = "micah5/railway-next"
    }
  }
}

provider "railway" {
  token            = var.railway_token
  token_type       = "account"
  graphql_endpoint = "https://backboard.railway.com/graphql/v2"
}
```

Account and workspace tokens use `Authorization: Bearer`. Project tokens use
`Project-Access-Token` and therefore require `token_type = "project"`; an
opaque token value cannot be safely inferred. The provider also reads
`RAILWAY_API_TOKEN`, then `RAILWAY_TOKEN`, and
`RAILWAY_GRAPHQL_ENDPOINT`. Tokens and unredacted GraphQL variables are never
logged.

## Implemented surface

Resources:

- `railway_project`
- `railway_environment`
- `railway_service`
- `railway_volume`
- `railway_variable_collection`
- `railway_secret`
- `railway_service_domain`
- `railway_bucket`
- `railway_postgres`

Data sources:

- `railway_project`
- `railway_environment`
- `railway_service`
- `railway_bucket`
- `railway_deployment_status`

The complete API/UI/PostgreSQL/bucket example is in
[`examples/complete`](examples/complete). It uses computed Railway reference
expressions, not credentials, for database and bucket connections.

## Safety model

- Project destroy deletes the complete Railway project.
- Volume and PostgreSQL destroy delete persistent data.
- Bucket region changes force replacement, and Railway can delay permanent
  deletion after the environment registration is removed.
- Variable collections preserve variables outside their owned key set and
  apply all owned upserts/removals in one environment change-set.
- `sensitive = true` is not presented as secret-state protection.
  `railway_secret.value_wo` is write-only and is never returned from Read.
- Non-idempotent mutations are not blindly retried. An ambiguous response is
  followed by a read where the requested result can be reconciled safely.

See [`docs/handbook/safety.md`](docs/handbook/safety.md) and the generated resource
documentation for exact lifecycle and import behavior.

## Development

```text
go generate ./...
go test ./...
go vet ./...
terraform fmt -check -recursive examples
```

Normal tests use GraphQL fixtures and `httptest.Server`; they create no Railway
resources. Schema refresh is an explicit maintainer operation:

```text
go run ./tools/schemafetch
go generate ./graphql
```

Acceptance tests are opt-in and guarded:

```text
TF_ACC=1 \
RAILWAY_API_TOKEN=... \
RAILWAY_ACC_PROJECT_PREFIX=tfacc-local- \
RAILWAY_ACC_GITHUB_REPOSITORY=owner/repository \
RAILWAY_ACC_GITHUB_BRANCH=main \
go test ./internal/provider -run '^TestAcc' -count=1 -timeout 30m -v
```

They must use a disposable account/workspace, are deliberately non-parallel,
and create services, volumes, and PostgreSQL that may be billable. See
[`docs/handbook/testing.md`](docs/handbook/testing.md).

Local provider overrides, imports, releases, and known API boundaries are
documented under [`docs/handbook`](docs/handbook).

## License and attribution

MPL-2.0. Opaque Railway environment behavior was independently reimplemented
in Go after studying Railway's MIT-licensed TypeScript SDK as a behavioral
reference. See [`NOTICE`](NOTICE), ADR
[`0001-direct-graphql`](docs/adr/0001-direct-graphql.md), and ADR
[`0002-opaque-environment-changesets`](docs/adr/0002-opaque-environment-changesets.md).
