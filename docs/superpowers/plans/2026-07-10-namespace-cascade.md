# Namespace Cascade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. At execution start: create branch `namespace-cascade` in `/home/perf3ct/repos/memini`, and copy this plan to `docs/superpowers/plans/2026-07-10-namespace-cascade.md` (the spec is already at `docs/superpowers/specs/2026-07-10-namespace-cascade-design.md`).

**Goal:** Replace memini's four fragmented scope mechanisms (global namespace, tenant `_shared`, subtree, explicit lists) with one always-on ancestor cascade + per-person home namespace + explicit links, with an LLM surface that never exposes raw paths.

**Architecture:** A write lands in exactly one namespace (unchanged). A read in `N` resolves: primary (all tiers) → ancestors nearest-first (durable-only) → home from `X-Memini-Home` (durable-only) → stored links, one hop (per-link tiers). The LLM chooses `visibility` (project | personal | ancestor-name) on write and `scope` (project | full | everywhere) on read; provenance annotations teach the topology in-context.

**Tech stack:** Go (chi + oapi-codegen, cobra, sqlite-vec + Postgres/VectorChord), MCP SDK, Node plugin scripts (`node --test`), mise tasks.

## Context

memini's namespaces are n-depth slash-paths but scope semantics are depth-1 and spread across `MEMINI_GLOBAL_NAMESPACE`, `MEMINI_TENANT_SHARED` + a `_shared` naming convention, `scope=subtree`, and per-call `namespaces`. Live-instance evidence (shared team + personal instance): users organically write shared facts to interior nodes (not `_shared`), real trees go 3 deep, per-person episodic digests pollute shared roots, and operators store procedural memories *teaching agents where to write* — the write-side surface has failed. Design decisions (spec §Goals, all user-approved): break-it-now compat, scoping not authz, briefing-only downward visibility, ancestor-name visibility vocabulary.

## Global constraints

- Branch: `namespace-cascade` in `/home/perf3ct/repos/memini`. Frequent small commits (`feat:`/`test:`/`docs:` prefixes).
- TDD: every task starts with a failing test; run `mise run test` (unit), `mise run test-integration` (Postgres/testcontainers), `mise run test-hooks` (plugin JS).
- **Docs/examples must use only fictional namespaces** (`acme/phoenix/api`, `acme/phoenix`, `acme`, `personal/kit`, `shared/golang`) — never real data from the user's live instance.
- Deleted knobs: `MEMINI_GLOBAL_NAMESPACE`, `MEMINI_TENANT_SHARED` (server refuses boot, T12). New knob: `MEMINI_HOME` (client-side). Header: `X-Memini-Home`.
- Tier rule everywhere: only durable tiers (semantic, procedural) cross namespace boundaries; episodic/working never do, in either direction.
- OpenAPI codegen order: edit `api/openapi.yaml` → `go generate ./...` → implement handlers → `mise run ui` regenerates `ui/src/api-schema.gen.ts`. Batch all spec edits into T6's single regen.

## File map (owner → responsibility)

| Layer | Files | Owns |
|---|---|---|
| Store | `internal/store/store.go`, `sqlitevec/store.go`, `postgres/store.go`, `storetest/conformance.go` | `namespace_links` persistence only; no scope logic |
| Service (authoritative) | `internal/service/readset.go`, `service.go`, `query.go` | cascade + home + link resolution, visibility mapping, provenance, briefing rollup |
| API | `internal/api/rest/middleware.go`, `rest.go`, `api/openapi.yaml`, `internal/api/mcp/mcp.go` | header extraction (`X-Memini-Namespace`, `X-Memini-Home`), LLM tool surface, REST parity |
| Client | `plugin/scripts/_shared.mjs`, `plugin/scripts/mcp-headers.mjs`, `packages/namespace-resolver/src/index.ts`, `integrations/*` | resolving namespace + home and sending headers; never scope logic |
| CLI | `cmd/memini/link.go` (new), `migrate.go` (new), `doctor.go` | operator surface; doctor mirrors service resolution |
| Docs | `docs/scopes.md` (new), `README.md` | knobs, data flow, ownership, examples |

