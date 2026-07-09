# Honcho → Memini: Feature Steal Report

## 1. Dialectic Memory (Query-Time Reasoning)

**What it is**: An agentic loop where an LLM answers queries about a peer by strategically calling tools (`search_memory`, `search_messages`, `grep_messages`, `get_reasoning_chain`) — rather than pre-gathering all context. The agent iteratively decides what to search, evaluates results, and synthesizes answers.

**Key files**:

- `src/dialectic/core.py` — `DialecticAgent` class (551 lines)
- `src/dialectic/prompts.py` — System prompt (237 lines)
- `src/dialectic/chat.py` — API endpoint

**Data flow**:

```
User query → DialecticAgent._prepare_query()
  → _initialize_session_history() (fetch recent messages)
  → _prefetch_relevant_observations() (2 parallel semantic searches: explicit + derived)
  → honcho_llm_call(tools=..., tool_executor=..., max_tool_iterations=N)
    → Agent loop: LLM chooses tool → tool_executor runs handler → result back to LLM
    → Repeat until answer found or max iterations
  → Response with telemetry (tool_calls, tokens, iterations)
```

**Key design decisions**:

- **Multi-level reasoning**: 4 reasoning levels (minimal → all) with different tool sets, iteration limits, and output token budgets.
- **Prefetch**: Embeds the query once, does 2 semantic searches (explicit vs derived levels), injects top results as context before tool loop.
- **Reasoning chains**: `get_reasoning_chain` tool lets the agent traverse the observation tree (premises → observation → conclusions) for grounding.
- **Peer cards injected**: Observer/observed peer cards are part of the system prompt for perspective.
- **Session history**: Configurable token limit for injecting recent conversation context.
- **Streaming**: `answer_stream()` for real-time output.

**Steal readiness**: ⭐⭐⭐⭐⭐ High — Core pattern (agentic tool loop for query answering) is directly portable. Requires: tool executor, search_memory, get_reasoning_chain, and a system prompt.

---

## 2. Deriver (Observation Extraction from Messages)

**What it is**: Batch processor that takes message chunks and uses a single LLM call to extract explicit atomic facts about a peer. Output is structured JSON (Pydantic) → stored as `Document` records with embeddings.

**Key files**:

- `src/deriver/deriver.py` — `process_representation_tasks_batch()` (322 lines)
- `src/deriver/prompts.py` — Minimal prompt (116 lines)
- `src/deriver/consumer.py` — Queue consumer

**Data flow**:

```
Batch of messages → format_new_turn_with_timestamp()
  → minimal_deriver_prompt(peer_id, messages, custom_instructions)
  → honcho_llm_call(response_model=PromptRepresentation, json_mode=True)
  → Representation.from_prompt_representation() → [ExplicitObservation, ...]
  → For each observer: RepresentationManager.save_representation()
    → Create Document records with embeddings
```

**Key design decisions**:

- **Single LLM call**: One call per batch, not per message. Speed-optimized.
- **JSON mode**: Uses `response_model=PromptRepresentation` for structured output.
- **Multi-observer**: Saves to all observer collections (one-to-many).
- **Custom instructions**: Per-peer instructions injected into prompt.
- **Queue-based**: Consumer processes from a queue, enabling async batch processing.
- **Token accounting**: Tracks input/output tokens, prompt scaffolding, message tokens separately.

**Prompt**: Simple, ~15 lines. Focus: "Extract explicit atomic facts about target peer from messages."

**Steal readiness**: ⭐⭐⭐⭐⭐ High — Very portable. Core: prompt + JSON-structured LLM output + embedding + DB storage.

---

## 3. Dreamer (Background Inductive/Deductive Reasoning)

**What it is**: Background process that runs "dream cycles" — autonomous agents that explore the observation space and create higher-level insights (deductive and inductive observations) from existing data.

**Key files**:

- `src/dreamer/orchestrator.py` — `run_dream()` (411 lines)
- `src/dreamer/specialists.py` — `DeductionSpecialist`, `InductionSpecialist` (770 lines)
- `src/dreamer/surprisal.py` — Surprisal-based sampling (492 lines)
- `src/dreamer/dream_scheduler.py` — Scheduling logic
- `src/dreamer/trees.py` — k-d tree for surprisal computation

**Data flow**:

