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
| controllers.fsck | object | `{"containers":{"fsck":{"command":["/bin/sh","-c","curl -sf -X POST --variable \"MEMINI_API_KEY=$MEMINI_API_KEY\" --expand-header \"Authorization: Bearer {{MEMINI_API_KEY}}\" http://memini:8080/v1/fsck"],"image":{"pullPolicy":"IfNotPresent","repository":"curlimages/curl","tag":"8.21.0"},"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}}},"cronjob":{"concurrencyPolicy":"Forbid","schedule":"0 * * * *"},"enabled":false,"pod":{"affinity":{"podAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"labelSelector":{"matchExpressions":[{"key":"app.kubernetes.io/controller","operator":"In","values":["main"]},{"key":"app.kubernetes.io/instance","operator":"In","values":["{{ .Release.Name }}"]}]},"topologyKey":"kubernetes.io/hostname"}]}},"securityContext":{"runAsNonRoot":true,"runAsUser":65532}},"type":"cronjob"}` | Periodic fsck CronJob. Disabled by default — the in-process sweeper already handles decay. If enabled, the container needs MEMINI_API_KEY in env and uses curl's --variable/--expand-header so the token never lands in argv. |
| controllers.fsck.pod.affinity | object | `{"podAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"labelSelector":{"matchExpressions":[{"key":"app.kubernetes.io/controller","operator":"In","values":["main"]},{"key":"app.kubernetes.io/instance","operator":"In","values":["{{ .Release.Name }}"]}]},"topologyKey":"kubernetes.io/hostname"}]}}` | Pin the fsck pod to a node that already runs the main StatefulSet pod. Useful when fsck is extended to touch the RWO PVC directly. Label- based, so it follows the StatefulSet if it ever moves. Drop this block (or set to `{}`) to let the scheduler pick any node. |
| controllers.main.containers | object | `{"main":{"env":{"MEMINI_BACKEND":"sqlite","MEMINI_DEFAULT_NAMESPACE":"default","MEMINI_EMBED_BASE_URL":"","MEMINI_EMBED_DIMS":"1536","MEMINI_EMBED_MODEL":"text-embedding-3-small","MEMINI_HTTP_ADDR":":8080","MEMINI_LOG_FORMAT":"json","MEMINI_LOG_LEVEL":"info","MEMINI_METRICS_ADDR":":9090","MEMINI_SQLITE_PATH":"/data/memini.db","MEMINI_SWEEP_INTERVAL":"1h","MEMINI_UI_ADDR":":8081","MEMINI_UI_ENABLED":"true"},"image":{"digest":"","pullPolicy":"IfNotPresent","repository":"registry.erwanleboucher.dev/eleboucher/memini","tag":""},"probes":{"liveness":{"custom":true,"enabled":true,"spec":{"httpGet":{"path":"/healthz","port":8080},"initialDelaySeconds":5,"periodSeconds":15}},"readiness":{"custom":true,"enabled":true,"spec":{"httpGet":{"path":"/readyz","port":8080},"initialDelaySeconds":3,"periodSeconds":10}}},"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}}}` | HorizontalPodAutoscaler (postgres / deployment backend only — sqlite is single-writer). Uncomment after switching type to `deployment`: horizontalPodAutoscaler:   minReplicas: 2   maxReplicas: 10   metrics:     - type: Resource       resource:         name: cpu         target:           type: Utilization           averageUtilization: 80 |
| controllers.main.containers.main.env | object | `{"MEMINI_BACKEND":"sqlite","MEMINI_DEFAULT_NAMESPACE":"default","MEMINI_EMBED_BASE_URL":"","MEMINI_EMBED_DIMS":"1536","MEMINI_EMBED_MODEL":"text-embedding-3-small","MEMINI_HTTP_ADDR":":8080","MEMINI_LOG_FORMAT":"json","MEMINI_LOG_LEVEL":"info","MEMINI_METRICS_ADDR":":9090","MEMINI_SQLITE_PATH":"/data/memini.db","MEMINI_SWEEP_INTERVAL":"1h","MEMINI_UI_ADDR":":8081","MEMINI_UI_ENABLED":"true"}` | Environment variables (MEMINI_*). Dictionary style. The always-on defaults below configure the sqlite backend; commented entries are optional knobs operators can uncomment. |
| controllers.main.containers.main.env.MEMINI_BACKEND | string | `"sqlite"` | Storage backend: "sqlite" (default) or "postgres". For postgres, set this to "postgres", switch controllers.main.type to deployment, disable persistence.data, and wire MEMINI_POSTGRES_DSN below. |
| controllers.main.containers.main.env.MEMINI_DEFAULT_NAMESPACE | string | `"default"` | Default namespace for memories. |
| controllers.main.containers.main.env.MEMINI_EMBED_BASE_URL | string | `""` | OpenAI-compatible embeddings endpoint (required for vector search). |
| controllers.main.containers.main.env.MEMINI_EMBED_DIMS | string | `"1536"` | Embedding dimensionality; MUST match the deployed model. |
| controllers.main.containers.main.env.MEMINI_EMBED_MODEL | string | `"text-embedding-3-small"` | Embedding model name. |
| controllers.main.containers.main.env.MEMINI_HTTP_ADDR | string | `":8080"` | HTTP listen address. |
| controllers.main.containers.main.env.MEMINI_LOG_FORMAT | string | `"json"` | Log format (json|text). |
| controllers.main.containers.main.env.MEMINI_LOG_LEVEL | string | `"info"` | Log level. |
| controllers.main.containers.main.env.MEMINI_METRICS_ADDR | string | `":9090"` | Dedicated metrics listener; keeps /metrics off the main port and every route, so it needs no bearer token. "" serves it on the main port. |
| controllers.main.containers.main.env.MEMINI_SQLITE_PATH | string | `"/data/memini.db"` | sqlite database path (kept on the PVC mounted at /data). |
| controllers.main.containers.main.env.MEMINI_SWEEP_INTERVAL | string | `"1h"` | Decay sweeper interval (Go duration). |
| controllers.main.containers.main.env.MEMINI_UI_ADDR | string | `":8081"` | Dedicated admin-UI listener. The UI shell is credential-free (it never contains MEMINI_API_KEY; the SPA signs in against /v1/self in the browser and keeps its token in localStorage). A dedicated port still keeps the UI off the main API port; expose it only on a trusted (LAN) gateway (see route.ui). "" serves the UI on the main port. |
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
| persistence | object | `{"api-keys":{"enabled":false,"globalMounts":[{"path":"/etc/memini/api-keys.yaml","readOnly":true,"subPath":"api-keys.yaml"}],"name":"memini-api-keys","type":"secret"},"data":{"accessMode":"ReadWriteOnce","enabled":true,"globalMounts":[{"path":"/data"}],"size":"5Gi","type":"persistentVolumeClaim"}}` | Persistent storage for the sqlite backend, mounted at /data. A standalone PVC (single-replica memini sqlite is single-writer). For the postgres backend set `persistence.data.enabled: false`. |
| persistence.api-keys | object | `{"enabled":false,"globalMounts":[{"path":"/etc/memini/api-keys.yaml","readOnly":true,"subPath":"api-keys.yaml"}],"name":"memini-api-keys","type":"secret"}` | Declarative API keys mounted from a Secret (MEMINI_API_KEYS_FILE above). The wiring is three pieces and all three are required: a Secret whose data key `api-keys.yaml` holds the file (SOPS-decrypted by your GitOps pipeline, referenced by `name` below), `enabled: true` here, and MEMINI_API_KEYS_FILE: /etc/memini/api-keys.yaml in env above. See docs/guides/access-control.md for the file's contents.  Upgrading with your own `persistence.api-keys` block from before v0.7.1: this chart-side block is new, and helm merges it underneath your values, so its `enabled: false` wins whenever your block does not set the flag at all. The failure is silent: the keys mount and volume simply vanish from the rendered pod and the server boots without the file. Add `enabled: true` to your block; nothing else needs to change. |
| persistence.data.globalMounts | list | `[{"path":"/data"}]` | Storage class; null uses the cluster default. storageClass: "" |
| route | object | `{"main":{"enabled":false,"hostnames":[],"kind":"HTTPRoute","parentRefs":[],"rules":[{"backendRefs":[{"identifier":"main","port":"http"}]}]},"ui":{"enabled":false,"hostnames":[],"kind":"HTTPRoute","parentRefs":[],"rules":[{"backendRefs":[{"identifier":"main","port":"ui"}]}]}}` | Gateway API HTTPRoutes (the modern replacement for Ingress). BOTH disabled by default — the operator enables and attaches them to an existing Gateway. Exposing /v1 or /mcp REQUIRES bearer auth: set MEMINI_API_KEY (see env above). |
| route.main | object | `{"enabled":false,"hostnames":[],"kind":"HTTPRoute","parentRefs":[],"rules":[{"backendRefs":[{"identifier":"main","port":"http"}]}]}` | Public API + MCP surface, on the `http` port. Enable, set the gateway in parentRefs and the hostname. Only /v1, /mcp, /healthz, /readyz live here (the UI is on its own port via MEMINI_UI_ADDR), so this route can safely forward all of `http` once MEMINI_API_KEY gates it. Add rules[].matches to scope it further if you want to keep the probes private. |
| route.ui | object | `{"enabled":false,"hostnames":[],"kind":"HTTPRoute","parentRefs":[],"rules":[{"backendRefs":[{"identifier":"main","port":"ui"}]}]}` | Admin UI, on the `ui` port (MEMINI_UI_ADDR). The shell is credential-free, but a signed-in token is same-origin-readable, so wire this to an INTERNAL gateway. Disabled by default; enable on installs that want the SPA through a Gateway. If MEMINI_UI_ADDR is "" the UI rides the main port instead — then point this at `port: http` and treat that whole route as trusted. |
| service.main.controller | string | `"main"` | Primary Service. Targets the main controller. Common derives the container ports from these ports. |
| service.main.ports.http.port | int | `8080` |  |
| service.main.ports.http.primary | bool | `true` |  |
| service.main.ports.metrics | object | `{"port":9090}` | Dedicated metrics port (MEMINI_METRICS_ADDR). Not referenced by any route, so /metrics stays in-cluster. |
| service.main.ports.ui | object | `{"port":8081}` | Dedicated admin-UI port (MEMINI_UI_ADDR). Serves the credential-free SPA (it signs in in-browser); wire route.ui to it and keep that route on an internal gateway, since a stored token is same-origin-readable. |
| serviceAccount | object | `{"main":{}}` | ServiceAccount created for the workload. |
| serviceMonitor | object | `{"main":{"enabled":false,"endpoints":[{"interval":"30s","path":"/metrics","port":"metrics","scrapeTimeout":"10s"}],"service":{"identifier":"main"}}}` | Prometheus Operator ServiceMonitor. Disabled by default; flip `enabled` to scrape /metrics from the dedicated `metrics` port (MEMINI_METRICS_ADDR / the service port below). That port is unauthenticated and kept off every route, so the scrape needs no bearer token. If you instead serve /metrics on the main port (MEMINI_METRICS_ADDR: ""), point the endpoint at `port: http` and, when MEMINI_API_KEY is set, add an `authorization.credentials` secret reference. |

## Observability

The service exposes Prometheus metrics on the dedicated `metrics` port
(`MEMINI_METRICS_ADDR`, `:9090` in this chart's defaults) — not on the main
`:8080` HTTP port. The chart ships a
single Grafana dashboard at `charts/memini/dashboards/memini.json` covering
the full metric surface: an at-a-glance "Now" row (live memories, traffic,
error ratio, repair backlog), the write path (rates by tier, degraded
vectorless writes, dedup and hygiene), retrieval quality (degradation,
floored candidates, reinforcement), aging and maintenance (promotion,
demotion, sweeps, tombstones, the self-repair backlog), embedder and rerank
health, LLM consolidation, the HTTP surface, and client injection telemetry.
Every panel query is verified against the metric registry and carries a
description of what bad looks like.

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

* <https://git.erwanleboucher.dev/eleboucher/memini>
