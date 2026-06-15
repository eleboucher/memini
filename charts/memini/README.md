# memini

A Kubernetes-ready memory service for AI agents (REST + MCP).

A Kubernetes-ready memory service for AI agents, exposing REST and MCP. This
chart is powered by the [bjw-s common library](https://github.com/bjw-s-labs/helm-charts)
and uses its native values API (`controllers`/`service`/`route`/`persistence`/
`serviceAccount`/`serviceMonitor`).

## Installing

```sh
# Embedded sqlite backend (single-replica StatefulSet + PVC) — point at your
# embeddings endpoint:
helm install memini ./charts/memini \
  --set controllers.main.containers.main.env.MEMINI_EMBED_BASE_URL=http://text-embeddings:80/v1 \
  --set controllers.main.containers.main.env.MEMINI_EMBED_MODEL=bge-m3 \
  --set controllers.main.containers.main.env.MEMINI_EMBED_DIMS=1024
```

### Postgres (scale-out) backend

The default backend is sqlite (a StatefulSet with a PVC at `/data`). To run the
scale-out postgres backend instead, override the controller and env via a values
file:

```yaml
controllers:
  main:
    type: deployment           # no PVC, scale-out
    containers:
      main:
        env:
          MEMINI_BACKEND: "postgres"
          MEMINI_POSTGRES_DSN:
            valueFrom:
              secretKeyRef:
                name: memini-pg
                key: dsn
    # horizontalPodAutoscaler:   # optional autoscaling
    #   minReplicas: 2
    #   maxReplicas: 10
persistence:
  data:
    enabled: false              # no sqlite volume
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| controllers.fsck | object | `{"containers":{"fsck":{"command":["/bin/sh","-c","curl -sf -X POST --variable \"MEMINI_API_KEY=$MEMINI_API_KEY\" --expand-header \"Authorization: Bearer {{MEMINI_API_KEY}}\" http://memini:8080/v1/fsck"],"image":{"pullPolicy":"IfNotPresent","repository":"curlimages/curl","tag":"8.20.0"},"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}}},"cronjob":{"concurrencyPolicy":"Forbid","schedule":"0 * * * *"},"enabled":false,"pod":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532}},"type":"cronjob"}` | Periodic fsck CronJob. Disabled by default — the in-process sweeper already handles decay. If enabled, the container needs MEMINI_API_KEY in env and uses curl's --variable/--expand-header so the token never lands in argv. |
| controllers.main.containers | object | `{"main":{"env":{"MEMINI_BACKEND":"sqlite","MEMINI_DEFAULT_NAMESPACE":"default","MEMINI_EMBED_BASE_URL":"","MEMINI_EMBED_DIMS":"1536","MEMINI_EMBED_MODEL":"text-embedding-3-small","MEMINI_HTTP_ADDR":":8080","MEMINI_LOG_FORMAT":"json","MEMINI_LOG_LEVEL":"info","MEMINI_NAMESPACE_HEADER":"X-Memini-Namespace","MEMINI_SQLITE_PATH":"/data/memini.db","MEMINI_SWEEP_INTERVAL":"1h","MEMINI_UI_ENABLED":"true"},"image":{"digest":"","pullPolicy":"IfNotPresent","repository":"registry.erwanleboucher.dev/eleboucher/memini","tag":""},"probes":{"liveness":{"custom":true,"enabled":true,"spec":{"httpGet":{"path":"/healthz","port":8080},"initialDelaySeconds":5,"periodSeconds":15}},"readiness":{"custom":true,"enabled":true,"spec":{"httpGet":{"path":"/readyz","port":8080},"initialDelaySeconds":3,"periodSeconds":10}}},"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}}}` | HorizontalPodAutoscaler (postgres / deployment backend only — sqlite is single-writer). Uncomment after switching type to `deployment`: horizontalPodAutoscaler:   minReplicas: 2   maxReplicas: 10   metrics:     - type: Resource       resource:         name: cpu         target:           type: Utilization           averageUtilization: 80 |
| controllers.main.containers.main.env | object | `{"MEMINI_BACKEND":"sqlite","MEMINI_DEFAULT_NAMESPACE":"default","MEMINI_EMBED_BASE_URL":"","MEMINI_EMBED_DIMS":"1536","MEMINI_EMBED_MODEL":"text-embedding-3-small","MEMINI_HTTP_ADDR":":8080","MEMINI_LOG_FORMAT":"json","MEMINI_LOG_LEVEL":"info","MEMINI_NAMESPACE_HEADER":"X-Memini-Namespace","MEMINI_SQLITE_PATH":"/data/memini.db","MEMINI_SWEEP_INTERVAL":"1h","MEMINI_UI_ENABLED":"true"}` | Environment variables (MEMINI_*). Dictionary style. The always-on defaults below configure the sqlite backend; commented entries are optional knobs operators can uncomment. |
| controllers.main.containers.main.env.MEMINI_BACKEND | string | `"sqlite"` | Storage backend: "sqlite" (default) or "postgres". For postgres, set this to "postgres", switch controllers.main.type to deployment, disable persistence.data, and wire MEMINI_POSTGRES_DSN below. |
| controllers.main.containers.main.env.MEMINI_DEFAULT_NAMESPACE | string | `"default"` | Default namespace for memories. |
| controllers.main.containers.main.env.MEMINI_EMBED_BASE_URL | string | `""` | OpenAI-compatible embeddings endpoint (required for vector search). |
| controllers.main.containers.main.env.MEMINI_EMBED_DIMS | string | `"1536"` | Embedding dimensionality; MUST match the deployed model. |
| controllers.main.containers.main.env.MEMINI_EMBED_MODEL | string | `"text-embedding-3-small"` | Embedding model name. |
| controllers.main.containers.main.env.MEMINI_HTTP_ADDR | string | `":8080"` | HTTP listen address. |
| controllers.main.containers.main.env.MEMINI_LOG_FORMAT | string | `"json"` | Log format (json|text). |
| controllers.main.containers.main.env.MEMINI_LOG_LEVEL | string | `"info"` | Log level. |
| controllers.main.containers.main.env.MEMINI_NAMESPACE_HEADER | string | `"X-Memini-Namespace"` | HTTP header that selects the namespace per request. |
| controllers.main.containers.main.env.MEMINI_SQLITE_PATH | string | `"/data/memini.db"` | sqlite database path (kept on the PVC mounted at /data). |
| controllers.main.containers.main.env.MEMINI_SWEEP_INTERVAL | string | `"1h"` | Decay sweeper interval (Go duration). |
| controllers.main.containers.main.env.MEMINI_UI_ENABLED | string | `"true"` | Serve the embedded admin SPA at /. Set "false" for headless API/MCP. |
| controllers.main.containers.main.image.digest | string | `""` | Image digest (sha256:...); takes precedence over tag when set. The release pipeline rewrites this line. |
| controllers.main.containers.main.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| controllers.main.containers.main.image.repository | string | `"registry.erwanleboucher.dev/eleboucher/memini"` | Container image repository. |
| controllers.main.containers.main.image.tag | string | `""` | Image tag. Empty falls back to the chart appVersion. |
| controllers.main.containers.main.probes | object | `{"liveness":{"custom":true,"enabled":true,"spec":{"httpGet":{"path":"/healthz","port":8080},"initialDelaySeconds":5,"periodSeconds":15}},"readiness":{"custom":true,"enabled":true,"spec":{"httpGet":{"path":"/readyz","port":8080},"initialDelaySeconds":3,"periodSeconds":10}}}` | Container probes. Liveness hits /healthz, readiness hits /readyz. |
| controllers.main.containers.main.securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container security context. |
| controllers.main.pod.securityContext | object | `{"fsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Pod-level security context. |
| controllers.main.replicas | int | `1` | Replica count. sqlite is single-writer so keep at 1. With postgres (type: deployment) you can scale out, or drive it via horizontalPodAutoscaler. |
| controllers.main.type | string | `"statefulset"` | Controller type. `statefulset` for the default sqlite backend (PVC at /data, single replica). For the postgres backend switch this to `deployment` (no PVC, scale-out) — see env MEMINI_BACKEND below. |
| grafanaDashboards | object | `{"enabled":false,"folder":"memini"}` | Bundled Grafana dashboards, rendered as ConfigMaps for the grafana-operator (a custom chart key, not part of the common library). Point your Grafana CR's `dashboardsConfigMaps` selector at this release's namespace; the operator picks up any ConfigMap labelled `grafana_dashboard: "1"`. |
| grafanaDashboards.enabled | bool | `false` | Render the bundled dashboard as a ConfigMap. |
| grafanaDashboards.folder | string | `"memini"` | grafana_dashboard_folder annotation; controls where it lands in Grafana. |
| persistence | object | `{"data":{"accessMode":"ReadWriteOnce","enabled":true,"globalMounts":[{"path":"/data"}],"size":"5Gi","type":"persistentVolumeClaim"}}` | Persistent storage for the sqlite backend, mounted at /data. A standalone PVC (single-replica memini sqlite is single-writer). For the postgres backend set `persistence.data.enabled: false`. |
| persistence.data.globalMounts | list | `[{"path":"/data"}]` | Storage class; null uses the cluster default. storageClass: "" |
| route | object | `{"internal":{"enabled":false,"hostnames":["memini-internal.example.com"],"kind":"HTTPRoute","parentRefs":[{"name":"envoy-internal","namespace":"network","sectionName":"https"}],"rules":[{"backendRefs":[{"identifier":"main","port":"http"}]}]},"main":{"enabled":false,"hostnames":["memini.example.com"],"kind":"HTTPRoute","parentRefs":[{"name":"envoy-external","namespace":"network","sectionName":"https"}],"rules":[{"backendRefs":[{"identifier":"main","port":"http"}],"matches":[{"path":{"type":"PathPrefix","value":"/v1"}},{"path":{"type":"PathPrefix","value":"/mcp"}},{"path":{"type":"PathPrefix","value":"/.well-known/"}}]}]}}` | Gateway API HTTPRoutes (the modern replacement for Ingress). BOTH disabled by default — the operator enables and attaches them to an existing Gateway. Exposing /v1 or /mcp REQUIRES bearer auth: set MEMINI_API_KEY (see env above). |
| route.internal | object | `{"enabled":false,"hostnames":["memini-internal.example.com"],"kind":"HTTPRoute","parentRefs":[{"name":"envoy-internal","namespace":"network","sectionName":"https"}],"rules":[{"backendRefs":[{"identifier":"main","port":"http"}]}]}` | Internal catch-all surface (full app incl. the UI shell, which embeds the API key). Keep this on an internal-only gateway. |
| route.main | object | `{"enabled":false,"hostnames":["memini.example.com"],"kind":"HTTPRoute","parentRefs":[{"name":"envoy-external","namespace":"network","sectionName":"https"}],"rules":[{"backendRefs":[{"identifier":"main","port":"http"}],"matches":[{"path":{"type":"PathPrefix","value":"/v1"}},{"path":{"type":"PathPrefix","value":"/mcp"}},{"path":{"type":"PathPrefix","value":"/.well-known/"}}]}]}` | Public API + MCP surface. Enable, set the gateway in parentRefs, and the hostname, then scope to the token-gated paths via rules[].matches. |
| service.main.controller | string | `"main"` | Primary Service. Targets the main controller. Common derives the container port from this port (8080). |
| service.main.ports.http.port | int | `8080` |  |
| service.main.ports.http.primary | bool | `true` |  |
| serviceAccount | object | `{"main":{}}` | ServiceAccount created for the workload. |
| serviceMonitor | object | `{"main":{"enabled":false,"endpoints":[{"interval":"30s","path":"/metrics","port":"http","scrapeTimeout":"10s"}],"service":{"identifier":"main"}}}` | Prometheus Operator ServiceMonitor. Disabled by default. |

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
  --set serviceMonitor.main.enabled=true \
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
