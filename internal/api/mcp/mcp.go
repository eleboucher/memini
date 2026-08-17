// Package mcp exposes memini over the Model Context Protocol. It is a thin
// adapter over the same service.Service the REST API uses, served over stdio
// and Streamable HTTP.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eleboucher/memini/internal/api/render"
	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/version"
)

var (
	readOnly = &mcpsdk.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: new(false),
	}
	additive = &mcpsdk.ToolAnnotations{
		DestructiveHint: new(false),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
	}
	destructive = &mcpsdk.ToolAnnotations{
		DestructiveHint: new(true),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
	}
)

// enumSchema infers the JSON schema for T (keeping the jsonschema struct-tag
// descriptions) and applies enum constraints, which the tag syntax cannot
// express. Array-typed properties get the enum on their items. It panics on an
// unknown property; the vars below run at init, so a typo fails any test run.
func enumSchema[T any](enums map[string][]any) *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{})
	if err != nil {
		panic(err)
	}
	for name, vals := range enums {
		p, ok := s.Properties[name]
		if !ok {
			panic("enumSchema: no property " + name)
		}
		if p.Items != nil { // array property: constrain its items
			p.Items.Enum = vals
		} else {
			p.Enum = vals
		}
	}
	return s
}

// Property names shared by several enum maps below.
const (
	propTiers  = "tiers"
	propLevels = "levels"
	propScope  = "scope"
)

// Input schemas with enum constraints, computed once. get/forget (idArgs) have
// no enum-valued parameters and keep the SDK's inferred schema.
var (
	tierEnum  = []any{"working", "episodic", "semantic", "procedural"}
	levelEnum = []any{"explicit", "deduced"}
	// scopeEnum is the LLM-facing semantic scope vocabulary: "project" (this
	// namespace only), "full" (default: project + inherited ancestor/personal/
	// link context), or "everywhere" (full + nested sub-projects). It does NOT
	// include the legacy "exact"/"subtree" values — those remain a REST-only
	// back-compat alias (internal/api/rest.restScopeAlias); memory_recall,
	// memory_briefing, and memory_answer reject them outright (see
	// service.parseScope).
	scopeEnum = []any{"project", "full", "everywhere"}

	rememberSchema = enumSchema[rememberArgs](map[string][]any{"tier": tierEnum, "level": levelEnum})
	recallSchema   = enumSchema[recallArgs](map[string][]any{
		propTiers: tierEnum, propLevels: levelEnum, propScope: scopeEnum,
		"response_format": {"concise", "detailed"},
	})
	briefingSchema = enumSchema[briefingArgs](map[string][]any{propScope: scopeEnum})
	answerSchema   = enumSchema[answerArgs](map[string][]any{
		propTiers: tierEnum, propLevels: levelEnum, propScope: scopeEnum,
		"reasoning_level": {"minimal", "low", "medium", "high"},
	})
	listSchema   = enumSchema[listArgs](map[string][]any{propTiers: tierEnum, propLevels: levelEnum})
	updateSchema = enumSchema[updateArgs](map[string][]any{"tier": tierEnum})
)

// serverInstructions is sent to clients in the initialize response. It is the
// one server-controlled string that can teach cross-tool policy (call order,
// proactive use, storage conventions) — per-tool descriptions are read in
// isolation during tool selection and can't express it.
//
// This block is also the CANONICAL save-policy text. Six semantic invariants
// are mirrored (never byte-identically) across the surfaces named below:
//  1. trigger categories: decision+rationale; bug root cause/gotcha;
//     convention; stated preference or correction; environment/tool quirk;
//     non-obvious command/workflow.
//  2. exclusions: secrets/credentials; transient session state; task progress;
//     facts in project docs/CLAUDE.md or trivially recoverable; passthrough
//     text.
//  3. tier is optional — omit to auto-classify.
//  4. an explicit user request saves unconditionally, except secrets.
//  5. visibility semantics (project default; personal follows the user;
//     ancestor names share up the chain; episodic/working clamp to project).
//  6. correction hygiene: a memory discovered to be wrong or outdated is fixed
//     immediately (update) or removed (forget), never left in place.
//
// Mirror surfaces (owned by later tasks — do NOT edit them here):
// plugin/scripts/_shared.mjs (MEMORY_INSTRUCTION), plugin/scripts/stop.mjs
// (auto-save nudge wording), plugin/skills/remember/SKILL.md,
// integrations/pi/plugin/src/index.ts, integrations/openclaw/plugin/src/index.ts.
const serverInstructions = "memini is persistent cross-session memory for this agent. Namespaces are " +
	"managed for you — you never construct or type a raw namespace path; you make semantic choices " +
	"(scope to read, visibility to write) and learn the topology by reading provenance. Standing policy:\n" +
	"- At session start, call memory_briefing once to orient (pinned context, durable facts, " +
	"how-tos, recent activity, and a Scope line spelling out the ancestor chain you inherit from, " +
	"e.g. \"Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2)\"). Prefer it over " +
	"broad recall queries.\n" +
	"- Saving memories is your job — do not wait to be asked. When you learn something durable — " +
	"a decision and its rationale, a bug's root cause, a project convention, a stated user " +
	"preference or a correction (a correction IS a preference), an environment quirk, a " +
	"non-obvious command — call memory_remember before moving on: one atomic, self-contained " +
	"fact per call. When the user explicitly asks you to remember something, save it before " +
	"acknowledging, even if it seems trivial or already stored (near-duplicates are reinforced " +
	"or merged server-side); secrets and credentials are the one exception. Never store " +
	"secrets or credentials, transient session state, task " +
	"progress, or what's already in project docs/CLAUDE.md or trivially recoverable from code.\n" +
	"- Before work that may have history — an unfamiliar file, a recurring bug, a non-obvious " +
	"decision — call memory_recall first. Its scope argument is the only lever: \"project\" (just " +
	"this project's own memories), \"full\" (default: project + inherited context — ancestors, your " +
	"personal namespace, links), or \"everywhere\" (full + nested sub-projects).\n" +
	"- visibility on memory_remember decides who should know: \"project\" (default, this project " +
	"only), \"personal\" (about the user, follows them everywhere), or an ancestor namespace name " +
	"read off the briefing Scope line (e.g. the team or org level) — on a durable write an " +
	"unrecognized name errors listing the valid chain. Durable facts worth sharing go up; personal " +
	"preferences go personal; session/working detail always stays in the project — episodic and " +
	"working writes are clamped to project regardless of visibility (silently, before name " +
	"validation), so a session digest can never pollute a shared ancestor.\n" +
	"- tier: semantic = durable fact, procedural = how-to/command, episodic = what happened, " +
	"working = scratch. Omit to auto-classify.\n" +
	"- Every recall/briefing result carries provenance: an empty/absent \"from\" means it's this " +
	"project's own memory; otherwise \"from\" names the ancestor or personal namespace it came from, " +
	"or \"link:<ns>\" for a linked namespace. Read \"from\" (and the briefing Scope line) to learn " +
	"where knowledge actually lives — never guess or construct a namespace path.\n" +
	"- Conventions: tag critical always-relevant facts \"pinned\" (they surface in every briefing); " +
	"set metadata.category to a topic bucket (e.g. bug_fixes, architecture_decisions, coding_conventions).\n" +
	"- Keep the store correct: when a stored memory proves wrong or outdated — recall says one " +
	"thing, reality shows another — fix it immediately with memory_update on its id, or " +
	"memory_forget if it should not exist; never leave known-incorrect data in place, and never " +
	"write a near-duplicate instead of correcting. memory_get/update/forget " +
	"take a namespace argument purely for addressing — copy it verbatim from a memory_recall/" +
	"memory_list result's namespace field, never type one yourself.\n" +
	"- Empty recall means nothing is known — proceed from first principles, never invent a " +
	"remembered fact. A degraded field means results are keyword-only and incomplete, not a confident negative."

