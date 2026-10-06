# sdk-proxy Helm Chart

Deploys the [Dynatrace Feature Management SDK Proxy](../../README.md) to Kubernetes.

## Install

```bash
helm install sdk-proxy ./charts/sdk-proxy --namespace sdk-proxy --create-namespace
```

Point SDKs at the Service:

```json
{ "proxyUrl": "http://sdk-proxy.sdk-proxy.svc.cluster.local:8080" }
```

Verify with `helm test sdk-proxy -n sdk-proxy`, which calls `/readyz` through the Service.

## What the Chart Configures

- `/livez` liveness, `/readyz` readiness, and a startup probe that holds off liveness checks while snapshots load.
- `PROXY_INSTANCE_ID` set to the pod name, so usage records identify each replica.
- `terminationGracePeriodSeconds: 60`, which covers `SHUTDOWN_TIMEOUT` (30s) plus the SSE, snapshot, and usage flush
  steps. Raise it if you raise `SHUTDOWN_TIMEOUT`. See [docs/shutdown.md](../../docs/shutdown.md).
- Non-root UID 65532, read-only root filesystem, all capabilities dropped, no ServiceAccount token mounted.
- A rolling update with `maxUnavailable: 0`, plus a PodDisruptionBudget (`maxUnavailable: 1`).
- A ConfigMap checksum on the pod template, so config changes roll the pods.

The chart validates `snapshot.*` at render time and fails with a clear message instead of a pod that exits 1 at startup.

## Values

| Key                                                                              | Default                                          | Description                                                                              |
|----------------------------------------------------------------------------------|--------------------------------------------------|------------------------------------------------------------------------------------------|
| `replicaCount`                                                                   | `2`                                              | Ignored when `autoscaling.enabled`.                                                      |
| `image.repository`                                                               | `ghcr.io/dynatrace/feature-management-sdk-proxy` |                                                                                          |
| `image.tag`                                                                      | `v` + `appVersion`                               | Release images use the git tag.                                                          |
| `containerPort`                                                                  | `8080`                                           | Rendered as `PORT`.                                                                      |
| `config`                                                                         | `LOG_LEVEL`, `SHUTDOWN_TIMEOUT`                  | Map of env var name → string value. See [configuration.md](../../docs/configuration.md). |
| `otel.endpoint`                                                                  | `""`                                             | `OTEL_EXPORTER_OTLP_ENDPOINT`; telemetry export is disabled when empty.                  |
| `otel.serviceName` / `resourceAttributes` / `tracesSampler` / `tracesSamplerArg` | `""`                                             | Map to the matching `OTEL_*` variables.                                                  |
| `snapshot.backend`                                                               | `""`                                             | `""` (disabled), `s3`, or `local`.                                                       |
| `snapshot.maxAge` / `writeQueueSize`                                             | `""`                                             | `SNAPSHOT_MAX_AGE` / `SNAPSHOT_WRITE_QUEUE_SIZE`.                                        |
| `snapshot.s3.bucket` / `region`                                                  | `""`                                             | Required for `s3`.                                                                       |
| `snapshot.s3.endpoint` / `prefix`                                                | `""`                                             | For S3-compatible stores and shared buckets.                                             |
| `snapshot.s3.existingSecret`                                                     | `""`                                             | Secret with keys `accessKey` and `secretKey`.                                            |
| `snapshot.s3.accessKey` / `secretKey`                                            | `""`                                             | Chart creates the Secret. Prefer workload identity via `serviceAccount.annotations`.     |
| `snapshot.local.dir`                                                             | `/snapshots`                                     | Mount path for the snapshot PVC.                                                         |
| `snapshot.local.persistence.*`                                                   | `ReadWriteOnce`, `1Gi`                           | `existingClaim`, `storageClass`, `accessModes`, `size`. The PVC is kept on uninstall.    |
| `serviceAccount.annotations`                                                     | `{}`                                             | For example, an IRSA role ARN that grants access to the snapshot bucket.                 |
| `service.type` / `port`                                                          | `ClusterIP` / `8080`                             |                                                                                          |
| `service.sessionAffinity`                                                        | `None`                                           | See [Multiple Replicas](#multiple-replicas).                                             |
| `ingress.*`                                                                      | disabled                                         | Standard `networking.k8s.io/v1` ingress. See [Ingress](#ingress).                        |
| `autoscaling.*`                                                                  | disabled, 2–10 replicas, 70% CPU                 | HPA (`autoscaling/v2`).                                                                  |
| `podDisruptionBudget.*`                                                          | enabled, `maxUnavailable: 1`                     | Set `minAvailable` instead if preferred.                                                 |
| `resources`                                                                      | 100m / 128Mi requests, 512Mi memory limit        |                                                                                          |
| `extraEnv` / `extraEnvFrom` / `extraVolumes` / `extraVolumeMounts`               | `[]`                                             | Escape hatches.                                                                          |

See [values.yaml](values.yaml) for the full list, including probes, security contexts, and scheduling.

## Examples

S3 snapshots with IRSA, plus telemetry:

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/sdk-proxy-snapshots
snapshot:
  backend: s3
  s3:
    bucket: my-proxy-snapshots
    region: us-east-1
    prefix: prod/
otel:
  endpoint: http://otel-collector.monitoring:4318
  resourceAttributes: deployment.environment=production
```

Single replica with local snapshots on a PVC:

```yaml
replicaCount: 1
podDisruptionBudget:
  enabled: false
snapshot:
  backend: local
```

With a `ReadWriteOnce` volume the chart switches the Deployment to the `Recreate` strategy, because the volume can't be
attached to the old and new pod at the same time. It refuses to render `local` with more than one replica unless the
volume is `ReadWriteMany` or `existingClaim` is set. Use `s3` for multi-replica deployments.

## Multiple Replicas

Each replica keeps its own cache and its own upstream SSE connection per SDK key. An SDK's config request and its
`GET /sse` subscription can land on different pods. If that pod hasn't cached the key yet, it answers `412` and the SDK
retries. Two ways to avoid this:

- `service.sessionAffinity: ClientIP` pins each client to one pod. Behind an ingress, configure affinity on the ingress
  controller instead, because the Service only sees the controller's IPs.
- `snapshot.backend: s3` lets new pods start warm from snapshots that other replicas wrote.

## Ingress

`/sse` is a long-lived Server-Sent Events stream. Turn off response buffering and raise read timeouts on your ingress
controller. For ingress-nginx:

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
  hosts:
    - host: sdk-proxy.example.com
      paths:
        - path: /
          pathType: Prefix
```

## Development

```bash
make helm-lint
```
