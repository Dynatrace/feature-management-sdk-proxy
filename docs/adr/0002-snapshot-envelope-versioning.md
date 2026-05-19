# ADR-0002: Snapshot Envelope Versioning Strategy

**Status:** Accepted
**Date:** 2026-05-03
**Deciders:** Michael Beemer

---

## Context and Problem Statement

The snapshot persistence feature serializes cache entries (flagd config payload + revalidation metadata) to an object
storage backend so that a proxy restart during an upstream outage does not reset the cache to empty. The persisted
format needs a versioning scheme to support future evolution while maintaining safety properties across heterogeneous
fleet deployments.

---

## Decision Drivers

- **Forward-compatibility safety:** an older proxy reading a snapshot written by a newer proxy must never silently
  corrupt state or crash on startup.
- **Simplicity:** versioning machinery that requires a compatibility matrix adds disproportionate complexity to what is
  a startup-hint mechanism.
- **Local-file parity:** the same versioning scheme must work for the local-file backend (no HTTP HEAD, no object
  metadata queries).
- **Rolling-deploy safety:** during a rolling deployment, pods running different binary versions will share the same
  object storage bucket.

---

## Considered Options

### Option A: Integer version field in the JSON envelope

A `snapshot_version` integer field is embedded in the envelope JSON alongside the payload, ETag, and timestamp. The
initial value is `1`. Readers that encounter a version number higher than they understand skip the object, log a
structured warning, and increment a metric; they never fail startup.

**Forward-compat rule:** unknown fields in a known version are ignored (standard `encoding/json` behaviour). An unknown
version number is treated as unreadable.

### Option B: Semantic version string (e.g. `"1.0.0"`)

A `snapshot_version` string field follows semver conventions. Readers parse major/minor/patch and apply a compatibility
matrix (same major → compatible; higher major → skip).

**Rejected:** semver introduces a compatibility matrix with no practical benefit. The proxy has one writer and one
reader (itself). Minor and patch increments carry no semantics here. Integer comparison is unambiguous and requires no
parsing.

### Option C: No version field; rely on JSON forward-compatibility

Omit a version field. New envelope fields are added with `omitempty`; old readers ignore unknown fields.

**Rejected:** this provides no escape hatch when a breaking change is genuinely required (e.g. changing the payload
encoding from base64 to raw bytes). Without a version field, a reader has no way to detect that it is holding an
unreadable object.

---

## Decision

**Option A: integer `snapshot_version` embedded in the envelope JSON.**

- Field name: `snapshot_version` (snake_case, consistent with `schema_version` in `internal/telemetry`).
- Initial value: `1`.
- On read: if `snapshot_version > CurrentSnapshotVersion`, skip the object, log at WARN with `sdk_key_public_id`, and
  increment `proxy.snapshot.load.total{result="skipped_unknown_version"}`. Startup continues; the cache entry for that
  key starts empty and fills on the first live request.
- On read: unknown fields within a known version are ignored (default `encoding/json` behaviour).
- No `x-amz-meta-*` or sidecar metadata files: the version is always embedded in the object body so the local-file
  backend needs no special treatment.

---

## Rolling-Deploy Behaviour

During a rolling deployment where pods running version N+1 coexist with pods running version N:

- **N+1 pod writes a version-2 snapshot.** Version-N pods boot, encounter version-2 objects, skip them, emit
  `skipped_unknown_version` metrics, and take the cold-fetch path on the first SDK request. The first successful cold
  fetch writes a version-1 snapshot, overwriting the version-2 object.
- **N pods write version-1 snapshots.** Version-N+1 pods read them normally.

Snapshots are a **startup hint**, not a source of truth. The cold-fetch path is always safe. The fleet self-heals within
one cache refresh cycle after the rollout completes.

---

## Consequences

- **Positive:** no compatibility matrix; a single integer comparison is the entire skip logic.
- **Positive:** works identically for S3-compatible and local-file backends.
- **Positive:** a breaking envelope change in the future is handled by bumping the integer and updating the skip logic.
- **Negative:** during a cross-version rolling deploy, some pods may start cold (miss one startup optimisation) until
  the version-N+1 rollout completes or the older snapshots are overwritten. This is acceptable; the proxy is designed to
  handle cold starts.