// NewServer builds an MCP server exposing memini's memory tools. defaultNS is
// used whenever a tool call omits the namespace argument. home is the
// caller's personal namespace (X-Memini-Home / MEMINI_HOME), merged
// read-only into every recall/briefing/answer/remember; "" means no home leg.
// There is no per-call override — home is a transport-level default, fixed
// for the life of this server instance. author is the name of the NAMED
// table key that authenticated this session ("" for the admin key, an
// unauthenticated stdio session, or auth-disabled dev mode); it is stamped
// as metadata.author on writes via RememberInput.Author — see
// service.stampAuthor. authorKind classifies author for activity attribution
// ("key" for a named key, "env" for the admin env key, "none" for an
// unauthenticated stdio/dev session — see store.Event); a receiving middleware
// stamps (author, authorKind) onto every tool call's context via
// service.WithActor, so all tools inherit it without threading a parameter.
func NewServer(svc *service.Service, defaultNS, home, author, authorKind string, opts ...ServerOption) *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "memini",
		Version: version.Version,
	}, &mcpsdk.ServerOptions{Instructions: serverInstructions})

	// Attribution is session-fixed and automatic: stamp it once here so every
	// tool handler's ctx carries who is calling. Survives service.logEvents'
	// fire-and-forget hop (context.WithoutCancel keeps values).
	s.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			return next(service.WithActor(ctx, author, authorKind), method, req)
		}
	})

	// Authorization, when the session's credential carries it. Registered after
	// attribution so a refused call is still attributed, and only when needed so
	// an ordinary session pays nothing for it.
	var cfg serverOpts
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.readOnly {
		s.AddReceivingMiddleware(readOnlyMiddleware)
	}

	h := &tools{svc: svc, defaultNS: defaultNS, defaultHome: home, defaultAuthor: author}

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_remember",
		Title: "Remember a fact",
		Description: "Store a fact, decision, preference, or event for later recall. Do not wait " +
			"to be asked — call this the moment you learn: a decision and why it was made, a bug's " +
			"root cause, a project convention, a stated user preference, a correction from the user " +
			"(a correction IS a durable preference), an environment or tool quirk, or a non-obvious " +
			"command/workflow. When the user says 'remember this', 'note that', 'don't forget', " +
			"'going forward...', or corrects you, call this tool FIRST, then acknowledge — and on an " +
			"explicit request save unconditionally, even if it seems trivial or already stored; " +
			"secrets and credentials are the one exception. Keep " +
			"memories atomic — one self-contained fact per call; search works better on small " +
			"records. Do NOT store secrets or credentials, transient session state, task progress, " +
			"or facts already in project docs/CLAUDE.md or trivially recoverable from code. tier: " +
			"semantic=durable fact, procedural=how-to, episodic=event, working=scratch (default intake, omit to " +
			"auto-classify). visibility: 'project' (default) keeps it here; 'personal' follows the " +
			"user everywhere; or name an ancestor from the briefing Scope line to share it up that " +
			"chain — episodic/working writes always stay in project regardless. If the result " +
			"carries merge_hint, the content nearly duplicates an " +
			"existing memory — either call memory_update with id=merge_hint.similar_id to fold " +
			"them together, or ignore it to keep both. Returns {id, tier, stored} plus optional " +
			"flags. stored=false means a low-signal write was dropped by the value gate (not an " +
			"error); its tier reports what the write resolved to, including an auto-classified " +
			"tier. reinforced=true means the fact was ALREADY KNOWN: no new memory was created, " +
			"the existing one was strengthened, and `id` names that pre-existing memory rather " +
			"than anything you just wrote — do not report it to the user as a new save, and be " +
			"careful updating or forgetting it. auto_superseded=true means this write replaced a " +
			"near-duplicate, which was tombstoned in the background.",
		InputSchema: rememberSchema,
		Annotations: additive,
	}, h.remember)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_recall",
		Title: "Recall memories",
		Description: "Search prior context via hybrid (semantic + keyword) retrieval, ranked by " +
			"relevance, recency, and corroboration. Call BEFORE starting work that may have " +
			"history: editing an unfamiliar file, debugging a recurring issue, making a " +
			"non-obvious decision, or when asked 'what do we know about X'. Prefer a short " +
			"descriptive query ('JWT auth setup'). Results include created_at — on conflicting " +
			"memories prefer the most recent, surface the conflict, and fix the stale one " +
			"(memory_update to correct it, memory_forget if it should not exist). Empty results mean " +
			"nothing is known: proceed from first principles, never invent a remembered fact. A " +
			"degraded field means semantic search was unavailable and results are keyword-only — " +
			"treat as incomplete. scope picks how wide to read: 'project' (just this project), " +
			"'full' (default: project plus inherited ancestor/personal/link context), or " +
			"'everywhere' (full plus nested sub-projects). Supports time-travel (as_of) and " +
			"query_rewrite (LLM expands the query into variants, fused via RRF — better recall, " +
			"slower). Each result's namespace field is provenance data, not a choice — never " +
			"construct one; copy it verbatim into memory_get/update/forget to address that memory.",
		InputSchema: recallSchema,
		Annotations: readOnly,
	}, h.recall)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_briefing",
		Title: "Session briefing",
		Description: "Layered session-start briefing for this namespace — pinned context, " +
			"durable facts, how-to procedures, and recent activity — in one query-less call. " +
			"Call it when a session opens to orient yourself. Prefer this over broad recall " +
			"queries at session start. The scope_header line ('Scope: acme/phoenix/api ← " +
			"acme/phoenix(3) ← acme(4)') spells out the ancestor chain you inherit from — read it " +
			"instead of guessing namespace paths. scope='everywhere' also briefs nested sub-projects, " +
			"surfaced as a compact per-child rollup (name, count, pinned/recent titles).",
		InputSchema: briefingSchema,
		Annotations: readOnly,
	}, h.briefing)
	// memory_answer requires an LLM; only advertise it when one is configured,
	// so headless deployments don't list a tool that would error on every call.
	if svc.HasAnswerer() {
		mcpsdk.AddTool(s, &mcpsdk.Tool{
			Name:  "memory_answer",
			Title: "Answer from memory",
			Description: "Recall relevant memories and answer a question grounded on them (requires " +
				"an LLM). Slower than memory_recall; use when you want a synthesized answer with " +
				"sources rather than raw memories. scope picks how wide to ground, same as " +
				"memory_recall: 'project' (just this project), 'full' (default: project plus " +
				"inherited ancestor/personal/link context), or 'everywhere' (full plus nested " +
				"sub-projects).",
			InputSchema: answerSchema,
			Annotations: readOnly,
		}, h.answer)
	}
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_list",
		Title: "Browse memories",
		Description: "Browse memories without a query — filter by tier, tags, or metadata " +
			"(e.g. all procedural memories, or everything tagged/categorized X). Returns at " +
			"most limit (default 20) newest-first; page with offset. For relevance-ranked " +
			"search use memory_recall.",
		InputSchema: listSchema,
		Annotations: readOnly,
	}, h.list)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_get",
		Title: "Get a memory",
		Description: "Fetch one memory with full metadata, tags, and timestamps by ID (ids come " +
			"from memory_recall / memory_list results). A unique id prefix of at least 8 hex " +
			"chars also works; an ambiguous prefix errors listing the colliding full ids — " +
			"retry with a longer prefix.",
		Annotations: readOnly,
	}, h.get)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_history",
		Title: "Trace a memory's history",
		Description: "Trace the bi-temporal supersession lineage of a memory by ID: the fact " +
			"itself plus every fact it superseded and every fact that superseded it, oldest-first, " +
			"including tombstoned rows. Use to answer \"what was believed before, and what replaced " +
			"it\" — each result's valid_from/valid_to bound when it was true and superseded_by links " +
			"the fact that replaced it.",
		Annotations: readOnly,
	}, h.history)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_update",
		Title: "Update a memory",
		Description: "Update fields of an existing memory by ID (partial: only provided fields " +
			"change; metadata merges key-by-key). Use to correct or enrich a fact — e.g. to fold " +
			"a near-duplicate flagged by memory_remember's merge_hint into the surviving memory. " +
			"To delete instead, use memory_forget.",
		InputSchema: updateSchema,
		Annotations: additive,
	}, h.update)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_forget",
		Title: "Forget a memory",
		Description: "Permanently delete a memory by ID — use for wrong, outdated, or unwanted " +
			"memories. To correct a fact instead, prefer memory_update so history is preserved.",
		Annotations: destructive,
	}, h.forget)
	return s
}

