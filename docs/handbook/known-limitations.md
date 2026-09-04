# Known Railway API limitations and assumptions

- Live acceptance tests are opt-in. The disposable service/PostgreSQL lifecycle
  suite last passed on September 3, 2026. Buckets remain protocol-tested rather
  than part of live teardown because of the deletion limitation below.
- Environment configuration/change sets are opaque JSON pinned to current
  introspection and Railway TypeScript SDK v3.6.0 fixtures.
- **Buckets cannot currently be deleted through the public API at all.** The
  schema exposes `bucketCreate` and `bucketUpdate` but no `bucketDelete`, and
  the documented fallback — a `resource.delete` change set — is ACCEPTED AND
  THEN IGNORED. Verified against live Railway on 2026-08-27: the change set
  previews with no diagnostics and the correct `resource.delete` effect,
  `environmentApplyChangeSet` returns an id, and afterwards the bucket is still
  in `environment.config.buckets` with `deletedAt` null. This holds whether the
  change is addressed by bucket name or by bucket id, and whether or not
  `waitForCompletion: true` is passed.

  The provider therefore reports a deletion it cannot confirm rather than
  claiming success: `Delete` waits for the deregistration it requested and
  raises `Unable to confirm Railway bucket deletion` when the bucket is still
  registered at the end of the practitioner's delete timeout. Terraform will
  keep the resource in state, which is the honest outcome — the bucket exists.

  **Consequence for `region`.** `region` forces replacement, and the replacement
  cannot complete: the destroy does not remove the bucket, so the create then
  fails with `Railway bucket name already exists`. Until Railway ships a working
  delete, treat a bucket's region as immovable — choose it at creation, and
  reach for a differently-named bucket rather than a region change.
- Secret drift proves name presence, not plaintext equality.
- Service creation is distinct from deployment success; consult
  `railway_deployment_status`.
- PostgreSQL import assumes
  `ghcr.io/railwayapp-templates/postgres-ssl:<version>` and one
  `/var/lib/postgresql/data` volume.
- Project-token capabilities remain bounded by Railway permissions.
