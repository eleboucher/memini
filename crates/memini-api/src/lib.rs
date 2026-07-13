use axum::{
    Extension, Json, Router,
    extract::{FromRequest, Path, Request, State},
    http::{HeaderMap, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, patch, post},
};
use axum_extra::extract::Query;
use chrono::Duration as ChronoDuration;
use memini_auth::{Config as AuthConfig, Principal, normalize_namespace, validate_namespace};
use memini_core::{
    memory::{Level, Memory, Tier},
    search::Scored,
};
use memini_service::{
    AnswerInput, BriefingOptions, ListInput, RecallInput, RememberInput, Service, ServiceError,
};
use memini_store::{ApiKey, ApiKeyStore, LinkStore, NamespaceLink, Sort, SortKey, StoreError};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use serde_json::{Map, Value, json};
use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tower_http::{timeout::TimeoutLayer, trace::TraceLayer};
mod mcp;
pub use mcp::{SERVER_INSTRUCTIONS as MCP_SERVER_INSTRUCTIONS, tool_definitions as mcp_tools};
pub(crate) const VERSION: &str = match option_env!("MEMINI_BUILD_VERSION") {
    Some(value) => value,
    None => env!("CARGO_PKG_VERSION"),
};

#[derive(Clone)]
pub struct ApiConfig {
    pub auth: AuthConfig,
    pub namespace_header: String,
    pub home_header: String,
    pub default_namespace: String,
    pub request_timeout: Duration,
    pub key_store: Option<Arc<dyn ApiKeyStore>>,
    pub link_store: Option<Arc<dyn LinkStore>>,
    pub ui_enabled: bool,
    pub ui_api_key: String,
    pub metrics_enabled: bool,
    pub llm_configured: bool,
    pub embedder_configured: bool,
    pub metrics: memini_observability::Registry,
    pub dependencies: memini_observability::DependencyTracker,
}
#[derive(Clone)]
pub(crate) struct StateData {
    pub(crate) service: Arc<Service>,
    pub(crate) config: ApiConfig,
    pub(crate) mcp_sessions: Arc<Mutex<std::collections::HashMap<String, RequestScope>>>,
}
#[derive(Clone)]
pub(crate) struct RequestScope {
    pub(crate) namespace: String,
    pub(crate) home: String,
    pub(crate) principal: Option<Principal>,
}

pub fn router(service: Arc<Service>, config: ApiConfig) -> Router {
    let state = StateData {
        service,
        config: config.clone(),
        mcp_sessions: Arc::new(Mutex::new(std::collections::HashMap::new())),
    };
    let api_routes = Router::new()
        .route("/v1/memories", post(remember).get(list).delete(forget_tag))
        .route("/v1/memories/{id}", get(get_memory).delete(forget))
        .route("/v1/memories/{id}/history", get(history))
        .route("/v1/memories/{id}/supersede", post(supersede))
        .route("/v1/memories/{id}/reassign", post(reassign))
        .route("/v1/search", post(search))
        .route("/v1/answer", post(answer))
        .route("/v1/stats", get(stats))
        .route("/v1/namespaces", get(namespaces).delete(delete_namespace))
        .route("/v1/namespaces/briefing", get(briefing))
        .route("/v1/namespaces/read-set", get(read_set))
        .route("/v1/namespaces/move", post(move_namespace))
        .route("/v1/namespaces/split", post(split_namespace))
        .route("/v1/fsck", post(fsck))
        .route("/v1/dedup", post(dedup))
        .route("/v1/activity", get(activity))
        .route("/v1/keys", get(list_keys).post(create_key))
        .route("/v1/keys/{name}", patch(update_key).delete(delete_key))
        .route("/v1/keys/{name}/rotate", post(rotate_key))
        .route(
            "/v1/links",
            get(list_links).post(put_link).delete(delete_link),
        )
        .route_layer(middleware::from_fn_with_state(
            state.clone(),
            scope_middleware,
        ));
    let api_routes = if config.request_timeout.is_zero() {
        api_routes
    } else {
        api_routes.layer(TimeoutLayer::with_status_code(
            StatusCode::SERVICE_UNAVAILABLE,
            config.request_timeout,
        ))
    };
    // MCP GET streams are intentionally long-lived and must never inherit the
    // ordinary REST request timeout.
    let mcp_routes = Router::new()
        .route(
            "/mcp",
            post(mcp::handler)
                .get(mcp::stream)
                .delete(mcp::delete_session),
        )
        .route_layer(middleware::from_fn_with_state(
            state.clone(),
            scope_middleware,
        ));
    let mut app = Router::new()
        .merge(api_routes)
        .merge(mcp_routes)
        .route("/healthz", get(health))
        .route("/readyz", get(ready))
        .layer(TraceLayer::new_for_http());
    if config.metrics_enabled {
        app = app.route("/metrics", get(metrics_authenticated));
    }
    if config.ui_enabled {
        app = app
            .route("/assets/index.js", get(ui_js))
            .route("/assets/index.css", get(ui_css))
            .fallback(ui_fallback)
    }
    app.layer(middleware::from_fn_with_state(
        state.clone(),
        metrics_middleware,
    ))
    .with_state(state)
}

pub fn metrics_router(metrics: memini_observability::Registry) -> Router {
    Router::new()
        .route("/metrics", get(metrics_plain))
        .with_state(metrics)
}

async fn scope_middleware(
    State(state): State<StateData>,
    mut request: Request,
    next: Next,
) -> Response {
    let token = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("");
    let (principal, ok) = match state.config.auth.authenticate(token).await {
        Ok(v) => v,
        Err(_) => return error(StatusCode::INTERNAL_SERVER_ERROR, "internal error"),
    };
    if !ok {
        return error(StatusCode::UNAUTHORIZED, "missing or invalid bearer token");
    }
    let header_ns = header_value(request.headers(), &state.config.namespace_header);
    let mut namespace = normalize_namespace(header_ns);
    if namespace.is_empty() {
        namespace = principal
            .as_ref()
            .filter(|v| !v.default_namespace.is_empty())
            .map_or_else(
                || state.config.default_namespace.clone(),
                |v| v.default_namespace.clone(),
            )
    }
    if let Err(e) = validate_namespace(&namespace) {
        return error(StatusCode::BAD_REQUEST, &format!("invalid namespace: {e}"));
    }
    let header_home =
        normalize_namespace(header_value(request.headers(), &state.config.home_header));
    let home = principal
        .as_ref()
        .filter(|v| !v.home_namespace.is_empty())
        .map_or(header_home, |v| v.home_namespace.clone());
    if !home.is_empty()
        && let Err(e) = validate_namespace(&home)
    {
        return error(
            StatusCode::BAD_REQUEST,
            &format!("invalid home namespace: {e}"),
        );
    }
    request.extensions_mut().insert(RequestScope {
        namespace,
        home,
        principal,
    });
    next.run(request).await
}
fn header_value<'a>(headers: &'a HeaderMap, name: &str) -> &'a str {
    headers
        .get(name)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
}