// mcpPrincipalKey is the context key HTTPHandler's outer auth check uses to
// bridge the resolved apiauth.Principal into the per-session getServer
// closure below: NewStreamableHTTPHandler calls getServer with the same
// *http.Request ServeHTTP received (confirmed against the SDK's
// StreamableHTTPHandler.ServeHTTP, which passes req straight through to
// h.getServer(req)), so stashing the principal on the request context before
// calling h.ServeHTTP is sufficient — no other channel is needed between the
// two closures.
type mcpCtxKeyType int

const mcpPrincipalKey mcpCtxKeyType = 0

// HTTPHandler returns an http.Handler serving MCP over Streamable HTTP.
//
// Auth: apiKey (the admin env key), fileKeys (optional MEMINI_API_KEYS_FILE
// capability, K2b — nil when unused), and keyStore (optional table-key
// capability, nil when unsupported/unused) are resolved via
// apiauth.Config.Authenticate exactly like REST's authMiddleware — see its
// doc for the full enforcement rules, including when table or file-key auth
// becomes mandatory. This guarantees the two HTTP surfaces authenticate
// identically for the same credentials.
//
// The resolved identity (principal, and the ns/home it carries) is fixed for
// the lifetime of an MCP session — captured once when the session's server
// is built below, not re-resolved per tool call — see NewServer's callers.
//
// Namespace: taken from nsHeader when present, else the authenticated key's
// DefaultNS, else defaultNS; tool calls may still override it per-call. This
// is namespace resolution as CONTEXT — the header always wins when present.
//
// Home: taken from homeHeader when present — canonicalized exactly like
// REST's homeMiddleware so both transports resolve the same client input to
// the same namespace key — else "" (no home leg — unlike the namespace
// header there is no per-call override). A key bound to a home namespace
// (HomeNS != "") overrides this outright: the header is ignored entirely
// (never validated, never consulted), a conflicting value is logged once at
// debug level, and the request is never rejected for it. This is home
// resolution as IDENTITY, the deliberate opposite of namespace's
// header-wins precedence above — see the K2 brief's "SCOPE ADDITION" and
// REST's homeMiddleware doc for the full rationale.
//
// An invalid nsHeader value is always rejected with 400 (matching the REST
// API); an invalid homeHeader value is rejected with 400 too, UNLESS the
// authenticated key is bound (in which case the header is never even looked
// at) — never silently falling back to the default namespace.
//
// This builds its own apiauth.Config from apiKey/keyStore/fileKeys, which is
// fine when MCP is the only HTTP surface in the process. When REST is also
// mounted in the SAME process, use HTTPHandlerWithAuth instead and pass it
// the exact same apiauth.Config REST uses — otherwise the two surfaces hold
// independent table-emptiness caches, and an apiauth.Config.Invalidate() call
// from a REST key mutation never reaches this one (see apiauth.Config's doc
// on the shared cache pointer).
func HTTPHandler(svc *service.Service, nsHeader, defaultNS, homeHeader, apiKey string,
	keyStore store.APIKeyStore, fileKeys *apiauth.FileKeySet,
) http.Handler {
	return HTTPHandlerWithAuth(svc, nsHeader, defaultNS, homeHeader,
		apiauth.New(apiKey, keyStore).WithFileKeys(fileKeys))
}

