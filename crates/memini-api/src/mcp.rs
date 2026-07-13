use axum::{
    Extension, Json,
    extract::State,
    http::{HeaderMap, HeaderValue, StatusCode},
    response::{
        IntoResponse, Response, Sse,
        sse::{Event, KeepAlive},
    },
};
use memini_core::{
    memory::{Memory, Tier},
    search::Scored,
};
use memini_service::{
    AnswerInput, BriefingOptions, ListInput, Origin, ReadSetEntry, RecallInput, RememberInput,
};
use serde::Deserialize;
use serde_json::{Value, json};
use std::{convert::Infallible, time::Duration};

use crate::{ApiJson, RequestScope, StateData};

#[derive(Deserialize)]
pub(crate) struct RpcRequest {
    #[serde(default)]
    id: Value,
    method: String,
    #[serde(default)]
    params: Value,
}
pub const SERVER_INSTRUCTIONS: &str = r#"memini is persistent cross-session memory for this agent. Namespaces are managed for you — you never construct or type a raw namespace path; you make semantic choices (scope to read, visibility to write) and learn the topology by reading provenance. Standing policy:
- At session start, call memory_briefing once to orient (pinned context, durable facts, how-tos, recent activity, and the inherited Scope line). Prefer it over broad recall queries.
- Before work that may have history, call memory_recall first. scope is "project", "full" (default: project plus ancestors, personal namespace, and links), or "everywhere" (full plus nested sub-projects).
- After learning something durable, call memory_remember proactively: one atomic, self-contained fact per call. Don't store what's already in project docs/CLAUDE.md or trivially recoverable from code.
- visibility decides who should know: "project", "personal", or an ancestor name read from the briefing Scope line. Durable shared facts go up; personal preferences go personal; episodic/working writes always stay in the project.
- tier: semantic = durable fact, procedural = how-to/command, episodic = what happened, working = scratch. Omit to auto-classify.
- Every recall/briefing result carries provenance. Read "from" and the Scope line; never guess namespace paths.
- Tag critical facts "pinned" and set metadata.category to a topic bucket.
- Correct or extend facts with memory_update. memory_forget is only for memories that should not exist. Copy addressing namespaces verbatim from results.
- Empty recall means nothing is known. A degraded field means results are keyword-only and incomplete, not a confident negative."#;
fn ok(id: Value, result: Value) -> Json<Value> {
    Json(json!({"jsonrpc":"2.0","id":id,"result":result}))
}
fn err(id: Value, code: i64, message: &str) -> Json<Value> {
    Json(json!({"jsonrpc":"2.0","id":id,"error":{"code":code,"message":message}}))
}
fn tool(
    name: &str,
    title: &str,
    description: &str,
    properties: Value,
    required: &[&str],
    read_only: bool,
    destructive: bool,
) -> Value {
    json!({"name":name,"title":title,"description":description,"inputSchema":{"type":"object","properties":properties,"required":required,"additionalProperties":false},"annotations":{"readOnlyHint":read_only,"destructiveHint":destructive,"openWorldHint":false}})
}
pub fn tool_definitions(answer: bool) -> Vec<Value> {
    let mut out = vec![
        tool(
            "memory_remember",
            "Remember a fact",
            "Store a fact, decision, preference, or event for later recall. Call proactively when the user says 'remember this', after an architectural decision (capture the why), after discovering a non-obvious bug or convention, or when a stated preference should outlive this session. Keep memories atomic — one self-contained fact per call; search works better on small records. Do NOT store facts already in project docs/CLAUDE.md or trivially recoverable from code. tier: semantic=durable fact, procedural=how-to, episodic=event, working=scratch (default intake, omit to auto-classify). visibility: 'project' (default) keeps it here; 'personal' follows the user everywhere; or name an ancestor from the briefing Scope line to share it up that chain — episodic/working writes always stay in project regardless. If the result carries merge_hint, call memory_update with id=merge_hint.similar_id to fold them together, or ignore it to keep both. Returns {id, tier, stored}; stored=false means a low-signal write was dropped by the value gate (not an error).",
            json!({
                "content":{"type":"string","description":"the fact to store — atomic and self-contained, readable without this conversation's context"},
                "tier":{"type":"string","enum":["working","episodic","semantic","procedural"],"description":"working, episodic, semantic, or procedural (omit to auto-classify)"},
                "level":{"type":"string","enum":["explicit","deduced"],"description":"provenance: explicit = user-stated, deduced = LLM-inferred; omit to leave unset"},
                "summary":{"type":"string","description":"optional one-line summary"},
                "tags":{"type":"array","items":{"type":"string"},"description":"topic labels for filtering and keyword search; tag a critical always-relevant fact 'pinned' so it surfaces in every session briefing"},
                "metadata":{"type":"object","description":"structured key/values for later filtering; set 'category' to a topic bucket"},
                "importance":{"type":"number","minimum":0,"maximum":1,"description":"0..1 ranking/retention bias; omit for the default"},
                "ttl_seconds":{"type":"integer","description":"overrides the tier default TTL; negative means never expire"},
                "id":{"type":"string","description":"upserts an existing memory when provided"},
                "confidence":{"type":"number","minimum":0,"maximum":1,"description":"0..1 seed corroboration for a durable fact; omit for default"},
                "valid_from":{"type":"string","format":"date-time","description":"RFC3339 start of the fact's validity; backdate for as_of recall"},
                "valid_to":{"type":"string","format":"date-time","description":"RFC3339 end of the fact's validity; omit if still true"},
                "visibility":{"type":"string","description":"who should remember this: project (default), personal, or an ancestor name from the briefing Scope line; episodic/working writes always stay in project"}
            }),
            &["content"],
            false,
            false,
        ),
        tool(
            "memory_recall",
            "Recall memories",
            "Search prior context via hybrid semantic and keyword retrieval, ranked by relevance, recency, and corroboration. Call BEFORE starting work that may have history. Prefer a short descriptive query. Empty results mean nothing is known; a degraded field means semantic search was unavailable and results are keyword-only. scope is project, full (default), or everywhere. Supports time-travel with as_of and optional LLM query rewriting. Each result's namespace is provenance: copy it verbatim into memory_get/update/forget.",
            json!({
                "query":{"type":"string","description":"natural-language search text; short and descriptive works best"},
                "limit":{"type":"integer","description":"max results (default 10)"},
                "tiers":{"type":"array","items":{"type":"string","enum":["working","episodic","semantic","procedural"]},"description":"restrict to tiers; empty means all"},
                "levels":{"type":"array","items":{"type":"string","enum":["explicit","deduced"]},"description":"restrict to levels; empty means all"},
                "tags":{"type":"array","items":{"type":"string"},"description":"only memories carrying every listed tag (AND)"},
                "metadata":{"type":"object","description":"only memories whose metadata has each key=value pair (AND)"},
                "exclude_metadata":{"type":"object","description":"drops memories matching each supplied key=value pair"},
                "include_fresh_turns":{"type":"boolean","description":"also return this session's just-captured conversation turns"},
                "query_rewrite":{"type":"boolean","description":"rewrite into variants and fuse via RRF"},
                "scope":{"type":"string","enum":["project","full","everywhere"],"description":"how wide to read: project, full (default), or everywhere"},
                "as_of":{"type":"string","format":"date-time","description":"RFC3339 time for time-travel recall"},
                "response_format":{"type":"string","enum":["concise","detailed"],"description":"concise returns summary-or-truncated content; detailed returns full content"}
            }),
            &["query"],
            true,
            false,
        ),
        tool(
            "memory_briefing",
            "Session briefing",
            "Layered session-start briefing for this namespace — pinned context, durable facts, how-to procedures, and recent activity — in one query-less call. Call it when a session opens. The scope_header spells out the inherited ancestor chain. scope='everywhere' also includes compact direct-child rollups.",
            json!({"scope":{"type":"string","enum":["project","full","everywhere"],"description":"how wide to brief: project, full (default), or everywhere"},"per_section":{"type":"integer","description":"default cap for a section whose dedicated cap is unset (default 5)"},"per_section_pinned":{"type":"integer","description":"max pinned memories; 0 disables"},"per_section_facts":{"type":"integer","description":"max durable semantic facts; 0 disables"},"per_section_procedures":{"type":"integer","description":"max procedural memories; 0 disables"},"per_section_recent":{"type":"integer","description":"max recent episodic entries; 0 disables"}}),
            &[],
            true,
            false,
        ),
        tool(
            "memory_list",
            "Browse memories",
            "Browse memories without a query — filter by tier, tags, or metadata. Returns at most limit (default 20) newest-first; page with offset. For relevance-ranked search use memory_recall.",
            json!({"namespace":{"type":"string","description":"tenant namespace; defaults to the server namespace"},"limit":{"type":"integer","description":"max results (default 20, newest first)"},"offset":{"type":"integer","description":"skip this many results for paging"},"tiers":{"type":"array","items":{"type":"string","enum":["working","episodic","semantic","procedural"]},"description":"restrict to tiers; empty means all"},"levels":{"type":"array","items":{"type":"string","enum":["explicit","deduced"]},"description":"restrict to levels; empty means all"},"tags":{"type":"array","items":{"type":"string"},"description":"only memories carrying every listed tag (AND)"},"metadata":{"type":"object","description":"only memories whose metadata has each key=value pair (AND)"}}),
            &[],
            true,
            false,
        ),
        tool(
            "memory_get",
            "Get a memory",
            "Fetch one memory with full metadata, tags, and timestamps by ID.",
            json!({"id":{"type":"string","description":"the memory ID"},"namespace":{"type":"string","description":"tenant namespace; defaults to the server namespace"}}),
            &["id"],
            true,
            false,
        ),
        tool(
            "memory_history",
            "Trace a memory's history",
            "Trace a memory's bi-temporal supersession lineage, oldest-first, including tombstoned rows.",
            json!({"id":{"type":"string","description":"the memory ID"},"namespace":{"type":"string","description":"tenant namespace; defaults to the server namespace"}}),
            &["id"],
            true,
            false,
        ),
        tool(
            "memory_update",
            "Update a memory",
            "Update fields of an existing memory by ID. Only provided fields change and metadata merges key-by-key. Use it to correct or enrich a fact; to delete, use memory_forget.",
            json!({"id":{"type":"string","description":"the memory ID to update"},"namespace":{"type":"string","description":"tenant namespace; defaults to the server namespace"},"content":{"type":"string","description":"replacement content; omit to keep"},"summary":{"type":"string","description":"replacement summary; omit to keep"},"tags":{"type":"array","items":{"type":"string"},"description":"replacement tag set; omit to keep"},"metadata":{"type":"object","description":"merged into existing metadata key-by-key"},"tier":{"type":"string","enum":["working","episodic","semantic","procedural"],"description":"move to this tier; omit to keep"},"importance":{"type":"number","minimum":0,"maximum":1,"description":"0..1; omit to keep"},"confidence":{"type":"number","minimum":0,"maximum":1,"description":"0..1; omit to keep"}}),
            &["id"],
            false,
            false,
        ),
        tool(
            "memory_forget",
            "Forget a memory",
            "Permanently delete a memory by ID — use for wrong, outdated, or unwanted memories. To correct a fact instead, prefer memory_update so history is preserved.",
            json!({"id":{"type":"string","description":"the memory ID"},"namespace":{"type":"string","description":"tenant namespace; defaults to the server namespace"}}),
            &["id"],
            false,
            true,
        ),
    ];
    if answer {
        out.push(tool("memory_answer","Answer from memory","Recall relevant memories and answer a question grounded on them (requires an LLM). Slower than memory_recall; use when you want a synthesized answer with sources.",json!({"query":{"type":"string","description":"the question to answer from memory"},"limit":{"type":"integer","description":"max memories to ground on (default 10)"},"tiers":{"type":"array","items":{"type":"string","enum":["working","episodic","semantic","procedural"]},"description":"restrict grounding to tiers"},"levels":{"type":"array","items":{"type":"string","enum":["explicit","deduced"]},"description":"restrict grounding to levels"},"tags":{"type":"array","items":{"type":"string"},"description":"ground only on memories with every listed tag (AND)"},"metadata":{"type":"object","description":"ground only on memories whose metadata has each key=value pair (AND)"},"scope":{"type":"string","enum":["project","full","everywhere"],"description":"how wide to ground: project, full (default), or everywhere"},"reasoning_level":{"type":"string","enum":["minimal","low","medium","high"],"description":"effort; higher means iterative search and more latency"}}),&["query"],true,false));
    }
    out
}
fn result(value: Value) -> Value {
    json!({"content":[{"type":"text","text":serde_json::to_string(&value).unwrap()}],"structuredContent":value,"isError":false})
}
fn from(namespace: &str, read_set: &[ReadSetEntry]) -> Option<String> {
    read_set
        .iter()
        .find(|entry| entry.namespace == namespace)
        .and_then(|entry| match entry.origin {
            Origin::Ancestor | Origin::Home => Some(entry.namespace.clone()),
            Origin::Link => Some(format!("link:{}", entry.namespace)),
            Origin::Call => Some(format!("call:{}", entry.namespace)),
            _ => None,
        })
}
fn mcp_time(value: chrono::DateTime<chrono::Utc>) -> String {
    value.format("%Y-%m-%dT%H:%M:%SZ").to_string()
}
fn recall_item(memory: Memory, score: f64, read_set: &[ReadSetEntry]) -> Value {
    let mut value = json!({
        "id":memory.id,
        "content":memory.content,
        "tier":format!("{:?}",memory.tier).to_lowercase(),
        "namespace":memory.namespace,
        "score":score,
        "created_at":mcp_time(memory.created_at),
    });
    if let Some(level) = memory.level {
        value["level"] = Value::String(format!("{level:?}").to_lowercase());
    }
    if let Some(confidence) = memory.confidence {
        value["confidence"] = json!(confidence);
    }
    if !memory.tags.is_empty() {
        value["tags"] = json!(memory.tags);
    }
    if let Some(origin) = from(value["namespace"].as_str().unwrap_or(""), read_set) {
        value["from"] = Value::String(origin);
    }
    value
}
fn scored_items(results: Vec<Scored>, read_set: &[ReadSetEntry]) -> Vec<Value> {
    results
        .into_iter()
        .map(|item| recall_item(item.memory, item.score, read_set))
        .collect()
}
fn memory_item(memory: Memory) -> Value {
    let mut value = json!({
        "id":memory.id,
        "content":memory.content,
        "tier":format!("{:?}",memory.tier).to_lowercase(),
        "importance":memory.importance,
        "created_at":mcp_time(memory.created_at),
        "updated_at":mcp_time(memory.updated_at),
        "access_count":memory.access_count,
    });
    if let Some(level) = memory.level {
        value["level"] = Value::String(format!("{level:?}").to_lowercase());
    }
    if !memory.summary.is_empty() {
        value["summary"] = Value::String(memory.summary);
    }
    if !memory.tags.is_empty() {
        value["tags"] = json!(memory.tags);
    }
    if !memory.metadata.is_empty() {
        value["metadata"] = Value::Object(memory.metadata);
    }
    if let Some(time) = memory.expires_at {
        value["expires_at"] = Value::String(mcp_time(time));
    }
    if let Some(time) = memory.valid_from {
        value["valid_from"] = Value::String(mcp_time(time));
    }
    if let Some(time) = memory.valid_to {
        value["valid_to"] = Value::String(mcp_time(time));
    }
    if let Some(id) = memory.superseded_by {
        value["superseded_by"] = Value::String(id);
    }
    value
}
fn tier(value: Option<&Value>) -> Option<Tier> {
    value.and_then(Value::as_str).and_then(|v| match v {
        "working" => Some(Tier::Working),
        "episodic" => Some(Tier::Episodic),
        "semantic" => Some(Tier::Semantic),
        "procedural" => Some(Tier::Procedural),
        _ => None,
    })
}
fn checked_tier(value: Option<&Value>) -> Result<Option<Tier>, String> {
    if value
        .and_then(Value::as_str)
        .is_some_and(|value| !value.is_empty())
    {
        tier(value).map(Some).ok_or_else(|| "invalid tier".into())
    } else {
        Ok(None)
    }
}
fn tiers(value: Option<&Value>) -> Vec<Tier> {
    value.and_then(Value::as_array).map_or_else(Vec::new, |v| {
        v.iter().filter_map(|v| tier(Some(v))).collect()
    })
}
fn checked_tiers(value: Option<&Value>) -> Result<Vec<Tier>, String> {
    let parsed = tiers(value);
    let supplied = value.and_then(Value::as_array).map_or(0, Vec::len);
    if parsed.len() == supplied {
        Ok(parsed)
    } else {
        Err("tiers contains an invalid tier".into())
    }
}
fn levels(value: Option<&Value>) -> Vec<memini_core::memory::Level> {
    value.and_then(Value::as_array).map_or_else(Vec::new, |v| {
        v.iter()
            .filter_map(Value::as_str)
            .filter_map(|value| match value {
                "explicit" => Some(memini_core::memory::Level::Explicit),
                "deduced" => Some(memini_core::memory::Level::Deduced),
                _ => None,
            })
            .collect()
    })
}
fn checked_levels(value: Option<&Value>) -> Result<Vec<memini_core::memory::Level>, String> {
    let parsed = levels(value);
    let supplied = value.and_then(Value::as_array).map_or(0, Vec::len);
    if parsed.len() == supplied {
        Ok(parsed)
    } else {
        Err("levels contains an invalid level".into())
    }
}
fn level(value: Option<&Value>) -> Option<memini_core::memory::Level> {
    value.and_then(Value::as_str).and_then(|value| match value {
        "explicit" => Some(memini_core::memory::Level::Explicit),
        "deduced" => Some(memini_core::memory::Level::Deduced),
        _ => None,
    })
}
fn strings(value: Option<&Value>) -> Vec<String> {
    value.and_then(Value::as_array).map_or_else(Vec::new, |v| {
        v.iter()
            .filter_map(Value::as_str)
            .map(str::to_owned)
            .collect()
    })
}
fn object(value: Option<&Value>) -> serde_json::Map<String, Value> {
    value
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default()
}
fn timestamp(
    value: Option<&Value>,
    name: &str,
) -> Result<Option<chrono::DateTime<chrono::Utc>>, String> {
    let Some(value) = value.and_then(Value::as_str).filter(|v| !v.is_empty()) else {
        return Ok(None);
    };
    chrono::DateTime::parse_from_rfc3339(value)
        .map(|v| Some(v.with_timezone(&chrono::Utc)))
        .map_err(|_| format!("{name} must be RFC3339"))
}

