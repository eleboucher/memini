# Align Pi's native memory tool contract

Status: ready-for-agent
Type: feature
Blocked by: 01

## What to build

Pi agents should have the same supported read, write, correction, provenance, and history capabilities as agents connected through Memini's maintained MCP contract.

## Acceptance criteria

- [ ] Pi exposes REST-backed equivalents for all always-available MCP memory tools, including get, history, and partial update.
- [ ] Optional server capabilities such as grounded answer are advertised only when support can be determined safely, or the limitation is explicitly documented and tested.
- [ ] Recall, list, remember, update, and forget accept the current filtering, temporal, provenance, paging, and addressing fields appropriate to each operation.
- [ ] Responses preserve timestamps, provenance, `stored:false`, merge hints, reinforcement, supersession, and other model-relevant flags.
- [ ] Inherited or personal memories can be addressed only by copying provenance returned by Memini; raw namespace invention is not encouraged.
- [ ] Contract-focused tests prevent silent drift from the generated MCP/OpenAPI behavior.

## Comments