// HTTPHandlerWithAuth is HTTPHandler but takes an already-built apiauth.Config
// instead of building one from apiKey/keyStore/fileKeys. Callers that also
// mount REST in the same process must construct exactly ONE apiauth.Config
// (e.g. via apiauth.New(...).WithFileKeys(...)) and pass that SAME value to
// both this function and rest.AuthConfig.KeyAuth — Config is a value type but
// its cache field is a shared pointer (see apiauth.Config's doc), so a copy
// still shares the cache and a REST-side Invalidate() reaches MCP immediately.
func HTTPHandlerWithAuth(svc *service.Service, nsHeader, defaultNS, homeHeader string,
	keyAuth apiauth.Config,
) http.Handler {
	h := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		p, _ := r.Context().Value(mcpPrincipalKey).(apiauth.Principal)
		ns := defaultNS
		if p.DefaultNS != "" {
			ns = p.DefaultNS
		}
		if v := httputil.NormalizeNamespace(r.Header.Get(nsHeader)); v != "" {
			ns = v
		}
		// Both headers are canonicalized like REST's middlewares do (trim
		// spaces, strip surrounding slashes, collapse "//"): the same client
		// input ("Work//Proj/") must resolve to the same namespace key on
		// both transports, or a caller switching between REST and MCP would
		// silently read and write two different namespaces. The namespace
		// header was TrimSpace-only until v0.8, so rows written over MCP
		// under a non-canonical header live in a sibling namespace — see
		// docs/operations/upgrading.md.
		home := httputil.NormalizeNamespace(r.Header.Get(homeHeader))
		if p.HomeNS != "" {
			if warn := httputil.HomeConflictWarning(p.Name, p.HomeNS, home); warn != "" {
				// Warn, don't whisper: the caller asked for one home namespace and
				// is getting another. The outer handler also puts this on the
				// response as X-Memini-Warning (it holds the ResponseWriter).
				slog.WarnContext(r.Context(), "X-Memini-Home ignored: request key is bound to a home namespace",
					"key", p.Name, "key_home", p.HomeNS, "header_home", home)
			}
			home = p.HomeNS
		}
		// Attribution kind mirrors REST's actorMiddleware: a named principal is
		// "key"; otherwise a presented bearer (the admin env key authenticated)
		// is "env", and no bearer at all (dev mode) is "none".
		kind := "none"
		if p.Name != "" {
			kind = "key"
		} else if strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != "" {
			kind = "env"
		}
		return NewServer(svc, ns, home, p.Name, kind, WithReadOnly(p.ReadOnly))
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok, err := keyAuth.Authenticate(r.Context(), token)
		if err != nil {
			slog.ErrorContext(r.Context(), "mcp auth: key store lookup failed", "err", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, `{"error":"missing or invalid bearer token"}`, http.StatusUnauthorized)
			return
		}
		// Record the actor for the access log (the outer request logger's
		// holder — see httputil.RecordActor), classified exactly like REST's
		// actorMiddleware and NewServer's attribution kind below. At auth
		// time, deliberately: even a request the MCP layer goes on to reject
		// is attributed in the log.
		switch {
		case p != nil && p.Name != "":
			httputil.RecordActor(r.Context(), p.Name, "key")
		case strings.TrimSpace(token) != "":
			httputil.RecordActor(r.Context(), "", "env")
		default:
			httputil.RecordActor(r.Context(), "", "none")
		}
		// Validate the normalized value (matching REST's namespaceMiddleware):
		// a header that normalizes to empty ("///") means "no namespace
		// header", not an error.
		if v := httputil.NormalizeNamespace(r.Header.Get(nsHeader)); v != "" {
			if err := httputil.ValidateNamespace(v); err != nil {
				http.Error(w, `{"error":"invalid namespace header"}`, http.StatusBadRequest)
				return
			}
		}
		// Skip home-header validation entirely for a bound key: the header is
		// ignored outright below (see doc comment), so a malformed value must
		// not 400 a request whose home leg it can never influence.
		if p == nil || p.HomeNS == "" {
			// Validate the normalized value (matching REST's homeMiddleware): a
			// header that normalizes to empty ("///") is "no home leg", not an
			// error, and one that survives normalization must be a valid namespace.
			if v := httputil.NormalizeNamespace(r.Header.Get(homeHeader)); v != "" {
				if err := httputil.ValidateNamespace(v); err != nil {
					http.Error(w, `{"error":"invalid home header"}`, http.StatusBadRequest)
					return
				}
			}
		}
		if p != nil {
			r = r.WithContext(context.WithValue(r.Context(), mcpPrincipalKey, *p))
			// Surface a home-header override on the response, as REST does: the
			// getServer callback above logs it but never sees the ResponseWriter.
			if warn := httputil.HomeConflictWarning(p.Name, p.HomeNS,
				httputil.NormalizeNamespace(r.Header.Get(homeHeader))); warn != "" {
				w.Header().Set(httputil.WarningHeader, warn)
			}
		}
		h.ServeHTTP(w, r)
	})
}

// RunStdio serves the MCP server over stdio, blocking until ctx is cancelled or
// the client disconnects. Used by `memini mcp` for local agent integrations.
// home is resolved by the caller from MEMINI_HOME (there are no headers on
// stdio); "" means no home leg.
func RunStdio(ctx context.Context, svc *service.Service, defaultNS, home string) error {
	// Stdio has no auth: an unauthenticated local session, so attribution kind
	// is "none" with no key name.
	return NewServer(svc, defaultNS, home, "", "none").Run(ctx, &mcpsdk.StdioTransport{})
}

// tools holds the MCP tool handlers.
type tools struct {
	svc       *service.Service
	defaultNS string
	// defaultHome is the caller's personal namespace (X-Memini-Home /
	// MEMINI_HOME), fixed for the life of this server instance; "" means no
	// home leg. See NewServer.
	defaultHome string
	// defaultAuthor is the NAMED table key that authenticated this session,
	// fixed for its life; "" for the admin key or an unauthenticated session.
	// See NewServer.
	defaultAuthor string
}

// parseTiers validates a tier filter. An unknown tier is an error rather than
// silently unfiltered results, matching the REST surface.
func parseTiers(in []string) ([]memory.Tier, error) {
	tiers := make([]memory.Tier, 0, len(in))
	for _, v := range in {
		t := memory.Tier(strings.TrimSpace(v))
		if !t.Valid() {
			return nil, fmt.Errorf("invalid tier %q: want one of working|episodic|semantic|procedural", t)
		}
		tiers = append(tiers, t)
	}
	return tiers, nil
}

// parseLevels validates a level filter. An unknown level is an error rather than
// silently unfiltered results, matching the REST surface.
func parseLevels(in []string) ([]memory.Level, error) {
	levels := make([]memory.Level, 0, len(in))
	for _, v := range in {
		l := memory.Level(strings.TrimSpace(v))
		if !l.Valid() {
			return nil, fmt.Errorf("invalid level %q: want one of explicit|deduced", l)
		}
		levels = append(levels, l)
	}
	return levels, nil
}

// parseOptionalTime parses an optional RFC3339 timestamp, returning nil for an
// empty string. field names the argument for error messages.
func parseOptionalTime(s, field string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: want RFC3339", field, s)
	}
	u := t.UTC()
	return &u, nil
}

// ns resolves a tool call's namespace argument: empty falls back to the server
// default, an invalid value is an error (never silently rerouted to the
// default namespace, which would mix data across namespaces). Used only by the
// addressing tools — memory_get/memory_update/memory_forget/memory_list —
// where namespace identifies an existing memory the caller already knows
// about (copied verbatim from a prior recall/list result), never a value the
// LLM chooses. memory_remember/memory_recall/memory_briefing/memory_answer
// have no namespace argument at all: they always operate on the server's
// primary namespace, steered only by semantic scope (recall/briefing/answer)
// or visibility (remember).
func (t *tools) ns(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return t.defaultNS, nil
	}
	if err := httputil.ValidateNamespace(arg); err != nil {
		return "", fmt.Errorf("invalid namespace: %w", err)
	}
	return arg, nil
}