pub(crate) async fn handler(
    State(state): State<StateData>,
    Extension(request_scope): Extension<RequestScope>,
    headers: HeaderMap,
    ApiJson(request): ApiJson<RpcRequest>,
) -> Response {
    let id = request.id.clone();
    if request.method.starts_with("notifications/") {
        return StatusCode::ACCEPTED.into_response();
    }
    let initialize = request.method == "initialize";
    let scope = if initialize {
        request_scope.clone()
    } else {
        let Some(session) = session_id(&headers) else {
            return StatusCode::NOT_FOUND.into_response();
        };
        let Ok(sessions) = state.mcp_sessions.lock() else {
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        };
        let Some(scope) = sessions.get(session).cloned() else {
            return StatusCode::NOT_FOUND.into_response();
        };
        scope
    };
    let body = match request.method.as_str() {
        "initialize" => ok(
            id,
            json!({"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":false}},"serverInfo":{"name":"memini","version":crate::VERSION},"instructions":SERVER_INSTRUCTIONS}),
        ),
        "notifications/initialized" => ok(id, Value::Null),
        "ping" => ok(id, json!({})),
        "tools/list" => ok(
            id,
            json!({"tools":tool_definitions(state.service.has_answerer())}),
        ),
        "tools/call" => {
            let name = request
                .params
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or("");
            let args = request
                .params
                .get("arguments")
                .cloned()
                .unwrap_or_else(|| json!({}));
            match call(&state, &scope, name, args).await {
                Ok(value) => ok(id, result(value)),
                Err(message) => ok(
                    id,
                    json!({"content":[{"type":"text","text":message}],"isError":true}),
                ),
            }
        }
        _ => err(id, -32601, "method not found"),
    };
    let mut response = body.into_response();
    if initialize
        && let Ok(secret) = memini_auth::generate_secret()
        && let Ok(value) = HeaderValue::from_str(&secret)
    {
        if let Ok(mut sessions) = state.mcp_sessions.lock() {
            sessions.insert(secret, request_scope);
        }
        response.headers_mut().insert("mcp-session-id", value);
    }
    response
}

