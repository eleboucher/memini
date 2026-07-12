# Deployment

memini is a single static binary with two storage backends and one hard external
dependency: an OpenAI-compatible **embeddings endpoint**. Without one, remember
and recall error out. Everything else (LLM consolidation, reranking) is optional.

Pick a shape:

- **Compose**, for a laptop or a single box. Postgres backend, everything wired.
- **Single container**, for the smallest useful server. SQLite backend.
- **Kubernetes**, via the bundled Helm chart. SQLite by default, Postgres for
  scale-out.

Environment variables are not restated here. See
[configuration.md](../reference/configuration.md).

## Compose

[`compose.yaml`](../../compose.yaml) brings up three services:

| Service      | Image                                                 | Role                                                                             |
| ------------ | ----------------------------------------------------- | -------------------------------------------------------------------------------- |
| `db`         | `ghcr.io/tensorchord/vchord-postgres`                 | Postgres with VectorChord, the vector index. Healthchecked; memini waits for it. |
| `embeddings` | `ghcr.io/huggingface/text-embeddings-inference` (CPU) | Serves `BAAI/bge-small-en-v1.5`, 384 dimensions. No GPU needed.                  |
| `memini`     | built from the local `Dockerfile`                     | REST + MCP + UI on `:8080`, pointed at both.                                     |

```sh
docker compose up --build
curl -s localhost:8080/healthz   # ok, once the db healthcheck passes
open http://localhost:8080/      # the admin UI
```

`docker compose down -v` tears it down and drops the Postgres volume.

The three embedding settings must agree with each other and with the model the
embeddings service actually serves. `MEMINI_EMBED_DIMS` is `384` here because
`bge-small-en-v1.5` is a 384-dimension model. Get this wrong and the store is
built at the wrong width.

To turn on the optional LLM pipeline (background consolidation, `/v1/answer`,
`llm` reranking), uncomment `MEMINI_LLM_BASE_URL` and `MEMINI_LLM_MODEL` on the
`memini` service and point them at any OpenAI-compatible chat endpoint.

## Single container (SQLite)

The default backend is SQLite: embedded, no external database, single writer.
This is a complete deployment for one person or one small team.

```sh
docker build -t memini .

docker run --rm -p 8080:8080 \
  -v memini-data:/data \
  -e MEMINI_SQLITE_PATH=/data/memini.db \
  -e MEMINI_EMBED_BASE_URL=http://host.docker.internal:8081/v1 \
  -e MEMINI_EMBED_MODEL=bge-small-en-v1.5 \
  -e MEMINI_EMBED_DIMS=384 \
  memini
```

Two things to get right:

- **The volume is not optional.** The image is distroless and the container has
  no useful writable filesystem of its own. Without a volume at `/data`, the
  database dies with the container.
- **The image runs as uid `65532`** (non-root, distroless). A bind-mounted host
  directory must be writable by that uid, or the server cannot create the
  database file. A named volume (as above) avoids the problem entirely, which is
  why the example uses one.

On Linux, `host.docker.internal` does not resolve by default. Use the host IP, or
add `--add-host=host.docker.internal:host-gateway`.

## Kubernetes

The chart lives in [`charts/memini`](../../charts/memini). It is built on the
[bjw-s common library](https://bjw-s-labs.github.io/helm-charts/docs/common-library/)
and uses its values API, so anything the common library supports works here.

```sh
helm dependency update charts/memini
helm install memini charts/memini -f my-values.yaml
```

Read [`values.yaml`](../../charts/memini/values.yaml) before you do. It is
commented as documentation and covers more than this page can.

### Defaults

- A **StatefulSet**, one replica, with a 5Gi `ReadWriteOnce` PVC mounted at
  `/data` (SQLite is single-writer, so do not scale it).
- Non-root (uid `65532`), read-only root filesystem, all capabilities dropped.
- Liveness on `/healthz`, readiness on `/readyz`.

### Three ports, on purpose

The chart splits the surface across three listeners so that each one can be
exposed, or not, on its own terms:

| Port               | Variable              | Carries                                                             |
| ------------------ | --------------------- | ------------------------------------------------------------------- |
| `8080` (`http`)    | `MEMINI_HTTP_ADDR`    | REST `/v1`, MCP, probes. Bearer-gated when `MEMINI_API_KEY` is set. |
| `8081` (`ui`)      | `MEMINI_UI_ADDR`      | The admin UI. Its shell **embeds `MEMINI_API_KEY`**.                |
| `9090` (`metrics`) | `MEMINI_METRICS_ADDR` | `/metrics`, unauthenticated, kept off every route.                  |

That split is the point. Because the UI has its own port, the `http` port carries
no embedded credential and can be routed publicly (with `MEMINI_API_KEY` set).
The `ui` port must go to an internal gateway only: anyone who can load it can
read the admin key. See [web-ui.md](web-ui.md).

Two `HTTPRoute`s (Gateway API) ship disabled: `route.main` for the API on `http`,
`route.ui` for the UI on `ui`. Enable them and attach them to your Gateways.
**Set `MEMINI_API_KEY` before exposing `/v1` or `/mcp` off-cluster.** The chart
creates no secret; reference your own with `valueFrom.secretKeyRef`.

### Postgres backend

For scale-out, switch to Postgres:

1. `controllers.main.type: deployment` (no PVC, multiple replicas).
2. `persistence.data.enabled: false`.
3. `MEMINI_BACKEND: "postgres"` and wire `MEMINI_POSTGRES_DSN` from a Secret.
4. Optionally uncomment the `horizontalPodAutoscaler` block.

### ServiceMonitor

`serviceMonitor.main.enabled: false` by default. Flip it to scrape `/metrics` from
the dedicated `metrics` port. That port is unauthenticated and not routed, so the
scrape needs no bearer token.

If you instead serve `/metrics` on the main port (`MEMINI_METRICS_ADDR: ""`),
point the endpoint at `port: http` and, when `MEMINI_API_KEY` is set, add an
`authorization.credentials` secret reference: on the main port, the API key gates
`/metrics` too.

A Grafana dashboard ships under `grafanaDashboards` (disabled by default),
rendered as a ConfigMap for the grafana-operator.

### fsck CronJob

`fsck.enabled: false` by default, and it should usually stay that way: the
in-process sweeper already handles decay. Enable it only if you want an explicit,
scheduled `POST /v1/fsck` on top (hourly, by default).

If you do, the fsck container needs `MEMINI_API_KEY` in its own env, pointing at
the same Secret as the main container. It uses curl's `--variable` /
`--expand-header` so the token never appears in `argv`.
