// GENERATED FILE — do not hand-edit.
// Produced by `npm run gen-api` (ui/scripts/gen-catalog.mjs) from
// api/openapi.yaml's ClientSettings schema, in spec order. Carries the
// field-level help text, built-in defaults, enums, and minimums that the
// openapi-typescript output (api-schema.gen.ts) doesn't preserve — the
// Config view's Settings tab renders this catalog as its row list so every
// server-known ClientSettings field is visible, described, and labeled with
// its built-in default even before the UI has bespoke copy for it.

export interface SettingsCatalogEntry {
  key: string
  type: 'boolean' | 'integer' | 'number' | 'string' | 'array'
  default: unknown
  enum?: string[]
  min?: number
  description: string
}

export const SETTINGS_CATALOG: SettingsCatalogEntry[] = [
  {
    "key": "capture_turns",
    "type": "boolean",
    "default": true,
    "description": "Capture each user→assistant turn as episodic memory."
  },
  {
    "key": "session_digest",
    "type": "boolean",
    "default": true,
    "description": "Record a session-end/stop/pre-compact digest memory."
  },
  {
    "key": "inline_extract",
    "type": "boolean",
    "default": true,
    "description": "Inject the directive asking the agent to save durable facts via memory_remember."
  },
  {
    "key": "auto_save",
    "type": "boolean",
    "default": true,
    "description": "Periodically nudge the agent to persist durable memories."
  },
  {
    "key": "auto_save_interval",
    "type": "integer",
    "default": 10,
    "description": "User-message interval between auto-save nudges.",
    "min": 1
  },
  {
    "key": "auto_save_min_events",
    "type": "integer",
    "default": 3,
    "description": "Minimum buffered state-changing tool events since the last auto-save baseline for the interval nudge to fire; below this the nudge defers, then fires as a discussion-variant prompt at twice the interval. 0 disables the activity gate (interval-only cadence).",
    "min": 0
  },
  {
    "key": "inject_briefing_pinned",
    "type": "integer",
    "default": 5,
    "description": "Max pinned memories in the session-start briefing.",
    "min": 0
  },
  {
    "key": "inject_briefing_facts",
    "type": "integer",
    "default": 5,
    "description": "Max durable semantic facts in the session-start briefing.",
    "min": 0
  },
  {
    "key": "inject_briefing_procedures",
    "type": "integer",
    "default": 5,
    "description": "Max procedural how-tos in the session-start briefing.",
    "min": 0
  },
  {
    "key": "inject_briefing_recent",
    "type": "integer",
    "default": 3,
    "description": "Max recent episodic entries in the session-start briefing.",
    "min": 0
  },
  {
    "key": "inject_briefing_max_tok",
    "type": "integer",
    "default": 0,
    "description": "Hard ceiling on briefing injection tokens; 0 is uncapped.",
    "min": 0
  },
  {
    "key": "inject_pretool_items",
    "type": "integer",
    "default": 3,
    "description": "Max recalled items injected per file on PreToolUse.",
    "min": 0
  },
  {
    "key": "inject_pretool_max_tok",
    "type": "integer",
    "default": 0,
    "description": "Hard ceiling on per-tool injection tokens; 0 is uncapped.",
    "min": 0
  },
  {
    "key": "inject_pretool_min_score",
    "type": "number",
    "default": 0,
    "description": "Floor on the fused score (>=) for a PreToolUse injection.",
    "min": 0
  },
  {
    "key": "inject_pretool_tools",
    "type": "array",
    "default": [
      "Read",
      "Write",
      "Edit",
      "Glob",
      "Grep"
    ],
    "description": "Tool-name allowlist that triggers a PreToolUse injection."
  },
  {
    "key": "inject_dedupe",
    "type": "boolean",
    "default": true,
    "description": "Suppress re-injecting an unchanged PreToolUse recall block for a file already injected this session. The recall call still runs; only the duplicate injection is skipped."
  },
  {
    "key": "inject_labels",
    "type": "array",
    "default": [],
    "description": "Which annotation labels to render alongside an injected memory.",
    "enum": [
      "tier",
      "confidence",
      "age",
      "reason"
    ]
  },
  {
    "key": "recall",
    "type": "boolean",
    "default": true,
    "description": "Enable recall-driven injection at all."
  },
  {
    "key": "capture",
    "type": "boolean",
    "default": true,
    "description": "Enable capture (turns/digests) at all."
  },
  {
    "key": "recall_limit",
    "type": "integer",
    "default": 3,
    "description": "Max memories per recall call.",
    "min": 0
  },
  {
    "key": "inject_recall_max_tok",
    "type": "integer",
    "default": 0,
    "description": "Hard ceiling on recall injection tokens; 0 is uncapped.",
    "min": 0
  },
  {
    "key": "inject_recall_min_score",
    "type": "number",
    "default": 0,
    "description": "Floor on the fused score (>=) for a recall injection.",
    "min": 0
  },
  {
    "key": "min_capture_chars",
    "type": "integer",
    "default": 0,
    "description": "Minimum content length worth bothering to capture a turn.",
    "min": 0
  },
  {
    "key": "request_timeout_ms",
    "type": "integer",
    "default": 30000,
    "description": "How long a client waits on one memini HTTP call before giving up (MEMINI_TIMEOUT_MS locally). It must stay above the server's own MEMINI_RERANK_TIMEOUT (default 10s): the server bounds a slow reranker and degrades to composite order, but a client that hangs up first gets nothing at all instead of an unranked result. Raise it when a cross-encoder reranker, a deep MEMINI_RERANK_POOL, or a cold model pushes recall past the default. The handshake keeps its own short timeout and is not covered by this setting.",
    "min": 100
  },
  {
    "key": "namespace_scope",
    "type": "string",
    "default": "repo",
    "description": "\"repo\" derives the namespace from the bare repo name; \"owner_repo\" disambiguates same-named repos across owners with an owner-repo slug (owner + \"-\" + repo).",
    "enum": [
      "repo",
      "owner_repo"
    ]
  },
  {
    "key": "namespace_prefix",
    "type": "string",
    "default": "",
    "description": "Namespace path prepended ahead of the derived/declared namespace."
  }
]