```
Dream cycle:
  0. [Optional] Surprisal sampling:
     - Fetch observations → build k-d tree → compute surprisal scores
     - Return top N% most "surprising" (anomalous/novel) observations
     - Convert to exploration hints for specialists
  1. Deduction specialist:
     - System prompt: "You are a deductive reasoning agent"
     - User prompt: exploration hints (optional) + peer card
     - Tool loop: search → create_observations_deductive → delete_observations → update_peer_card
     - Creates: "logical implications", "knowledge updates", "contradictions"
  2. Induction specialist:
     - System prompt: "You are an inductive reasoning agent"
     - Tool loop: search → create_observations_inductive
     - Creates: "behavioral patterns", "preferences", "personality traits"
```

**Deduction Specialist** (12 iterations, 8192 max tokens):

- Discovers: knowledge updates (same fact, different values), logical implications, contradictions
- Tools: `get_recent_observations`, `search_memory`, `search_messages`, `create_observations_deductive`, `delete_observations`, `update_peer_card`
- Peer card: YES — can update with stable identity markers

**Induction Specialist** (10 iterations, 8192 max tokens):

- Discovers: behavioral patterns, preferences, personality traits, temporal patterns
- Tools: `get_recent_observations`, `search_memory`, `search_messages`, `create_observations_inductive`
- Peer card: NO — behavioral patterns stay as observations

**Surprisal**: Uses k-d tree on embeddings to find anomalous observations. Sampling strategies: recent, random, all. Filters by TOP_PERCENT_SURPRISAL.

**Steal readiness**: ⭐⭐⭐⭐ High — The specialist pattern (autonomous agent with tool loop for observation creation) is directly portable. Surprisal requires numpy + k-d tree but is a nice-to-have.

---

## 4. Observation Levels & Reasoning Chain

**What it is**: A 4-level observation hierarchy with provenance tracking:

- **Explicit**: Direct facts from messages (deriver creates these)
- **Deductive**: Logical necessities from explicit facts (specialist creates these, requires `source_ids` + `premises`)
- **Inductive**: Patterns from multiple observations (specialist creates these, requires `source_ids` + `sources` + `pattern_type` + `confidence`)
- **Contradiction**: Conflicting statements (requires `source_ids` + `sources`)

**Key files**:

- `src/utils/representation.py` — `Representation`, `ExplicitObservation`, `DeductiveObservation`, `InductiveObservation`, `ContradictionObservation`
- `src/utils/agent_tools.py` — Observation schemas + tool definitions (2633 lines)
- `src/crud/representation.py` — `RepresentationManager`
- `src/crud/document.py` — Document CRUD with reasoning chain queries

**Data model**: Each observation/document has:

- `content`: The observation text
- `level`: explicit | deductive | inductive | contradiction
- `source_ids`: Links to parent observations (required for non-explicit)
- `premises`: Human-readable premise text (deductive only)
- `sources`: Human-readable source text (inductive/contradiction only)
- `pattern_type`: preference | behavior | personality | tendency | correlation (inductive only)
- `confidence`: high | medium | low (inductive only)
- `embedding`: Vector for semantic search
- `message_ids`: Source messages

**Reasoning chain**: `get_reasoning_chain(observation_id, direction)` traverses:

- `premises` direction: fetches source observations
- `conclusions` direction: fetches child observations (where this is a source)

**Steal readiness**: ⭐⭐⭐⭐ High — The level hierarchy + provenance model is a core architectural pattern. The Pydantic models and CRUD are portable.

---

## 5. Peer Card System

**What it is**: LLM-maintained biographical fact store with 4 strict entry types:

- `IDENTITY:` — canonical name, kind, aliases, IDs
- `ATTRIBUTE:` — stable durable property (location, language, standing preferences)
- `RELATIONSHIP:` — durable link to another entity
- `INSTRUCTION:` — standing rule of engagement (explicitly stated by observed)

**Key files**:

- `src/utils/agent_tools.py` — `update_peer_card` tool + validation (lines 490-520, 1466-1626)
- `src/crud/peer_card.py` — CRUD operations
- `src/dialectic/prompts.py` — Peer card injection in system prompt
- `src/dreamer/specialists.py` — Deduction specialist peer card updates

**Key design decisions**:

- Max 40 entries, 200 chars per entry
- Structural validation: must start with allowed prefix
- Case-insensitive deduplication
- Rejection feedback: LLM gets examples of rejected entries to self-correct
- Migration support: legacy entries auto-migrated to new format

**Steal readiness**: ⭐⭐⭐⭐ High — Self-contained system. The validation logic + tool + CRUD are portable.

---

## 6. Session Summarizer

