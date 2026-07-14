# Glossary

memini's vocabulary is small, but several of its words are one letter away
from meaning something else. This page pins each term to exactly one meaning.
Docs, code comments, UI labels, and API descriptions follow these definitions;
if you find a use that doesn't, it's a bug worth a PR.

## The terms

- **namespace** — a node in the slash-separated tree memini partitions
  memories into (`acme/phoenix/api`, `acme/phoenix`, `acme`). The only
  partition key, and a server-side concept: the store filters rows by
  namespace string and knows nothing else. Hierarchy is purely lexical —
  `acme` is an ancestor of `acme/phoenix/api` because it is a path prefix,
  not because anything stores a parent pointer. See [Scopes](scopes.md).

- **project** — a client-side concept: the git repository (or directory) a
  session runs in, identified by its remote URL or toplevel path. A project
  is not a namespace; it _resolves to_ one, through the precedence chain
  pin > `MEMINI_NAMESPACE` env > declared > derived from the repo > key
  default > server default. The server never treats "project" as a synonym
  for namespace, and neither does the UI.

- **pin** — the stored project → namespace binding (`/v1/pins`, the
  `pin`/`unpin` activity events, `/memini:namespace` in the plugin). A pin
  is why the same repo lands in the same namespace on every machine. Not to
  be confused with a **pinned memory**, below.

- **pinned memory** — a memory tagged `pinned` so briefings always surface
  it (`/memini:pin` in the plugin). Same word, unrelated mechanism: a pin
  binds a project to a namespace; a pinned memory is content that refuses
  to be forgotten. Prefer the two-word form "pinned memory" in prose.

- **scope** — the per-call read width: `"project"` (primary namespace
  only), `"full"` (default: primary + ancestors + home + links), or
  `"everywhere"` (full, plus the primary's own subtree). REST additionally
  accepts the deprecated aliases `"exact"` (→ `"project"`) and `"subtree"`
  (→ `"everywhere"`). Scope means nothing else; the settings key that also
  contains the word is a blessed legacy exception, below.

- **visibility** — the per-call write placement: `"project"` (default,
  stays in the primary namespace), `"personal"` (routes to the caller's
  home), or an ancestor's name (moves a durable write up the chain).

- **read set** — the ordered set of namespaces a read actually searches,
  composed by the server from the request namespace plus the cascade. Each
  entry carries an **origin**: `primary`, `ancestor`, `home`, `link`, or
  `call` (an explicit per-call namespace).

- **source** — how the request's namespace itself was resolved: `pin`,
  `env`, `declared`, `remote`, `toplevel`, `cwd`, `key_default`, or
  `server_default`. Origin describes where a search result came from;
  source describes where the namespace came from. They never mix.

- **home** — the caller's personal namespace (`MEMINI_HOME`, or a per-key
  `--home` binding). The durable-only leg every default read set includes,
  and where `visibility:"personal"` writes land.

- **tier** and **category** — orthogonal axes covered by their own pages:
  [tiers](tiers.md) decide durability and whether a memory may cross a
  namespace boundary; [categories](categories.md) say what it's about.

## Why `scope:"project"` is not a contradiction

If "project" is a client-side word, why do the `scope` and `visibility`
enums use it? Because those values are read from the caller's seat. The MCP
surface deliberately hides raw namespaces from the model — an agent asks
for "just this project's memories" (`scope:"project"`) or writes "to this
project" (`visibility:"project"`), and the server translates that to the
one namespace the caller's project resolved to. The enum names a
relationship to the caller, not a server object, which is precisely the
client-side meaning of project. Renaming these wire values would break
every deployed client to close a gap that isn't there.

## The one legacy exception: `namespace_scope`

The per-key setting `namespace_scope` (`repo` | `owner_repo`) predates this
glossary and has nothing to do with read width — it selects the slug style
used when deriving a namespace from a git remote. It stays as-is: it is
persisted in per-key settings and operator-authored key files, and renaming
it would require accepting the old key forever anyway. Read it as
"namespace derivation style" wherever it appears.

## Retired: "tenant"

Earlier releases used "tenant" informally for a top-level namespace. The
word is retired: a top-level namespace is just a namespace whose path has
one segment, and the isolation story is told entirely by namespaces, tiers,
and the read set. "Tenant" legitimately survives only in historical
references to the removed `MEMINI_TENANT_SHARED` knob and its
`<tenant>/_shared` layout (see [Upgrading](operations/upgrading.md)), and
in external import formats that memini reads but doesn't own (the mnemory
exporter's `tenant` field, the `project` metadata key some importers emit —
foreign vocabulary, mapped to namespaces at the border).