type rememberArgs struct {
	Content string `json:"content" jsonschema:"the fact to store — atomic and self-contained, readable without this conversation's context"`
	Tier    string `json:"tier,omitempty" jsonschema:"working, episodic, semantic, or procedural (omit to auto-classify)"`
	Level   string `json:"level,omitempty" jsonschema:"provenance: explicit = user-stated, deduced = LLM-inferred; omit to leave unset"`
	Summary string `json:"summary,omitempty" jsonschema:"optional one-line summary"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Tags []string `json:"tags,omitempty" jsonschema:"topic labels for filtering and keyword search; tag a critical always-relevant fact 'pinned' so it surfaces in every session briefing"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"structured key/values for later filtering; set 'category' to a topic bucket (e.g. bug_fixes, architecture_decisions, coding_conventions)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Importance float64  `json:"importance,omitempty" jsonschema:"0..1 ranking/retention bias — higher ranks higher and survives pruning longer; omit for the default and the server may assess one itself (assessed_importance); an explicit value always wins and clears that assessment"`
	TTLSeconds *int     `json:"ttl_seconds,omitempty" jsonschema:"overrides the tier default TTL; negative means never expire"`
	ID         string   `json:"id,omitempty" jsonschema:"upserts an existing memory when provided"`
	Confidence *float64 `json:"confidence,omitempty" jsonschema:"0..1 seed corroboration for a durable fact; omit for default"`
	ValidFrom  string   `json:"valid_from,omitempty" jsonschema:"RFC3339 start of the fact's validity; backdate for as_of recall"`
	ValidTo    string   `json:"valid_to,omitempty" jsonschema:"RFC3339 end of the fact's validity; omit if still true"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Visibility string `json:"visibility,omitempty" jsonschema:"who should remember this: 'project' (default, this project only), 'personal' (about the user, follows them everywhere), or an ancestor namespace name read off the briefing Scope line (e.g. the team or org level); for durable writes an unrecognized name errors listing the valid options. Episodic/working writes always stay in project regardless of this value (clamped silently, before name validation)."`
}

type rememberResult struct {
	ID   string `json:"id"`
	Tier string `json:"tier"`
	// Stored is false when the episodic value gate dropped the write (low signal).
	Stored bool `json:"stored"`
	// AutoSuperseded is true when the write's near-duplicate crossed the
	// auto-supersede gate and the older memory was tombstoned in the background.
	AutoSuperseded bool `json:"auto_superseded,omitempty"`
	// Reinforced is true when the fact was already known: no new memory was
	// created, the existing one was strengthened, and ID names THAT memory rather
	// than anything this call wrote. Without it, `stored: true` on those paths
	// tells the agent it created something it did not.
	Reinforced bool `json:"reinforced,omitempty"`
	// MergeHint points at a near-duplicate the caller may want to merge into
	// (via memory_update) when the write landed in the merge-hint band. nil
	// otherwise.
	MergeHint *mergeHintResult `json:"merge_hint,omitempty"`
	// Degraded is set to "pending_embed" when the content embed failed or timed
	// out (WriteEmbedTimeout) and the memory was stored keyword-searchable only,
	// without a vector. Empty on a healthy write.
	Degraded string `json:"degraded,omitempty"`
	// Note explains Degraded in plain language for an agent consuming the tool
	// result; omitted alongside Degraded on a healthy write.
	Note string `json:"note,omitempty"`
}

// mergeHintResult mirrors service.MergeHint for the MCP wire shape.
type mergeHintResult struct {
	SimilarID      string  `json:"similar_id"`
	SimilarContent string  `json:"similar_content"`
	Score          float64 `json:"score"`
	Tier           string  `json:"tier"`
}

func (t *tools) remember(ctx context.Context, _ *mcpsdk.CallToolRequest, in rememberArgs) (*mcpsdk.CallToolResult, rememberResult, error) {
	input := service.RememberInput{
		Namespace:  t.defaultNS,
		Home:       t.defaultHome,
		Author:     t.defaultAuthor,
		Visibility: in.Visibility,
		Content:    in.Content,
		Tier:       memory.Tier(in.Tier),
		Level:      memory.Level(in.Level),
		Summary:    in.Summary,
		Tags:       in.Tags,
		Metadata:   in.Metadata,
		Importance: in.Importance,
		ID:         in.ID,
		Confidence: in.Confidence,
	}
	if in.TTLSeconds != nil {
		d := time.Duration(*in.TTLSeconds) * time.Second
		input.TTL = &d
	}
	var err error
	if input.ValidFrom, err = parseOptionalTime(in.ValidFrom, "valid_from"); err != nil {
		return nil, rememberResult{}, err
	}
	if input.ValidTo, err = parseOptionalTime(in.ValidTo, "valid_to"); err != nil {
		return nil, rememberResult{}, err
	}
	var hint service.MergeHint
	var superseded bool
	var reinforced bool
	var effectiveTier memory.Tier
	input.MergeHint = &hint
	input.AutoSuperseded = &superseded
	input.Reinforced = &reinforced
	input.EffectiveTier = &effectiveTier
	m, err := t.svc.Remember(ctx, input)
	if err != nil {
		return nil, rememberResult{}, err
	}
	if m == nil { // value gate dropped the write: report the tier it resolved to
		return nil, rememberResult{Tier: string(effectiveTier), Stored: false}, nil
	}
	res := rememberResult{
		ID:             m.ID,
		Tier:           string(m.Tier),
		Stored:         true,
		AutoSuperseded: superseded,
		Reinforced:     reinforced,
	}
	if hint.SimilarID != "" {
		res.MergeHint = &mergeHintResult{
			SimilarID:      hint.SimilarID,
			SimilarContent: hint.SimilarContent,
			Score:          hint.Score,
			Tier:           string(hint.Tier),
		}
	}
	if m.PendingEmbed() {
		res.Degraded = memory.PendingEmbedKey
		res.Note = "embeddings unavailable; stored keyword-searchable only, vector will be backfilled automatically"
	}
	return nil, res, nil
}

type recallArgs struct {
	Query  string   `json:"query" jsonschema:"natural-language search text; short and descriptive works best (e.g. 'JWT auth setup')"`
	Tiers  []string `json:"tiers,omitempty" jsonschema:"restrict to tiers; empty means all"`
	Levels []string `json:"levels,omitempty" jsonschema:"restrict to levels (explicit/deduced); empty means all"`
	Tags   []string `json:"tags,omitempty" jsonschema:"only memories carrying every listed tag (AND)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"only memories whose metadata has each key=value pair (AND)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	ExcludeMetadata map[string]string `json:"exclude_metadata,omitempty" jsonschema:"inverse of metadata; drops matching memories (e.g. {\"source\": \"turn_capture\"} hides auto-captured conversation turns)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	ExcludeIDs []string `json:"exclude_ids,omitempty" jsonschema:"drop memories with these ids, before ranking and limit (an excluded hit never consumes a result slot); for skipping memories already seen this session"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	IncludeFreshTurns bool `json:"include_fresh_turns,omitempty" jsonschema:"also return this session's just-captured conversation turns (hidden by default — they are still in your live context); only for 'what did I just say' queries"`
	QueryRewrite      bool `json:"query_rewrite,omitempty" jsonschema:"rewrite query into 2-3 variants and fuse via RRF"`
	Limit             int  `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	MinRankScore float64 `json:"min_rank_score,omitempty" jsonschema:"drop results whose final ranked score is below this ([0,1)); rarely needed — the server already gates relevance"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Scope string `json:"scope,omitempty" jsonschema:"how wide to read: 'project' = just this project's own memories; 'full' (default) = project plus inherited context (ancestors, your personal namespace, links); 'everywhere' = full plus nested sub-projects"`
	AsOf  string `json:"as_of,omitempty" jsonschema:"RFC3339 time for time-travel recall (facts true then)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"'concise' returns summary-or-truncated content (~1 line each; fetch full text with memory_get); 'detailed' (default) returns full content"`
}

type recallItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	// ContentHash is the content-identity hash (render.ContentHash: first 16
	// hex chars of sha256 over the full stored content, summary only when
	// content is empty) — stable across response formats, so a client can
	// dedupe injected memories without comparing (possibly concise) text.
	ContentHash string `json:"content_hash,omitempty"`
	// ContentTruncated marks a concise rendering whose content is a
	// truncating cut of the stored content; absent when the concise text is
	// a stored summary or short content, and always absent in detailed
	// responses.
	ContentTruncated bool   `json:"content_truncated,omitempty"`
	Tier             string `json:"tier"`
	Level            string `json:"level,omitempty"`
	// Namespace is read provenance: the namespace the memory lives in. With a
	// multi-namespace read set (namespaces/scope=subtree) it tells the caller
	// which partition each hit came from.
	Namespace  string   `json:"namespace,omitempty"`
	Score      float64  `json:"score"`
	Confidence *float64 `json:"confidence,omitempty"`
	CreatedAt  string   `json:"created_at"`
	Tags       []string `json:"tags,omitempty"`
	// From is read-set provenance beyond the raw namespace: empty for a hit
	// from the primary namespace (the common case — no annotation needed),
	// the ancestor/home namespace itself for those two origins ("acme",
	// "personal/kit"), and a prefixed form for a stored link or an explicit
	// per-call namespace ("link:shared/golang", "call:acme/other"). See
	// readSetFrom. Omitted (not just empty) so a primary-only recall/briefing
	// produces no "from" noise at all.
	From string `json:"from,omitempty"`
}

