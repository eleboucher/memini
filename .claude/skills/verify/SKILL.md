---
name: verify
description: Launch and drive memini (server + Claude Code plugin hooks) for end-to-end verification of recall/hook changes
---

# Verifying memini changes end-to-end

## Build & launch the server

```bash
env -u GOROOT go build -o /tmp/memini-e2e ./cmd/memini
```

The binary serves on the ROOT command (no `serve` subcommand). Boot it with `env -i PATH HOME ...` — the workstation shell exports production `MEMINI_*` vars (API key, sqlite path) that silently enable auth or point at the wrong DB. Minimal boot needs an OpenAI-compatible embeddings endpoint:

```bash
env -i PATH="$PATH" HOME="$HOME" \
  MEMINI_HTTP_ADDR=:18202 MEMINI_METRICS_ADDR=:18203 MEMINI_UI_ENABLED=false \
  MEMINI_SQLITE_PATH=/tmp/e2e.db \
  MEMINI_EMBED_BASE_URL=http://127.0.0.1:18201/v1 MEMINI_EMBED_MODEL=stub MEMINI_EMBED_DIMS=8 \
  /tmp/memini-e2e
```

A ~40-line node stub serving `/v1/embeddings` (deterministic 8-dim vectors) and Cohere-style `/v1/rerank` (`{results:[{index,relevance_score}]}`) covers both backends. Wait on `curl -sf :18202/healthz`.

## Driving the plugin hooks

Pipe the hook payload JSON to `plugin/scripts/run.sh <script>.mjs` with `MEMINI_BASE_URL` + isolated `XDG_CACHE_HOME`/`XDG_CONFIG_HOME`, from a scratch **git repo** cwd (namespace derives from it).

**Gotcha: the handshake cache is keyed by the hook process's ppid.** SessionStart writes it; the hot-path hooks (pre-tool-use, user-prompt-submit) read it cache-only and silently skip on a miss. Every Bash tool call is a fresh shell (fresh pid), so all hook invocations for one scenario must run from ONE driver script (the script's bash is the shared parent). Cache lands at `$XDG_CACHE_HOME/memini/sessions/pid-<ppid>.handshake.json`.

**Gotcha: even inside one driver script, `$(...)` command substitution forks a subshell with a NEW pid**, so `out=$(echo "$payload" | run.sh hook.mjs)` breaks the ppid keying. Invoke hooks as direct pipelines redirecting stdout to files (`echo "$payload" | run.sh hook.mjs > /tmp/out.json`) so every hook shares the driver's pid.

**Gotcha: the HOOKS inherit ambient production `MEMINI_*` too, not just the server.** A leaked `MEMINI_API_KEY` 401s the handshake (hooks then silently no-op) and `MEMINI_NAMESPACE_PREFIX` rewrites the namespace. Strip them at the top of every driver script (`unset` every `MEMINI_*` var, then set only what the scenario needs).

**Gotcha: the server wraps the embedder in an LRU** (`embed.NewCached`, cmd/memini/root.go). To demonstrate degraded (keyword-only) recall by killing the embedder, use a query the server has never embedded — a repeated query serves from cache and is genuinely healthy.

## Useful surfaces

- Seed: `curl -X POST -H "X-Memini-Namespace: <ns>" -d '{"content":"...","tier":"semantic"}' :18202/v1/memories`
- Search: `POST /v1/search {"query":"..."}` (check `degraded`/`note` fields)
- Activity: `GET /v1/activity?limit=50` (per-recall `source` values)
- Boot-validation checks: run the binary with the misconfiguration and assert exit code + `fatal:` stderr.
