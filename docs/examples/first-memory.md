# First memory

From zero to a remembered fact: bring a server up, save one memory over REST, and watch the namespace materialize and recall find it. Nothing in this walkthrough needs the plugin — it is the raw API doing exactly what the plugin automates.

The shell steps in this section are illustrative deploy steps (they depend on your machine); every request/response shape from the first `POST` onward is pinned by the test named at the bottom, trimmed to the asserted fields with ids marked illustrative.

## Bring a server up

The quickest complete stack is the bundled compose file — Postgres with VectorChord, a CPU embeddings service, and memini wired to both:

```sh
docker compose up --build
```

Or the smallest useful server, a single SQLite-backed container — see [deployment](../operations/deployment.md) for both shapes and the traps (volume, uid, embedding dims). Either way, you know it is alive when:

```sh
curl -s localhost:8080/healthz
# ok
```

The compose file ships with auth off (dev mode). If you set `MEMINI_API_KEY`, add `-H "Authorization: Bearer $MEMINI_API_KEY"` to every request below.

## The first write

Save a decision. Two things are deliberately absent: no tier — the server classifies it — and no setup for the namespace, because there is no such thing. The namespace rides in on a header:

```sh
curl -s localhost:8080/v1/memories \
  -H 'X-Memini-Namespace: acme/checkout' \
  -H 'Content-Type: application/json' \
  -d '{
    "content": "We decided to use Stripe webhooks instead of polling for payment status, the reason is polling kept tripping the rate limits."
  }'
```

```json
201 Created
{
  "id": "01J8",                        // illustrative
  "namespace": "acme/checkout",
  "tier": "semantic",
  "metadata": { "tier_classified": "marker" }
}
```

Three things happened in that one call:

- **The tier was classified, not defaulted.** The content is a short, unhedged decision ("we decided", "instead of", "the reason is"), so the marker heuristics raised it to `semantic` — a durable fact that never expires — and stamped `metadata.tier_classified` so the call is auditable. Vague chatter would have landed in the working tier instead. See [the write path](../how-it-works/write-path.md).
- **The namespace came from the `X-Memini-Namespace` header**, echoed back in the response. The plugin derives this for you per project; raw REST callers state it per request.
- **The id in the response is the handle** for everything later: update, history, supersede, forget.

## The namespace now exists — because the memory does

Namespaces are never created explicitly. There is no "create namespace" endpoint to have called first; a namespace exists exactly while at least one memory carries the name. Before the write, the listing was empty:

```sh
curl -s localhost:8080/v1/namespaces
```

```json
{ "namespaces": [] }
```

After it:

```json
{ "namespaces": ["acme/checkout"] }
```

That single write materialized `acme/checkout`. Delete the namespace's last memory and it vanishes from this list again, just as implicitly. How this plays with hierarchies, ancestors, and the plugin's namespace derivation is covered in [namespaces](../how-it-works/namespaces.md).

## Recall it

```sh
curl -s localhost:8080/v1/search \
  -H 'X-Memini-Namespace: acme/checkout' \
  -H 'Content-Type: application/json' \
  -d '{ "query": "webhooks or polling for payment status", "limit": 5 }'
```

```json
{
  "results": [
    {
      "memory": {
        "id": "01J8",
        "content": "We decided to use Stripe webhooks instead of polling for payment status, the reason is polling kept tripping the rate limits."
      },
      "score": 0.93 // illustrative
    }
  ]
}
```

The fact comes back, ranked with a score. That is the entire loop: write, materialize, recall — one memory, one namespace, no ceremony.

## Next: stop doing this by hand

Everything above is what the plugin does automatically in every session: it derives the namespace from your project, captures and saves what matters, and pulls relevant memories back in at session start and per prompt. Install it once and the loop runs itself — see the [plugin README](../../plugin/README.md). For where a second project, a team ancestor, or a personal home namespace fit, continue with [namespace lifecycle](namespace-lifecycle.md).

## Validated by

- `TestExampleFirstMemory` in `internal/api/rest/example_first_memory_test.go` — the empty listing, the 201 shape (id, auto-classified tier, namespace), the materialized namespace, and the search hit, in that order against the real REST handlers. The deploy shell steps above are illustrative and not covered by the test.