// readSetFrom and originMapFrom render read-set provenance into recallItem.From
// — see service.ReadSetFrom / service.OriginMap, shared with the REST layer
// (internal/api/rest) so both surfaces render "from" identically.
var (
	readSetFrom   = service.ReadSetFrom
	originMapFrom = service.OriginMap
)

// scoredItem is the single funnel from a store.Scored hit to the MCP wire
// shape, shared by memory_recall's results and memory_answer's sources (T5) —
// origins, built once per call via originMapFrom, is how both get read-set
// provenance without duplicating the rendering logic. The concise projection
// (summary, else a word/sentence-boundary cut at render.SearchMax runes) and
// the content_hash identity live in internal/api/render, shared with REST so
// both surfaces agree byte-for-byte.
func scoredItem(s store.Scored, responseFormat string, origins map[string]string) recallItem {
	content := s.Memory.Content
	truncated := false
	if responseFormat == "concise" {
		content, truncated = render.Concise(s.Memory.Content, s.Memory.Summary, render.SearchMax)
	}
	return recallItem{
		ID: s.Memory.ID, Content: content, ContentHash: render.ContentHash(s.Memory.Content, s.Memory.Summary),
		ContentTruncated: truncated, Tier: string(s.Memory.Tier),
		Level: string(s.Memory.Level), Namespace: s.Memory.Namespace,
		Score: s.Score, Confidence: s.Memory.Confidence,
		CreatedAt: s.Memory.CreatedAt.Format(time.RFC3339), Tags: s.Memory.Tags,
		From: readSetFrom(origins, s.Memory.Namespace),
	}
}

type recallResult struct {
	Results []recallItem `json:"results"`
	// Degraded is set to "keyword_only" when the query embed failed or timed
	// out and recall fell back to keyword-only search; omitted on a healthy
	// (vector+keyword) recall.
	Degraded string `json:"degraded,omitempty"`
	// Note explains Degraded in plain language for an agent consuming the
	// tool result; omitted alongside Degraded on a healthy recall.
	Note string `json:"note,omitempty"`
}

func (t *tools) recall(ctx context.Context, _ *mcpsdk.CallToolRequest, in recallArgs) (*mcpsdk.CallToolResult, recallResult, error) {
	tiers, err := parseTiers(in.Tiers)
	if err != nil {
		return nil, recallResult{}, err
	}
	levels, err := parseLevels(in.Levels)
	if err != nil {
		return nil, recallResult{}, err
	}
	input := service.RecallInput{
		Namespace: t.defaultNS,
		Home:      t.defaultHome,
		Query:     in.Query,
		// A recall over MCP is always sourced "mcp": the tool has no source
		// argument (adding one is surface for no gain — a caller could only
		// mislabel its own call), so the transport fixes it.
		Source:            "mcp",
		Tiers:             tiers,
		Levels:            levels,
		Tags:              in.Tags,
		Metadata:          in.Metadata,
		ExcludeMetadata:   in.ExcludeMetadata,
		ExcludeIDs:        in.ExcludeIDs,
		IncludeFreshTurns: in.IncludeFreshTurns,
		QueryRewrite:      in.QueryRewrite,
		Limit:             in.Limit,
		MinRankScore:      in.MinRankScore,
		// Scope carries the LLM's semantic choice ("project"/"full"/
		// "everywhere") straight through — service.Recall validates it via
		// the shared parseScope, so an old "exact"/"subtree" value (or any
		// other unrecognized string) errors there with the current vocabulary
		// rather than being silently aliased.
		Scope: in.Scope,
	}
	if in.AsOf != "" {
		asOf, perr := time.Parse(time.RFC3339, in.AsOf)
		if perr != nil {
			return nil, recallResult{}, fmt.Errorf("invalid as_of %q: want RFC3339", in.AsOf)
		}
		input.AsOf = asOf.UTC()
	}
	var degraded string
	var dropped []string
	input.Degraded = &degraded
	input.DroppedNamespaces = &dropped
	var readset []service.ReadSetEntry
	input.ReadSet = &readset
	res, err := t.svc.Recall(ctx, input)
	if err != nil {
		return nil, recallResult{}, err
	}
	origins := originMapFrom(readset)
	out := recallResult{Results: make([]recallItem, len(res))}
	for i, s := range res {
		out.Results[i] = scoredItem(s, in.ResponseFormat, origins)
	}
	out.Degraded, out.Note = service.DegradedWire(degraded, dropped)
	return nil, out, nil
}

