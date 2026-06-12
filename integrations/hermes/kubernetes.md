# Deploying the memini plugin to Hermes on Kubernetes

For Hermes running as a container (e.g. the bjw-s `app-template` HelmRelease),
the plugin has to land in `$HERMES_HOME/plugins/memini` inside the persistent
data volume. Use an initContainer that fetches it at rollout, plus three config
edits.

memini is reached over its in-cluster Service. Deployed in the `ai` namespace
that is `http://memini.ai.svc.cluster.local:8080`.

## 1. initContainer — install the plugin into the data volume

Add under `spec.values.controllers.hermes.initContainers`. It clones memini and
moves the plugin into the mounted data volume (`/opt/data` here = `HERMES_HOME`):

```yaml
install-memini-plugin:
  image:
    repository: docker.io/alpine/git
    tag: "2.47.1"
  command:
    - sh
    - -c
    - |
      set -e
      if [ ! -d /opt/data/plugins/memini ]; then
        mkdir -p /opt/data/plugins
        cd /tmp
        rm -rf memini-src
        git clone --depth 1 --branch main https://github.com/eleboucher/memini memini-src
        mv memini-src/integrations/hermes/plugin/memini /opt/data/plugins/memini
        rm -rf memini-src
      fi
  securityContext:
    allowPrivilegeEscalation: false
    capabilities:
      drop: ["ALL"]
    runAsGroup: 1000
    runAsUser: 1000
```

Pin `--branch` to a released tag (or a commit) rather than `main` for
reproducible rollouts. The plugin is stdlib-only Python, so no pip/npm step.

## 2. config.yaml — activate the provider

In the Hermes `config.yaml` (your `hermes-configmap`), set memini as the active
memory provider. Memory providers are single-select and activated via
`memory.provider` (not `plugins.enabled`) — switching providers means changing
this single value:

```yaml
memory:
  provider: memini
```

## 3. Container env — point at memini and authenticate

Set these on the `app` container (`spec.values.controllers.hermes.containers.app.env`).
Setting them as real container env vars (not just in the Hermes `.env`) means
they are in `os.environ` from process start, before the plugin loads:

```yaml
MEMINI_URL: http://memini.ai.svc.cluster.local:8080
MEMINI_NAMESPACE: hermes # share this string across agents to share memory
MEMINI_API_KEY: # memini in this cluster requires auth
  valueFrom:
    secretKeyRef:
      name: hermes-config
      key: MEMINI_API_KEY
```

memini requires a bearer token, so pull `MEMINI_API_KEY` into the Hermes secret.
With an ExternalSecret (1Password) backing `hermes-config`, add the memini key:

```yaml
# in the Hermes ExternalSecret's spec.data
- secretKey: MEMINI_API_KEY
  remoteRef:
    key: memini # the 1Password item memini already uses
    property: MEMINI_API_KEY
```

> The plugin warns once on stderr when sending a bearer token over plaintext
> `http://` to a non-loopback host (cluster-internal traffic). That is expected
> in-cluster; leave `MEMINI_REQUIRE_HTTPS` unset. Set it to `1` only if you
> terminate TLS in front of memini.

## Verify

After Flux reconciles and the pod restarts:

```bash
kubectl -n ai exec deploy/hermes -c app -- ls /opt/data/plugins/memini
# -> __init__.py  plugin.yaml
```

Then check the Hermes logs for the plugin loading and, on the next turn, recalled
memini context appearing in the prompt. New turns are written back to memini
under the `hermes` namespace.