pub(crate) async fn stream(State(state): State<StateData>, headers: HeaderMap) -> Response {
    let Some(session) = session_id(&headers) else {
        return StatusCode::NOT_FOUND.into_response();
    };
    if !state
        .mcp_sessions
        .lock()
        .is_ok_and(|sessions| sessions.contains_key(session))
    {
        return StatusCode::NOT_FOUND.into_response();
    }
    Sse::new(futures_util::stream::pending::<Result<Event, Infallible>>())
        .keep_alive(
            KeepAlive::new()
                .interval(Duration::from_secs(15))
                .text("keepalive"),
        )
        .into_response()
}

pub(crate) async fn delete_session(
    State(state): State<StateData>,
    headers: HeaderMap,
) -> StatusCode {
    let Some(session) = session_id(&headers) else {
        return StatusCode::NOT_FOUND;
    };
    match state.mcp_sessions.lock() {
        Ok(mut sessions) => {
            if sessions.remove(session).is_some() {
                StatusCode::NO_CONTENT
            } else {
                StatusCode::NOT_FOUND
            }
        }
        Err(_) => StatusCode::INTERNAL_SERVER_ERROR,
    }
}

fn session_id(headers: &HeaderMap) -> Option<&str> {
    headers
        .get("mcp-session-id")
        .and_then(|value| value.to_str().ok())
        .filter(|value| !value.is_empty())
}