---

### T1 — `namespace_links` store layer

**Files:** Modify `internal/store/store.go`, `internal/store/sqlitevec/store.go` (migrate, ~L119), `internal/store/postgres/store.go` (migrate, ~L126); Test `internal/store/storetest/conformance.go`.

**Produces:**
```go
type NamespaceLink struct {
    Src, Dst  string
    Tiers     []memory.Tier // nil = durable default applied by service
    Note      string
    CreatedAt time.Time
}
// Optional capability interface, EmbedModelStore precedent (store.go:175)
type LinkStore interface {
    PutLink(ctx context.Context, l NamespaceLink) error            // upsert on (src,dst)
    DeleteLink(ctx context.Context, src, dst string) (bool, error)
    ListLinks(ctx context.Context, src string) ([]NamespaceLink, error)
    ListAllLinks(ctx context.Context) ([]NamespaceLink, error)     // CLI/UI
}
```

- [ ] Add `testNamespaceLinks(t, st, dims)` to conformance.go (put/list round-trip incl. tiers + note; upsert overwrites; delete returns existed-bool; empty src → empty list; type-assert `st.(store.LinkStore)`, `t.Skip` if absent). Register `t.Run("NamespaceLinks", ...)` in `Run` (~L49). Run `mise run test` → FAIL (interface unimplemented → skip fires; then real FAIL once methods stubbed).
- [ ] DDL, appended to both backends' `stmts` (idempotent, both migrations run on every Open):
```sql
CREATE TABLE IF NOT EXISTS namespace_links (
  src_ns TEXT NOT NULL, dst_ns TEXT NOT NULL,
  tiers TEXT NOT NULL DEFAULT '[]',   -- JSON array; jsonb in postgres
  note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,    -- TEXT in sqlite, RFC3339
  PRIMARY KEY (src_ns, dst_ns)
)
```
- [ ] Implement all four methods on both backends (sqlite: `INSERT ... ON CONFLICT(src_ns,dst_ns) DO UPDATE`; postgres same). `mise run test` PASS; `mise run test-integration` PASS. Commit.
- [ ] **Link lifecycle integrity** (gap G5): `DeleteNamespace` (sqlitevec/store.go:618, postgres equivalent) also deletes `namespace_links` rows where `src_ns` OR `dst_ns` matches; `Reassign`-based namespace moves rewrite link endpoints — add `RenameLinkEndpoints(ctx, from, to string) error` to `LinkStore`, called by `maintenance.Move` (renamespace.go:44) after the memory move. Conformance tests for both. Commit.

### T2 — Cascade read-set (core)

**Files:** Modify `internal/service/readset.go`, `internal/service/service.go`, `internal/service/query.go`, `cmd/memini/root.go` (drop option wiring L262-263); Test `internal/service/readset_test.go`.

**Consumes:** `store.LinkStore` (T1). **Produces:** `ancestorsOf(ns string) []string`; `readScope{primary, home string; explicit []string; subtree, bare bool; reqTiers []memory.Tier}`; `RecallInput.Home/.Scope string`; `BriefingOpts.Home/.Scope string`.

