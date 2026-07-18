# Align Pi's native memory tool contract

Status: resolved
Type: feature
Blocked by: 01

## What to build

Pi agents should have the same supported read, write, correction, provenance, and history capabilities as agents connected through Memini's maintained MCP contract.

## Acceptance criteria

- [x] Pi exposes REST-backed equivalents for all always-available MCP memory tools, including get, history, and partial update.
- [x] Optional server capabilities such as grounded answer are advertised only when support can be determined safely, or the limitation is explicitly documented and tested.
- [x] Recall, list, remember, update, and forget accept the current filtering, temporal, provenance, paging, and addressing fields appropriate to each operation.
- [x] Responses preserve timestamps, provenance, `stored:false`, merge hints, reinforcement, supersession, and other model-relevant flags.
- [x] Inherited or personal memories can be addressed only by copying provenance returned by Memini; raw namespace invention is not encouraged.
- [x] Contract-focused tests prevent silent drift from the generated MCP/OpenAPI behavior.

## Comments

Implemented in the Pi extension with eight always-available native tools:
`memory_briefing`, `memory_recall`, `memory_list`, `memory_remember`,
`memory_get`, `memory_history`, `memory_update`, and `memory_forget`.

Contract evidence and deliberate limitations:

- Grounded `memory_answer` is registered dynamically only when authenticated
  verbose health returns the literal boolean `deps.llm.configured: true`.
  Configured-but-unhealthy remains supported; false, plain-health downgrade,
  ingress 404, malformed payload, and network failure all omit the tool.
- Pi's REST-backed answer schema intentionally omits MCP `reasoning_level`:
  current OpenAPI `AnswerRequest` does not accept or thread it. No client env or
  endpoint-existence guess is used.
- Current REST briefing returns child memories but not the service's
  truncated-child count. Pi preserves every returned child as the MCP compact
  rollup shape and emits `children_note` only if a future REST response supplies
  literal evidence; it does not fabricate a note.
- Addressing tools validate and copy `namespace` verbatim from provenance;
  choice-side tools expose only semantic `scope` or write `visibility`.
- All explicit tools retain full JSON in model/session-facing `content` while
  the shared renderer keeps collapsed output one line and expanded output
  bounded.

Validation: focused helper contract suite, build, typecheck, full package tests,
pack dry-run, and diff checks pass on the implementation commit.