async fn call(
    state: &StateData,
    scope: &RequestScope,
    name: &str,
    args: Value,
) -> Result<Value, String> {
    let namespace = args
        .get("namespace")
        .and_then(Value::as_str)
        .unwrap_or(&scope.namespace)
        .to_owned();
    memini_auth::validate_namespace(&namespace).map_err(|e| format!("invalid namespace: {e}"))?;
    match name {
        "memory_remember" => {
            let content = args
                .get("content")
                .and_then(Value::as_str)
                .ok_or("content is required")?;
            let outcome = state
                .service
                .remember_with_outcome(RememberInput {
                    namespace,
                    home: scope.home.clone(),
                    content: content.into(),
                    tier: checked_tier(args.get("tier"))?,
                    level: level(args.get("level")),
                    summary: args
                        .get("summary")
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .into(),
                    tags: strings(args.get("tags")),
                    metadata: object(args.get("metadata")),
                    importance: args.get("importance").and_then(Value::as_f64),
                    ttl: args
                        .get("ttl_seconds")
                        .and_then(Value::as_i64)
                        .map(chrono::Duration::seconds),
                    id: args.get("id").and_then(Value::as_str).unwrap_or("").into(),
                    confidence: args.get("confidence").and_then(Value::as_f64),
                    valid_from: timestamp(args.get("valid_from"), "valid_from")?,
                    valid_to: timestamp(args.get("valid_to"), "valid_to")?,
                    visibility: args
                        .get("visibility")
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .into(),
                    author: scope
                        .principal
                        .as_ref()
                        .map_or_else(String::new, |v| v.name.clone()),
                })
                .await
                .map_err(|e| e.to_string())?;
            Ok(outcome.memory.map_or_else(
                || json!({"stored":false,"reason":"low_signal"}),
                |m| {
                    let mut value = json!({"id":m.id,"tier":format!("{:?}",m.tier).to_lowercase(),"stored":true});
                    if let Some(hint) = outcome.merge_hint { value["merge_hint"] = serde_json::to_value(hint).unwrap(); }
                    if outcome.auto_superseded { value["auto_superseded"] = Value::Bool(true); }
                    if m.metadata.get("pending_embed").is_some() {
                        value["degraded"] = Value::String("pending_embed".into());
                        value["note"] = Value::String("embeddings unavailable; stored keyword-searchable only, vector will be backfilled automatically".into());
                    }
                    value
                },
            ))
        }
        "memory_recall" => {
            let query = args
                .get("query")
                .and_then(Value::as_str)
                .ok_or("query is required")?;
            let (mut hits, degraded, read_set) = state
                .service
                .recall(RecallInput {
                    namespace,
                    home: scope.home.clone(),
                    query: query.into(),
                    limit: args.get("limit").and_then(Value::as_u64).unwrap_or(0) as usize,
                    tiers: checked_tiers(args.get("tiers"))?,
                    levels: checked_levels(args.get("levels"))?,
                    tags: strings(args.get("tags")),
                    metadata: object(args.get("metadata")),
                    exclude_metadata: object(args.get("exclude_metadata")),
                    include_fresh_turns: args
                        .get("include_fresh_turns")
                        .and_then(Value::as_bool)
                        .unwrap_or(false),
                    query_rewrite: args
                        .get("query_rewrite")
                        .and_then(Value::as_bool)
                        .unwrap_or(false),
                    as_of: timestamp(args.get("as_of"), "as_of")?,
                    scope: args
                        .get("scope")
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .into(),
                    ..RecallInput::default()
                })
                .await
                .map_err(|e| e.to_string())?;
            if args.get("response_format").and_then(Value::as_str) == Some("concise") {
                for hit in &mut hits {
                    hit.memory.content = if !hit.memory.summary.is_empty() {
                        hit.memory.summary.clone()
                    } else if hit.memory.content.chars().count() > 240 {
                        hit.memory.content.chars().take(240).collect::<String>() + "…"
                    } else {
                        hit.memory.content.clone()
                    };
                }
            }
            let results = scored_items(hits, &read_set);
            let mut value = json!({"results":results});
            if let Some(reason) = degraded {
                value["degraded"] = Value::String("keyword_only".into());
                value["note"] = Value::String(format!(
                    "semantic search unavailable ({reason}); results are keyword-only and may be incomplete"
                ));
            }
            Ok(value)
        }
        "memory_briefing" => {
            let section = |name: &str| {
                args.get(name)
                    .and_then(Value::as_u64)
                    .map(|value| value as usize)
                    .or_else(|| {
                        args.get("per_section")
                            .and_then(Value::as_u64)
                            .map(|value| value as usize)
                    })
            };
            let (briefing, read_set) = state
                .service
                .briefing(
                    &namespace,
                    BriefingOptions {
                        home: scope.home.clone(),
                        scope: args
                            .get("scope")
                            .and_then(Value::as_str)
                            .unwrap_or("")
                            .into(),
                        pinned: section("per_section_pinned"),
                        facts: section("per_section_facts"),
                        procedures: section("per_section_procedures"),
                        recent: section("per_section_recent"),
                        ..BriefingOptions::default()
                    },
                )
                .await
                .map_err(|e| e.to_string())?;
            let mut value = json!({"namespace":briefing.namespace});
            if !briefing.scope_header.is_empty() {
                value["scope_header"] = Value::String(briefing.scope_header);
            }
            for (name, memories) in [
                ("pinned", briefing.pinned),
                ("facts", briefing.facts),
                ("procedures", briefing.procedures),
                ("recent", briefing.recent),
            ] {
                if !memories.is_empty() {
                    value[name] = Value::Array(
                        memories
                            .into_iter()
                            .map(|memory| {
                                let mut item = recall_item(memory, 0.0, &read_set);
                                item.as_object_mut().unwrap().remove("level");
                                item
                            })
                            .collect(),
                    );
                }
            }
            if !briefing.children.is_empty() {
                value["children"] = Value::Array(
                    briefing
                        .children
                        .into_iter()
                        .map(|child| {
                            let title = |memory: Memory| {
                                if !memory.summary.is_empty() {
                                    memory.summary
                                } else if memory.content.chars().count() > 60 {
                                    memory.content.chars().take(60).collect::<String>() + "…"
                                } else {
                                    memory.content
                                }
                            };
                            let pinned = child.pinned.into_iter().map(&title).collect::<Vec<_>>();
                            let recent = child.recent.into_iter().map(&title).collect::<Vec<_>>();
                            let mut item = json!({"namespace":child.namespace,"total":child.total});
                            if !pinned.is_empty() {
                                item["pinned"] = json!(pinned);
                            }
                            if !recent.is_empty() {
                                item["recent"] = json!(recent);
                            }
                            item
                        })
                        .collect(),
                );
            }
            if briefing.children_truncated > 0 {
                value["children_note"] = Value::String(format!(
                    "… and {} more child namespace{}",
                    briefing.children_truncated,
                    if briefing.children_truncated == 1 {
                        ""
                    } else {
                        "s"
                    }
                ));
            }
            Ok(value)
        }
        "memory_list" => {
            let memories = state
                .service
                .list(ListInput {
                    namespace,
                    tiers: checked_tiers(args.get("tiers"))?,
                    levels: checked_levels(args.get("levels"))?,
                    tags: strings(args.get("tags")),
                    metadata: object(args.get("metadata")),
                    limit: Some(
                        args.get("limit")
                            .and_then(Value::as_u64)
                            .filter(|value| *value > 0)
                            .unwrap_or(20) as usize,
                    ),
                    offset: args.get("offset").and_then(Value::as_u64).unwrap_or(0) as usize,
                    ..ListInput::default()
                })
                .await
                .map_err(|e| e.to_string())?;
            Ok(json!({"memories":memories.into_iter().map(memory_item).collect::<Vec<_>>() }))
        }
        "memory_get" => {
            let id = args
                .get("id")
                .and_then(Value::as_str)
                .ok_or("id is required")?;
            state
                .service
                .get(&namespace, id)
                .await
                .map(memory_item)
                .map_err(|e| e.to_string())
        }
        "memory_history" => {
            let id = args
                .get("id")
                .and_then(Value::as_str)
                .ok_or("id is required")?;
            state
                .service
                .history(&namespace, id)
                .await
                .map(|v| json!({"memories":v.into_iter().map(memory_item).collect::<Vec<_>>() }))
                .map_err(|e| e.to_string())
        }
        "memory_forget" => {
            let id = args
                .get("id")
                .and_then(Value::as_str)
                .ok_or("id is required")?;
            state
                .service
                .forget(&namespace, id)
                .await
                .map(|()| json!({"deleted":true}))
                .map_err(|e| e.to_string())
        }
        "memory_update" => {
            let id = args
                .get("id")
                .and_then(Value::as_str)
                .ok_or("id is required")?;
            let old = state
                .service
                .get(&namespace, id)
                .await
                .map_err(|e| e.to_string())?;
            let mut metadata = old.metadata;
            metadata.extend(
                args.get("metadata")
                    .and_then(Value::as_object)
                    .cloned()
                    .unwrap_or_default(),
            );
            let memory = state
                .service
                .remember(RememberInput {
                    namespace,
                    home: scope.home.clone(),
                    id: id.into(),
                    content: args
                        .get("content")
                        .and_then(Value::as_str)
                        .unwrap_or(&old.content)
                        .into(),
                    tier: checked_tier(args.get("tier"))?.or(Some(old.tier)),
                    summary: args
                        .get("summary")
                        .and_then(Value::as_str)
                        .unwrap_or(&old.summary)
                        .into(),
                    tags: args
                        .get("tags")
                        .and_then(Value::as_array)
                        .map_or(old.tags, |v| {
                            v.iter()
                                .filter_map(Value::as_str)
                                .map(str::to_owned)
                                .collect()
                        }),
                    metadata,
                    importance: Some(
                        args.get("importance")
                            .and_then(Value::as_f64)
                            .unwrap_or(old.importance),
                    ),
                    level: old.level,
                    confidence: args
                        .get("confidence")
                        .and_then(Value::as_f64)
                        .or(old.confidence),
                    valid_from: old.valid_from,
                    valid_to: old.valid_to,
                    author: scope
                        .principal
                        .as_ref()
                        .map_or_else(String::new, |value| value.name.clone()),
                    ..RememberInput::default()
                })
                .await
                .map_err(|e| e.to_string())?;
            memory
                .map(memory_item)
                .ok_or_else(|| "memory update was dropped as low signal".into())
        }
        "memory_answer" => {
            let query = args
                .get("query")
                .and_then(Value::as_str)
                .ok_or("query is required")?;
            let value = state
                .service
                .answer(AnswerInput {
                    namespace,
                    home: scope.home.clone(),
                    query: query.into(),
                    limit: args.get("limit").and_then(Value::as_u64).unwrap_or(0) as usize,
                    tiers: checked_tiers(args.get("tiers"))?,
                    levels: checked_levels(args.get("levels"))?,
                    tags: strings(args.get("tags")),
                    metadata: object(args.get("metadata")),
                    scope: args
                        .get("scope")
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .into(),
                    reasoning: args
                        .get("reasoning_level")
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .into(),
                })
                .await
                .map_err(|e| e.to_string())?;
            Ok(json!({"answer":value.answer,"sources":scored_items(value.sources,&value.read_set)}))
        }
        _ => Err(format!("unknown tool {name}")),
    }
}
