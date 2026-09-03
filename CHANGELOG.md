# Changelog

All notable changes follow Keep a Changelog and Semantic Versioning.

## [0.2.0] - 2026-09-03

### Fixed

- **Every resource lost its `id` during an update, so the provider sent an empty
  string to Railway.** `id` was `Computed` with no plan modifier on all nine
  resources, which makes it UNKNOWN in the plan of any update — so
  `plan.ID.ValueString()` was `""` and the provider asked Railway to modify an
  object with no id. Railway answers that with `Not Authorized`, which names
  neither the id nor the resource and sends every reader to look at their token;
  the same mutation issued by hand succeeded every time. Fixed with
  `UseStateForUnknown`, which is correct precisely because a Railway id is
  immutable for the life of the resource.
- **`environmentApplyChangeSet` was never told to wait.** Railway returns as
  soon as a change set is accepted unless `waitForCompletion` is passed, and
  every change-set resource was treating acceptance as completion. The argument
  is now always sent, bounded by the caller's own `timeouts` block.
- **Ephemeral resources received no provider client.** `EphemeralResourceData`
  was not set during `Configure`, so the client was nil and the provider crashed
  with a SIGSEGV rather than raising a diagnostic. Both are fixed: the field is
  assigned, and a nil client is reported as an error.
- **A service with no private-network endpoint crashed the provider.** Railway
  reports "not attached yet" as `privateNetworkEndpoint: null` with no error,
  so dereferencing the result took the whole provider process down with a
  SIGSEGV during a plain `terraform import` — the triggering operation and
  every other resource in the same graph walk failing together, with a Go stack
  trace instead of a diagnostic.
- **`BigInt` was bound to `string` while Railway sends a JSON number**, so every
  read of a `BigInt` field failed with `cannot unmarshal number into Go struct
  field ... of type string`. It is now `json.Number`, which accepts either form
  and keeps the digits exactly as they arrived.
- Persist Terraform state for every resource that is created remotely but not
  fully configured, generalising the `railway_volume` fix in 0.1.3 to
  `railway_service`, `railway_bucket`, `railway_postgres` and
  `railway_service_domain`. A source-connect, rename, reference-read or
  reconciliation failure after creation no longer discards the object from
  state, so a retry updates what it already made instead of failing with
  "already exists" until the object is deleted by hand.
- `railway_project` persisted partial state without resolving unknown
  attributes, so the one path that recovers an orphaned project would have
  failed the apply with `Provider returned invalid result object after apply`.
- A deleted bucket is no longer counted as an existing one. Railway keeps a
  deleted bucket in the project listing through its delayed permanent-deletion
  window, so the duplicate-name guard fired on the provider's own tombstone and
  refused the create half of a replacement.
- `railway_bucket` deletion is now verified rather than assumed. The change set
  reported success while leaving the bucket registered, so Terraform recorded a
  destroy that had not happened.
- TRACE request logging no longer includes GraphQL variable values. Variables
  can contain write-only secrets, so all log levels now expose only operation
  names, top-level variable keys, and the names of empty string variables.
- Changing `railway_service.branch` now updates the deployment trigger before
  state is refreshed, avoiding `Provider produced inconsistent result after
  apply` when moving a service from one watched branch to another.
- Bucket and PostgreSQL creates that time out after Railway accepts their
  change set now save a non-tainted provisional identity. A later refresh
  adopts the real bucket, service, and volume IDs by scoped name instead of
  submitting a duplicate create or leaving an orphan in Railway.

### Added

- **`railway_deployment_trigger` for explicit trigger management.** A GitHub
  `railway_service` continues to manage one trigger by default, preserving the
  original push-to-deploy workflow. Set `auto_deploy = false` on the service
  when the standalone resource should be the sole owner, such as for explicit
  check-suite gating, multiple triggers, or a source deployed only by CI.
- `ephemeral.railway_bucket_credentials` and
  `data.railway_bucket_credentials` — the same S3 credential lookup in two
  forms. The ephemeral resource is the default and never persists. The data
  source exists because an ephemeral value cannot be used where Terraform must
  persist it (`doppler_secret.value`, for one), and a provider offering only the
  ephemeral form would be deciding that a whole class of configuration is not
  allowed to exist. The data source marks the key `Sensitive` and warns on every
  read that it lands in state.
