# memini

A Kubernetes-ready memory service for AI agents (REST + MCP).

A Kubernetes-ready memory service for AI agents, exposing REST and MCP.

## Installing

```sh
# Embedded sqlite backend (single replica + PVC) — point at your embeddings endpoint:
helm install memini ./charts/memini \
  --set embeddings.baseURL=http://text-embeddings:80/v1 \
  --set embeddings.model=bge-m3 --set embeddings.dims=1024

# Scale-out Postgres/VectorChord backend:
helm install memini ./charts/memini \
  --set backend=postgres \
  --set postgres.dsnSecret.name=memini-pg \
  --set autoscaling.enabled=true \
  --set embeddings.baseURL=http://text-embeddings:80/v1
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
| auth | object | `{"apiKey":"","apiKeySecret":{"key":"api-key","name":""}}` | Bearer-token auth (protects both /v1 and /mcp). REQUIRED before exposing memini off-cluster (see httpRoute). Either reference an existing secret, or set `auth.apiKey` to have the chart create one. |
| auth.apiKey | string | `""` | Plaintext token; when set, the chart creates a Secret holding it. |
| auth.apiKeySecret | object | `{"key":"api-key","name":""}` | Existing secret holding the token (used when auth.apiKey is empty). |
| autoscaling | object | `{"enabled":false,"maxReplicas":10,"minReplicas":2,"targetCPUUtilizationPercentage":80}` | Horizontal Pod Autoscaler (postgres backend only) |
| backend | string | `"sqlite"` | Storage backend: "sqlite" (embedded, single replica + PVC) or "postgres" (scale-out) |
| config | object | `{"defaultNamespace":"default","logFormat":"json","logLevel":"info","namespaceHeader":"X-Memini-Namespace","sweepInterval":"1h"}` | Service-level (non-secret) configuration, mapped to MEMINI_* env vars |
| config.sweepInterval | string | `"1h"` | Decay sweeper interval (Go duration) |
| embeddings | object | `{"apiKeySecret":{"key":"api-key","name":""},"baseURL":"","dims":1536,"model":"text-embedding-3-small"}` | External OpenAI-compatible embeddings endpoint (required for vector search) |
| embeddings.apiKeySecret | object | `{"key":"api-key","name":""}` | Optional existing secret holding the embeddings API key |
| embeddings.dims | int | `1536` | Embedding dimensionality; MUST match the deployed model |
| fsck | object | `{"cronjob":{"enabled":false,"image":"curlimages/curl:8.20.0","schedule":"0 * * * *"}}` | Periodic fsck CronJob (the in-process sweeper already handles decay) |
| fullnameOverride | string | `""` |  |
| grafanaDashboards | object | `{"enabled":false,"folder":"memini"}` | Bundled Grafana dashboards, rendered as ConfigMaps for the grafana-operator. Point your Grafana CR's `dashboardsConfigMaps` selector at this release's namespace; the operator will pick up any ConfigMap with the `grafana_dashboard: "1"` label. |
| grafanaDashboards.enabled | bool | `false` | Render dashboards as ConfigMaps with the standard grafana_dashboard label |
| grafanaDashboards.folder | string | `"memini"` | grafana_dashboard_folder annotation; controls where dashboards land in Grafana |
| httpRoute | object | `{"annotations":{},"enabled":false,"hostnames":["memini.example.com"],"parentRefs":[{"name":""}]}` | Gateway API HTTPRoute to expose memini off-cluster (the modern replacement for the deprecated Ingress). Attaches to an existing Gateway. Set auth (above) before enabling. TLS is configured on the Gateway listener, not here. |
| httpRoute.parentRefs | list | `[{"name":""}]` | Gateways to attach to (parentRefs) |
| image | object | `{"digest":"","pullPolicy":"IfNotPresent","repository":"ghcr.io/eleboucher/memini","tag":""}` | Container image |
| image.digest | string | `""` | Image digest (sha256:...); takes precedence over tag when set |
| image.tag | string | `""` | Image tag; defaults to the chart appVersion when empty |
| imagePullSecrets | list | `[]` |  |
| llm | object | `{"apiKeySecret":{"key":"api-key","name":""},"baseURL":"","model":"gpt-4o-mini"}` | Opt-in LLM endpoint for consolidation; leave baseURL empty to run headless |
| metrics | object | `{"serviceMonitor":{"enabled":false,"interval":"30s","scrapeTimeout":"10s"}}` | Prometheus Operator ServiceMonitor |
| nameOverride | string | `""` |  |
| nodeSelector | object | `{}` |  |
| podAnnotations | object | `{}` |  |
| podSecurityContext.fsGroup | int | `65532` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `65532` |  |
| postgres | object | `{"dsnSecret":{"key":"dsn","name":""}}` | postgres backend connection |
| postgres.dsnSecret | object | `{"key":"dsn","name":""}` | Existing secret holding the libpq DSN (required when backend=postgres) |
| replicaCount | int | `1` | Replica count (postgres only; sqlite is pinned to 1) |
| resources | object | `{}` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| service.port | int | `8080` |  |
| service.type | string | `"ClusterIP"` |  |
| serviceAccount.create | bool | `true` | Create a ServiceAccount |
| serviceAccount.name | string | `""` | ServiceAccount name; generated when empty |
| sqlite | object | `{"persistence":{"accessModes":["ReadWriteOnce"],"enabled":true,"size":"5Gi","storageClass":""}}` | sqlite backend persistence (StatefulSet volumeClaimTemplate) |
| tolerations | list | `[]` |  |

## Observability

The service exposes Prometheus metrics at `:8080/metrics`. The chart ships a
single Grafana dashboard at `charts/memini/dashboards/memini.json` that
surfaces the actual memory value: live counts by tier, write/recall traffic,
consolidation outcomes (including LLM dedup effectiveness), decay sweeps,
embedder latency and tokens, and end-to-end op latency.

For automatic loading via the [grafana-operator](https://github.com/grafana-operator/grafana-operator),
enable the ConfigMap renderer and point your `Grafana` CR's `dashboardsConfigMaps`
selector at the release namespace:

```sh
helm install memini ./charts/memini \
  --set metrics.serviceMonitor.enabled=true \
  --set grafanaDashboards.enabled=true
```

The chart creates a single ConfigMap labelled `grafana_dashboard: "1"` and
annotated with `grafana_dashboard_folder: memini`.

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| elebouch |  |  |
## Source Code

* <https://github.com/eleboucher/memini>
