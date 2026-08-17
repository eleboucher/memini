# Deployment

memini is a single static binary with two storage backends and one strongly
recommended external dependency: an OpenAI-compatible **embeddings endpoint**.
Without one the server still runs, but degraded: writes are stored
keyword-searchable only (flagged `pending_embed` and re-embedded automatically
once an endpoint appears) and recall falls back to keyword-only search, which
retrieves noticeably worse. Everything else (LLM consolidation, reranking) is
optional.

Pick a shape:

- **Compose**, for a laptop or a single box. Postgres backend, everything wired.
- **Single container**, for the smallest useful server. SQLite backend.
- **Kubernetes**, via the bundled Helm chart. SQLite by default, Postgres for
  scale-out.
- **Bare metal (systemd)**, for a VM or home server with no container runtime.
  The release binary is fully static.

Environment variables are not restated here. See
[configuration.md](../reference/configuration.md).

## Prebuilt artifacts

Every release tag (`vX.Y.Z`) publishes the same set of artifacts from
[`release.yml`](../../.forgejo/workflows/release.yml). Examples below use
`0.7.18`; substitute the release you want.

**Container images**, multi-arch (`linux/amd64`, `linux/arm64`), pushed to two
registries with plain-semver tags (no `v` prefix):

| Image                                           | Release tags    |
| ----------------------------------------------- | --------------- |
| `registry.erwanleboucher.dev/eleboucher/memini` | `0.7.18`, `0.7` |
| `git.erwanleboucher.dev/eleboucher/memini`      | `0.7.18`, `0.7` |

Pushes to `main` additionally publish the moving tags `latest` and
`sha-<full commit>` — to `git.erwanleboucher.dev/eleboucher/memini` only. Pin a
release tag (or better, a digest) for anything you care about.

**The Helm chart**, as an OCI artifact on the same two registries. The chart
version keeps the `v` prefix (it is the git tag), unlike the image tags:

```sh
helm install memini \
  oci://registry.erwanleboucher.dev/eleboucher/charts/memini \
  --version v0.7.18 -f my-values.yaml
```

A released chart has the exact image **digest** of that release's build pinned
into its values, so it never depends on tag resolution. A chart installed from
a git checkout does not — see the callout in the Kubernetes section.

**Release tarballs**, attached to the release at
`https://git.erwanleboucher.dev/eleboucher/memini/releases`:
`memini_0.7.18_linux_amd64.tar.gz` and `memini_0.7.18_linux_arm64.tar.gz`, each
holding the static `memini` binary plus LICENSE and README. The binaries are
extracted from the same build that produced the container image, not
recompiled. Next to them sit `memini_0.7.18_checksums.txt` and its cosign
bundle `memini_0.7.18_checksums.txt.cosign.bundle`.

### Verifying signatures

Signing is **key-based** cosign (`cosign sign --key`), not keyless: you verify
against the project's public key, not an OIDC identity. Obtain the public key
out of band (the project page) and keep it as `cosign.pub`; trusting these
signatures means trusting that key.

```sh
# Container image (either registry; images are signed by digest)
cosign verify --key cosign.pub \
  registry.erwanleboucher.dev/eleboucher/memini:0.7.18

# Helm chart (also a signed OCI artifact; note the v-prefixed chart tag)
cosign verify --key cosign.pub \
  registry.erwanleboucher.dev/eleboucher/charts/memini:v0.7.18

# Tarballs: verify the checksums file's signature, then the checksums
cosign verify-blob --key cosign.pub \
  --bundle memini_0.7.18_checksums.txt.cosign.bundle \
  memini_0.7.18_checksums.txt
sha256sum -c memini_0.7.18_checksums.txt
```

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