type briefingArgs struct {
	PerSection       *int `json:"per_section,omitempty" jsonschema:"default cap for any section when its dedicated cap is unset (default 5)"`
	PerSectionPinned *int `json:"per_section_pinned,omitempty" jsonschema:"max pinned memories; 0 disables this section"`
	PerSectionFacts  *int `json:"per_section_facts,omitempty" jsonschema:"max durable semantic facts; 0 disables"`
	PerSectionProc   *int `json:"per_section_procedures,omitempty" jsonschema:"max procedural how-to memories; 0 disables"`
	PerSectionRecent *int `json:"per_section_recent,omitempty" jsonschema:"max recent episodic entries; 0 disables"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Scope string `json:"scope,omitempty" jsonschema:"how wide to brief: 'project' = just this namespace's own memories; 'full' (default) = project plus inherited context (ancestors, your personal namespace, links); 'everywhere' = full plus nested sub-projects"`
}

type briefingResult struct {
	Namespace string `json:"namespace"`
	// ScopeHeader is the service's one-line read-set summary (see
	// service.Briefing.ScopeHeader), e.g.
	// "Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2), +1 link".
	ScopeHeader string       `json:"scope_header,omitempty"`
	Pinned      []recallItem `json:"pinned,omitempty"`
	Facts       []recallItem `json:"facts,omitempty"`
	Procedures  []recallItem `json:"procedures,omitempty"`
	Recent      []recallItem `json:"recent,omitempty"`
	// Children is the direct-child rollup, rendered compactly — titles only,
	// never full memory objects: the briefing is LLM-facing context, so
	// token size matters (REST carries the full objects for the admin UI).
	Children []briefingChild `json:"children,omitempty"`
	// ChildrenNote reports children omitted by the service's 10-child cap
	// ("… and N more child namespaces"); empty when nothing was truncated.
	ChildrenNote string `json:"children_note,omitempty"`
	// Degraded names read-set namespaces that could not be loaded and were
	// skipped. It matters for an agent specifically: a briefing that silently
	// omits an ancestor's durable facts is the case where the agent cannot know
	// what it is missing. The briefed namespace never appears here — losing it
	// fails the call instead.
	Degraded []string `json:"degraded,omitempty"`
	// Note explains Degraded in plain language; omitted alongside it.
	Note string `json:"note,omitempty"`
}

// briefingChild is the MCP wire shape of one child rollup entry: namespace,
// all-tier live count, and compact pinned/recent display titles.
type briefingChild struct {
	Namespace string   `json:"namespace"`
	Total     int      `json:"total"`
	Pinned    []string `json:"pinned,omitempty"`
	Recent    []string `json:"recent,omitempty"`
}

// childTitles maps a rollup highlight set to display titles (render.Title:
// the summary, else a word/sentence-boundary cut at render.TitleMax runes —
// shorter than the concise caps because the rollup is an index of what's
// under a namespace, not the content itself), nil-for-empty so the JSON
// field is omitted.
func childTitles(mems []*memory.Memory) []string {
	if len(mems) == 0 {
		return nil
	}
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = render.Title(m.Content, m.Summary)
	}
	return out
}

func briefingItems(mems []*memory.Memory, origins map[string]string) []recallItem {
	out := make([]recallItem, len(mems))
	for i, m := range mems {
		out[i] = recallItem{
			ID: m.ID, Content: m.Content, ContentHash: render.ContentHash(m.Content, m.Summary),
			Tier: string(m.Tier), Namespace: m.Namespace,
			Confidence: m.Confidence,
			CreatedAt:  m.CreatedAt.Format(time.RFC3339), Tags: m.Tags,
			From: readSetFrom(origins, m.Namespace),
		}
	}
	return out
}

func (t *tools) briefing(ctx context.Context, _ *mcpsdk.CallToolRequest, in briefingArgs) (*mcpsdk.CallToolResult, briefingResult, error) {
	// PerSection is the default cap applied to any section whose dedicated
	// per_section_X is unset (nil). Matches the REST /briefing semantics so
	// MCP and HTTP callers see the same shape: nil = default, 0 = disable.
	// The section fields are *int so "unset" (omitted) and "explicitly 0"
	// (disable) are distinguishable — a plain int can't express that.
	pick := func(dedicated *int) *int {
		if dedicated != nil {
			return dedicated
		}
		return in.PerSection
	}
	var readset []service.ReadSetEntry
	b, err := t.svc.Briefing(ctx, t.defaultNS, service.BriefingOpts{
		Pinned:     pick(in.PerSectionPinned),
		Facts:      pick(in.PerSectionFacts),
		Procedures: pick(in.PerSectionProc),
		Recent:     pick(in.PerSectionRecent),
		Home:       t.defaultHome,
		// Scope carries the LLM's semantic choice through unvalidated here —
		// see the matching comment in recall; service.Briefing validates it
		// via the shared parseScope.
		Scope:   in.Scope,
		ReadSet: &readset,
	})
	if err != nil {
		return nil, briefingResult{}, err
	}
	origins := originMapFrom(readset)
	out := briefingResult{
		Namespace:   b.Namespace,
		ScopeHeader: b.ScopeHeader,
		Pinned:      briefingItems(b.Pinned, origins),
		Facts:       briefingItems(b.Facts, origins),
		Procedures:  briefingItems(b.Procedures, origins),
		Recent:      briefingItems(b.Recent, origins),
	}
	if len(b.Degraded) > 0 {
		out.Degraded = b.Degraded
		_, out.Note = service.DegradedWire("", b.Degraded)
	}
	if len(b.Children) > 0 {
		out.Children = make([]briefingChild, len(b.Children))
		for i, c := range b.Children {
			out.Children[i] = briefingChild{
				Namespace: c.NS,
				Total:     c.Total,
				Pinned:    childTitles(c.Pinned),
				Recent:    childTitles(c.Recent),
			}
		}
	}
	if n := b.ChildrenTruncated; n > 0 {
		// The T6 wire shape has no truncated-count field, so the cap is
		// surfaced here at the render layer as a note.
		out.ChildrenNote = fmt.Sprintf("… and %d more child namespace", n)
		if n > 1 {
			out.ChildrenNote += "s"
		}
	}
	return nil, out, nil
}

type answerArgs struct {
	Query    string            `json:"query" jsonschema:"the question to answer from memory"`
	Tiers    []string          `json:"tiers,omitempty" jsonschema:"restrict grounding to tiers (working/episodic/semantic/procedural)"`
	Levels   []string          `json:"levels,omitempty" jsonschema:"restrict grounding to levels (explicit/deduced); empty means all"`
	Tags     []string          `json:"tags,omitempty" jsonschema:"ground only on memories with every listed tag (AND)"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"ground only on memories whose metadata has each key=value pair (AND)"`
	Limit    int               `json:"limit,omitempty" jsonschema:"max memories to ground on (default 10)"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Scope string `json:"scope,omitempty" jsonschema:"how wide to ground: 'project' = just this project's own memories; 'full' (default) = project plus inherited context (ancestors, your personal namespace, links); 'everywhere' = full plus nested sub-projects"`
	// ReasoningLevel is the latency/cost dial: higher levels let the model
	// search memory iteratively before answering.
	ReasoningLevel string `json:"reasoning_level,omitempty" jsonschema:"effort: minimal|low|medium|high; higher = iterative search (slower)"`
}

type answerResult struct {
	Answer  string       `json:"answer"`
	Sources []recallItem `json:"sources"`
}

func (t *tools) answer(ctx context.Context, _ *mcpsdk.CallToolRequest, in answerArgs) (*mcpsdk.CallToolResult, answerResult, error) {
	tiers, err := parseTiers(in.Tiers)
	if err != nil {
		return nil, answerResult{}, err
	}
	levels, err := parseLevels(in.Levels)
	if err != nil {
		return nil, answerResult{}, err
	}
	var readset []service.ReadSetEntry
	res, err := t.svc.Answer(ctx, service.AnswerInput{
		Namespace: t.defaultNS,
		Home:      t.defaultHome,
		Query:     in.Query,
		Tiers:     tiers,
		Levels:    levels,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     in.Limit,
		Reasoning: service.ReasoningLevel(in.ReasoningLevel),
		// Scope passes through unvalidated here, like recall/briefing —
		// service.Answer rejects an unrecognized value up front via the
		// shared parseScope.
		Scope:   in.Scope,
		ReadSet: &readset,
	})
	if err != nil {
		return nil, answerResult{}, err
	}
	origins := originMapFrom(readset)
	out := answerResult{Answer: res.Answer, Sources: make([]recallItem, len(res.Sources))}
	for i, s := range res.Sources {
		out.Sources[i] = scoredItem(s, "", origins)
	}
	return nil, out, nil
}

type idArgs struct {
	ID        string `json:"id" jsonschema:"the memory ID"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace; defaults to the server namespace"`
}

// memoryItem is the full single-memory DTO returned by memory_get (recall
// results stay slim via recallItem; a get has no score and should not drop
// the record's metadata).
type memoryItem struct {
	ID         string         `json:"id"`
	Content    string         `json:"content"`
	Tier       string         `json:"tier"`
	Level      string         `json:"level,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Importance float64        `json:"importance"`
	// AssessedImportance is the LLM's own read of how important the content is,
	// absent when it was never assessed. Read-only: supplying importance on a
	// write clears it.
	AssessedImportance *float64 `json:"assessed_importance,omitempty"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
	AccessCount        int      `json:"access_count"`
	ExpiresAt          string   `json:"expires_at,omitempty"`
	ValidFrom          string   `json:"valid_from,omitempty"`
	ValidTo            string   `json:"valid_to,omitempty"`
	SupersededBy       string   `json:"superseded_by,omitempty"`
}