#[derive(Debug)]
struct ApiError(StatusCode, String);
impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        error(self.0, &self.1)
    }
}
fn error(status: StatusCode, message: &str) -> Response {
    (status, Json(json!({"error":message}))).into_response()
}
struct ApiJson<T>(T);
impl<S, T> FromRequest<S> for ApiJson<T>
where
    S: Send + Sync,
    T: DeserializeOwned,
{
    type Rejection = ApiError;
    async fn from_request(request: Request, state: &S) -> Result<Self, Self::Rejection> {
        Json::<T>::from_request(request, state)
            .await
            .map(|Json(value)| Self(value))
            .map_err(|error| {
                ApiError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid JSON body: {error}"),
                )
            })
    }
}
fn map_error(value: ServiceError) -> ApiError {
    let status = match &value {
        ServiceError::InvalidInput(_) => StatusCode::BAD_REQUEST,
        ServiceError::Store(StoreError::NotFound) => StatusCode::NOT_FOUND,
        ServiceError::Store(StoreError::Conflict) => StatusCode::CONFLICT,
        ServiceError::Embed(_) => StatusCode::SERVICE_UNAVAILABLE,
        _ => StatusCode::INTERNAL_SERVER_ERROR,
    };
    let message = if status == StatusCode::INTERNAL_SERVER_ERROR {
        "internal error".into()
    } else {
        value.to_string()
    };
    ApiError(status, message)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RememberRequest {
    content: String,
    tier: Option<Tier>,
    level: Option<Level>,
    summary: Option<String>,
    tags: Option<Vec<String>>,
    metadata: Option<Map<String, Value>>,
    importance: Option<f64>,
    ttl_seconds: Option<i64>,
    id: Option<String>,
    confidence: Option<f64>,
    visibility: Option<String>,
    valid_from: Option<chrono::DateTime<chrono::Utc>>,
    valid_to: Option<chrono::DateTime<chrono::Utc>>,
}
async fn remember(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<RememberRequest>,
) -> Result<(StatusCode, Json<Value>), ApiError> {
    let outcome = state
        .service
        .remember_with_outcome(RememberInput {
            namespace: scope.namespace,
            home: scope.home,
            visibility: req.visibility.unwrap_or_default(),
            content: req.content,
            tier: req.tier,
            level: req.level,
            summary: req.summary.unwrap_or_default(),
            tags: req.tags.unwrap_or_default(),
            metadata: req.metadata.unwrap_or_default(),
            importance: req.importance,
            ttl: req.ttl_seconds.map(ChronoDuration::seconds),
            id: req.id.unwrap_or_default(),
            confidence: req.confidence,
            valid_from: req.valid_from,
            valid_to: req.valid_to,
            author: scope.principal.map_or_else(String::new, |v| v.name),
        })
        .await
        .map_err(map_error)?;
    Ok(match outcome.memory {
        Some(memory) => {
            let mut value = serde_json::to_value(memory).unwrap();
            if let Some(hint) = outcome.merge_hint {
                value["merge_hint"] = serde_json::to_value(hint).unwrap();
            }
            if outcome.auto_superseded {
                value["auto_superseded"] = Value::Bool(true);
            }
            if outcome.reinforced {
                value["reinforced"] = Value::Bool(true);
            }
            (StatusCode::CREATED, Json(value))
        }
        None => (
            StatusCode::OK,
            Json(json!({"stored":false,"reason":"low_signal"})),
        ),
    })
}
async fn get_memory(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(id): Path<String>,
) -> Result<Json<Memory>, ApiError> {
    state
        .service
        .get(&scope.namespace, &id)
        .await
        .map(Json)
        .map_err(map_error)
}
async fn forget(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(id): Path<String>,
) -> Result<StatusCode, ApiError> {
    state
        .service
        .forget(&scope.namespace, &id)
        .await
        .map_err(map_error)?;
    Ok(StatusCode::NO_CONTENT)
}
async fn history(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    let memories = state
        .service
        .history(&scope.namespace, &id)
        .await
        .map_err(map_error)?;
    Ok(Json(json!({"memories":memories})))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SupersedeRequest {
    #[serde(alias = "superseded_by")]
    by: String,
}
async fn supersede(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(id): Path<String>,
    ApiJson(req): ApiJson<SupersedeRequest>,
) -> Result<StatusCode, ApiError> {
    state
        .service
        .supersede(&scope.namespace, &id, &req.by)
        .await
        .map_err(map_error)?;
    Ok(StatusCode::NO_CONTENT)
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ReassignRequest {
    #[serde(alias = "namespace")]
    to: String,
}
async fn reassign(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(id): Path<String>,
    ApiJson(req): ApiJson<ReassignRequest>,
) -> Result<Json<Value>, ApiError> {
    let count = state
        .service
        .reassign(&scope.namespace, &id, &req.to)
        .await
        .map_err(map_error)?;
    Ok(Json(json!({"moved":count})))
}

#[derive(Deserialize, Default)]
#[serde(deny_unknown_fields)]
struct SearchRequest {
    query: String,
    #[serde(default)]
    tiers: Vec<Tier>,
    #[serde(default)]
    levels: Vec<Level>,
    #[serde(default)]
    tags: Vec<String>,
    #[serde(default)]
    metadata: Map<String, Value>,
    #[serde(default)]
    exclude_metadata: Map<String, Value>,
    limit: Option<usize>,
    include_expired: Option<bool>,
    include_superseded: Option<bool>,
    as_of: Option<chrono::DateTime<chrono::Utc>>,
    include_fresh_turns: Option<bool>,
    scope: Option<String>,
    namespaces: Option<Vec<String>>,
    min_score: Option<f64>,
    min_semantic_score: Option<f64>,
    include_linked: Option<bool>,
    query_rewrite: Option<bool>,
}
#[derive(Serialize)]
pub(crate) struct ScoredMemory {
    memory: Memory,
    score: f64,
    #[serde(skip_serializing_if = "Option::is_none")]
    from: Option<String>,
}
pub(crate) fn scored(
    results: Vec<Scored>,
    read_set: &[memini_service::ReadSetEntry],
) -> Vec<ScoredMemory> {
    results
        .into_iter()
        .map(|item| {
            let from = read_set
                .iter()
                .find(|v| v.namespace == item.memory.namespace)
                .and_then(|v| match v.origin {
                    memini_service::Origin::Ancestor | memini_service::Origin::Home => {
                        Some(v.namespace.clone())
                    }
                    memini_service::Origin::Link => Some(format!("link:{}", v.namespace)),
                    memini_service::Origin::Call => Some(format!("call:{}", v.namespace)),
                    _ => None,
                });
            ScoredMemory {
                memory: item.memory,
                score: item.score,
                from,
            }
        })
        .collect()
}
async fn search(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<SearchRequest>,
) -> Result<Json<Value>, ApiError> {
    let scope_name = match req.scope.as_deref() {
        Some("exact") => "project",
        Some("subtree") => "everywhere",
        Some(value) => value,
        None => "",
    };
    let (results, degraded, read_set) = state
        .service
        .recall(RecallInput {
            namespace: scope.namespace,
            home: scope.home,
            query: req.query,
            tiers: req.tiers,
            levels: req.levels,
            tags: req.tags,
            metadata: req.metadata,
            exclude_metadata: req.exclude_metadata,
            limit: req.limit.unwrap_or(0),
            include_expired: req.include_expired.unwrap_or(false),
            include_superseded: req.include_superseded.unwrap_or(false),
            as_of: req.as_of,
            scope: scope_name.into(),
            namespaces: req.namespaces.unwrap_or_default(),
            min_score: req.min_score.unwrap_or(0.0),
            min_semantic_score: req.min_semantic_score.unwrap_or(0.0),
            include_linked: req.include_linked.unwrap_or(false),
            include_fresh_turns: req.include_fresh_turns.unwrap_or(false),
            query_rewrite: req.query_rewrite.unwrap_or(false),
            ..RecallInput::default()
        })
        .await
        .map_err(map_error)?;
    let results = scored(results, &read_set);
    Ok(Json(if let Some(reason) = degraded {
        json!({"results":results,"degraded":"keyword_only","note":format!("semantic search unavailable ({reason}); results are keyword-only and may be incomplete")})
    } else {
        json!({"results":results})
    }))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AnswerRequest {
    query: String,
    limit: Option<usize>,
    #[serde(default)]
    tiers: Vec<Tier>,
    #[serde(default)]
    levels: Vec<Level>,
    #[serde(default)]
    tags: Vec<String>,
    #[serde(default)]
    metadata: Map<String, Value>,
    scope: Option<String>,
}
async fn answer(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<AnswerRequest>,
) -> Result<Json<Value>, ApiError> {
    let scope_name = match req.scope.as_deref() {
        Some("exact") => "project",
        Some("subtree") => "everywhere",
        Some(value) => value,
        None => "",
    };
    let output = state
        .service
        .answer(AnswerInput {
            namespace: scope.namespace,
            home: scope.home,
            query: req.query,
            limit: req.limit.unwrap_or(0),
            tiers: req.tiers,
            levels: req.levels,
            tags: req.tags,
            metadata: req.metadata,
            scope: scope_name.into(),
            reasoning: String::new(),
        })
        .await
        .map_err(map_error)?;
    Ok(Json(
        json!({"answer":output.answer,"sources":scored(output.sources,&output.read_set)}),
    ))
}

#[derive(Deserialize, Default)]
struct ListQuery {
    limit: Option<usize>,
    all_namespaces: Option<bool>,
    #[serde(default)]
    tier: Vec<String>,
    #[serde(default)]
    level: Vec<String>,
    #[serde(default)]
    tag: Vec<String>,
    #[serde(default)]
    meta: Vec<String>,
    #[serde(default)]
    namespace: Vec<String>,
    #[serde(default)]
    memory_type: Vec<String>,
    include_expired: Option<bool>,
    include_superseded: Option<bool>,
    created_after: Option<chrono::DateTime<chrono::Utc>>,
    accessed_after: Option<chrono::DateTime<chrono::Utc>>,
    sort: Option<String>,
    order: Option<String>,
}
async fn list(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Query(query): Query<ListQuery>,
) -> Result<Json<Value>, ApiError> {
    let split = |values: Vec<String>| {
        values
            .into_iter()
            .flat_map(|v| {
                v.split(',')
                    .map(str::trim)
                    .filter(|v| !v.is_empty())
                    .map(str::to_owned)
                    .collect::<Vec<_>>()
            })
            .collect::<Vec<_>>()
    };
    let tiers = split(query.tier)
        .into_iter()
        .map(|v| match v.as_str() {
            "working" => Ok(Tier::Working),
            "episodic" => Ok(Tier::Episodic),
            "semantic" => Ok(Tier::Semantic),
            "procedural" => Ok(Tier::Procedural),
            _ => Err(ApiError(
                StatusCode::BAD_REQUEST,
                format!("invalid tier {v:?}"),
            )),
        })
        .collect::<Result<Vec<_>, _>>()?;
    let levels = split(query.level)
        .into_iter()
        .map(|v| match v.as_str() {
            "explicit" => Ok(Level::Explicit),
            "deduced" => Ok(Level::Deduced),
            _ => Err(ApiError(
                StatusCode::BAD_REQUEST,
                format!("invalid level {v:?}"),
            )),
        })
        .collect::<Result<Vec<_>, _>>()?;
    let metadata = split(query.meta)
        .into_iter()
        .map(|v| {
            let (key, value) = v.split_once('=').ok_or_else(|| {
                ApiError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid meta filter {v:?}: want key=value"),
                )
            })?;
            let key = key.trim();
            if key.is_empty() {
                return Err(ApiError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid meta filter {v:?}: want key=value"),
                ));
            }
            Ok((key.into(), Value::String(value.into())))
        })
        .collect::<Result<Map<_, _>, _>>()?;
    let sort = Sort {
        key: match query.sort.as_deref() {
            None | Some("created_at") => SortKey::CreatedAt,
            Some("updated_at") => SortKey::UpdatedAt,
            Some("last_accessed_at") => SortKey::LastAccessedAt,
            Some("access_count") => SortKey::AccessCount,
            Some("importance") => SortKey::Importance,
            Some(value) => {
                return Err(ApiError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid sort {value:?}"),
                ));
            }
        },
        ascending: match query.order.as_deref() {
            None | Some("desc") => false,
            Some("asc") => true,
            Some(value) => {
                return Err(ApiError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid order {value:?}"),
                ));
            }
        },
    };
    let memories = state
        .service
        .list(ListInput {
            namespace: scope.namespace,
            tiers,
            levels,
            tags: split(query.tag),
            metadata,
            memory_types: split(query.memory_type),
            namespaces: query.namespace,
            include_expired: query.include_expired.unwrap_or(false),
            include_superseded: query.include_superseded.unwrap_or(false),
            created_after: query.created_after,
            accessed_after: query.accessed_after,
            sort,
            limit: query.limit,
            offset: 0,
            all_namespaces: query.all_namespaces.unwrap_or(false),
        })
        .await
        .map_err(map_error)?;
    Ok(Json(json!({"memories":memories})))
}
#[derive(Deserialize)]
struct TagQuery {
    tag: String,
}
async fn forget_tag(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Query(query): Query<TagQuery>,
) -> Result<Json<Value>, ApiError> {
    let deleted = state
        .service
        .forget_by_tag(&scope.namespace, &query.tag)
        .await
        .map_err(map_error)?;
    Ok(Json(json!({"deleted":deleted})))
}
#[derive(Deserialize, Default)]
struct AllQuery {
    all_namespaces: Option<bool>,
}
async fn stats(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Query(query): Query<AllQuery>,
) -> Result<Json<memini_service::Stats>, ApiError> {
    if query.all_namespaces.unwrap_or(false) {
        state.service.stats_all().await
    } else {
        state.service.stats(&scope.namespace).await
    }
    .map(Json)
    .map_err(map_error)
}
async fn namespaces(State(state): State<StateData>) -> Result<Json<Value>, ApiError> {
    state
        .service
        .namespaces()
        .await
        .map(|v| Json(json!({"namespaces":v})))
        .map_err(map_error)
}
async fn delete_namespace(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
) -> Result<Json<Value>, ApiError> {
    let deleted = state
        .service
        .delete_namespace(&scope.namespace)
        .await
        .map_err(map_error)?;
    Ok(Json(json!({"deleted":deleted})))
}
#[derive(Deserialize, Default)]
struct BriefingQuery {
    scope: Option<String>,
    per_section: Option<usize>,
    per_section_pinned: Option<usize>,
    per_section_facts: Option<usize>,
    per_section_procedures: Option<usize>,
    per_section_recent: Option<usize>,
    #[serde(default)]
    namespaces: Vec<String>,
}
async fn briefing(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Query(query): Query<BriefingQuery>,
) -> Result<Json<Value>, ApiError> {
    let section = |dedicated: Option<usize>| dedicated.or(query.per_section);
    let scope_name = match query.scope.as_deref() {
        Some("exact") => "project",
        Some("subtree") => "everywhere",
        Some(value) => value,
        None => "",
    };
    let (output, read_set) = state
        .service
        .briefing(
            &scope.namespace,
            BriefingOptions {
                home: scope.home,
                scope: scope_name.into(),
                namespaces: query.namespaces,
                pinned: section(query.per_section_pinned),
                facts: section(query.per_section_facts),
                procedures: section(query.per_section_procedures),
                recent: section(query.per_section_recent),
                ..BriefingOptions::default()
            },
        )
        .await
        .map_err(map_error)?;
    let item = |memory: Memory| {
        let from = read_set
            .iter()
            .find(|entry| entry.namespace == memory.namespace)
            .and_then(|entry| match entry.origin {
                memini_service::Origin::Ancestor | memini_service::Origin::Home => {
                    Some(entry.namespace.clone())
                }
                memini_service::Origin::Link => Some(format!("link:{}", entry.namespace)),
                memini_service::Origin::Call => Some(format!("call:{}", entry.namespace)),
                _ => None,
            });
        let mut value = json!({"memory":memory});
        if let Some(from) = from {
            value["from"] = Value::String(from);
        }
        value
    };
    let mut response = Map::new();
    response.insert("namespace".into(), Value::String(output.namespace));
    if !output.scope_header.is_empty() {
        response.insert("scope_header".into(), Value::String(output.scope_header));
    }
    for (name, memories) in [
        ("facts", output.facts),
        ("procedures", output.procedures),
        ("recent", output.recent),
        ("pinned", output.pinned),
    ] {
        if !memories.is_empty() {
            response.insert(
                name.into(),
                Value::Array(memories.into_iter().map(&item).collect()),
            );
        }
    }
    if !output.children.is_empty() {
        response.insert(
            "children".into(),
            serde_json::to_value(output.children).unwrap(),
        );
    }
    Ok(Json(Value::Object(response)))
}
async fn read_set(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
) -> Result<Json<Value>, ApiError> {
    let entries = state
        .service
        .resolve_read_set_info(&scope.namespace, &scope.home)
        .await
        .map_err(map_error)?;
    Ok(Json(
        json!({"entries":entries.iter().map(|v|json!({"namespace":v.namespace,"origin":format!("{:?}",v.origin).to_lowercase(),"tiers":v.tiers})).collect::<Vec<_>>() }),
    ))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MoveRequest {
    to: String,
    dry_run: Option<bool>,
}
async fn move_namespace(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<MoveRequest>,
) -> Result<Json<Value>, ApiError> {
    let out = state
        .service
        .move_namespace(&scope.namespace, &req.to, req.dry_run.unwrap_or(false))
        .await
        .map_err(map_error)?;
    Ok(Json(serde_json::to_value(out).unwrap()))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SplitRequest {
    #[serde(alias = "keys")]
    by: Option<Vec<String>>,
    dry_run: Option<bool>,
}
async fn split_namespace(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<SplitRequest>,
) -> Result<Json<Value>, ApiError> {
    let out = state
        .service
        .split_namespace(
            &scope.namespace,
            &req.by.unwrap_or_else(|| {
                vec![
                    "import_source_namespace".into(),
                    "user_id".into(),
                    "agent_id".into(),
                    "run_id".into(),
                    "project".into(),
                ]
            }),
            req.dry_run.unwrap_or(false),
        )
        .await
        .map_err(map_error)?;
    Ok(Json(serde_json::to_value(out).unwrap()))
}
async fn fsck(State(state): State<StateData>) -> Result<Json<Value>, ApiError> {
    let out = state.service.fsck(0).await.map_err(map_error)?;
    Ok(Json(serde_json::to_value(out).unwrap()))
}
#[derive(Deserialize, Default)]
#[serde(deny_unknown_fields)]
struct DedupRequest {
    similarity: Option<f64>,
    min_cluster_size: Option<usize>,
    neighbours_per_anchor: Option<usize>,
    dry_run: Option<bool>,
    all_namespaces: Option<bool>,
    #[serde(default)]
    tiers: Vec<Tier>,
}
async fn dedup(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    body: Option<Json<DedupRequest>>,
) -> Result<Json<Value>, ApiError> {
    let req = body.map(|v| v.0).unwrap_or_default();
    let options = memini_maintenance::DedupOptions {
        similarity: req.similarity.unwrap_or(0.85),
        min_cluster_size: req.min_cluster_size.unwrap_or(2),
        neighbours_per_anchor: req.neighbours_per_anchor.unwrap_or(20),
        dry_run: req.dry_run.unwrap_or(false),
        namespaces: if req.all_namespaces.unwrap_or(false) {
            vec![]
        } else {
            vec![scope.namespace]
        },
        tiers: req.tiers,
        ..memini_maintenance::DedupOptions::default()
    };
    let report = state.service.dedup(options).await.map_err(map_error)?;
    Ok(Json(serde_json::to_value(report).unwrap()))
}
async fn health(
    State(state): State<StateData>,
    headers: HeaderMap,
    Query(query): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    let verbose = query.get("verbose").is_some_and(|value| value == "1")
        && (state.config.ui_api_key.is_empty()
            || valid_admin_header(&headers, &state.config.ui_api_key));
    if !verbose {
        return Json(json!({"status":"ok","version":VERSION}));
    }
    let store =
        match tokio::time::timeout(Duration::from_secs(2), state.service.store().ping()).await {
            Err(_) => json!({"ok":false,"last_error":"store ping timed out"}),
            Ok(Ok(())) => json!({"ok":true}),
            Ok(Err(error)) => json!({"ok":false,"last_error":error.to_string()}),
        };
    let embedder = state.config.dependencies.get("embedder");
    let llm = state.config.dependencies.get("llm");
    Json(json!({
        "status":"ok",
        "version":VERSION,
        "deps":{
            "store":store,
            "embedder":{"ok":embedder.ok,"last_error":(!embedder.last_error.is_empty()).then_some(embedder.last_error),"last_success":memini_observability::timestamp(embedder.last_success)},
            "llm":{"ok":state.config.llm_configured && llm.ok,"configured":state.config.llm_configured,"last_error":(!llm.last_error.is_empty()).then_some(llm.last_error),"last_success":memini_observability::timestamp(llm.last_success)}
        }
    }))
}
async fn ready(State(state): State<StateData>) -> impl IntoResponse {
    match state.service.store().ping().await {
        Ok(()) => (StatusCode::OK, Json(json!({"status":"ready"}))),
        Err(_) => (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({"status":"not ready"})),
        ),
    }
}
async fn metrics_middleware(
    State(state): State<StateData>,
    request: Request,
    next: Next,
) -> Response {
    let method = request.method().to_string();
    let start = Instant::now();
    let response = next.run(request).await;
    let status = response.status().as_u16().to_string();
    state.config.metrics.inc(
        "memini_http_requests_total",
        &[("route", "all"), ("method", &method), ("status", &status)],
    );
    state.config.metrics.observe(
        "memini_http_request_duration_seconds",
        &[("route", "all"), ("method", &method)],
        start.elapsed().as_secs_f64(),
    );
    response
}
fn metrics_body(metrics: &memini_observability::Registry) -> String {
    let mut output = format!(
        "# HELP memini_build_info Build information.\n# TYPE memini_build_info gauge\nmemini_build_info{{version=\"{}\"}} 1\n# HELP memini_http_requests_total Total HTTP requests.\n# TYPE memini_http_requests_total counter\n",
        VERSION
    );
    output.push_str(&metrics.render());
    output
}
async fn metrics_plain(State(metrics): State<memini_observability::Registry>) -> impl IntoResponse {
    (
        [(
            header::CONTENT_TYPE,
            "text/plain; version=0.0.4; charset=utf-8",
        )],
        metrics_body(&metrics),
    )
}
async fn metrics_authenticated(State(state): State<StateData>, headers: HeaderMap) -> Response {
    if !state.config.ui_api_key.is_empty()
        && !valid_admin_header(&headers, &state.config.ui_api_key)
    {
        return error(StatusCode::UNAUTHORIZED, "missing or invalid bearer token");
    }
    (
        [(
            header::CONTENT_TYPE,
            "text/plain; version=0.0.4; charset=utf-8",
        )],
        metrics_body(&state.config.metrics),
    )
        .into_response()
}
fn valid_admin_header(headers: &HeaderMap, key: &str) -> bool {
    headers
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.strip_prefix("Bearer "))
        == Some(key)
}
const UI_INDEX: &str = include_str!(concat!(env!("OUT_DIR"), "/ui/index.html"));
const UI_JS: &[u8] = include_bytes!(concat!(env!("OUT_DIR"), "/ui/index.js"));
const UI_CSS: &[u8] = include_bytes!(concat!(env!("OUT_DIR"), "/ui/index.css"));
async fn ui_js() -> impl IntoResponse {
    (
        [
            (header::CONTENT_TYPE, "text/javascript; charset=utf-8"),
            (header::CACHE_CONTROL, "public, max-age=31536000, immutable"),
        ],
        UI_JS,
    )
}
async fn ui_css() -> impl IntoResponse {
    (
        [
            (header::CONTENT_TYPE, "text/css; charset=utf-8"),
            (header::CACHE_CONTROL, "public, max-age=31536000, immutable"),
        ],
        UI_CSS,
    )
}
async fn ui_fallback(State(state): State<StateData>, request: Request) -> Response {
    if request.method() != axum::http::Method::GET
        || request.uri().path().starts_with("/.well-known/")
    {
        return (StatusCode::NOT_FOUND, Json(json!({}))).into_response();
    }
    let mut index = UI_INDEX.to_owned();
    if !state.config.ui_api_key.is_empty() {
        let escaped = state
            .config
            .ui_api_key
            .replace('&', "&amp;")
            .replace('"', "&#34;")
            .replace('<', "&lt;")
            .replace('>', "&gt;");
        let tag = format!(r#"<meta name="memini-token" content="{escaped}">"#);
        index = if let Some(at) = index.find("</head>") {
            format!("{}{}{}", &index[..at], tag, &index[at..])
        } else {
            tag + &index
        };
    }
    (
        [
            (header::CONTENT_TYPE, "text/html; charset=utf-8"),
            (header::CACHE_CONTROL, "no-cache"),
        ],
        index,
    )
        .into_response()
}

#[derive(Deserialize, Default)]
struct ActivityQuery {
    limit: Option<usize>,
    #[serde(alias = "text")]
    q: Option<String>,
    since: Option<chrono::DateTime<chrono::Utc>>,
    before: Option<String>,
    #[serde(default)]
    kind: Vec<String>,
    #[serde(default)]
    tier: Vec<String>,
    #[serde(default)]
    namespace: Vec<String>,
    all_namespaces: Option<bool>,
}
fn event_kind(value: &str) -> Option<memini_store::EventKind> {
    match value {
        "recall" => Some(memini_store::EventKind::Recall),
        "get" => Some(memini_store::EventKind::Get),
        "briefing" => Some(memini_store::EventKind::Briefing),
        "remember" => Some(memini_store::EventKind::Remember),
        "update" => Some(memini_store::EventKind::Update),
        "forget" => Some(memini_store::EventKind::Forget),
        "supersede" => Some(memini_store::EventKind::Supersede),
        _ => None,
    }
}
async fn activity(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Query(query): Query<ActivityQuery>,
) -> Result<Json<Value>, ApiError> {
    let split = |values: Vec<String>| {
        values
            .into_iter()
            .flat_map(|value| {
                value
                    .split(',')
                    .map(str::trim)
                    .filter(|value| !value.is_empty())
                    .map(str::to_owned)
                    .collect::<Vec<_>>()
            })
            .collect::<Vec<_>>()
    };
    let kinds = split(query.kind)
        .into_iter()
        .map(|value| {
            event_kind(&value).ok_or_else(|| {
                ApiError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid event kind {value:?}"),
                )
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    let tiers = split(query.tier)
        .into_iter()
        .map(|value| match value.as_str() {
            "working" => Ok(Tier::Working),
            "episodic" => Ok(Tier::Episodic),
            "semantic" => Ok(Tier::Semantic),
            "procedural" => Ok(Tier::Procedural),
            _ => Err(ApiError(
                StatusCode::BAD_REQUEST,
                format!("invalid tier {value:?}"),
            )),
        })
        .collect::<Result<Vec<_>, _>>()?;
    let (before, before_id) = query
        .before
        .as_deref()
        .map(decode_activity_cursor)
        .transpose()?
        .unwrap_or((None, 0));
    let all_namespaces = query.all_namespaces.unwrap_or(false);
    let page = state
        .service
        .events(memini_service::EventsInput {
            namespace: if all_namespaces {
                String::new()
            } else {
                scope.namespace
            },
            namespaces: if all_namespaces {
                split(query.namespace)
            } else {
                Vec::new()
            },
            kinds,
            tiers,
            text: query.q.unwrap_or_default().trim().into(),
            since: query.since,
            before,
            before_id,
            limit: query.limit.unwrap_or(0),
        })
        .await
        .map_err(map_error)?;
    let events = page
        .events
        .into_iter()
        .map(activity_event_json)
        .collect::<Vec<_>>();
    let mut response = json!({"events":events,"has_more":page.has_more});
    if page.has_more
        && let Some(before) = page.next_before
    {
        response["next_cursor"] = Value::String(format!(
            "{}-{}",
            before.timestamp_millis(),
            page.next_before_id
        ));
    }
    Ok(Json(response))
}

fn decode_activity_cursor(
    cursor: &str,
) -> std::result::Result<(Option<chrono::DateTime<chrono::Utc>>, i64), ApiError> {
    let invalid = || {
        ApiError(
            StatusCode::BAD_REQUEST,
            format!("invalid cursor {cursor:?}"),
        )
    };
    let (millis, id) = cursor.split_once('-').ok_or_else(invalid)?;
    let millis = millis.parse::<i64>().map_err(|_| invalid())?;
    let id = id.parse::<i64>().map_err(|_| invalid())?;
    let before = chrono::DateTime::from_timestamp_millis(millis).ok_or_else(invalid)?;
    Ok((Some(before), id))
}

fn activity_event_json(event: memini_service::ActivityEvent) -> Value {
    let mut value = json!({
        "op_id":event.operation_id,
        "kind":event.kind,
        "time":event.time,
        "namespace":event.namespace,
    });
    if !event.query.is_empty() {
        value["query"] = Value::String(event.query);
    }
    if !event.detail.is_empty() {
        value["detail"] = Value::Object(event.detail);
    }
    if !event.memories.is_empty() {
        value["memories"] = Value::Array(
            event
                .memories
                .into_iter()
                .map(|memory| {
                    let mut item = json!({
                        "id":memory.id,
                        "namespace":memory.namespace,
                        "summary":memory.summary,
                        "tier":memory.tier,
                    });
                    if memory.rank > 0 {
                        item["rank"] = json!(memory.rank);
                    }
                    if let Some(score) = memory.score {
                        item["score"] = json!(score);
                    }
                    if !memory.section.is_empty() {
                        item["section"] = Value::String(memory.section);
                    }
                    item
                })
                .collect(),
        );
    }
    value
}

fn admin(scope: &RequestScope) -> std::result::Result<(), ApiError> {
    if scope.principal.is_some() {
        Err(ApiError(StatusCode::FORBIDDEN,"admin key required: /v1/keys manages API keys and is not reachable with a named API key".into()))
    } else {
        Ok(())
    }
}
fn key_store(state: &StateData) -> std::result::Result<&Arc<dyn ApiKeyStore>, ApiError> {
    state.config.key_store.as_ref().ok_or_else(|| {
        ApiError(
            StatusCode::NOT_IMPLEMENTED,
            "api keys are not supported by this storage backend".into(),
        )
    })
}
#[derive(Serialize)]
struct KeyResponse {
    name: String,
    disabled: bool,
    source: &'static str,
    #[serde(skip_serializing_if = "String::is_empty")]
    home: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    default_namespace: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    created_at: Option<chrono::DateTime<chrono::Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    secret: Option<String>,
}
fn key_response(key: ApiKey, source: &'static str, secret: Option<String>) -> KeyResponse {
    KeyResponse {
        name: key.name,
        disabled: key.disabled,
        source,
        home: key.home_namespace,
        default_namespace: key.default_namespace,
        created_at: key.created_at,
        secret,
    }
}
async fn list_keys(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
) -> Result<Json<Value>, ApiError> {
    admin(&scope)?;
    let mut keys = key_store(&state)?
        .list_api_keys()
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
        .into_iter()
        .map(|v| key_response(v, "db", None))
        .collect::<Vec<_>>();
    keys.extend(
        state
            .config
            .auth
            .file_keys()
            .into_iter()
            .map(|v| key_response(v, "file", None)),
    );
    Ok(Json(json!({"keys":keys})))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CreateKeyRequest {
    name: String,
    home: Option<String>,
    default_namespace: Option<String>,
    disabled: Option<bool>,
}
fn optional_namespace(value: Option<String>) -> std::result::Result<String, ApiError> {
    let value = normalize_namespace(value.as_deref().unwrap_or(""));
    if !value.is_empty() {
        validate_namespace(&value).map_err(|e| ApiError(StatusCode::BAD_REQUEST, e))?
    }
    Ok(value)
}
async fn create_key(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<CreateKeyRequest>,
) -> Result<(StatusCode, Json<Value>), ApiError> {
    admin(&scope)?;
    let store = key_store(&state)?;
    let name = req.name.trim();
    if name.is_empty() || name.contains('/') {
        return Err(ApiError(
            StatusCode::BAD_REQUEST,
            "api key name must not be empty or contain /".into(),
        ));
    }
    if state.config.auth.is_file_key(name)
        || store
            .list_api_keys()
            .await
            .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
            .iter()
            .any(|v| v.name == name)
    {
        return Err(ApiError(
            StatusCode::CONFLICT,
            format!("api key {name:?} already exists"),
        ));
    }
    let secret = memini_auth::generate_secret()
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    let key = ApiKey {
        name: name.into(),
        hash: memini_auth::hash_token(&secret),
        home_namespace: optional_namespace(req.home)?,
        default_namespace: optional_namespace(req.default_namespace)?,
        created_at: None,
        disabled: req.disabled.unwrap_or(false),
    };
    store
        .put_api_key(&key)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    state.config.auth.invalidate();
    let stored = store
        .get_api_key_by_hash(&key.hash)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
        .unwrap_or(key);
    Ok((
        StatusCode::CREATED,
        Json(serde_json::to_value(key_response(stored, "db", Some(secret))).unwrap()),
    ))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct UpdateKeyRequest {
    home: Option<String>,
    default_namespace: Option<String>,
    disabled: Option<bool>,
}
async fn update_key(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(name): Path<String>,
    ApiJson(req): ApiJson<UpdateKeyRequest>,
) -> Result<Json<Value>, ApiError> {
    admin(&scope)?;
    if state.config.auth.is_file_key(&name) {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "file key is declaratively managed".into(),
        ));
    }
    let store = key_store(&state)?;
    let mut key = store
        .list_api_keys()
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
        .into_iter()
        .find(|v| v.name == name)
        .ok_or_else(|| ApiError(StatusCode::NOT_FOUND, "api key not found".into()))?;
    if req.home.is_some() {
        key.home_namespace = optional_namespace(req.home)?
    }
    if req.default_namespace.is_some() {
        key.default_namespace = optional_namespace(req.default_namespace)?
    }
    if let Some(v) = req.disabled {
        key.disabled = v
    }
    store
        .put_api_key(&key)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    state.config.auth.invalidate();
    Ok(Json(
        serde_json::to_value(key_response(key, "db", None)).unwrap(),
    ))
}
async fn delete_key(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(name): Path<String>,
) -> Result<StatusCode, ApiError> {
    admin(&scope)?;
    if state.config.auth.is_file_key(&name) {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "file key is declaratively managed".into(),
        ));
    }
    if !key_store(&state)?
        .delete_api_key(&name)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
    {
        return Err(ApiError(StatusCode::NOT_FOUND, "api key not found".into()));
    }
    state.config.auth.invalidate();
    Ok(StatusCode::NO_CONTENT)
}
async fn rotate_key(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Path(name): Path<String>,
) -> Result<Json<Value>, ApiError> {
    admin(&scope)?;
    if state.config.auth.is_file_key(&name) {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "file key is declaratively managed".into(),
        ));
    }
    let store = key_store(&state)?;
    let mut key = store
        .list_api_keys()
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
        .into_iter()
        .find(|v| v.name == name)
        .ok_or_else(|| ApiError(StatusCode::NOT_FOUND, "api key not found".into()))?;
    let secret = memini_auth::generate_secret()
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    key.hash = memini_auth::hash_token(&secret);
    store
        .put_api_key(&key)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    state.config.auth.invalidate();
    Ok(Json(
        serde_json::to_value(key_response(key, "db", Some(secret))).unwrap(),
    ))
}