**What it is**: Creates rolling short/long summaries of conversation sessions. Summaries are incremental — each new summary includes content from the previous summary + new messages.

**Key files**:

- `src/utils/summarizer.py` — Full summarization logic (966 lines)

**Data flow**:

```
New message → summarize_if_needed()
  → Check thresholds (every N messages)
  → Get latest summary from session.internal_metadata
  → Get messages since latest summary
  → create_short_summary() or create_long_summary()
    → Prompt: previous_summary + new_messages → LLM → new_summary
  → Save to session.internal_metadata["summaries"][type]
```

**Key design decisions**:

- **Incremental**: Each summary folds in the previous one (not just new messages)
- **Two tiers**: Short (faster, less detail) and Long (comprehensive)
- **Word limit**: Output constrained to configurable word count
- **Fallback**: If LLM returns empty, use basic "N messages about X"
- **Session context**: `get_session_context()` combines latest summary + recent messages for API responses

**Steal readiness**: ⭐⭐⭐⭐ High — Simple pattern: prompt template + incremental update + DB storage.

---

## 7. Agent Tool System

**What it is**: A unified tool executor framework with 18 tools, shared across all agents (dialectic, deriver, dreamer, specialists). Each tool is a handler function + schema definition.

**Key files**:

- `src/utils/agent_tools.py` — All tool definitions and handlers (2633 lines)

**Tool categories**:
| Category | Tools |
|----------|-------|
| Observation creation | `create_observations`, `create_observations_deductive`, `create_observations_inductive` |
| Peer card | `update_peer_card`, `get_peer_card` |
| Memory search | `search_memory` (semantic), `grep_messages` (exact text) |
| Message search | `search_messages`, `search_messages_temporal`, `get_messages_by_date_range` |
| Context | `get_recent_history`, `get_observation_context`, `get_session_summary` |
| Observation management | `get_recent_observations`, `get_most_derived_observations`, `delete_observations` |
| Reasoning | `get_reasoning_chain` |
| Consolidation | `finish_consolidation`, `extract_preferences` |

**Key design decisions**:

- **Unified executor**: `create_tool_executor()` returns a callable that dispatches to `_TOOL_HANDLERS`
- **ToolContext**: Shared state (workspace, observer, observed, session, DB lock, telemetry)
- **Short-lived DB sessions**: Every handler uses `tracked_db()` for auto-cleanup
- **Concurrency**: `asyncio.Lock` per workspace/observer/observed prevents race conditions
- **Telemetry**: Every tool call emits `AgentToolCallCompletedEvent`
- **Truncation**: Output capped at `MAX_TOOL_OUTPUT_CHARS` with signal in metadata
- **Tool sets per agent**: `DIALECTIC_TOOLS`, `DIALECTIC_TOOLS_MINIMAL`, `DREAMER_TOOLS`, `DEDUCTION_SPECIALIST_TOOLS`, `INDUCTION_SPECIALIST_TOOLS`

**Steal readiness**: ⭐⭐⭐⭐⭐ High — The tool system is the backbone. The most directly portable piece.

---

## 8. LLM API & Tool Loop

**What it is**: The core LLM calling infrastructure with retry, tool execution loop, and telemetry.

**Key files**:

- `src/llm/api.py` — `honcho_llm_call()` entrypoint
- `src/llm/tool_loop.py` — Multi-iteration tool execution loop

**Data flow**:

```
honcho_llm_call(model, prompt, tools=..., tool_executor=..., max_tool_iterations=N)
  → Build messages (system prompt + conversation)
  → Loop:
    - LLM generates response with potential tool calls
    - For each tool call: tool_executor(tool_name, tool_input) → result
    - Append tool results to messages
    - Repeat until no more tool calls or max_iterations reached
  → Return response + metadata (tokens, iterations, tool_calls)
```

**Steal readiness**: ⭐⭐⭐⭐ High — Depends on litellm/AsyncOpenAI, but the tool loop pattern is portable.

---

## Top 5 Ranked Steal Candidates

### #1: Deriver (Observation Extraction) — Score: 9.5/10

**Why**: Simplest to implement, highest ROI. Single LLM call, structured JSON output, direct value (turns raw messages into searchable facts).
**Effort**: Low (1-2 days)
**Dependencies**: JSON-mode LLM, embedding client, DB storage
**Honcho files**: `deriver/deriver.py`, `deriver/prompts.py`, `utils/representation.py`

