# Testing

`go test ./...` is credential-free and covers framework schemas, references,
generated GraphQL requests, response normalization, deterministic change sets,
redaction, HTTP behavior, Terraform plan/state/import, bucket registration,
one-commit variable updates, ambiguous mutation reconciliation, and remote
deletion.

Live tests require `TF_ACC=1`, an explicit Railway token,
`RAILWAY_ACC_PROJECT_PREFIX` beginning with `tfacc-`, and an explicit public
GitHub repository:

```text
TF_ACC=1 RAILWAY_API_TOKEN=... \
RAILWAY_ACC_PROJECT_PREFIX=tfacc-local- \
RAILWAY_ACC_GITHUB_REPOSITORY=owner/repository \
RAILWAY_ACC_GITHUB_BRANCH=main \
go test ./internal/provider -run '^TestAcc' -count=1 -timeout 30m -v
```

`TestAccParallelServiceAndPostgresLifecycle` creates a disposable project, two
GitHub-backed services, PostgreSQL 18, and volumes using Terraform's normal
parallelism. It checks that service state has no unknown values, tests an empty
second plan and composite imports, and then destroys everything. Buckets are
covered by protocol tests rather than this live lifecycle because Railway's
public API currently accepts but ignores bucket deletion; see
`known-limitations.md`. The Go acceptance test is not marked `t.Parallel` to
avoid multiple projects competing for Railway rate limits. GitHub acceptance
is manual, protected by an environment, and requires a billable-resource
confirmation input.

The complete five-step parallel lifecycle last passed locally on September 3,
2026 in 31.24 seconds. A post-run account query confirmed that no
`tfacc-pr1-v020*` projects remained.