- [ ] Rewrite `readset_test.go` tables (same commit as impl — old global/tenant cases die): flat ns → primary only; depth-3 nearest-first order (`acme/phoenix/api` → `acme/phoenix` → `acme`); home present/absent; home == primary or ancestor (existing `addEntry` widest-tiers merge, readset.go:101); link leg with tier override; `bare` skips all legs; non-durable tier filter skips legs (existing `durableTiers` gate L113); clamp keeps home (`promoteProtected`) and near ancestors; explicit path unchanged; **explicit `namespaces` beats `scope`** (precedence test, gap G8: explicit replaces cascade even with `scope:"everywhere"`).
- [ ] Note for executor (verified, do NOT "fix"): cross-namespace reinforcement already works — `reinforce` groups by `r.Memory.Namespace` (service.go:2433-2452), so ancestor/home/link hits slide their own TTLs. No change needed.
- [ ] Implement in `readset.go`:
```go
// ancestorsOf returns every proper path prefix of ns, nearest first:
// "acme/phoenix/api" -> ["acme/phoenix", "acme"]. Nearest-first ordering is
// load-bearing: FuseScores breaks ties first-seen (search/fusion.go:53-68).
func ancestorsOf(ns string) []string {
    var out []string
    for i := strings.LastIndexByte(ns, '/'); i > 0; i = strings.LastIndexByte(ns[:i], '/') {
        out = append(out, ns[:i])
    }
    return out
}
```
  In `resolveDefaultReadSet`: replace global+tenant legs (L121-135) with ancestors → home → links (one `ListLinks(ctx, sc.primary)` behind `s.store.(store.LinkStore)` assert; per-link tiers intersected with `gt`), then `promoteProtected(entries, sc.home)`. Delete `tenantSharedNamespace`, `tenantSharedLeaf`, `Service.globalNamespace/.tenantShared`, `WithGlobalNamespace`, `WithTenantShared` (service.go:670/679). Config fields stay unread until T12.
- [ ] Thread `Home`/`Scope` into `readScope` at `Recall` (service.go:1725) and `Briefing` (query.go:236). Scope mapping: `"project"`→`bare:true`; `""`/`"full"`→default; `"everywhere"`→`subtree:true`. Answer needs no change (answer.go:59 inherits via RecallInput).
- [ ] `mise run test` PASS. Commit.

### T3 — `X-Memini-Home` header plumbing

**Files:** Modify `internal/api/rest/middleware.go`, `internal/api/rest/rest.go`, `internal/api/mcp/mcp.go` (L230-254); Test `internal/api/rest/rest_test.go`.

- [ ] Test via extended `do()` helper (optional home header): durable memory written to `personal/kit` surfaces in recall from `acme/phoenix` with `X-Memini-Home: personal/kit`; absent header → no home leg.
- [ ] `homeMiddleware` cloned from `namespaceMiddleware` (L42-63): normalize+validate, **no default when absent**; `homeFromContext` helper. MCP handler captures the header per-request alongside namespace. All read/write handlers pass home into service inputs. Commit.
- [ ] **Answer path home threading** (gap G1, verified): `AnswerInput` (answer.go:59) gains `Home string`; thread into every `RecallInput{...}` construction site — `answer.go:104`, `answer_expand.go:113`, `answer_loop.go:135`, `answer_loop.go:230`, `answer_loop.go:248` — plus the REST `/v1/answer` handler and MCP `memory_answer`. Test: answer citation sourced from home ns. Commit.
- [ ] **stdio MCP has no headers** (gap G2): `memini mcp` (local stdio server) resolves home from `MEMINI_HOME` env the same way it resolves the default namespace from config — headers exist only on the HTTP path. Test in the stdio server setup. Commit.

### T4 — Write-side `visibility`

**Files:** Modify `internal/service/service.go` (`Remember`, ~L938); Test service-level.

**Produces:** `RememberInput.Visibility/.Home string`; `resolveVisibility(in RememberInput, tier memory.Tier) (string, error)`.

