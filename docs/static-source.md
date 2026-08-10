# Static (Offline) Config Source

The proxy can run fully offline, serving flag config from a single local JSON file instead of the
Dynatrace CDN. In this mode there is **no upstream connectivity for config**: no CDN fetch, no
background TTL refresh, and no upstream SSE. Config changes are driven exclusively by changes to the
backing file.

Enable it with `CONFIG_SOURCE=static` and `STATIC_FILE_PATH`. See
[configuration.md](configuration.md) for the full variable reference and
[adr/0005-offline-static-file-config-source.md](adr/0005-offline-static-file-config-source.md) for
the design rationale.

## When to use it

- The proxy cannot reach the Dynatrace CDN (air-gapped or restricted-egress environments).
- You want to pin an exact, reviewable set of flag values and roll them out by updating a file (for
  example, a Kubernetes ConfigMap).

## The backing file

The file is a single [flagd](https://flagd.dev) config document. Flags are grouped into **flag
sets** by a `flagSetId`, following flagd's flag-set paradigm. `flagSetId` is a `metadata` property,
resolvable at two levels:

- **Flag-set level** — the top-level `metadata.flagSetId` is the default for every flag in the file.
- **Per-flag level** — a flag's own `metadata.flagSetId` overrides the default for that flag.

Per-flag metadata is a shallow merge over the set-level metadata: a flag that overrides only another
key (e.g. `version`) still inherits the set-level `flagSetId`. Flags that resolve to no `flagSetId`
at either level belong to no flag set and are never served (a warning is logged with their count).

### The flagSetId is a full SDK key

The `flagSetId` value must be the **full SDK key** the SDK client uses, matched exactly:
`{token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}`. An SDK requesting
`GET /server/<full_key>.json` is served the subset of flags whose resolved `flagSetId` equals that
full key.

> ⚠️ The file therefore contains full SDK keys, including their private segment. Treat the file as
> sensitive: restrict its permissions and never commit it to a public repository. The proxy never
> logs or persists the file's contents; logs and metrics use only the key's public prefix.

### Example

```json
{
  "$schema": "https://flagd.dev/schema/v0/flags.json",
  "metadata": {
    "flagSetId": "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a",
    "version": "v1"
  },
  "flags": {
    "enable-checkout": {
      "state": "ENABLED",
      "variants": { "on": true, "off": false },
      "defaultVariant": "on"
    },
    "eu-only-flag": {
      "state": "ENABLED",
      "variants": { "on": true, "off": false },
      "defaultVariant": "off",
      "metadata": {
        "flagSetId": "dt01.server_eu_zz9yy8xx7w.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
      }
    }
  }
}
```

Here `enable-checkout` belongs to the `..._us_...` flag set (inherited), and `eu-only-flag` belongs
to the `..._eu_...` flag set (per-flag override).

## What the proxy serves

For each flag set, the proxy precomputes a served flagd document into memory:

- All top-level keys are copied through **except** `flags` (replaced with that set's flags) and
  `eventStreams` (omitted — there is no SSE offline).
- `metadata.flagSetId` is set to the served set's id, so the document is self-consistent.
- A strong `ETag` is computed from the document. SDK clients that send `If-None-Match` receive `304`
  until the flag set's content actually changes.

A request for a `flagSetId` that has no flags returns `404`.

## Reloads

A watcher polls the backing file on an interval (`STATIC_FILE_POLL_INTERVAL`, default `10s`). On
each tick it checks the file's mtime and size, then a content hash. When the content changes, it
rebuilds the in-memory snapshot and swaps it in atomically — in-flight requests are never disrupted.

Reloads are **fail-safe**: if the file becomes unreadable or contains invalid JSON, the proxy logs
an error, increments `proxy.static.reload.total{result="read_error"|"parse_error"}`, and **keeps
serving the last-good snapshot**. Only a successful parse replaces the served config.

The **initial** load at startup is not fail-safe: if the file is missing or invalid, the proxy exits
non-zero (there is no config to serve). `/readyz` and `/healthz` return `200` only after the initial
load succeeds.

## Metrics passthrough

`POST /v1/metrics` is available in static mode only when `OFFLINE_METRICS_UPSTREAM_URL` is set to the
full upstream URL. When set, the request body and `Authorization` header are forwarded verbatim to
that URL (no geo derivation, no SDK-key format checks). When unset, `POST /v1/metrics` returns `501`.

## SSE

`GET /sse` returns `501` in static mode. SDK clients fall back to polling
`GET /server/<key>.json`; the `ETag`/`If-None-Match` flow means they only download a new payload
when the file has changed.

## Observability

- `proxy.static.reload.total` — reload attempts, labeled `result`
  (`loaded`/`unchanged`/`parse_error`/`read_error`).
- `proxy.static.flag_sets` — number of flag sets currently served (gauge, updated after each load).
- `proxy.requests.total` — config requests, labeled `result`
  (`hit`/`not_modified`/`not_found`/`client_error`).

## Kubernetes example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sdk-proxy-flags
data:
  flags.json: |
    { "metadata": { "flagSetId": "dt01.server_us_ab3xy9kp2q...._..." }, "flags": { } }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sdk-proxy
spec:
  template:
    spec:
      containers:
        - name: sdk-proxy
          image: sdk-proxy:latest
          env:
            - name: CONFIG_SOURCE
              value: static
            - name: STATIC_FILE_PATH
              value: /config/flags.json
          volumeMounts:
            - name: flags
              mountPath: /config
      volumes:
        - name: flags
          configMap:
            name: sdk-proxy-flags
```

Updating the ConfigMap re-mounts `flags.json`; the watcher picks up the change within one poll
interval.