func toMemoryItem(m *memory.Memory) memoryItem {
	out := memoryItem{
		ID: m.ID, Content: m.Content, Tier: string(m.Tier), Level: string(m.Level),
		Summary: m.Summary, Tags: m.Tags, Metadata: m.Metadata, Importance: m.Importance,
		AssessedImportance: m.AssessedImportance,
		CreatedAt:          m.CreatedAt.Format(time.RFC3339), UpdatedAt: m.UpdatedAt.Format(time.RFC3339),
		AccessCount: m.AccessCount,
	}
	if m.ExpiresAt != nil {
		out.ExpiresAt = m.ExpiresAt.Format(time.RFC3339)
	}
	if m.ValidFrom != nil {
		out.ValidFrom = m.ValidFrom.Format(time.RFC3339)
	}
	if m.ValidTo != nil {
		out.ValidTo = m.ValidTo.Format(time.RFC3339)
	}
	if m.SupersededBy != nil {
		out.SupersededBy = *m.SupersededBy
	}
	return out
}

// notFoundErr wraps err with the standard "no memory" guidance when err is
// store.ErrNotFound, and returns err unchanged otherwise — a transient store
// error must never be misreported as "this id doesn't exist", which would
// send an agent down the wrong path (fabricating an id or giving up) instead
// of retrying. Used by get, forget, and update so the wording is identical
// across all three surfaces.
func notFoundErr(id, ns string, err error) error {
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return fmt.Errorf("no memory %q in namespace %q — ids come from memory_recall "+
		"or memory_list results: %w", id, ns, err)
}

func (t *tools) get(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, memoryItem, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, memoryItem{}, err
	}
	m, err := t.svc.Get(ctx, ns, in.ID)
	if err != nil {
		return nil, memoryItem{}, notFoundErr(in.ID, ns, err)
	}
	return nil, toMemoryItem(m), nil
}

func (t *tools) history(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, listResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, listResult{}, err
	}
	mems, err := t.svc.History(ctx, ns, in.ID)
	if err != nil {
		return nil, listResult{}, notFoundErr(in.ID, ns, err)
	}
	out := listResult{Memories: make([]memoryItem, len(mems))}
	for i, m := range mems {
		out.Memories[i] = toMemoryItem(m)
	}
	return nil, out, nil
}

type updateArgs struct {
	ID         string         `json:"id" jsonschema:"the memory ID to update (from memory_recall/memory_list)"`
	Content    *string        `json:"content,omitempty" jsonschema:"replacement content; omit to keep"`
	Summary    *string        `json:"summary,omitempty" jsonschema:"replacement summary; omit to keep"`
	Tier       *string        `json:"tier,omitempty" jsonschema:"move to this tier; omit to keep"`
	Tags       []string       `json:"tags,omitempty" jsonschema:"replacement tag set; omit to keep"`
	Metadata   map[string]any `json:"metadata,omitempty" jsonschema:"merged into existing metadata key-by-key; a null value deletes that key"`
	Importance *float64       `json:"importance,omitempty" jsonschema:"0..1; omit to keep; an explicit value clears assessed_importance"`
	Confidence *float64       `json:"confidence,omitempty" jsonschema:"0..1; omit to keep"`
	Namespace  string         `json:"namespace,omitempty" jsonschema:"namespace; defaults to the server namespace"`
}

// update edits an existing memory in place via service.Update, which REST's
// PATCH /v1/memories/{id} shares: only fields explicitly provided are changed,
// metadata merges key-by-key (a null value deletes that key), and the write-time
// lifecycle still runs, so an update is not a bare field patch. Content is
// re-embedded only when it actually changes.
func (t *tools) update(ctx context.Context, _ *mcpsdk.CallToolRequest, in updateArgs) (*mcpsdk.CallToolResult, memoryItem, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, memoryItem{}, err
	}
	upd := service.UpdateInput{
		Namespace: ns, ID: in.ID, Home: t.defaultHome, Author: t.defaultAuthor,
		Content: in.Content, Summary: in.Summary, Tags: in.Tags, Metadata: in.Metadata,
		Importance: in.Importance, Confidence: in.Confidence,
	}
	if in.Tier != nil {
		upd.Tier = new(memory.Tier(*in.Tier))
	}
	m, err := t.svc.Update(ctx, upd)
	if err != nil {
		// notFoundErr passes anything else through untouched; it only enriches
		// the ErrNotFound case with where a valid ID comes from.
		return nil, memoryItem{}, notFoundErr(in.ID, ns, err)
	}
	return nil, toMemoryItem(m), nil
}

// defaultListLimit caps memory_list results when the caller omits (or sets
// non-positive) limit. This is an MCP-only cap: an unbounded list is a
// context blowout for LLM callers. The REST API keeps its 0 = all semantics.
const defaultListLimit = 20

type listArgs struct {
	Tiers     []string          `json:"tiers,omitempty" jsonschema:"restrict to tiers (working/episodic/semantic/procedural); empty means all"`
	Levels    []string          `json:"levels,omitempty" jsonschema:"restrict to levels (explicit/deduced); empty means all"`
	Tags      []string          `json:"tags,omitempty" jsonschema:"only memories carrying every listed tag (AND)"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"only memories whose metadata has each key=value pair (AND)"`
	Limit     int               `json:"limit,omitempty" jsonschema:"max results (default 20, newest first)"`
	Offset    int               `json:"offset,omitempty" jsonschema:"skip this many results for paging"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"namespace; defaults to the server namespace"`
}

type listResult struct {
	Memories []memoryItem `json:"memories"`
}

func (t *tools) list(ctx context.Context, _ *mcpsdk.CallToolRequest, in listArgs) (*mcpsdk.CallToolResult, listResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, listResult{}, err
	}
	tiers, err := parseTiers(in.Tiers)
	if err != nil {
		return nil, listResult{}, err
	}
	levels, err := parseLevels(in.Levels)
	if err != nil {
		return nil, listResult{}, err
	}
	// Clamp a negative offset to 0: limit+offset below would otherwise go
	// <= 0, which the service layer treats as "no limit" — reintroducing
	// the unbounded listing the default cap exists to prevent.
	if in.Offset < 0 {
		in.Offset = 0
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	mems, err := t.svc.List(ctx, service.ListInput{
		Namespace: ns,
		Tiers:     tiers,
		Levels:    levels,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     limit + in.Offset,
	})
	if err != nil {
		return nil, listResult{}, err
	}
	if in.Offset > 0 {
		if in.Offset >= len(mems) {
			mems = nil
		} else {
			mems = mems[in.Offset:]
		}
	}
	out := listResult{Memories: make([]memoryItem, len(mems))}
	for i, m := range mems {
		out.Memories[i] = toMemoryItem(m)
	}
	return nil, out, nil
}

type forgetResult struct {
	Deleted bool `json:"deleted"`
}

func (t *tools) forget(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, forgetResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, forgetResult{}, err
	}
	if err := t.svc.Forget(ctx, ns, in.ID); err != nil {
		return nil, forgetResult{}, notFoundErr(in.ID, ns, err)
	}
	return nil, forgetResult{Deleted: true}, nil
}