Instead of building locally, any prebuilt image from
[Prebuilt artifacts](#prebuilt-artifacts) works in its place.

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

> **Installing from a git checkout renders an image that does not exist.** In
> the committed `values.yaml`, both `image.tag` and `image.digest` are empty
> strings — the release pipeline injects the image digest when it packages the
> chart, and only then. From a checkout, the empty tag falls back to the
> chart's `appVersion`, a `0.1.0` placeholder that was never published, so the
> pod tries to pull `registry.erwanleboucher.dev/eleboucher/memini:0.1.0` and
> sits in `ImagePullBackOff` (`manifest unknown`). Two fixes:
>
> - Install the released OCI chart instead (see
>   [Prebuilt artifacts](#prebuilt-artifacts)); it pins that release's image by
>   digest.
> - Keep the checkout, but set a real tag:
>   `--set controllers.main.containers.main.image.tag=0.7.18`.

### Defaults

- A **StatefulSet**, one replica, with a 5Gi `ReadWriteOnce` PVC mounted at
  `/data` (SQLite is single-writer, so do not scale it).
- Non-root (uid `65532`), read-only root filesystem, all capabilities dropped.
- Liveness on `/healthz`, readiness on `/readyz`.

### Three ports, on purpose

The chart splits the surface across three listeners so that each one can be
exposed, or not, on its own terms:

| Port               | Variable              | Carries                                                              |
| ------------------ | --------------------- | -------------------------------------------------------------------- |
| `8080` (`http`)    | `MEMINI_HTTP_ADDR`    | REST `/v1`, MCP, probes. Bearer-gated when `MEMINI_API_KEY` is set.  |
| `8081` (`ui`)      | `MEMINI_UI_ADDR`      | The admin UI (a credential-free shell; the SPA signs in in-browser). |
| `9090` (`metrics`) | `MEMINI_METRICS_ADDR` | `/metrics`, unauthenticated, kept off every route.                   |

The UI shell carries no credential (it never contains `MEMINI_API_KEY`; the SPA
signs in against `/v1/self` in the browser and keeps its token in
`localStorage`). A dedicated `ui` port is still worth keeping internal, because a
stored token is readable by any same-origin script, but it is no longer the
key-in-public-HTML hazard it once was. See [web-ui.md](web-ui.md).

Two `HTTPRoute`s (Gateway API) ship disabled: `route.main` for the API on `http`,
`route.ui` for the UI on `ui`. Enable them and attach them to your Gateways.
**Set `MEMINI_API_KEY` before exposing `/v1` or `/mcp` off-cluster.** The chart
creates no secret; reference your own with `valueFrom.secretKeyRef`.

`MEMINI_API_KEY` is the break-glass admin credential. For per-person and
per-agent keys (the recommended shape on a shared server), mount a
`MEMINI_API_KEYS_FILE` from a Secret. `values.yaml` carries a commented
`persistence` example that mounts one at `/etc/memini/api-keys.yaml`; the
[access control guide](../guides/access-control.md) walks the full file (one
admin key per human, non-admin keys per agent) and the Secret it comes from.

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

The token it uses must **not** be read-only: `POST /v1/fsck` is a mutating
maintenance call, so a read-only credential gets a `403` and the CronJob fails
every run. See [api-keys.md](../api-keys.md#the-read-only-attribute).

If you do, the fsck container needs `MEMINI_API_KEY` in its own env, pointing at
the same Secret as the main container. It uses curl's `--variable` /
`--expand-header` so the token never appears in `argv`.

## Bare metal (systemd)

The tarball binary is fully static — no libc, no runtime dependencies. Unpack
it, install it, and write one unit file:

```sh
tar -xzf memini_0.7.18_linux_amd64.tar.gz
sudo install -m 0755 memini_0.7.18_linux_amd64/memini /usr/local/bin/memini
```

**The trap to know about first:** `MEMINI_SQLITE_PATH` defaults to `memini.db`
**relative to the working directory**. Under systemd, an unconfigured service
starts in `/`, so the default either fails to create the database (read-only
`/`) or, with a permissive setup, drops it somewhere you will not look. Always
set an absolute path under `/var/lib/memini`, and a matching
`WorkingDirectory` for defense in depth.

`/etc/systemd/system/memini.service`:

```ini
[Unit]
Description=memini memory server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/memini
DynamicUser=yes
StateDirectory=memini
WorkingDirectory=/var/lib/memini
Environment=MEMINI_SQLITE_PATH=/var/lib/memini/memini.db
EnvironmentFile=-/etc/memini/env
Restart=on-failure
RestartSec=2

# Hardening. DynamicUser already implies most of ProtectSystem=strict;
# stating these keeps the unit safe if you switch to a static user later.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

Choices worth understanding:

- **`DynamicUser` + `StateDirectory`**: systemd allocates a throwaway uid and
  creates `/var/lib/memini` owned by it — no user management, and the rest of
  the filesystem stays read-only to the service. If backup tooling outside
  systemd needs stable file ownership, use a dedicated account instead:
  `User=memini` with `useradd --system --home /var/lib/memini memini`, keeping
  `StateDirectory=memini`.
- **`EnvironmentFile` for secrets**: put `MEMINI_API_KEY=...`,
  `MEMINI_EMBED_BASE_URL=...` and friends in `/etc/memini/env`, root-owned mode
  `0600`, out of the unit file (unit files are world-readable). The leading `-`
  makes the file optional so first boot works before you create it.
- **`Restart=on-failure`**: memini exits non-zero on fatal misconfiguration
  (removed variables, embedding-model mismatch); a restart loop on those is
  noise, but transient crashes should come back. Check
  `journalctl -u memini` before assuming the loop is transient.

Then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now memini
curl -s localhost:8080/healthz
```

## Going further

- [production.md](production.md) — TLS and reverse proxies, resource sizing,
  Postgres operations, API-key rotation, and how to expose (or not expose)
  `/metrics`.
- [backup-restore.md](backup-restore.md) — backing up each backend, PVC
  snapshots, the portable export path, and the invariants a restore must
  respect (embedding model and dimensionality).
