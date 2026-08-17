# Production hardening

What changes when memini stops being a toy on your laptop and starts holding a team's memory. [deployment.md](deployment.md) gets it running; this page is about running it seriously.

## TLS and reverse proxies

memini serves plain HTTP. Terminate TLS in front of it — and do it from day one, because the clients enforce it from their side too.

**The client-side guard:** every bundled client (the Claude Code/Codex hooks, the MCP header helper, the other integrations) checks whether a bearer token is about to be sent over plaintext HTTP to a non-loopback host. By default that only warns; with `MEMINI_REQUIRE_HTTPS=1` set in the client's environment it is a hard refusal — the hook or MCP request throws instead of sending the key. Enabling the guard is the recommended posture for any remote server, which means a "TLS later" proxy quietly breaks every remote client the day someone turns the guard on. Loopback (`localhost`, SSH tunnels landing on `127.0.0.1`) is always allowed.

Caddy, which provisions certificates itself, is the short version:

```
memini.example.com {
	reverse_proxy localhost:8080
}
```

nginx needs the certificate wired up, and one non-default: `/mcp` is a long-lived SSE stream, so buffering must be off and the read timeout generous.

```nginx
server {
    listen 443 ssl;
    server_name memini.example.com;

    ssl_certificate     /etc/nginx/certs/memini.example.com.pem;
    ssl_certificate_key /etc/nginx/certs/memini.example.com.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_http_version 1.1;

        # /mcp is long-lived SSE: never buffer it, and outlive idle stretches.
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

Align timeouts while you are here: the server bounds a single `/v1` REST request at `MEMINI_REQUEST_TIMEOUT` (default 60s; it never applies to `/mcp`, `/healthz`, `/readyz`, or `/metrics`). A proxy timeout below it turns slow but legitimate requests — `POST /v1/answer` riding a long LLM call — into proxy 502s that the server-side logs will not explain. See [`MEMINI_REQUEST_TIMEOUT`](../reference/configuration.md#memini_request_timeout).

If you route the admin UI, keep it on a trusted (internal) hostname even though the shell is credential-free — the signed-in SPA keeps its token in `localStorage`. See [web-ui.md](web-ui.md).

## Resource sizing

Starting points, not gospel — measure your own workload via `/metrics`.

memini itself is a small static Go binary. Baseline memory is tens of megabytes; what grows it is the SQLite page cache and WAL on the sqlite backend, or the client connection pool on Postgres. CPU is bursty: embedding calls are remote, so the server's own work is parsing, SQL, and ranking.

The heavy part of a memini deployment is usually not memini. A co-resident embeddings service (text-embeddings-inference on CPU, llama.cpp, vLLM) costs more memory and CPU than the server it feeds. Size that separately and first.

Conservative starting numbers for the memini container on Kubernetes:

| Resource | Request | Limit   |
| -------- | ------- | ------- |
| CPU      | `100m`  | `500m`  |
| Memory   | `128Mi` | `512Mi` |

Raise the memory numbers if you run a large sqlite store with heavy recall traffic, or a big Postgres pool. There is no memory limit tuning inside memini itself; it uses what the workload demands.

## Postgres operations

The Postgres backend targets Postgres with the **VectorChord** extension. At first boot the server runs its own migration, including `CREATE EXTENSION IF NOT EXISTS vchord CASCADE` — so the database image must ship VectorChord (compose and CI use `ghcr.io/tensorchord/vchord-postgres`), and the role in the DSN needs privileges to create the extension on first boot against a fresh database.

```sh
MEMINI_BACKEND=postgres
MEMINI_POSTGRES_DSN="postgres://memini:secret@db.internal:5432/memini?sslmode=require&pool_max_conns=10"
```

**Pool sizing lives in the DSN.** The server builds its pool with pgx's `pgxpool`, which reads pool parameters from the connection string: `pool_max_conns`, `pool_min_conns`, `pool_max_conn_lifetime`, and friends. There is no separate environment variable. When you scale replicas (`controllers.main.type: deployment`), remember the total connection count is `replicas × pool_max_conns` — budget it against the database's `max_connections`.

**Vector indexes** are `vchordrq` indexes on the memory and chunk embedding columns, created by the migration with `CREATE INDEX IF NOT EXISTS`. On a store restored or bulk-imported into a fresh database, the first boot (or the `pg_restore` itself) pays the index build; on a large corpus that is the slow step, not the data copy.

**What CI actually tests:** the sqlite backend runs in every `go test ./...`. The Postgres backend runs behind the `integration` build tag — a store conformance suite (`internal/store/postgres`) and the end-to-end server tests (`cmd/memini`) — against `ghcr.io/tensorchord/vchord-postgres` (currently pg18), enabled by `MEMINI_TEST_POSTGRES_DSN`. Plain Postgres with only pgvector, or other vector extensions, is not a tested combination.

## API keys: rotation means restart

`MEMINI_API_KEYS_FILE` is read **once, at boot**, with fail-loud validation (a malformed file refuses the boot, naming the offending entry). There is no live reload and no SIGHUP handler: editing the file on disk changes nothing until the process restarts. That is deliberate — the file is the GitOps-shaped source of truth, and a config change rolling the Deployment (or `systemctl restart memini`) is the apply step.

Consequences worth planning around:

- Rotating or revoking a file key = update the file (Secret), then restart. Until the restart, the old secret still authenticates.
- A file key that shares a name with a database key **shadows** it at auth time; the server logs a warning at boot listing shadowed keys.
- Keys created at runtime via `memini key` / the REST API live in the store, not the file, and need no restart — but they are not in Git either.

The full rotation ceremony (overlap window, client rollout, revocation) is in [access-control.md](../guides/access-control.md#step-5-the-rotation-ceremony).

## Metrics topology

The Helm chart lays the surface out across three ports; the same split applies to any deployment shape:

| Port   | Listener              | Auth                                                     |
| ------ | --------------------- | -------------------------------------------------------- |
| `8080` | `MEMINI_HTTP_ADDR`    | REST `/v1` + MCP, bearer-gated when `MEMINI_API_KEY` set |
| `8081` | `MEMINI_UI_ADDR`      | Admin UI shell (credential-free; SPA signs in itself)    |
| `9090` | `MEMINI_METRICS_ADDR` | `/metrics`, **unauthenticated by design**                |

The `/metrics` semantics follow from where it is served:

- On a **dedicated port** (`MEMINI_METRICS_ADDR: ":9090"`), it is unauthenticated on purpose: the port is meant to stay in-cluster or behind a firewall, attached to no route or ingress, so the scraper needs no bearer token. Exposing that port publicly exposes your metrics — do not route it.
- On the **main port** (`MEMINI_METRICS_ADDR: ""`), `/metrics` sits behind the same bearer gate as everything else: when `MEMINI_API_KEY` is set, the scraper must send it.

The chart's ServiceMonitor wiring for both cases is in [deployment.md](deployment.md#servicemonitor); the reasoning behind the metrics/healthz asymmetry is in [api-keys.md](../api-keys.md#the-metricshealthz-asymmetry).

## Related

- [backup-restore.md](backup-restore.md) — before it matters.
- [upgrading.md](upgrading.md) — what breaks between versions.