fn link_store(state: &StateData) -> std::result::Result<&Arc<dyn LinkStore>, ApiError> {
    state.config.link_store.as_ref().ok_or_else(|| {
        ApiError(
            StatusCode::NOT_IMPLEMENTED,
            "namespace links are not supported by this storage backend".into(),
        )
    })
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LinkRequest {
    #[serde(alias = "destination")]
    dst: String,
    tiers: Option<Vec<Tier>>,
    note: Option<String>,
}
fn link_json(link: NamespaceLink) -> Value {
    let mut value = json!({
        "src":link.source,
        "dst":link.destination,
        "created_at":link.created_at,
    });
    if !link.tiers.is_empty() {
        value["tiers"] = json!(link.tiers);
    }
    if !link.note.is_empty() {
        value["note"] = Value::String(link.note);
    }
    value
}
async fn list_links(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
) -> Result<Json<Value>, ApiError> {
    let links = link_store(&state)?
        .list_links(&scope.namespace)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    Ok(Json(
        json!({"links":links.into_iter().map(link_json).collect::<Vec<_>>() }),
    ))
}
async fn put_link(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    ApiJson(req): ApiJson<LinkRequest>,
) -> Result<(StatusCode, Json<Value>), ApiError> {
    let destination = normalize_namespace(&req.dst);
    validate_namespace(&destination).map_err(|e| {
        ApiError(
            StatusCode::BAD_REQUEST,
            format!("invalid dst namespace: {e}"),
        )
    })?;
    if destination.contains('*') {
        return Err(ApiError(
            StatusCode::BAD_REQUEST,
            "invalid dst namespace: \"*\" is reserved for read-set patterns".into(),
        ));
    }
    if destination == scope.namespace {
        return Err(ApiError(
            StatusCode::BAD_REQUEST,
            "dst namespace equals the request namespace (no self-links)".into(),
        ));
    }
    let link = NamespaceLink {
        source: scope.namespace,
        destination,
        tiers: req.tiers.unwrap_or_default(),
        note: req.note.unwrap_or_default(),
        created_at: chrono::Utc::now(),
    };
    link_store(&state)?
        .put_link(&link)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?;
    Ok((StatusCode::OK, Json(link_json(link))))
}
#[derive(Deserialize, Default)]
struct DeleteLinkQuery {
    #[serde(default, alias = "destination")]
    dst: String,
}
async fn delete_link(
    State(state): State<StateData>,
    Extension(scope): Extension<RequestScope>,
    Query(req): Query<DeleteLinkQuery>,
    body: Option<Json<DeleteLinkQuery>>,
) -> Result<StatusCode, ApiError> {
    let destination = if req.dst.is_empty() {
        body.map_or_else(String::new, |Json(value)| value.dst)
    } else {
        req.dst
    };
    let destination = normalize_namespace(&destination);
    if destination.is_empty() {
        return Err(ApiError(StatusCode::BAD_REQUEST, "dst is required".into()));
    }
    if !link_store(&state)?
        .delete_link(&scope.namespace, &destination)
        .await
        .map_err(|_| ApiError(StatusCode::INTERNAL_SERVER_ERROR, "internal error".into()))?
    {
        return Err(ApiError(
            StatusCode::NOT_FOUND,
            "namespace link not found".into(),
        ));
    }
    Ok(StatusCode::NO_CONTENT)
}

#[cfg(test)]
mod tests {
    use super::*;
    use async_trait::async_trait;
    use axum::{body::Body, http::Request};
    use http_body_util::BodyExt;
    use memini_embed::{Embedder, Result as EmbedResult};
    use tower::ServiceExt;
    struct Fake;
    #[async_trait]
    impl Embedder for Fake {
        async fn embed(&self, texts: &[String]) -> EmbedResult<Vec<Vec<f32>>> {
            Ok(texts.iter().map(|v| vec![v.len() as f32, 1.0]).collect())
        }
        fn dimensions(&self) -> usize {
            2
        }
    }
    fn setup_with_observability() -> (
        Router,
        memini_observability::Registry,
        memini_observability::DependencyTracker,
    ) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.keep().join("db");
        let store = Arc::new(memini_sqlite::SqliteStore::open(path, 2).unwrap());
        let metrics = memini_observability::Registry::default();
        let dependencies = memini_observability::DependencyTracker::default();
        let service = Arc::new(
            Service::new(store.clone(), Arc::new(Fake))
                .with_event_store(store.clone())
                .with_metrics(metrics.clone()),
        );
        let app = router(
            service,
            ApiConfig {
                auth: AuthConfig::new("secret", Some(store.clone())),
                namespace_header: "X-Memini-Namespace".into(),
                home_header: "X-Memini-Home".into(),
                default_namespace: "default".into(),
                request_timeout: Duration::from_secs(5),
                key_store: Some(store.clone()),
                link_store: Some(store),
                ui_enabled: true,
                ui_api_key: "secret".into(),
                metrics_enabled: true,
                llm_configured: false,
                embedder_configured: true,
                metrics: metrics.clone(),
                dependencies: dependencies.clone(),
            },
        );
        (app, metrics, dependencies)
    }
    fn setup() -> Router {
        setup_with_observability().0
    }
    async fn json(response: Response) -> Value {
        let bytes = response.into_body().collect().await.unwrap().to_bytes();
        serde_json::from_slice(&bytes).unwrap()
    }
    #[tokio::test]
    async fn rest_auth_and_memory_contract() {
        let app = setup();
        let unauthorized=app.clone().oneshot(Request::post("/v1/memories").header("content-type","application/json").body(Body::from(r#"{"content":"The project uses Postgres and by default the service runs migrations."}"#)).unwrap()).await.unwrap();
        assert_eq!(unauthorized.status(), StatusCode::UNAUTHORIZED);
        let response=app.clone().oneshot(Request::post("/v1/memories").header("authorization","Bearer secret").header("content-type","application/json").body(Body::from(r#"{"content":"The project uses Postgres and by default the service runs migrations."}"#)).unwrap()).await.unwrap();
        assert_eq!(response.status(), StatusCode::CREATED);
        let body = json(response).await;
        assert_eq!(body["tier"], "semantic");
        assert_eq!(body["namespace"], "default");
        let repeated = app
            .clone()
            .oneshot(
                Request::post("/v1/memories")
                    .header("authorization", "Bearer secret")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"content":"The project uses Postgres and by default the service runs migrations.","tier":"semantic"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(json(repeated).await["reinforced"], true);
        let activity = app
            .oneshot(
                Request::get("/v1/activity")
                    .header("authorization", "Bearer secret")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(activity.status(), StatusCode::OK);
        let activity = json(activity).await;
        assert_eq!(activity["events"][0]["kind"], "remember");
        assert!(activity["events"][0]["op_id"].is_string());
        assert!(activity["events"][0].get("operation_id").is_none());
        assert!(activity.get("next_before").is_none());
    }
    #[tokio::test]
    async fn rest_links_search_and_query_validation_contract() {
        let app = setup();
        let link = app
            .clone()
            .oneshot(
                Request::post("/v1/links")
                    .header("authorization", "Bearer secret")
                    .header("X-Memini-Namespace", "project/api")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"dst":" shared/facts "}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(link.status(), StatusCode::OK);
        let link = json(link).await;
        assert_eq!(link["src"], "project/api");
        assert_eq!(link["dst"], "shared/facts");
        assert!(link.get("note").is_none());
        assert!(link.get("tiers").is_none());

        let self_link = app
            .clone()
            .oneshot(
                Request::post("/v1/links")
                    .header("authorization", "Bearer secret")
                    .header("X-Memini-Namespace", "project/api")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"dst":"project/api"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(self_link.status(), StatusCode::BAD_REQUEST);

        let search = app
            .clone()
            .oneshot(
                Request::post("/v1/search")
                    .header("authorization", "Bearer secret")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"query":"facts","scope":"exact","include_fresh_turns":true,"as_of":"2025-01-01T00:00:00Z"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(search.status(), StatusCode::OK);

        let invalid = app
            .clone()
            .oneshot(
                Request::get("/v1/memories?tier=semantic,bogus")
                    .header("authorization", "Bearer secret")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(invalid.status(), StatusCode::BAD_REQUEST);

        let valid_filter = app
            .clone()
            .oneshot(
                Request::get("/v1/memories?tier=semantic")
                    .header("authorization", "Bearer secret")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(valid_filter.status(), StatusCode::OK);

        let deleted = app
            .oneshot(
                Request::delete("/v1/links?dst=%20shared/facts%20")
                    .header("authorization", "Bearer secret")
                    .header("X-Memini-Namespace", "project/api")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(deleted.status(), StatusCode::NO_CONTENT);
    }
    #[tokio::test]
    async fn mcp_initialize_and_tools_contract() {
        let app = setup();
        let initialize = app
            .clone()
            .oneshot(
                Request::post("/mcp")
                    .header("authorization", "Bearer secret")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(initialize.status(), StatusCode::OK);
        assert!(initialize.headers().contains_key("mcp-session-id"));
        let session = initialize.headers()["mcp-session-id"]
            .to_str()
            .unwrap()
            .to_owned();
        assert_eq!(
            json(initialize).await["result"]["serverInfo"]["name"],
            "memini"
        );
        let list = app
            .clone()
            .oneshot(
                Request::post("/mcp")
                    .header("authorization", "Bearer secret")
                    .header("mcp-session-id", &session)
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        let body = json(list).await;
        assert!(
            body["result"]["tools"]
                .as_array()
                .unwrap()
                .iter()
                .any(|v| v["name"] == "memory_recall")
        );
        let stream = app
            .clone()
            .oneshot(
                Request::get("/mcp")
                    .header("authorization", "Bearer secret")
                    .header("mcp-session-id", &session)
                    .header("accept", "text/event-stream")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(stream.status(), StatusCode::OK);
        assert_eq!(stream.headers()[header::CONTENT_TYPE], "text/event-stream");
        let deleted = app
            .oneshot(
                Request::delete("/mcp")
                    .header("authorization", "Bearer secret")
                    .header("mcp-session-id", &session)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(deleted.status(), StatusCode::NO_CONTENT);
    }
    #[tokio::test]
    async fn mcp_tool_result_shapes_match_go_contract() {
        let app = setup();
        let initialize = app
            .clone()
            .oneshot(
                Request::post("/mcp")
                    .header("authorization", "Bearer secret")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        let session = initialize.headers()["mcp-session-id"]
            .to_str()
            .unwrap()
            .to_owned();
        let call = |id: i64, name: &str, arguments: Value| {
            Request::post("/mcp")
                .header("authorization", "Bearer secret")
                .header("mcp-session-id", &session)
                .header("content-type", "application/json")
                .body(Body::from(
                    json!({"jsonrpc":"2.0","id":id,"method":"tools/call","params":{"name":name,"arguments":arguments}}).to_string(),
                ))
                .unwrap()
        };
        let remembered = app
            .clone()
            .oneshot(call(
                1,
                "memory_remember",
                json!({"content":"The release database is PostgreSQL.","tier":"semantic","id":"mcp-shape"}),
            ))
            .await
            .unwrap();
        let remembered = json(remembered).await;
        assert_eq!(remembered["result"]["structuredContent"]["stored"], true);
        assert_eq!(remembered["result"]["structuredContent"]["id"], "mcp-shape");
        let repeated = app
            .clone()
            .oneshot(call(
                5,
                "memory_remember",
                json!({"content":"The release database is PostgreSQL.","tier":"semantic"}),
            ))
            .await
            .unwrap();
        assert_eq!(
            json(repeated).await["result"]["structuredContent"]["reinforced"],
            true
        );

        let recalled = app
            .clone()
            .oneshot(call(
                2,
                "memory_recall",
                json!({"query":"release database"}),
            ))
            .await
            .unwrap();
        let recalled = json(recalled).await;
        let hit = &recalled["result"]["structuredContent"]["results"][0];
        assert_eq!(hit["id"], "mcp-shape");
        assert!(hit["content"].is_string());
        assert!(hit["score"].is_number());
        assert!(hit.get("memory").is_none());
        assert!(
            recalled["result"]["structuredContent"]
                .get("degraded")
                .is_none()
        );

        let listed = app
            .clone()
            .oneshot(call(3, "memory_list", json!({})))
            .await
            .unwrap();
        let listed = json(listed).await;
        let item = &listed["result"]["structuredContent"]["memories"][0];
        assert_eq!(item["id"], "mcp-shape");
        assert!(item.get("last_accessed_at").is_none());
        assert!(item.get("linked_memory_ids").is_none());

        let forgotten = app
            .oneshot(call(4, "memory_forget", json!({"id":"mcp-shape"})))
            .await
            .unwrap();
        assert_eq!(
            json(forgotten).await["result"]["structuredContent"],
            json!({"deleted":true})
        );
    }
    #[tokio::test]
    async fn ui_and_metrics_contract() {
        let app = setup();
        let ui = app
            .clone()
            .oneshot(
                Request::get("/deep/client/route")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(ui.status(), StatusCode::OK);
        let body = ui.into_body().collect().await.unwrap().to_bytes();
        assert!(String::from_utf8_lossy(&body).contains("memini-token"));
        let discovery = app
            .clone()
            .oneshot(
                Request::get("/.well-known/oauth-protected-resource")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(discovery.status(), StatusCode::NOT_FOUND);
        assert_eq!(json(discovery).await, json!({}));
        let unauthorized = app
            .clone()
            .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(unauthorized.status(), StatusCode::UNAUTHORIZED);
        let metrics = app
            .oneshot(
                Request::get("/metrics")
                    .header("authorization", "Bearer secret")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let body = metrics.into_body().collect().await.unwrap().to_bytes();
        assert!(String::from_utf8_lossy(&body).contains("memini_build_info"));
    }

    #[tokio::test]
    async fn verbose_health_reports_live_dependency_state() {
        let (app, _, dependencies) = setup_with_observability();
        dependencies.record("embedder", Some("embedding backend unavailable"));
        dependencies.record("llm", None);

        let response = app
            .oneshot(
                Request::get("/healthz?verbose=1")
                    .header("authorization", "Bearer secret")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = json(response).await;
        assert_eq!(body["deps"]["embedder"]["ok"], false);
        assert_eq!(
            body["deps"]["embedder"]["last_error"],
            "embedding backend unavailable"
        );
        assert_eq!(body["deps"]["llm"]["configured"], false);
        assert_eq!(body["deps"]["llm"]["ok"], false);
        assert!(body["deps"]["llm"]["last_success"].is_string());
    }

    #[tokio::test]
    async fn metrics_include_http_and_service_operations() {
        let (app, _, _) = setup_with_observability();
        let remembered = app
            .clone()
            .oneshot(
                Request::post("/v1/memories")
                    .header("authorization", "Bearer secret")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"content":"The deployment uses Postgres for durable storage."}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(remembered.status(), StatusCode::CREATED);

        let response = app
            .oneshot(
                Request::get("/metrics")
                    .header("authorization", "Bearer secret")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let body = String::from_utf8_lossy(&body);
        assert!(body.contains("memini_http_requests_total"));
        assert!(body.contains("memini_remember_results_total"));
        assert!(body.contains("memini_store_upserts_total"));
        assert!(body.contains("memini_op_duration_seconds_count"));
    }
}