- [ ] Tests: default `"project"`→primary; `"personal"`+home→home ns; `"personal"` no home→error naming `MEMINI_HOME`; ancestor by full path (`"acme/phoenix"`) and by unambiguous last segment (`"acme"`); ambiguous segment→error listing the chain; unknown→error listing the chain; **tier clamp:** episodic/working always→primary regardless of visibility.
- [ ] Implementation sketch:
```go
func resolveVisibility(in RememberInput, tier memory.Tier) (string, error) {
    v := strings.TrimSpace(in.Visibility)
    if v == "" || v == "project" { return in.Namespace, nil }
    if !tier.Durable() { return in.Namespace, nil } // clamp: episodic/working never travel
    if v == "personal" {
        if in.Home == "" { return "", invalidInputf("visibility \"personal\" requires a home namespace (set MEMINI_HOME on the client)") }
        return in.Home, nil
    }
    chain := ancestorsOf(in.Namespace)
    var matches []string
    for _, a := range chain {
        if a == v || strings.HasSuffix(a, "/"+v) || a == v { matches = append(matches, a) }
        if lastSegment(a) == v { matches = append(matches, a) } // dedupe exact-vs-segment double match
    }
    // exact full-path match wins outright; else exactly one segment match required.
    // error messages enumerate: valid targets: project, personal, acme/phoenix, acme
}
```
  (Executor: dedupe matches; exact path beats segment; keep error format `visibility %q not in scope; valid: project, personal, <chain...>` — the error is the LLM's teacher.)
- [ ] **Write pipeline follows the target namespace** (gap G4): after `resolveVisibility` picks the target, the entire Remember pipeline — write-time similarity/dedup gate, fingerprint check (`GetByFingerprint`), distillation, extraction — runs against the *target* ns, not the request primary. Test: remembering a fact with `visibility:"acme"` that duplicates an existing `acme` fact triggers the dedup gate there.
- [ ] REST `visibility` field lands in openapi at T6; MCP at T8. Commit.

### T5 — Provenance

**Files:** Modify `internal/service/readset.go` (or new `internal/service/provenance.go`), `internal/service/service.go` (Recall out-metadata, mirroring the `Degraded` out-param pattern ~L1641), `internal/api/mcp/mcp.go` `scoredItem` (L483-494) + `briefingItems` (L587-597); Test unit + MCP render.

**Produces:** `ReadSetEntry{NS string; Origin string /* primary|ancestor|home|link|call */; Tiers []memory.Tier}`; resolver returns `[]ReadSetEntry` alongside `[]scopeEntry`; `recallItem.From string` rendered `(from: acme)` / `(from: personal)` / `(from: link — shared/golang)`; primary hits render nothing.

- [ ] Tests: all five origins; primary silent; answer citations (via `scoredItem`) also annotated — do not assume recall-only callers.
- [ ] Implement: origin is recorded when each leg is appended during resolution (not re-derived after). Service exposes the resolved entries so MCP/REST/read-set endpoint share one source. Commit.

### T6 — OpenAPI batch + `GET /v1/namespaces/read-set` (single codegen)

**Files:** Modify `api/openapi.yaml`, `internal/api/rest/rest.go`; Regen `internal/api/rest/api.gen.go` (`go generate ./...`), `ui/src/api-schema.gen.ts` (`mise run ui`); Test `rest_test.go`.

**Batch ALL spec edits here:** `visibility` on RememberRequest; `from` on scored/briefing items; `Briefing.scope_header`/`children` fields (T9 shapes — define now); `/v1/links` CRUD schemas; `GET /v1/namespaces/read-set` (header-scoped like `/v1/namespaces/briefing` — no path param, matches existing convention). REST `scope` keeps `exact|subtree` as aliases for `full|everywhere` (back-compat for scripts; MCP enum swaps fully in T8).

- [ ] Test: GET read-set with ns `acme/phoenix/api`, home `personal/kit`, one stored link → entries `[{ns, origin, tiers}]` ordered primary→ancestors→home→link.
- [ ] Service method `ResolveReadSetInfo(ctx, ns, home string) ([]ReadSetEntry, error)` wrapping the resolver; handler modeled on `MoveNamespace` (rest.go:699-734). Commit.

### T7 — Links CRUD API + CLI

**Files:** Modify `internal/api/rest/rest.go`; Create `cmd/memini/link.go`; Tests `rest_test.go` + CLI test.

- [ ] REST tests: POST `/v1/links` {dst, tiers?, note?} (src = header namespace), GET list, DELETE; reject self-link and invalid ns.
- [ ] CLI `memini link add|rm|ls` mirroring `namespaceCmd` parent+children (namespace.go:23/62-63) via `withLocalStore` (namespace.go:68). `ls` prints a tabwriter table (doctor style). Links to not-yet-existing namespaces are **allowed** (namespaces exist implicitly); doctor warns on dangling dst (T10). Commit.
- [ ] **Export/import round-trip links** (gap G6, verified: native export has no links today): `memini export` native format gains a top-level `links: []` array (populated via `ListAllLinks`, filtered to the exported namespace(s)); `memini import` (native source) restores them. Test: export → wipe → import → `ListLinks` identical. Commit.

### T8 — MCP surface rewrite

**Files:** Modify `internal/api/mcp/mcp.go`; Tests Go schema/mapping tests + `plugin/scripts/_test.mjs` expectations (same change as T13's scope args — see coupling note).

- [ ] `scopeEnum` (L81) → `{"project","full","everywhere"}`, default `full`; mapping at recall L521-526 and briefing L605-610.
- [ ] `rememberArgs` gains `visibility` (string; description: `who should remember this: "project" (default), "personal" (follows you everywhere), or an ancestor namespace name from the briefing scope line`). **Remove `namespace`/`namespaces` from remember+recall tool schemas** (REST keeps them).
- [ ] **Addressing vs choosing** (gap G3): raw namespaces disappear only as *choices*. They remain as *data*: recall/list result items keep their `namespace` field (needed so `memory_update`/`memory_forget`/`memory_get` can address a memory by namespace+id — the LLM copies it verbatim from a result, never constructs it). `memory_list` defaults to primary; its namespace arg stays for addressing parity. Test: update a memory recalled from an ancestor ns.
- [ ] Rewrite `serverInstructions` (L101-116): semantic scope guidance, provenance reading, no raw paths. Wire `From` (T5) and briefing header/rollup (T9) into `recallResult`/`briefingResult`. Commit.

### T9 — Briefing scope header + child rollup

**Files:** Modify `internal/service/query.go` (Briefing struct L183, entries loop L271-282), `internal/api/mcp/mcp.go`, `internal/api/rest/rest.go`; Test service-level.

**Produces:** `Briefing.ScopeHeader string`; `Briefing.Children []ChildSummary{NS string; Total int; Pinned, Recent []string /* titles */}`.

- [ ] Test: header exactly `Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2), +1 link` (counts = durable memories contributed per leg, from the existing per-entry loop); at interior node `acme`, rollup lists each direct child with total + up to 3 pinned/recent durable titles; leaf node → empty rollup.
- [ ] Implement rollup: `ListNamespaces` (already fetched for subtree cases) → direct-children filter → per-child `List` with small limit. **Cap at 10 direct children** (gap G9), sorted by most-recent write; over-cap prints `… and N more` so a wide tenant root can't balloon briefing cost or token size. Commit.

### T10 — Doctor

**Files:** Modify `cmd/memini/doctor.go` (`resolveDoctorReadSet` L444-461, `printRetrievalScope` L544); Test `doctor_test.go`.

- [ ] Prefer `GET /v1/namespaces/read-set` when a server is reachable; keep local mirror for offline. New warnings: `MEMINI_NAMESPACE` pinned globally while cwd resolves elsewhere (catch-all trap — live instance's `default` holds 86 misfiled memories); `MEMINI_HOME` unset (simplified from spec's "personal attempted recently" — that needed nonexistent tracking; plan default, flag in PR description). Commit.

### T11 — `memini migrate scopes`

**Files:** Create `cmd/memini/migrate.go`, `internal/maintenance/scopes.go`; Tests `internal/maintenance/scopes_test.go`.

- [ ] Tests: `<t>/_shared` merged into `<t>` via `maintenance.Move` (renamespace.go:44); **post-merge dedup pass runs** (spec said "dedup gate handles collisions" but `Move`/`Reassign` move by unique ID with no content dedup — reuse `internal/maintenance/dedup.go` scoped to the target ns); idempotent re-run (no `_shared` left → no-op); `--dry-run` reports; when `MEMINI_GLOBAL_NAMESPACE` env is set, prints adoption instructions (`MEMINI_HOME=<old value>` for single-operator, `memini link` for team-wide) and does NOT silently rewrite. Commit.

### T12 — Boot guard + config deletion (LAST Go task)

**Files:** Modify `internal/config/config.go` (delete `GlobalNamespace` L151, `TenantShared` L159; add both to `deprecatedVars` table L326-344 with a **fatal** variant), `cmd/memini/root.go`; Test `config_test.go`, `cmd/memini/integration_test.go`.

- [ ] Test: boot with either env var set → refuses, message names `memini migrate scopes` + `MEMINI_HOME`. Sequenced last so mid-branch dev envs/integration tests that export these keep booting until everything else is green. Commit.

### T13 — Plugins + integrations (JS lane; scope-arg part lands atomically with T8)

**Files:** Modify `plugin/scripts/_shared.mjs` (env read near L19; `authHeaders` L318-322 — the single choke point; `postRemember` L568 forwards `visibility`), `plugin/scripts/mcp-headers.mjs` (L19-20), `packages/namespace-resolver/src/index.ts` (+`test/resolver.test.ts`), integrations opencode/openclaw/pi/hermes/openwebui (grep for hardcoded `subtree`); Test `plugin/scripts/_test.mjs`.

- [ ] Tests (`mise run test-hooks`): `MEMINI_HOME` env → `X-Memini-Home` on every POST/GET; unset → header absent; visibility forwarded by postRemember; hooks send new scope values. Resolver package: `ResolveResult` gains `home` (env-only resolution, mirrors `MEMINI_NAMESPACE` precedence).
- [ ] Note: resolver logic is intentionally triplicated (`_shared.mjs`, TS package, doctor) — update all three; do not attempt to unify in this branch (YAGNI, separate refactor). Commit.
- [ ] **Prose sweep** (gap G11): grep plugin skills/commands (`plugin/skills/`, `plugin/commands/`), integration READMEs, and the Helm chart (`charts/memini` values/README) for `MEMINI_GLOBAL_NAMESPACE`, `MEMINI_TENANT_SHARED`, `_shared`, and old scope language; update every hit. Commit.

### T14 — Docs (user-critical deliverable)

**Files:** Create `docs/scopes.md`; Modify `README.md` (knob table L296-298, "Namespace resolution" + "Retrieval scope" sections L302-386, purge `_shared` layout recommendation — grep README + readset.go comment), release notes.

`docs/scopes.md` (mirrors tiers.md style: H1, orienting para cross-linking tiers.md/categories.md, tables), **fictional namespaces only**, containing:
- [ ] **Data flow — write:** worked example: session in `acme/phoenix/api`, `memory_remember(content, visibility: "acme")` → durable → lands in `acme`; same call with `tier: episodic` → clamped to `acme/phoenix/api`; `visibility: "personal"` → `personal/kit`. Show the error text for `visibility: "widgets"` (teaches the chain).
- [ ] **Data flow — read:** `memory_recall(q, scope: "full")` from `acme/phoenix/api` → resolution table: `acme/phoenix/api` (primary, all tiers) → `acme/phoenix` (ancestor, durable) → `acme` (ancestor, durable) → `personal/kit` (home, durable) → `shared/golang` (link, durable) → RRF fusion, nearest-first tie-break → provenance-annotated results.
- [ ] **Isolation contract table** (from spec) + **ownership table**: client owns namespace+home resolution (headers); server owns cascade/link/visibility resolution; store owns partitioning only.
- [ ] **Knob table:** `MEMINI_HOME` (client), removed knobs with migration pointers, per-call escape hatches (`namespaces`, REST `scope` aliases). README rows updated; release notes lead with the semantic widening: nested namespaces now always read ancestors (old opt-out `MEMINI_TENANT_SHARED=false` is gone). Commit.

### T16 — Admin UI workstream (gap G10, expanded per user: structure changed, UI follows)

Depends on T6 (spec regen: `npm run gen-api` refreshes `ui/src/api-schema.gen.ts` — generated, never hand-edit shapes per `types.ts:1-3`). Gate: `npm run typecheck` + manual verification against the scratch server (no UI test infra exists; not adding one in this branch). Dev loop: `mise run ui-dev` (Vite HMR proxying `/v1` to :8080). Sub-tasks in order:

- [ ] **T16a — API client:** `ui/src/api.ts` gains `readSet()` (GET `/v1/namespaces/read-set`, header-scoped via existing `headers()` at api.ts:26-31), `links()`, `addLink(dst, tiers?, note?)`, `deleteLink(dst)`; re-export new types in `ui/src/types.ts`. Typecheck. Commit.
- [ ] **T16b — Shared hierarchy helpers:** extract n-level tree math into `ui/src/util.ts` (`nsTree(namespaces: string[]): NsNode[]` with `{ns, leaf, children}`; `ancestorsOf` mirror; `depth`). Replace the duplicated `tenantOf`/`groupByTenant` logic in `views/Projects.tsx:72-112` and `views/Graph.tsx:15-19` with it. Behavior-preserving refactor first (Projects still renders tenant→pod), then generalize `TenantBox`/`Pod` rendering to nested levels (collapse state keyed by full ns path in the existing `memini.collapsedTenants` localStorage key). Commit.
- [ ] **T16c — Namespace tree selector:** rework `ui/src/components/NamespaceSelect.tsx` from flat `.opt` list to an indented tree (depth-based padding, chevron collapse for nodes with children, keyboard nav preserved). Selection semantics unchanged (`pick(ns)` sets the signal + `refresh()`); "All projects" stays first. New CSS in `styles.css` (no `.tree` class exists yet). Commit.
- [ ] **T16d — Scopes view:** new `ui/src/views/Scopes.tsx` + single-segment `NAV` row `/scopes` (app.tsx:35-49; single-segment constraint per app.tsx:26-34). Two panels: (1) effective read-set for the selected namespace — Dashboard.tsx `useAsync` pattern, rows `ns / origin-chip / tiers`, origins color-coded chips (primary/ancestor/home/link/call); (2) links management — table via `api.links()` + add form + delete button, Health.tsx action-panel pattern with `ErrorBanner`. Commit.
- [ ] **T16e — Provenance badges:** `ui/src/components/MemoryCard.tsx` header (~L19-21, next to the existing namespace `.chip`) renders a `from` chip when present and ≠ primary (`ancestor`→segment name, `home`→"personal", `link`→"link: dst"); `views/Search.tsx:96-104` threads the new field from `ScoredMemory`; `MemoryDrawer.tsx` attributes `.kv` grid (~L178-227) gains an "In scope via" row. Commit.
- [ ] **T16f — Graph namespace mode:** `ui/src/views/Graph.tsx` gains a mode toggle (memories | namespaces). Namespace mode: nodes = namespaces (sized by memory count from per-ns stats, colored by depth), edges = implicit parent→child (cascade) + explicit links from `api.links()` (distinct `GLink.kind` values + legend entries at Graph.tsx:242-254). Reuses the existing ForceGraph setup (L124-189) with a second `build` function. Commit.

### T15 — Bench regression gate

- [ ] Before first code commit: run `bench/` LongMemEval-S on `main`, save baseline. After T2+T5: rerun, compare R@5/MRR (ancestor legs change RRF pools; first-seen tie-breaks make ordering behavioral). Regression >1pt → investigate before merge. Record both numbers in the PR description.

---

## Dependency graph

```
T1 → T2 → {T5, T9, T11} ;  T3 → {T2 threading, T4}
T2+T3 → T6 → {T7, T10}
{T2,T3,T4,T5,T9} → T8  ⟷  T13(scope args) [atomic pair]
T12 last Go task; T14 after surfaces stabilize; T15 brackets branch; T16a-f after T6+T7 (own lane, order a→f)
Parallel lanes: store+core (T1→T2), header (T3), JS (T13 prep), docs skeleton (T14)
```

## Gap audit (iteration 2 — all resolved into tasks above)

| # | Gap | Resolution |
|---|---|---|
| G1 | Answer never threads home (5 `RecallInput` sites, verified) | T3 |
| G2 | stdio MCP (`memini mcp`) has no headers to carry home | T3 (env fallback) |
| G3 | Hiding raw paths breaks update/forget addressing | T8 (namespace stays as *data*, dies as a *choice*) |
| G4 | Dedup/fingerprint/distill must follow visibility target ns | T4 |
| G5 | DeleteNamespace / Move leave dangling link rows | T1 (`RenameLinkEndpoints`, cascade delete) |
| G6 | export/import loses links (verified: no links in native format) | T7 (round-trip) |
| G7 | Cross-ns reinforcement — **verified already correct** (service.go:2433), guard note so nobody "fixes" it | T2 note |
| G8 | explicit-namespaces vs scope precedence undefined | T2 test (explicit wins) |
| G9 | Unbounded briefing child rollup at wide roots | T9 (cap 10 + `… and N more`) |
| G10 | Spec promised admin-UI read-set/tree rendering; plan had it out of scope | new T16 |
| G11 | Stale env-var/scope prose in Helm chart, plugin skills, integration READMEs | T13 sweep |
| G12 | Doctor "personal attempted recently" needs nonexistent tracking | T10 (simplified to `MEMINI_HOME` unset warning) |
| G13 | Agent segments (`project/reviewer`) now inherit project+tenant context automatically — behavior *improvement* worth documenting, easy to miss | T14 (scopes.md example) |
| G14 | `maintenance.Move` has no content dedup; spec's migration claim was wrong | T11 (post-merge dedup pass via `internal/maintenance/dedup.go`) |

## Key risks (full register in PR description)

- **Test rewrites are part of the task, never split:** readset_test.go (T2), doctor_test.go (T10), _test.mjs (T8+T13).
- **MCP/plugin scope skew:** un-updated hooks sending `scope=subtree` to the new MCP enum get rejections — T8 and T13's scope args land in one commit.
- **Codegen ordering:** openapi.yaml → `go generate ./...` → handlers → `mise run ui`. One batch (T6).
- **Migration duplicates:** Move has no content dedup — post-merge dedup pass is mandatory (T11).
- **Semantic widening on upgrade:** previously-isolated nested namespaces now read ancestors; release notes lead with it (T14).

## Verification (end-to-end)

1. `mise run test && mise run test-integration && mise run test-hooks` — all green.
2. Boot a scratch server (sqlite, fake embedder from `cmd/memini/integration_test.go` pattern). Seed: durable facts in `acme`, `acme/phoenix`, `personal/kit`; episodic in `acme/phoenix/api`; link `acme/phoenix/api → shared/golang`.
3. `curl GET /v1/namespaces/read-set` with ns `acme/phoenix/api` + home `personal/kit` → 5 entries, origins `primary/ancestor/ancestor/home/link`, correct order.
4. MCP smoke via the Claude Code plugin against the scratch server: `memory_briefing` shows `Scope:` header; `memory_recall` returns `(from: acme)` annotations; `memory_remember` with `visibility:"acme"` lands in `acme` (verify via `/v1/memories`), with `visibility:"bogus"` errors listing the chain, episodic + `visibility:"acme"` clamps to primary.
5. Seed `acme/_shared` with 2 facts (1 duplicating an `acme` fact) → `memini migrate scopes --dry-run` reports, real run merges + dedups, re-run no-ops. Boot with `MEMINI_GLOBAL_NAMESPACE=x` → refusal message.
6. `memini doctor` in a fake repo dir: prints effective read-set with origins; warns on pinned `MEMINI_NAMESPACE`.
7. UI against the scratch server (`mise run ui-dev`): namespace selector renders the `acme` tree with collapse; `/scopes` shows the 5-entry read-set with origin chips and can add/delete a link (refetch confirms); search from `acme/phoenix/api` shows `(from: acme)` chips; graph namespace mode draws cascade + link edges; `npm run typecheck` clean.
8. Bench numbers (T15) recorded and within tolerance.

**Not in scope** (spec non-goals): authz, transitive links, resolver-triplication refactor (Go/JS resolver unification), new UI test infrastructure (typecheck + manual verification only).