- **`private_dns_name` and `private_ips` on `railway_service`, as a resource
  attribute and on the data source.** A service's address on Railway's private
  network was unreachable through the provider, and it is the question anything
  reaching INTO Railway has to answer — a Tailscale subnet router deciding what
  to advertise, an ACL naming a destination, an operator working out why one
  service cannot see another. On the resource as well as the data source,
  because otherwise wiring a service you just declared means looking it up
  again. Empty until the service has a running deployment, which is a real
  state rather than an error.
- **`has_ever_deployed` on `railway_service`.** Railway's own description says
  it *"distinguishes a service that was torn down"* from one that never
  deployed — `latestDeployment` is null for both. That is exactly the state a
  missing deployment trigger produces, and it was previously indistinguishable
  from a teardown.
- `object_count` and `size_bytes` on `data.railway_bucket`, from Railway's
  `bucketInstanceDetails`. Whether destroying a bucket is safe was previously
  unanswerable through the provider.
- Structured request logging through `tflog`, so `TF_LOG` shows which GraphQL
  operation ran and — the line that would have made the empty-id bug a
  five-minute diagnosis — which of its variables arrived EMPTY. Variable values
  are never logged; keys and operation names are available at `DEBUG` and
  `TRACE`.

### Changed

- Every eventual-consistency wait now honours the practitioner's `timeouts`
  block. `service`, `volume`, `bucket` and `postgres` each built their own
  deadline — thirty seconds, or a minute in `postgres`, with no recorded reason
  for the difference — so `timeouts { create = "5m" }` still got thirty seconds
  and an error blaming Railway for being slow. A test now rejects any resource
  that builds its own deadline.
- The vendored GraphQL schema is refreshed from live introspection: 40 types
  added, including `Bucket.deletedAt` and `Bucket.parentServiceId`. The two
  removed types are deprecated deployment-session connections no operation used.
- Documentation generation works again. `tfplugindocs` assumes a provider lives
  under `hashicorp/`, so it asked the registry for `hashicorp/railway` and
  failed with a message about platform availability. `tools/generate.sh` now
  exports the schema from the locally built binary and renders from that.

### Known limitations

- **Railway buckets cannot be deleted through the public API.** There is no
  `bucketDelete` mutation, and the change-set fallback is accepted and then
  ignored — verified against live Railway on 2026-08-27, addressed by name and
  by id, with and without `waitForCompletion`. A `region` change therefore plans
  a replacement that cannot complete. See `docs/handbook/known-limitations.md`.

## [0.1.3] - 2026-07-27

- Wait for Railway's eventually consistent volume instance after
  `railway_volume` creation instead of returning without Terraform state when
  the volume exists but its environment attachment is not visible yet.
- Add protocol-v6 coverage for delayed volume-instance visibility and include
  an independently managed volume in the guarded parallel acceptance lifecycle.

## [0.1.2] - 2026-07-27

- Resolve every omitted `railway_service` Optional+Computed attribute to a
  known value after Create, including typed nulls for absent source, command,
  resource-limit, and collection values.
- Avoid sending an empty service-instance limits mutation when `memory_gb` and
  `vcpus` are unknown in the plan.
- Create sourced services as empty services followed by Railway's documented
  source-connect mutation, while preserving configured source values during
  Railway's post-create consistency window.
- Retry only `STALE_ENVIRONMENT_BASE` change-set rejections with a fresh
  environment ETag, rebuilt preview, and bounded backoff.
- Serialize direct service, volume, domain, secret, bucket-name, and PostgreSQL
  environment mutations with change-set commits in the provider process.
- Add protocol-v6 coverage for a GitHub service whose source is initially
  absent, stale-ETag retry coverage, and a guarded parallel Railway acceptance
  lifecycle for two services, PostgreSQL, and a bucket.

## [0.1.1] - 2026-07-27

- Clarify that this is an independent community provider with no Railway
  Corporation affiliation or endorsement.

## [0.1.0] - 2026-07-27

- Initial native Go provider implementation.
- Validate the disposable bucket/PostgreSQL create, empty-plan, import, and
  destroy lifecycle against Railway.
- Preview and normalize opaque environment change sets before apply.
- Track the bucket, PostgreSQL service, and volume IDs realized by Railway
  change sets.
- Serialize environment change-set commits and normalize Railway region IDs.