### #2: Agent Tool System (18 Tools) — Score: 9/10

**Why**: Foundation for everything else. Enables agentic behavior across all features. The unified tool executor pattern is elegant and reusable.
**Effort**: Medium (2-3 days)
**Dependencies**: LLM with tool calling, DB access, embedding client
**Honcho files**: `utils/agent_tools.py` (entire file)

### #3: Dialectic Memory (Query-Time Agentic Reasoning) — Score: 8.5/10

**Why**: Differentiated UX — answers are grounded in memory, not just the conversation. The agentic tool loop gives the LLM freedom to search strategically.
**Effort**: Medium-High (3-4 days)
**Dependencies**: Tool system, embedding client, reasoning chain support
**Honcho files**: `dialectic/core.py`, `dialectic/prompts.py`

### #4: Peer Card System — Score: 8/10

**Why**: Compact, high-value identity store. The 4-prefix format + validation + LLM management is self-contained.
**Effort**: Low-Medium (1-2 days)
**Dependencies**: DB storage, LLM tool calling
**Honcho files**: `utils/agent_tools.py` (update_peer_card handler), `crud/peer_card.py`

### #5: Session Summarizer (Incremental) — Score: 7.5/10

**Why**: Simple but effective. Reduces context window waste by summarizing old conversation. The incremental approach (fold in previous summary) is the key insight.
**Effort**: Low (1 day)
**Dependencies**: LLM, DB storage for session metadata
**Honcho files**: `utils/summarizer.py`

---

## Bonus: Lower Priority but Interesting

### Dreamer (Background Specialists) — Score: 7/10

**Why**: The deduction/induction specialist pattern is powerful but complex. Start with derivation (deriver) first, add background reasoning later.
**Effort**: High (4-5 days)
**Honcho files**: `dreamer/orchestrator.py`, `dreamer/specialists.py`, `dreamer/surprisal.py`

### Surprisal-Based Sampling — Score: 6/10

**Why**: Cool research idea but niche. k-d tree + numpy dependency adds complexity.
**Effort**: Medium (2 days)
**Honcho files**: `dreamer/surprisal.py`, `dreamer/trees.py`

---

## Prompt Files to Read in Full

| File                         | Lines   | Purpose                                               |
| ---------------------------- | ------- | ----------------------------------------------------- |
| `src/dialectic/prompts.py`   | 237     | Dialectic agent system prompt (very detailed)         |
| `src/deriver/prompts.py`     | 116     | Minimal deriver prompt (observation extraction)       |
| `src/dreamer/specialists.py` | 549-763 | Deduction + Induction specialist prompts (~215 lines) |
| `src/utils/summarizer.py`    | 101-164 | Short + Long summary prompts (~64 lines)              |

---

## Architecture Summary

```
                          ┌─────────────────────┐
                          │    User Message      │
                          └──────────┬──────────┘
                                     │
                    ┌────────────────┼────────────────┐
                    │                │                 │
                    ▼                ▼                 ▼
           ┌──────────────┐  ┌───────────────┐  ┌────────────┐
           │   Deriver    │  │  Dialectic    │  │  Dreamer   │
           │  (ingest)    │  │  (query-time) │  │(background)│
           └──────┬───────┘  └───────┬───────┘  └─────┬──────┘
                  │                  │                  │
                  ▼                  ▼                  ▼
           ┌──────────────────────────────────────────────────┐
           │           Observation Store (Documents)           │
           │  ┌─────────┬──────────┬──────────┬─────────────┐ │
           │  │ Explicit│ Deductive│ Inductive│ Contradiction│ │
           │  └─────────┴──────────┴──────────┴─────────────┘ │
           └──────────────────────┬───────────────────────────┘
                                  │
              ┌───────────────────┼───────────────────┐
              │                   │                   │
              ▼                   ▼                   ▼
       ┌─────────────┐   ┌──────────────┐   ┌──────────────┐
       │ search_memory│   │ get_reason_  │   │ get_peer_    │
       │ (semantic)  │   │ ing_chain    │   │ card         │
       └─────────────┘   └──────────────┘   └──────────────┘
```

**Key insight**: Honcho's memory system is a **pipeline**:

1. **Deriver** extracts explicit facts from messages
2. **Dreamer** creates deductive/inductive insights from those facts
3. **Dialectic** answers queries using all levels of memory
4. **Peer Card** maintains a compact identity summary
5. **Summarizer** compresses conversation history

Each layer feeds the next, creating a rich, multi-level memory graph.
