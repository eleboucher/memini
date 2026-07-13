mod events;
mod readset;

use std::{
    collections::{HashMap, HashSet, VecDeque},
    sync::{Arc, LazyLock},
    time::Duration as StdDuration,
};

use chrono::{DateTime, Duration, Utc};
use memini_core::{
    memory::{CONFIDENCE_SEED_FRESH, Level, Memory, Term, Tier, fingerprint},
    sanitize,
    search::{self, Scored},
};
use memini_embed::{Embedder, embed_one};
use memini_intelligence::{extract, redact};
use memini_llm::{ChatTurn, Client as LlmClient, Role, Tool, ToolCall, ToolChoice};
use memini_rerank::{Candidate, Reranker};
use memini_store::{ApiKeyStore, EventLogStore, Filter, LinkStore, Sort, Store, StoreError};
use regex::Regex;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use thiserror::Error;
use uuid::Uuid;

pub use events::{ActivityEvent, ActivityMemory, EventsInput, EventsPage};
pub use readset::{Origin, ReadSetEntry};
const MAX_RECALL_LIMIT: usize = 100;
const DEFAULT_POOL_FACTOR: usize = 5;
const DEFAULT_POOL_FLOOR: usize = 50;

#[derive(Debug, Error)]
pub enum ServiceError {
    #[error("invalid input: {0}")]
    InvalidInput(String),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error("embed: {0}")]
    Embed(#[from] memini_embed::EmbedError),
    #[error("llm: {0}")]
    Llm(#[from] memini_llm::LlmError),
    #[error("{0}")]
    Backend(String),
}
pub type Result<T> = std::result::Result<T, ServiceError>;

#[derive(Clone, Debug, Default)]
pub struct RememberInput {
    pub namespace: String,
    pub home: String,
    pub visibility: String,
    pub content: String,
    pub tier: Option<Tier>,
    pub summary: String,
    pub tags: Vec<String>,
    pub metadata: Map<String, Value>,
    pub importance: Option<f64>,
    pub ttl: Option<Duration>,
    pub id: String,
    pub confidence: Option<f64>,
    pub valid_from: Option<DateTime<Utc>>,
    pub valid_to: Option<DateTime<Utc>>,
    pub level: Option<Level>,
    pub author: String,
}
#[derive(Clone, Debug, Serialize)]
pub struct MergeHint {
    pub similar_id: String,
    pub similar_content: String,
    pub score: f64,
    pub tier: Tier,
}
#[derive(Clone, Debug)]
pub struct RememberOutcome {
    pub memory: Option<Memory>,
    pub merge_hint: Option<MergeHint>,
    pub auto_superseded: bool,
}
#[derive(Clone, Debug, Default)]
pub struct RecallInput {
    pub namespace: String,
    pub query: String,
    pub tiers: Vec<Tier>,
    pub levels: Vec<Level>,
    pub tags: Vec<String>,
    pub metadata: Map<String, Value>,
    pub exclude_metadata: Map<String, Value>,
    pub limit: usize,
    pub include_expired: bool,
    pub include_superseded: bool,
    pub as_of: Option<DateTime<Utc>>,
    pub subtree: bool,
    pub namespaces: Vec<String>,
    pub home: String,
    pub scope: String,
    pub min_score: f64,
    pub min_semantic_score: f64,
    pub include_linked: bool,
    pub include_fresh_turns: bool,
    pub semantic_reserve: usize,
    pub query_rewrite: bool,
}
#[derive(Clone, Debug, Default)]
pub struct ListInput {
    pub namespace: String,
    pub tiers: Vec<Tier>,
    pub levels: Vec<Level>,
    pub tags: Vec<String>,
    pub metadata: Map<String, Value>,
    pub memory_types: Vec<String>,
    pub created_after: Option<DateTime<Utc>>,
    pub accessed_after: Option<DateTime<Utc>>,
    pub include_expired: bool,
    pub include_superseded: bool,
    pub sort: Sort,
    pub limit: Option<usize>,
    pub offset: usize,
    pub all_namespaces: bool,
    pub namespaces: Vec<String>,
}

#[derive(Clone)]
struct DistillBuffer {
    items: Vec<Memory>,
    tokens: usize,
    oldest: DateTime<Utc>,
}

#[derive(Clone)]
pub struct Service {
    store: Arc<dyn Store>,
    embedder: Arc<dyn Embedder>,
    links: Option<Arc<dyn LinkStore>>,
    events: Option<Arc<dyn EventLogStore>>,
    keys: Option<Arc<dyn ApiKeyStore>>,
    reranker: Option<Arc<dyn Reranker>>,
    rerank_pool: usize,
    answerer: Option<Arc<dyn LlmClient>>,
    consolidator: Option<Arc<dyn LlmClient>>,
    distiller: Option<Arc<dyn LlmClient>>,
    consolidate_min_score: f64,
    consolidate_mode: String,
    write_dedup_score: f64,
    write_dedup_action: String,
    split_dedup_llm_merge: bool,
    dedup_llm_merge: bool,
    distill_on_write: bool,
    distill_batch_tokens: usize,
    distill_batch_max_age: Duration,
    distill_batches: Arc<tokio::sync::Mutex<HashMap<String, DistillBuffer>>>,
    extract_on_write: bool,
    promote_min_access: i64,
    now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
    id: Arc<dyn Fn() -> String + Send + Sync>,
    score_fusion_alpha: f64,
    pool_factor: usize,
    pool_floor: usize,
    min_score: f64,
    min_semantic_score: f64,
    recall_embed_timeout: Option<StdDuration>,
    recall_rewrite_timeout: Option<StdDuration>,
    write_embed_timeout: Option<StdDuration>,
    query_prefix: String,
    cascade: bool,
    redact_secrets: bool,
    quarantine_garbled: bool,
    fingerprint_dedup: bool,
    corroborate_min_score: f64,
    contradict_min_score: f64,
    temporal_boost: f64,
    stability_k: f64,
    turn_echo_window: Duration,
    episodic_min_chars: usize,
    semantic_reserve: usize,
    metrics: memini_observability::Registry,
}
impl Service {
    pub fn new(store: Arc<dyn Store>, embedder: Arc<dyn Embedder>) -> Self {
        Self {
            store,
            embedder,
            links: None,
            events: None,
            keys: None,
            reranker: None,
            rerank_pool: 0,
            answerer: None,
            consolidator: None,
            distiller: None,
            consolidate_min_score: 0.3,
            consolidate_mode: "sync".into(),
            write_dedup_score: 0.0,
            write_dedup_action: "off".into(),
            split_dedup_llm_merge: false,
            dedup_llm_merge: false,
            distill_on_write: false,
            distill_batch_tokens: 0,
            distill_batch_max_age: Duration::minutes(10),
            distill_batches: Arc::new(tokio::sync::Mutex::new(HashMap::new())),
            extract_on_write: false,
            promote_min_access: 3,
            now: Arc::new(Utc::now),
            id: Arc::new(|| Uuid::new_v4().to_string()),
            score_fusion_alpha: search::DEFAULT_FUSION_ALPHA,
            pool_factor: DEFAULT_POOL_FACTOR,
            pool_floor: DEFAULT_POOL_FLOOR,
            min_score: 0.0,
            min_semantic_score: 0.0,
            recall_embed_timeout: None,
            recall_rewrite_timeout: None,
            write_embed_timeout: None,
            query_prefix: String::new(),
            cascade: true,
            redact_secrets: true,
            quarantine_garbled: false,
            fingerprint_dedup: true,
            corroborate_min_score: 0.0,
            contradict_min_score: 0.0,
            temporal_boost: memini_core::temporal::DEFAULT_TEMPORAL_BOOST,
            stability_k: 1.0,
            turn_echo_window: Duration::minutes(5),
            episodic_min_chars: 0,
            semantic_reserve: 0,
            metrics: memini_observability::Registry::default(),
        }
    }
    pub fn with_link_store(mut self, v: Arc<dyn LinkStore>) -> Self {
        self.links = Some(v);
        self
    }
    pub fn with_event_store(mut self, v: Arc<dyn EventLogStore>) -> Self {
        self.events = Some(v);
        self
    }
    pub fn with_api_key_store(mut self, v: Arc<dyn ApiKeyStore>) -> Self {
        self.keys = Some(v);
        self
    }
    pub fn with_reranker(mut self, v: Arc<dyn Reranker>) -> Self {
        self.reranker = Some(v);
        self
    }
    pub fn with_rerank_pool(mut self, value: usize) -> Self {
        self.rerank_pool = value;
        self
    }
    pub fn with_answerer(mut self, v: Arc<dyn LlmClient>) -> Self {
        self.answerer = Some(v);
        self
    }
    pub fn with_consolidator(mut self, v: Arc<dyn LlmClient>, min_score: f64) -> Self {
        self.consolidator = Some(v);
        self.consolidate_min_score = min_score;
        self
    }
    pub fn with_consolidate_mode(mut self, value: impl Into<String>) -> Self {
        self.consolidate_mode = value.into();
        self
    }
    pub fn with_write_dedup(mut self, score: f64, action: impl Into<String>) -> Self {
        self.write_dedup_score = score;
        self.write_dedup_action = action.into();
        self
    }
    pub fn with_split_dedup_llm_merge(mut self, value: bool) -> Self {
        self.split_dedup_llm_merge = value;
        self
    }
    pub fn with_dedup_llm_merge(mut self, value: bool) -> Self {
        self.dedup_llm_merge = value;
        self
    }
    pub fn with_distiller(mut self, v: Arc<dyn LlmClient>) -> Self {
        self.distiller = Some(v);
        self
    }
    pub fn with_distill_on_write(mut self, value: bool) -> Self {
        self.distill_on_write = value;
        self
    }
    pub fn with_distill_batch(mut self, max_tokens: usize, max_age: Duration) -> Self {
        self.distill_batch_tokens = max_tokens;
        if max_age > Duration::zero() {
            self.distill_batch_max_age = max_age;
        }
        self
    }
    pub fn with_extract_on_write(mut self, value: bool) -> Self {
        self.extract_on_write = value;
        self
    }
    pub fn with_promote_min_access(mut self, value: i64) -> Self {
        self.promote_min_access = value;
        self
    }
    pub fn has_answerer(&self) -> bool {
        self.answerer.is_some()
    }
    pub fn with_clock(mut self, v: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>) -> Self {
        self.now = v;
        self
    }
    pub fn with_id_generator(mut self, v: Arc<dyn Fn() -> String + Send + Sync>) -> Self {
        self.id = v;
        self
    }
    pub fn with_score_fusion(mut self, v: f64) -> Self {
        self.score_fusion_alpha = v;
        self
    }
    pub fn with_recall_pool(mut self, factor: usize, floor: usize) -> Self {
        self.pool_factor = factor.max(1);
        self.pool_floor = floor;
        self
    }
    pub fn with_min_scores(mut self, fused: f64, semantic: f64) -> Self {
        self.min_score = fused;
        self.min_semantic_score = semantic;
        self
    }
    pub fn with_embed_timeouts(
        mut self,
        recall: Option<StdDuration>,
        write: Option<StdDuration>,
    ) -> Self {
        self.recall_embed_timeout = recall;
        self.write_embed_timeout = write;
        self
    }
    pub fn with_rewrite_timeout(mut self, value: Option<StdDuration>) -> Self {
        self.recall_rewrite_timeout = value;
        self
    }
    pub fn with_query_prefix(mut self, v: impl Into<String>) -> Self {
        self.query_prefix = v.into();
        self
    }
    pub fn with_cascade(mut self, v: bool) -> Self {
        self.cascade = v;
        self
    }
    pub fn with_secret_redaction(mut self, v: bool) -> Self {
        self.redact_secrets = v;
        self
    }
    pub fn with_corruption_quarantine(mut self, v: bool) -> Self {
        self.quarantine_garbled = v;
        self
    }
    pub fn with_fingerprint_dedup(mut self, v: bool) -> Self {
        self.fingerprint_dedup = v;
        self
    }
    pub fn with_corroboration(mut self, score: f64) -> Self {
        self.corroborate_min_score = score;
        self
    }
    pub fn with_contradiction_downrank(mut self, score: f64) -> Self {
        self.contradict_min_score = score;
        self
    }
    pub fn with_temporal_boost(mut self, value: f64) -> Self {
        self.temporal_boost = value;
        self
    }
    pub fn with_stability(mut self, value: f64) -> Self {
        self.stability_k = value.max(0.0);
        self
    }
    pub fn with_turn_echo_window(mut self, value: Duration) -> Self {
        self.turn_echo_window = value;
        self
    }
    pub fn with_episodic_min_chars(mut self, value: usize) -> Self {
        self.episodic_min_chars = value;
        self
    }
    pub fn with_semantic_reserve(mut self, value: usize) -> Self {
        self.semantic_reserve = value;
        self
    }
    pub fn with_metrics(mut self, metrics: memini_observability::Registry) -> Self {
        self.metrics = metrics;
        self
    }
    pub fn store(&self) -> &Arc<dyn Store> {
        &self.store
    }

    #[async_recursion::async_recursion]
    pub async fn remember(&self, input: RememberInput) -> Result<Option<Memory>> {
        Ok(self.remember_with_outcome(input).await?.memory)
    }

    #[async_recursion::async_recursion]
    pub async fn remember_with_outcome(&self, input: RememberInput) -> Result<RememberOutcome> {
        let started = std::time::Instant::now();
        let tier = input
            .tier
            .map(|tier| format!("{tier:?}").to_lowercase())
            .unwrap_or_else(|| "auto".into());
        let result = self.remember_with_outcome_inner(input).await;
        self.metrics.inc(
            "memini_remember_results_total",
            &[
                ("result", if result.is_ok() { "ok" } else { "error" }),
                ("tier", &tier),
            ],
        );
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "remember")],
            started.elapsed().as_secs_f64(),
        );
        result
    }

    #[async_recursion::async_recursion]
    async fn remember_with_outcome_inner(
        &self,
        mut input: RememberInput,
    ) -> Result<RememberOutcome> {
        if input.namespace.is_empty() {
            return Err(ServiceError::InvalidInput(
                "remember: namespace is required".into(),
            ));
        }
        if input.content.is_empty() {
            return Err(ServiceError::InvalidInput(
                "remember: content is required".into(),
            ));
        }
        let tier = input.tier.unwrap_or_else(|| {
            extract::classify(&input.content).map_or(Tier::Working, extract::Kind::tier)
        });
        if input.tier.is_none() && tier != Tier::Working {
            let tier_label = format!("{tier:?}").to_lowercase();
            self.metrics
                .inc("memini_tier_classified_total", &[("tier", &tier_label)]);
        }
        input.namespace =
            resolve_visibility(&input.namespace, &input.home, &input.visibility, tier)?;
        if input.tier.is_none() && tier != Tier::Working {
            input.metadata.insert(
                "classified_tier".into(),
                Value::String(
                    extract::classify(&input.content)
                        .map_or("", extract::Kind::as_str)
                        .into(),
                ),
            );
        }
        if !input.author.is_empty() {
            input
                .metadata
                .entry("author")
                .or_insert(Value::String(input.author.clone()));
        }
        if self.redact_secrets {
            input.content = redact::secrets(&input.content);
            input.summary = redact::secrets(&input.summary);
            redact::metadata(&mut input.metadata);
        }
        let clean_content = sanitize::clean(&input.content);
        let clean_summary = sanitize::clean(&input.summary);
        if clean_content != input.content || clean_summary != input.summary {
            self.metrics
                .inc("memini_write_sanitized_total", &[("action", "cleaned")]);
        }
        input.content = clean_content;
        input.summary = clean_summary;
        if input.content.is_empty() {
            return Err(ServiceError::InvalidInput(
                "remember: content is empty after sanitization".into(),
            ));
        }
        if self.quarantine_garbled && sanitize::garbled(&input.content) {
            input.importance = Some(0.0);
            input
                .metadata
                .insert("quarantined".into(), Value::Bool(true));
            self.metrics
                .inc("memini_write_sanitized_total", &[("action", "quarantined")]);
        }
        if tier == Tier::Episodic
            && self.episodic_min_chars > 0
            && signal_chars(&input.content) < self.episodic_min_chars
        {
            return Ok(RememberOutcome {
                memory: None,
                merge_hint: None,
                auto_superseded: false,
            });
        }
        let now = (self.now)();
        if input.id.is_empty()
            && self.fingerprint_dedup
            && let Ok(mut existing) = self
                .store
                .get_by_fingerprint(
                    &input.namespace,
                    tier,
                    &fingerprint(&input.content),
                    Some(now),
                )
                .await
        {
            self.store
                .reinforce(
                    &input.namespace,
                    std::slice::from_ref(&existing.id),
                    now,
                    existing
                        .expires_at
                        .map(|_| now + tier.default_ttl().unwrap_or_default()),
                )
                .await?;
            existing = self.store.get(&input.namespace, &existing.id).await?;
            return Ok(RememberOutcome {
                memory: Some(existing),
                merge_hint: None,
                auto_superseded: false,
            });
        }
        let mut existing = None;
        if !input.id.is_empty() {
            match self.store.get(&input.namespace, &input.id).await {
                Ok(v) => existing = Some(v),
                Err(StoreError::NotFound) => {}
                Err(e) => return Err(e.into()),
            }
        }
        let embedding = match self.write_embed_timeout {
            Some(timeout) => match tokio::time::timeout(
                timeout,
                embed_one(self.embedder.as_ref(), &input.content),
            )
            .await
            {
                Ok(Ok(v)) => v,
                Err(_) => {
                    self.metrics.inc(
                        "memini_remember_degraded_total",
                        &[("reason", "embed_timeout")],
                    );
                    input
                        .metadata
                        .insert("pending_embed".into(), Value::String("true".into()));
                    vec![]
                }
                Ok(Err(_)) => {
                    self.metrics.inc(
                        "memini_remember_degraded_total",
                        &[("reason", "embed_error")],
                    );
                    input
                        .metadata
                        .insert("pending_embed".into(), Value::String("true".into()));
                    vec![]
                }
            },
            None => embed_one(self.embedder.as_ref(), &input.content).await?,
        };
        let expires_at = match input.ttl {
            Some(v) if v < Duration::zero() => None,
            Some(v) => Some(now + v),
            None => tier.default_ttl().map(|v| now + v),
        };
        if let Some(v) = input.ttl.filter(|v| *v > Duration::zero()) {
            input
                .metadata
                .insert("ttl_seconds".into(), Value::from(v.num_seconds()));
        }
        let durable = tier.term() == Term::Long;
        let fresh = input.id.is_empty();
        let memory = Memory {
            id: if input.id.is_empty() {
                (self.id)()
            } else {
                input.id
            },
            namespace: input.namespace,
            tier,
            level: input.level,
            content: input.content,
            summary: input.summary,
            metadata: input.metadata,
            tags: input.tags,
            importance: input.importance.unwrap_or_else(|| seed_importance(tier)),
            created_at: existing.as_ref().map_or(now, |v| v.created_at),
            updated_at: now,
            last_accessed_at: now,
            access_count: existing.as_ref().map_or(0, |v| v.access_count),
            expires_at,
            superseded_by: existing.as_ref().and_then(|v| v.superseded_by.clone()),
            valid_from: input
                .valid_from
                .or_else(|| existing.as_ref().and_then(|v| v.valid_from))
                .or(durable.then_some(now)),
            valid_to: input.valid_to,
            confidence: if durable {
                input
                    .confidence
                    .or_else(|| existing.as_ref().and_then(|v| v.confidence))
                    .or(Some(CONFIDENCE_SEED_FRESH))
            } else {
                None
            },
            linked_memory_ids: existing
                .as_ref()
                .map_or_else(Vec::new, |v| v.linked_memory_ids.clone()),
            embedding,
        };
        let mut fuzzy_supersede = None;
        let mut merge_hint = None;
        if fresh
            && !memory.embedding.is_empty()
            && self.write_dedup_score > 0.0
            && self.write_dedup_action != "off"
            && (self.write_dedup_action != "hint" || durable)
            && let Ok(candidates) = self
                .store
                .vector_search(
                    &memory.namespace,
                    &memory.embedding,
                    &Filter {
                        tiers: vec![tier],
                        now: Some(now),
                        ..Filter::default()
                    },
                    5,
                )
                .await
            && let Some(nearest) = candidates
                .into_iter()
                .find(|v| v.score >= self.write_dedup_score)
        {
            let mut llm_decided = false;
            if self.split_dedup_llm_merge
                && let Some(client) = &self.consolidator
                && let Ok(close) = self
                    .store
                    .vector_search(
                        &memory.namespace,
                        &memory.embedding,
                        &Filter {
                            tiers: vec![tier],
                            now: Some(now),
                            ..Filter::default()
                        },
                        5,
                    )
                    .await
            {
                let eligible = close
                    .into_iter()
                    .filter(|candidate| candidate.score >= self.write_dedup_score)
                    .collect::<Vec<_>>();
                if eligible.len() >= 2
                    && (eligible[0].score - eligible[1].score).abs() <= 0.05
                    && let Ok(decision) = client
                        .consolidate(&memini_llm::Input {
                            new_memory: memory.content.clone(),
                            tier: format!("{:?}", tier).to_lowercase(),
                            candidates: eligible
                                .iter()
                                .map(|candidate| memini_llm::Candidate {
                                    id: candidate.memory.id.clone(),
                                    content: candidate.memory.content.clone(),
                                })
                                .collect(),
                        })
                        .await
                {
                    match decision.action {
                        memini_llm::Action::Supersede => {
                            fuzzy_supersede =
                                (!decision.target.is_empty()).then_some(decision.target);
                            llm_decided = true;
                        }
                        memini_llm::Action::Update => {
                            if let Some(candidate) = eligible
                                .iter()
                                .find(|candidate| candidate.memory.id == decision.target)
                            {
                                let mut updated = candidate.memory.clone();
                                if !decision.content.is_empty() {
                                    updated.content = decision.content;
                                }
                                if !decision.summary.is_empty() {
                                    updated.summary = decision.summary;
                                }
                                updated.embedding =
                                    embed_one(self.embedder.as_ref(), &updated.content).await?;
                                updated.updated_at = now;
                                self.store.upsert(&updated).await?;
                                return Ok(RememberOutcome {
                                    memory: Some(updated),
                                    merge_hint: None,
                                    auto_superseded: false,
                                });
                            }
                        }
                        memini_llm::Action::New => {}
                    }
                }
            }
            if llm_decided {
                // The LLM selected the supersession target; bypass the deterministic action.
            } else {
                match self.write_dedup_action.as_str() {
                    "coalesce"
                        if word_set_score(&memory.content)
                            <= word_set_score(&nearest.memory.content) =>
                    {
                        self.store
                            .reinforce(
                                &memory.namespace,
                                std::slice::from_ref(&nearest.memory.id),
                                now,
                                nearest
                                    .memory
                                    .expires_at
                                    .map(|_| now + tier.default_ttl().unwrap_or_default()),
                            )
                            .await?;
                        return Ok(RememberOutcome {
                            memory: Some(
                                self.store
                                    .get(&memory.namespace, &nearest.memory.id)
                                    .await?,
                            ),
                            merge_hint: None,
                            auto_superseded: false,
                        });
                    }
                    "coalesce" | "supersede" => fuzzy_supersede = Some(nearest.memory.id),
                    "hint" => {
                        merge_hint = Some(MergeHint {
                            similar_id: nearest.memory.id,
                            similar_content: nearest.memory.content,
                            score: nearest.score,
                            tier: nearest.memory.tier,
                        })
                    }
                    _ => {}
                }
            }
        }
        if fresh
            && durable
            && !memory.embedding.is_empty()
            && self.consolidate_mode == "sync"
            && let Some(client) = &self.consolidator
        {
            let candidates = self
                .store
                .vector_search(
                    &memory.namespace,
                    &memory.embedding,
                    &Filter {
                        tiers: vec![tier],
                        now: Some(now),
                        ..Filter::default()
                    },
                    10,
                )
                .await?
                .into_iter()
                .filter(|v| v.score >= self.consolidate_min_score)
                .collect::<Vec<_>>();
            if candidates.is_empty() {
                self.metrics
                    .inc("memini_consolidate_results_total", &[("result", "gated")]);
            } else {
                let decision = client
                    .consolidate(&memini_llm::Input {
                        new_memory: memory.content.clone(),
                        tier: format!("{:?}", tier).to_lowercase(),
                        candidates: candidates
                            .iter()
                            .map(|v| memini_llm::Candidate {
                                id: v.memory.id.clone(),
                                content: v.memory.content.clone(),
                            })
                            .collect(),
                    })
                    .await?;
                let consolidation_result = match decision.action {
                    memini_llm::Action::New => "new",
                    memini_llm::Action::Update => "update",
                    memini_llm::Action::Supersede => "supersede",
                };
                self.metrics.inc(
                    "memini_consolidate_results_total",
                    &[("result", consolidation_result)],
                );
                match decision.action {
                    memini_llm::Action::New => {}
                    memini_llm::Action::Update => {
                        if let Some(candidate) =
                            candidates.iter().find(|v| v.memory.id == decision.target)
                        {
                            let mut updated = candidate.memory.clone();
                            if !decision.content.is_empty() {
                                updated.content = decision.content
                            }
                            if !decision.summary.is_empty() {
                                updated.summary = decision.summary
                            }
                            updated.embedding =
                                embed_one(self.embedder.as_ref(), &updated.content).await?;
                            updated.updated_at = now;
                            updated.linked_memory_ids = decision.linked_ids;
                            self.store.upsert(&updated).await?;
                            self.log_memory(
                                memini_store::EventKind::Update,
                                &updated.namespace,
                                Some(&updated),
                                Map::new(),
                            )
                            .await;
                            return Ok(RememberOutcome {
                                memory: Some(updated),
                                merge_hint,
                                auto_superseded: false,
                            });
                        }
                    }
                    memini_llm::Action::Supersede => {
                        self.store.upsert(&memory).await?;
                        if !decision.target.is_empty() {
                            self.store
                                .set_superseded(&memory.namespace, &decision.target, &memory.id)
                                .await?
                        }
                        self.log_memory(
                            memini_store::EventKind::Remember,
                            &memory.namespace,
                            Some(&memory),
                            Map::new(),
                        )
                        .await;
                        return Ok(RememberOutcome {
                            memory: Some(memory),
                            merge_hint,
                            auto_superseded: true,
                        });
                    }
                }
            }
        }
        self.store.upsert(&memory).await?;
        let operation = if existing.is_some() {
            "update"
        } else {
            "insert"
        };
        let tier_label = format!("{:?}", memory.tier).to_lowercase();
        let memory_type = memory
            .metadata
            .get("memory_type")
            .and_then(Value::as_str)
            .unwrap_or("");
        self.metrics.inc(
            "memini_store_upserts_total",
            &[
                ("op", operation),
                ("tier", &tier_label),
                ("memory_type", memory_type),
            ],
        );
        let auto_superseded = fuzzy_supersede.is_some();
        if let Some(old_id) = fuzzy_supersede {
            self.store
                .set_superseded(&memory.namespace, &old_id, &memory.id)
                .await?;
            self.metrics.inc("memini_store_soft_deletes_total", &[]);
        }
        self.log_memory(
            if existing.is_some() {
                memini_store::EventKind::Update
            } else {
                memini_store::EventKind::Remember
            },
            &memory.namespace,
            Some(&memory),
            Map::new(),
        )
        .await;
        if fresh && !memory.embedding.is_empty() {
            self.route_confidence(&memory).await?;
        }
        if fresh && memory.tier.term() == Term::Short && !self.enqueue_distill(&memory).await {
            self.build_facts(&memory).await?;
        }
        if fresh
            && durable
            && !memory.embedding.is_empty()
            && self.consolidate_mode == "async"
            && self.consolidator.is_some()
        {
            let service = self.clone();
            let queued = memory.clone();
            tokio::spawn(async move {
                let _ = tokio::time::timeout(
                    StdDuration::from_secs(60),
                    service.consolidate_existing(queued),
                )
                .await;
            });
        }
        Ok(RememberOutcome {
            memory: Some(memory),
            merge_hint,
            auto_superseded,
        })
    }

    async fn consolidate_existing(&self, memory: Memory) -> Result<()> {
        let client = match &self.consolidator {
            Some(value) => value,
            None => return Ok(()),
        };
        let filter = Filter {
            tiers: vec![memory.tier],
            now: Some((self.now)()),
            ..Filter::default()
        };
        let mut candidates = self
            .store
            .vector_search(&memory.namespace, &memory.embedding, &filter, 11)
            .await?;
        if let Ok(keyword) = self
            .store
            .keyword_search(&memory.namespace, &memory.content, &filter, 11)
            .await
        {
            let mut seen: HashSet<String> =
                candidates.iter().map(|v| v.memory.id.clone()).collect();
            candidates.extend(
                keyword
                    .into_iter()
                    .filter(|v| seen.insert(v.memory.id.clone())),
            );
        }
        candidates.retain(|v| v.memory.id != memory.id);
        if candidates
            .first()
            .is_none_or(|v| v.score < self.consolidate_min_score)
        {
            self.metrics
                .inc("memini_consolidate_results_total", &[("result", "gated")]);
            return Ok(());
        }
        let decision = client
            .consolidate(&memini_llm::Input {
                new_memory: memory.content.clone(),
                tier: format!("{:?}", memory.tier).to_lowercase(),
                candidates: candidates
                    .iter()
                    .take(10)
                    .map(|v| memini_llm::Candidate {
                        id: v.memory.id.clone(),
                        content: v.memory.content.clone(),
                    })
                    .collect(),
            })
            .await?;
        let consolidation_result = match decision.action {
            memini_llm::Action::New => "new",
            memini_llm::Action::Update => "update",
            memini_llm::Action::Supersede => "supersede",
        };
        self.metrics.inc(
            "memini_consolidate_results_total",
            &[("result", consolidation_result)],
        );
        match decision.action {
            memini_llm::Action::New => {
                if !decision.linked_ids.is_empty() {
                    let mut updated = self.store.get(&memory.namespace, &memory.id).await?;
                    updated.linked_memory_ids = decision.linked_ids;
                    updated.updated_at = (self.now)();
                    self.store.upsert(&updated).await?;
                }
            }
            memini_llm::Action::Update
                if !decision.target.is_empty() && decision.target != memory.id =>
            {
                if let Ok(mut target) = self.store.get(&memory.namespace, &decision.target).await {
                    target.content = if decision.content.is_empty() {
                        memory.content.clone()
                    } else {
                        decision.content
                    };
                    if !decision.summary.is_empty() {
                        target.summary = decision.summary;
                    }
                    target.embedding = embed_one(self.embedder.as_ref(), &target.content).await?;
                    target.updated_at = (self.now)();
                    self.store.upsert(&target).await?;
                    self.store
                        .set_superseded(&memory.namespace, &memory.id, &target.id)
                        .await?;
                }
            }
            memini_llm::Action::Supersede
                if !decision.target.is_empty() && decision.target != memory.id =>
            {
                self.store
                    .set_superseded(&memory.namespace, &decision.target, &memory.id)
                    .await?;
            }
            _ => {}
        }
        Ok(())
    }

    pub async fn recall(
        &self,
        input: RecallInput,
    ) -> Result<(Vec<Scored>, Option<String>, Vec<ReadSetEntry>)> {
        let started = std::time::Instant::now();
        let tier_filter = if input.tiers.len() == 1 {
            format!("{:?}", input.tiers[0]).to_lowercase()
        } else if input.tiers.is_empty() {
            "all".into()
        } else {
            "mixed".into()
        };
        let result = self.recall_inner(input).await;
        let hits = result.as_ref().map_or(0, |value| value.0.len());
        let bucket = match hits {
            0 => "0",
            1 => "1",
            2..=5 => "2-5",
            6..=20 => "6-20",
            _ => "21+",
        };
        self.metrics.inc(
            "memini_recall_results_total",
            &[
                ("result", if result.is_ok() { "ok" } else { "error" }),
                ("tier_filter", &tier_filter),
                ("hits_bucket", bucket),
            ],
        );
        if let Ok((_, Some(reason), _)) = &result {
            self.metrics
                .inc("memini_recall_degraded_total", &[("reason", reason)]);
        }
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "recall")],
            started.elapsed().as_secs_f64(),
        );
        result
    }

    async fn recall_inner(
        &self,
        input: RecallInput,
    ) -> Result<(Vec<Scored>, Option<String>, Vec<ReadSetEntry>)> {
        if input.namespace.is_empty() {
            return Err(ServiceError::InvalidInput(
                "recall: namespace is required".into(),
            ));
        }
        if input.query.is_empty() {
            return Err(ServiceError::InvalidInput(
                "recall: query is required".into(),
            ));
        }
        if input.query_rewrite
            && should_expand(&input.query)
            && let Some(answerer) = &self.answerer
        {
            let prompt = format!("Question: {}", input.query);
            let complete=answerer.complete("You rewrite a question into memory-search queries. Reply with 2 or 3 short search queries, one per line, with no numbering or commentary. Make them lexically diverse and preserve exact names.",&prompt);
            let output = match self.recall_rewrite_timeout {
                Some(timeout) => tokio::time::timeout(timeout, complete)
                    .await
                    .ok()
                    .and_then(std::result::Result::ok),
                None => complete.await.ok(),
            };
            if let Some(output) = output {
                let mut queries = vec![input.query.clone()];
                for line in output.lines() {
                    let query = line
                        .trim()
                        .trim_start_matches(|c: char| {
                            c == '-' || c == '*' || c == '.' || c.is_ascii_digit()
                        })
                        .trim();
                    if !query.is_empty() && queries.len() < 4 {
                        queries.push(query.into())
                    }
                }
                if queries.len() > 1 {
                    let mut lists = Vec::new();
                    let mut degraded = None;
                    let mut read_set = Vec::new();
                    for (query_index, query) in queries.into_iter().enumerate() {
                        let mut sub = input.clone();
                        sub.query = query;
                        sub.query_rewrite = false;
                        let (results, reason, entries) = Box::pin(self.recall(sub)).await?;
                        if degraded.is_none() {
                            degraded = reason
                        }
                        if query_index == 0 {
                            read_set = entries
                        }
                        lists.push(results)
                    }
                    return Ok((
                        search::fuse(&lists, input.limit, search::DEFAULT_RRF_K),
                        degraded,
                        read_set,
                    ));
                }
            }
        }
        let limit = if input.limit == 0 {
            10
        } else {
            input.limit.min(MAX_RECALL_LIMIT)
        };
        let pool = (limit * self.pool_factor).max(self.pool_floor);
        let filter = Filter {
            tiers: input.tiers.clone(),
            levels: input.levels.clone(),
            tags: input.tags.clone(),
            metadata: input.metadata.clone(),
            exclude_metadata: input.exclude_metadata.clone(),
            include_expired: input.include_expired,
            include_superseded: input.include_superseded,
            now: Some((self.now)()),
            as_of: input.as_of,
            ..Filter::default()
        };
        let embed = async {
            match self.recall_embed_timeout {
                Some(v) => tokio::time::timeout(
                    v,
                    embed_one(
                        self.embedder.as_ref(),
                        &(self.query_prefix.clone() + &input.query),
                    ),
                )
                .await
                .ok()
                .and_then(std::result::Result::ok),
                None => embed_one(
                    self.embedder.as_ref(),
                    &(self.query_prefix.clone() + &input.query),
                )
                .await
                .ok(),
            }
        };
        let (vector, entries) = tokio::join!(embed, self.resolve_read_set(&input));
        let entries = entries?;
        let degraded = vector.is_none().then(|| "embed_error".to_owned());
        let mut lists = Vec::new();
        for entry in &entries {
            let mut f = filter.clone();
            if let Some(tiers) = &entry.tiers {
                f.tiers = tiers.clone();
            }
            let keyword_search =
                self.store
                    .keyword_search(&entry.namespace, &input.query, &f, pool);
            let vector_search = async {
                if let Some(value) = &vector {
                    self.store
                        .vector_search(&entry.namespace, value, &f, pool)
                        .await
                } else {
                    Ok(Vec::new())
                }
            };
            let (keyword, vector_hits) = tokio::try_join!(keyword_search, vector_search)?;
            let allowed: HashSet<_> = vector_hits
                .iter()
                .filter(|v| v.score >= input.min_semantic_score.max(self.min_semantic_score))
                .map(|v| v.memory.id.clone())
                .collect();
            let keyword = if input.min_semantic_score.max(self.min_semantic_score) > 0.0 {
                keyword
                    .into_iter()
                    .filter(|v| allowed.contains(&v.memory.id))
                    .collect()
            } else {
                keyword
            };
            lists.push(if self.score_fusion_alpha >= 0.0 {
                search::fuse_scores(
                    &[vector_hits, keyword],
                    &[self.score_fusion_alpha, 1.0 - self.score_fusion_alpha],
                    0,
                )
            } else {
                search::fuse(&[vector_hits, keyword], 0, search::DEFAULT_RRF_K)
            });
        }
        let mut fused = if lists.len() == 1 {
            lists.pop().unwrap()
        } else if self.score_fusion_alpha >= 0.0 {
            search::fuse_scores(&lists, &[], 0)
        } else {
            search::fuse(&lists, 0, search::DEFAULT_RRF_K)
        };
        if self.score_fusion_alpha >= 0.0 {
            let floor = input.min_score.max(self.min_score);
            fused.retain(|v| v.score >= floor);
        }
        let now = (self.now)();
        let mut ranked = if self.temporal_boost > 0.0 {
            memini_core::temporal::rerank_temporal(
                &fused,
                &input.query,
                now,
                search::DEFAULT_RERANK_WEIGHTS,
                self.stability_k,
                self.temporal_boost,
            )
        } else {
            search::rerank(&fused, now, self.stability_k)
        };
        if !input.include_fresh_turns && self.turn_echo_window > Duration::zero() {
            let cutoff = now - self.turn_echo_window;
            ranked.retain(|v| {
                !(v.memory.tier.term() == Term::Short
                    && v.memory.created_at > cutoff
                    && v.memory.metadata.get("format").and_then(Value::as_str) == Some("turn"))
            });
        }
        let ranking_limit = if self.reranker.is_some() && self.rerank_pool > 0 {
            self.rerank_pool.max(limit)
        } else {
            limit
        };
        reserve_durable(
            &mut ranked,
            ranking_limit,
            if input.semantic_reserve > 0 {
                input.semantic_reserve
            } else {
                self.semantic_reserve
            },
        );
        if let Some(reranker) = &self.reranker {
            let candidates = ranked
                .iter()
                .map(|v| Candidate {
                    id: v.memory.id.clone(),
                    content: v.memory.content.clone(),
                })
                .collect::<Vec<_>>();
            let reranked = reranker.rerank(&input.query, &candidates).await;
            self.metrics.inc(
                "memini_rerank_results_total",
                &[
                    ("backend", reranker.backend()),
                    ("result", if reranked.is_ok() { "ok" } else { "fallback" }),
                ],
            );
            if let Ok(order) = reranked {
                let positions: HashMap<_, _> = order
                    .into_iter()
                    .enumerate()
                    .map(|(i, id)| (id, i))
                    .collect();
                ranked.sort_by_key(|v| positions.get(&v.memory.id).copied().unwrap_or(usize::MAX));
            }
        }
        ranked = search::dedup(&ranked, limit);
        if input.include_linked {
            ranked = self.expand_linked(ranked, limit).await?;
        }
        self.reinforce(&ranked).await;
        self.log_recall(&input.namespace, &input.query, &ranked, degraded.as_deref())
            .await;
        Ok((ranked, degraded, entries))
    }

    pub async fn get(&self, namespace: &str, id: &str) -> Result<Memory> {
        let memory = self.store.get(namespace, id).await?;
        self.log_memory(
            memini_store::EventKind::Get,
            namespace,
            Some(&memory),
            Map::new(),
        )
        .await;
        Ok(memory)
    }
    pub async fn forget(&self, namespace: &str, id: &str) -> Result<()> {
        let started = std::time::Instant::now();
        let result = self.forget_inner(namespace, id).await;
        let outcome = match &result {
            Ok(()) => "ok",
            Err(ServiceError::Store(StoreError::NotFound)) => "not_found",
            Err(_) => "error",
        };
        self.metrics
            .inc("memini_forget_results_total", &[("result", outcome)]);
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "forget")],
            started.elapsed().as_secs_f64(),
        );
        result
    }
    async fn forget_inner(&self, namespace: &str, id: &str) -> Result<()> {
        let memory = self.store.get(namespace, id).await.ok();
        self.store.delete(namespace, id).await?;
        self.metrics.inc("memini_store_deletes_total", &[]);
        self.log_memory(
            memini_store::EventKind::Forget,
            namespace,
            memory.as_ref(),
            Map::new(),
        )
        .await;
        Ok(())
    }
    pub async fn supersede(&self, namespace: &str, id: &str, replacement: &str) -> Result<()> {
        let started = std::time::Instant::now();
        let result = self.supersede_inner(namespace, id, replacement).await;
        let outcome = match &result {
            Ok(()) => "ok",
            Err(ServiceError::Store(StoreError::NotFound)) => "not_found",
            Err(_) => "error",
        };
        self.metrics
            .inc("memini_supersede_results_total", &[("result", outcome)]);
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "supersede")],
            started.elapsed().as_secs_f64(),
        );
        result
    }
    async fn supersede_inner(&self, namespace: &str, id: &str, replacement: &str) -> Result<()> {
        if id.trim().is_empty() || replacement.trim().is_empty() {
            return Err(ServiceError::InvalidInput(
                "supersede: id and replacement are required".into(),
            ));
        }
        let memory = self.store.get(namespace, id).await.ok();
        self.store
            .set_superseded(namespace, id, replacement)
            .await?;
        self.metrics.inc("memini_store_soft_deletes_total", &[]);
        self.log_memory(
            memini_store::EventKind::Supersede,
            namespace,
            memory.as_ref(),
            Map::from_iter([("superseded_by".into(), Value::String(replacement.into()))]),
        )
        .await;
        Ok(())
    }
    pub async fn history(&self, namespace: &str, id: &str) -> Result<Vec<Memory>> {
        let root = self.store.get(namespace, id).await?;
        let mut seen = HashMap::from([(root.id.clone(), root.clone())]);
        let mut queue = VecDeque::from([root]);
        while let Some(current) = queue.pop_front() {
            let mut ids = self.store.predecessor_ids(namespace, &current.id).await?;
            if let Some(v) = current.superseded_by {
                ids.push(v);
            }
            for id in ids {
                if seen.contains_key(&id) {
                    continue;
                }
                if let Ok(v) = self.store.get(namespace, &id).await {
                    seen.insert(id, v.clone());
                    queue.push_back(v);
                }
            }
        }
        let mut out: Vec<_> = seen.into_values().collect();
        out.sort_by(|a, b| {
            a.created_at
                .cmp(&b.created_at)
                .then_with(|| a.id.cmp(&b.id))
        });
        Ok(out)
    }
    pub async fn list(&self, input: ListInput) -> Result<Vec<Memory>> {
        let filter = Filter {
            tiers: input.tiers,
            levels: input.levels,
            tags: input.tags,
            metadata: input.metadata,
            memory_types: input.memory_types,
            created_after: input.created_after,
            accessed_after: input.accessed_after,
            include_expired: input.include_expired,
            include_superseded: input.include_superseded,
            sort: input.sort,
            ..Filter::default()
        };
        if !input.all_namespaces {
            let fetch = input.limit.map(|limit| limit.saturating_add(input.offset));
            let values = self.store.list(&input.namespace, &filter, fetch).await?;
            return Ok(values
                .into_iter()
                .skip(input.offset)
                .take(input.limit.unwrap_or(usize::MAX))
                .collect());
        }
        let names = if input.namespaces.is_empty() {
            self.store.list_namespaces().await?
        } else {
            input.namespaces
        };
        let mut all = Vec::new();
        for ns in names {
            all.extend(self.store.list(&ns, &filter, input.limit).await?)
        }
        all.sort_by(|a, b| memini_store::compare_memories(a, b, &filter));
        Ok(all
            .into_iter()
            .skip(input.offset)
            .take(input.limit.unwrap_or(usize::MAX))
            .collect())
    }
    pub async fn namespaces(&self) -> Result<Vec<String>> {
        Ok(self.store.list_namespaces().await?)
    }
    pub async fn resolve_read_set_info(
        &self,
        namespace: &str,
        home: &str,
    ) -> Result<Vec<ReadSetEntry>> {
        self.resolve_read_set(&RecallInput {
            namespace: namespace.into(),
            home: home.into(),
            ..RecallInput::default()
        })
        .await
    }
    pub async fn delete_namespace(&self, namespace: &str) -> Result<u64> {
        Ok(self.store.delete_namespace(namespace).await?)
    }
    pub async fn move_namespace(
        &self,
        from: &str,
        to: &str,
        dry_run: bool,
    ) -> Result<memini_maintenance::RenamespaceReport> {
        let report =
            memini_maintenance::move_namespace(self.store.as_ref(), from, to, dry_run).await?;
        if !dry_run && report.moved > 0 {
            if let Some(links) = &self.links {
                links.rename_link_endpoints(from, to).await?
            }
            if let Some(keys) = &self.keys {
                keys.rename_api_key_namespaces(from, to).await?
            }
        }
        Ok(report)
    }
    pub async fn split_namespace(
        &self,
        from: &str,
        keys: &[String],
        dry_run: bool,
    ) -> Result<memini_maintenance::RenamespaceReport> {
        Ok(memini_maintenance::split_namespace(self.store.as_ref(), from, keys, dry_run).await?)
    }
    pub async fn reassign(&self, from: &str, id: &str, to: &str) -> Result<u64> {
        Ok(self.store.reassign(from, &[id.to_owned()], to).await?)
    }

    async fn reinforce(&self, results: &[Scored]) {
        let now = (self.now)();
        let mut groups: HashMap<(String, i64), Vec<String>> = HashMap::new();
        for r in results {
            let seconds = r
                .memory
                .metadata
                .get("ttl_seconds")
                .and_then(Value::as_i64)
                .or_else(|| r.memory.tier.default_ttl().map(|v| v.num_seconds()))
                .unwrap_or(0);
            groups
                .entry((r.memory.namespace.clone(), seconds))
                .or_default()
                .push(r.memory.id.clone());
        }
        for ((ns, seconds), ids) in groups {
            let result = self
                .store
                .reinforce(
                    &ns,
                    &ids,
                    now,
                    (seconds > 0).then_some(now + Duration::seconds(seconds)),
                )
                .await;
            self.metrics.inc(
                "memini_reinforce_results_total",
                &[("result", if result.is_ok() { "ok" } else { "error" })],
            );
        }
    }
    async fn route_confidence(&self, memory: &Memory) -> Result<()> {
        let now = (self.now)();
        if memory.tier.term() == Term::Short && self.corroborate_min_score > 0.0 {
            let candidates = match self
                .store
                .vector_search(
                    &memory.namespace,
                    &memory.embedding,
                    &Filter {
                        tiers: vec![Tier::Semantic, Tier::Procedural],
                        now: Some(now),
                        ..Filter::default()
                    },
                    1,
                )
                .await
            {
                Ok(candidates) => candidates,
                Err(error) => {
                    self.metrics
                        .inc("memini_corroborate_results_total", &[("result", "error")]);
                    return Err(error.into());
                }
            };
            let mut corroborated = false;
            if let Some(hit) = candidates
                .first()
                .filter(|v| v.score >= self.corroborate_min_score)
                && let Some(confidence) = hit.memory.confidence
            {
                let grown = memini_core::memory::grow_confidence(
                    hit.memory.effective_confidence(now).max(confidence),
                );
                self.store
                    .set_confidence(&hit.memory.namespace, &hit.memory.id, grown, now)
                    .await?;
                self.store
                    .reinforce(
                        &hit.memory.namespace,
                        std::slice::from_ref(&hit.memory.id),
                        now,
                        None,
                    )
                    .await?;
                corroborated = true;
            }
            self.metrics.inc(
                "memini_corroborate_results_total",
                &[("result", if corroborated { "corroborated" } else { "miss" })],
            );
        } else if memory.tier.term() == Term::Long && self.contradict_min_score > 0.0 {
            let candidates = match self
                .store
                .vector_search(
                    &memory.namespace,
                    &memory.embedding,
                    &Filter {
                        tiers: vec![Tier::Semantic, Tier::Procedural],
                        now: Some(now),
                        ..Filter::default()
                    },
                    3,
                )
                .await
            {
                Ok(candidates) => candidates,
                Err(error) => {
                    self.metrics
                        .inc("memini_contradict_results_total", &[("result", "error")]);
                    return Err(error.into());
                }
            };
            let mut contradicted = false;
            for hit in candidates
                .into_iter()
                .filter(|v| v.memory.id != memory.id && v.score >= self.contradict_min_score)
            {
                if hit.memory.confidence.is_some()
                    && now - hit.memory.created_at >= Duration::hours(24)
                    && memini_intelligence::contradict::classify(
                        &memory.content,
                        &hit.memory.content,
                        memini_intelligence::contradict::DEFAULT,
                    )
                    .class
                        == memini_intelligence::contradict::Class::Update
                {
                    let usage = 1.0 + (hit.memory.access_count as f64).ln_1p();
                    let target = hit
                        .memory
                        .effective_confidence(now)
                        .min(0.9 * CONFIDENCE_SEED_FRESH / usage);
                    self.store
                        .mark_contradicted(
                            &hit.memory.namespace,
                            &hit.memory.id,
                            &memory.id,
                            target,
                            now,
                        )
                        .await?;
                    contradicted = true;
                    break;
                }
            }
            self.metrics.inc(
                "memini_contradict_results_total",
                &[(
                    "result",
                    if contradicted {
                        "contradicted"
                    } else {
                        "no_signal"
                    },
                )],
            );
        }
        Ok(())
    }
    async fn enqueue_distill(&self, source: &Memory) -> bool {
        if !self.distill_on_write || self.distill_batch_tokens == 0 || self.distiller.is_none() {
            return false;
        }
        let Some(session) = source
            .metadata
            .get("session_id")
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
        else {
            return false;
        };
        let key = format!("{}\0{session}", source.namespace);
        let mut batches = self.distill_batches.lock().await;
        let buffer = batches.entry(key.clone()).or_insert_with(|| DistillBuffer {
            items: Vec::new(),
            tokens: 0,
            oldest: (self.now)(),
        });
        buffer.tokens += source.content.len().div_ceil(4);
        buffer.items.push(source.clone());
        let ready = (buffer.tokens >= self.distill_batch_tokens)
            .then(|| batches.remove(&key))
            .flatten();
        if batches.len() > 64 {
            let stale = batches
                .iter()
                .min_by_key(|(_, value)| value.oldest)
                .map(|(key, _)| key.clone());
            if let Some(stale) = stale
                && let Some(buffer) = batches.remove(&stale)
            {
                let service = self.clone();
                tokio::spawn(async move {
                    let _ = service.build_facts_batch(&buffer.items).await;
                });
            }
        }
        drop(batches);
        if let Some(buffer) = ready {
            let service = self.clone();
            tokio::spawn(async move {
                let _ = service.build_facts_batch(&buffer.items).await;
            });
        }
        true
    }

    pub async fn flush_distill_batches(&self, force: bool) -> usize {
        let now = (self.now)();
        let mut batches = self.distill_batches.lock().await;
        let keys = batches
            .iter()
            .filter(|(_, value)| force || now - value.oldest >= self.distill_batch_max_age)
            .map(|(key, _)| key.clone())
            .collect::<Vec<_>>();
        let buffers = keys
            .iter()
            .filter_map(|key| batches.remove(key))
            .collect::<Vec<_>>();
        drop(batches);
        let count = buffers.len();
        for buffer in buffers {
            let _ = self.build_facts_batch(&buffer.items).await;
        }
        count
    }

    async fn build_facts_batch(&self, sources: &[Memory]) -> Result<usize> {
        if sources.is_empty() {
            return Ok(0);
        }
        if sources.len() == 1 {
            return self.build_facts(&sources[0]).await;
        }
        let Some(distiller) = &self.distiller else {
            return Ok(0);
        };
        let facts = distiller
            .distill(&memini_llm::DistillInput {
                episodes: sources
                    .iter()
                    .map(|source| memini_llm::Episode {
                        content: source.content.clone(),
                        date: source.created_at.format("%Y-%m-%d").to_string(),
                    })
                    .collect(),
                now: (self.now)().format("%Y-%m-%d").to_string(),
            })
            .await?;
        self.write_facts(&sources[0], facts, Some(sources)).await
    }

    async fn build_facts(&self, source: &Memory) -> Result<usize> {
        let facts = if self.distill_on_write
            && let Some(distiller) = &self.distiller
        {
            distiller
                .distill(&memini_llm::DistillInput {
                    episodes: vec![memini_llm::Episode {
                        content: source.content.clone(),
                        date: source.created_at.format("%Y-%m-%d").to_string(),
                    }],
                    now: (self.now)().format("%Y-%m-%d").to_string(),
                })
                .await?
        } else if self.extract_on_write {
            extract::typed(&source.content)
                .into_iter()
                .map(|v| memini_llm::Fact {
                    content: v.content,
                    summary: String::new(),
                    category: match v.kind {
                        extract::Kind::Preference => "preference",
                        extract::Kind::HowTo => "procedure",
                        _ => "fact",
                    }
                    .into(),
                    confidence: Some(CONFIDENCE_SEED_FRESH),
                })
                .collect()
        } else {
            vec![]
        };
        self.write_facts(source, facts, None).await
    }

    async fn write_facts(
        &self,
        source: &Memory,
        facts: Vec<memini_llm::Fact>,
        sources: Option<&[Memory]>,
    ) -> Result<usize> {
        let mut written = 0;
        for fact in facts {
            let tier = if fact.category == "procedure" {
                Tier::Procedural
            } else {
                Tier::Semantic
            };
            let mut metadata = Map::new();
            metadata.insert("source_memory_id".into(), Value::String(source.id.clone()));
            if let Some(sources) = sources {
                metadata.insert(
                    "source_memory_ids".into(),
                    Value::Array(
                        sources
                            .iter()
                            .map(|source| Value::String(source.id.clone()))
                            .collect(),
                    ),
                );
            }
            metadata.insert("memory_type".into(), Value::String(fact.category.clone()));
            let result = Box::pin(self.remember(RememberInput {
                namespace: source.namespace.clone(),
                content: fact.content,
                tier: Some(tier),
                summary: fact.summary,
                metadata,
                confidence: fact.confidence.map(|v| v.clamp(0.1, 0.7)),
                level: Some(if self.distill_on_write {
                    Level::Deduced
                } else {
                    Level::Explicit
                }),
                ..RememberInput::default()
            }))
            .await?;
            if result.is_some() {
                written += 1
            }
        }
        Ok(written)
    }
    pub async fn promote(&self) -> Result<usize> {
        let started = std::time::Instant::now();
        let result = self.promote_inner().await;
        self.metrics.inc(
            "memini_promote_results_total",
            &[("result", if result.is_ok() { "ok" } else { "error" })],
        );
        if let Ok(facts) = &result {
            self.metrics
                .add("memini_promote_facts_total", &[], *facts as f64);
        }
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "promote")],
            started.elapsed().as_secs_f64(),
        );
        result
    }
    async fn promote_inner(&self) -> Result<usize> {
        let now = (self.now)();
        let mut total = 0;
        for namespace in self.store.list_namespaces().await? {
            let candidates = self
                .store
                .list(
                    &namespace,
                    &Filter {
                        tiers: vec![Tier::Working, Tier::Episodic],
                        now: Some(now),
                        ..Filter::default()
                    },
                    None,
                )
                .await?;
            for memory in candidates
                .into_iter()
                .filter(|v| v.access_count >= self.promote_min_access)
            {
                total += self.build_facts(&memory).await?;
                if memory.tier == Tier::Working {
                    self.store
                        .retier(
                            &namespace,
                            &memory.id,
                            Tier::Episodic,
                            Some(now + Tier::Episodic.default_ttl().unwrap()),
                        )
                        .await?
                }
            }
        }
        Ok(total)
    }
    pub async fn backfill_embeddings(&self) -> Result<usize> {
        let mut total = 0;
        for namespace in self.store.list_namespaces().await? {
            let pending = self
                .store
                .list(&namespace, &Filter::default(), None)
                .await?
                .into_iter()
                .filter(|memory| {
                    memory
                        .metadata
                        .get("pending_embed")
                        .is_some_and(|value| value == "true" || value == &Value::Bool(true))
                })
                .collect::<Vec<_>>();
            if pending.is_empty() {
                continue;
            }
            let texts = pending
                .iter()
                .map(|v| v.content.clone())
                .collect::<Vec<_>>();
            let vectors = self.embedder.embed(&texts).await?;
            for (mut memory, vector) in pending.into_iter().zip(vectors) {
                memory.embedding = vector;
                memory.metadata.remove("pending_embed");
                memory.updated_at = (self.now)();
                self.store.upsert(&memory).await?;
                total += 1;
            }
        }
        self.metrics.set("memini_embed_backfill_pending", &[], 0.0);
        Ok(total)
    }
    async fn expand_linked(&self, mut results: Vec<Scored>, limit: usize) -> Result<Vec<Scored>> {
        let mut seen: HashSet<_> = results.iter().map(|v| v.memory.id.clone()).collect();
        let sources = results.clone();
        for source in sources {
            for id in source.memory.linked_memory_ids {
                if seen.insert(id.clone())
                    && let Ok(memory) = self.store.get(&source.memory.namespace, &id).await
                    && memory.superseded_by.is_none()
                {
                    results.push(Scored {
                        memory,
                        score: source.score,
                    });
                }
            }
        }
        results.truncate(limit);
        Ok(results)
    }
}

const ANSWER_SYSTEM: &str = "You answer the question using ONLY the provided memories. Scan all memories twice. For conflicting values of the same fact, use the most recent memory; different people or contexts are not conflicts. Resolve relative dates using bracketed dates. Connect facts when implied, but never invent a name, date, number, or place. If the specific fact is absent, reply \"I don't know\". Reason inside <mem_thinking> tags, then put the brief final answer after </mem_thinking>.";
const ANSWER_EXPAND_SYSTEM: &str = "You rewrite a question into memory-search queries. Reply with 2 or 3 short search queries, one per line, with no numbering and no commentary. Make them lexically diverse: use synonyms and different angles (the entity, the value, the event), keep an exact keyword or name from the question in at least one, and when the question concerns something that changes over time add a query for later updates (e.g. append 'changed', 'default', 'now').";
const ANSWER_GATE_SYSTEM: &str = "You answer the question using ONLY the provided memories. Scan all memories twice. For conflicting values use the most recent memory. Never invent facts. Instead of guessing or replying I don't know, reply with exactly INSUFFICIENT when the specific fact is absent, several memories must be combined, memories conflict, or a current value may have been superseded. Only answer directly when one provided memory plainly settles the question.";
const ANSWER_LOOP_SYSTEM: &str = "You answer using ONLY memories and have memory-search tools. Search several differently phrased queries for aggregation, use keyword_search for exact terms, and recall_as_of for date questions. Search for later updates before trusting a dated fact. Stop when the memories answer the question. Never invent facts; reply I don't know if the searches do not establish the answer. Reply with the final answer only.";
#[derive(Clone, Debug, Default)]
pub struct AnswerInput {
    pub namespace: String,
    pub home: String,
    pub query: String,
    pub limit: usize,
    pub tiers: Vec<Tier>,
    pub levels: Vec<Level>,
    pub tags: Vec<String>,
    pub metadata: Map<String, Value>,
    pub scope: String,
    pub reasoning: String,
}
#[derive(Clone, Debug)]
pub struct AnswerResult {
    pub answer: String,
    pub sources: Vec<Scored>,
    pub read_set: Vec<ReadSetEntry>,
}
impl Service {
    pub async fn answer(&self, input: AnswerInput) -> Result<AnswerResult> {
        let started = std::time::Instant::now();
        let result = self.answer_inner(input).await;
        self.metrics.inc(
            "memini_answer_results_total",
            &[("result", if result.is_ok() { "ok" } else { "error" })],
        );
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "answer")],
            started.elapsed().as_secs_f64(),
        );
        result
    }
    async fn answer_inner(&self, input: AnswerInput) -> Result<AnswerResult> {
        let answerer = self
            .answerer
            .as_ref()
            .ok_or_else(|| ServiceError::InvalidInput("answer: no LLM configured".into()))?;
        if input.reasoning == "expand" {
            return self.answer_expand(input, answerer.as_ref()).await;
        }
        if let Some(iterations) = match input.reasoning.as_str() {
            "low" => Some(3),
            "medium" => Some(6),
            "high" => Some(10),
            "" | "minimal" => None,
            other => {
                return Err(ServiceError::InvalidInput(format!(
                    "unknown reasoning level {other:?} (want minimal|expand|low|medium|high)"
                )));
            }
        } {
            return self
                .answer_agentic(input, answerer.as_ref(), iterations)
                .await;
        }
        let (sources, _, read_set) = self
            .recall(RecallInput {
                namespace: input.namespace,
                home: input.home,
                query: input.query.clone(),
                limit: input.limit,
                tiers: input.tiers,
                levels: input.levels,
                tags: input.tags,
                metadata: input.metadata,
                scope: input.scope,
                include_linked: true,
                ..RecallInput::default()
            })
            .await?;
        let prompt = self.answer_prompt(&sources, &input.query);
        let raw = answerer.complete(ANSWER_SYSTEM, &prompt).await?;
        let answer = raw
            .rsplit_once("</mem_thinking>")
            .map_or(raw.trim(), |v| v.1.trim())
            .to_owned();
        Ok(AnswerResult {
            answer,
            sources,
            read_set,
        })
    }

    fn answer_prompt(&self, sources: &[Scored], query: &str) -> String {
        format!(
            "Today's date: {}\nMemories:\n{}\nQuestion: {}\nAnswer:",
            (self.now)().format("%Y-%m-%d"),
            format_answer_context(sources),
            query
        )
    }

    async fn answer_expand(
        &self,
        input: AnswerInput,
        answerer: &dyn LlmClient,
    ) -> Result<AnswerResult> {
        let raw = answerer
            .complete(ANSWER_EXPAND_SYSTEM, &format!("Question: {}", input.query))
            .await?;
        let mut queries = vec![input.query.clone()];
        for line in raw.lines() {
            let query = line
                .trim()
                .trim_start_matches(|c: char| {
                    c == '-' || c == '*' || c == '.' || c.is_ascii_digit()
                })
                .trim();
            if !query.is_empty() && queries.len() < 4 {
                queries.push(query.to_owned());
            }
        }
        let read_set = self
            .resolve_read_set_info(&input.namespace, &input.home)
            .await?;
        let mut seen = HashSet::new();
        let mut sources = Vec::new();
        for query in queries {
            let (found, _, _) = self.recall(answer_recall(&input, query)).await?;
            collect_sources(&mut sources, &mut seen, found);
        }
        let answer = answerer
            .complete(ANSWER_SYSTEM, &self.answer_prompt(&sources, &input.query))
            .await?;
        Ok(AnswerResult {
            answer: strip_mem_thinking(&answer),
            sources,
            read_set,
        })
    }

    async fn answer_agentic(
        &self,
        input: AnswerInput,
        answerer: &dyn LlmClient,
        iterations: usize,
    ) -> Result<AnswerResult> {
        let (prefetch, _, read_set) = self
            .recall(answer_recall(&input, input.query.clone()))
            .await?;
        let mut sources = Vec::new();
        let mut seen = HashSet::new();
        collect_sources(&mut sources, &mut seen, prefetch.clone());
        let direct = answerer
            .complete(
                ANSWER_GATE_SYSTEM,
                &self.answer_prompt(&prefetch, &input.query),
            )
            .await?;
        if !gate_insufficient(&direct) {
            return Ok(AnswerResult {
                answer: strip_mem_thinking(&direct),
                sources,
                read_set,
            });
        }
        let mut turns = vec![ChatTurn {
            role: Role::User,
            text: format!(
                "{}\nA first read found these memories insufficient. Search for what is missing, then answer.",
                self.answer_prompt(&prefetch, &input.query)
            ),
            calls: vec![],
            call_id: String::new(),
            name: String::new(),
        }];
        let tools = answer_tools();
        for _ in 0..iterations {
            let response = answerer
                .chat_tools(ANSWER_LOOP_SYSTEM, &turns, &tools, ToolChoice::Auto)
                .await?;
            if response.calls.is_empty() {
                return Ok(AnswerResult {
                    answer: strip_mem_thinking(&response.text),
                    sources,
                    read_set,
                });
            }
            turns.push(ChatTurn {
                role: Role::Assistant,
                text: response.text,
                calls: response.calls.clone(),
                call_id: String::new(),
                name: String::new(),
            });
            for call in response.calls {
                let text = match self.exec_answer_tool(&input, &call).await {
                    Ok(found) => {
                        collect_sources(&mut sources, &mut seen, found.clone());
                        if found.is_empty() {
                            "no memories found".into()
                        } else {
                            format_answer_context(&found)
                        }
                    }
                    Err(error) => format!("error: {error}"),
                };
                turns.push(ChatTurn {
                    role: Role::Tool,
                    text,
                    calls: vec![],
                    call_id: call.id,
                    name: call.name,
                });
            }
        }
        turns.push(ChatTurn {
            role: Role::User,
            text: "Answer now from the memories you have found.".into(),
            calls: vec![],
            call_id: String::new(),
            name: String::new(),
        });
        let response = answerer
            .chat_tools(ANSWER_LOOP_SYSTEM, &turns, &[], ToolChoice::None)
            .await?;
        Ok(AnswerResult {
            answer: strip_mem_thinking(&response.text),
            sources,
            read_set,
        })
    }

    async fn exec_answer_tool(&self, input: &AnswerInput, call: &ToolCall) -> Result<Vec<Scored>> {
        let query = call
            .arguments
            .get("query")
            .and_then(Value::as_str)
            .unwrap_or("")
            .trim();
        if query.is_empty() {
            return Err(ServiceError::InvalidInput("query is required".into()));
        }
        let mut recall = answer_recall(input, query.to_owned());
        match call.name.as_str() {
            "search_memory" => match call.arguments.get("tier").and_then(Value::as_str) {
                Some("episodic") => recall.tiers = vec![Tier::Working, Tier::Episodic],
                Some("durable") => recall.tiers = vec![Tier::Semantic, Tier::Procedural],
                _ => {}
            },
            "recall_as_of" => {
                let date = call
                    .arguments
                    .get("date")
                    .and_then(Value::as_str)
                    .unwrap_or("");
                let parsed = chrono::NaiveDate::parse_from_str(date, "%Y-%m-%d")
                    .map_err(|_| ServiceError::InvalidInput("date must be YYYY-MM-DD".into()))?;
                recall.as_of = parsed.and_hms_opt(23, 59, 59).map(|v| v.and_utc());
            }
            "keyword_search" => {
                let filter = Filter {
                    tiers: input.tiers.clone(),
                    levels: input.levels.clone(),
                    tags: input.tags.clone(),
                    metadata: input.metadata.clone(),
                    now: Some((self.now)()),
                    ..Filter::default()
                };
                return self
                    .store
                    .keyword_search(&input.namespace, query, &filter, input.limit.max(5))
                    .await
                    .map_err(Into::into);
            }
            other => return Err(ServiceError::InvalidInput(format!("unknown tool {other}"))),
        }
        self.recall(recall).await.map(|v| v.0)
    }
}
fn answer_recall(input: &AnswerInput, query: String) -> RecallInput {
    RecallInput {
        namespace: input.namespace.clone(),
        home: input.home.clone(),
        query,
        limit: input.limit,
        tiers: input.tiers.clone(),
        levels: input.levels.clone(),
        tags: input.tags.clone(),
        metadata: input.metadata.clone(),
        scope: input.scope.clone(),
        include_linked: true,
        ..RecallInput::default()
    }
}
fn collect_sources(target: &mut Vec<Scored>, seen: &mut HashSet<String>, found: Vec<Scored>) {
    target.extend(
        found
            .into_iter()
            .filter(|item| seen.insert(item.memory.id.clone())),
    );
}
fn strip_mem_thinking(value: &str) -> String {
    let mut value = value.trim().to_owned();
    while let Some(start) = value.find("<mem_thinking>") {
        if let Some(relative_end) = value[start..].find("</mem_thinking>") {
            let end = start + relative_end + "</mem_thinking>".len();
            value.replace_range(start..end, "");
        } else {
            value.truncate(start);
        }
    }
    value.trim().to_owned()
}
fn gate_insufficient(value: &str) -> bool {
    let value = value.to_uppercase();
    value.contains("INSUFFICIENT") || value.contains("DON'T KNOW") || value.contains("DO NOT KNOW")
}
fn format_answer_context(sources: &[Scored]) -> String {
    let mut ordered = sources.to_vec();
    ordered.sort_by_key(|v| v.memory.valid_from.unwrap_or(v.memory.created_at));
    ordered
        .iter()
        .enumerate()
        .map(|(index, item)| {
            format!(
                "{}. [{}] {}",
                index + 1,
                item.memory
                    .valid_from
                    .unwrap_or(item.memory.created_at)
                    .format("%Y-%m-%d"),
                item.memory.content
            )
        })
        .collect::<Vec<_>>()
        .join("\n")
}
fn answer_tools() -> Vec<Tool> {
    vec![
        Tool {
            name: "search_memory".into(),
            description: "Hybrid semantic and keyword search. Phrase each call differently.".into(),
            schema: json!({"type":"object","properties":{"query":{"type":"string"},"tier":{"type":"string","enum":["all","episodic","durable"]}},"required":["query"]}),
        },
        Tool {
            name: "keyword_search".into(),
            description: "Exact lexical search for names, codes, counting, and quoted terms."
                .into(),
            schema: json!({"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}),
        },
        Tool {
            name: "recall_as_of".into(),
            description: "Search memories valid on a date.".into(),
            schema: json!({"type":"object","properties":{"query":{"type":"string"},"date":{"type":"string"}},"required":["query","date"]}),
        },
    ]
}
fn seed_importance(tier: Tier) -> f64 {
    match tier {
        Tier::Semantic | Tier::Procedural => 0.6,
        Tier::Episodic => 0.3,
        Tier::Working => 0.1,
    }
}
static TURN_SCAFFOLD: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?im)^[ \t]*(user|assistant|human|ai)[ \t]*:[ \t]*").unwrap());
fn signal_chars(value: &str) -> usize {
    TURN_SCAFFOLD.replace_all(value, "").trim().chars().count()
}
fn word_set_score(value: &str) -> usize {
    value
        .split(|c: char| !c.is_alphanumeric())
        .filter(|v| v.len() > 1)
        .map(str::to_lowercase)
        .collect::<HashSet<_>>()
        .len()
}
fn should_expand(value: &str) -> bool {
    let value = value.trim();
    value.split_whitespace().count() >= 3
        && !value.contains(['\"', '\''])
        && !Regex::new(r"^[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}$")
            .unwrap()
            .is_match(value)
        && !(value == value.to_uppercase()
            && value.len() < 40
            && (value.contains('_') || value.contains('-')))
}
fn reserve_durable(results: &mut Vec<Scored>, limit: usize, reserve: usize) {
    if reserve == 0 || limit == 0 || results.len() <= limit {
        return;
    }
    let limit = limit.min(results.len());
    let reserve = reserve.min(limit);
    let mut selected = (0..limit).collect::<HashSet<_>>();
    let mut durable = results[..limit]
        .iter()
        .filter(|value| value.memory.tier.term() == Term::Long)
        .count();
    if durable >= reserve {
        return;
    }
    for index in limit..results.len() {
        if durable >= reserve || results[index].memory.tier.term() != Term::Long {
            continue;
        }
        let Some(evicted) = (0..limit).rev().find(|candidate| {
            selected.contains(candidate) && results[*candidate].memory.tier.term() == Term::Short
        }) else {
            break;
        };
        let bar = (0.5 * results[evicted].score).max(0.4 * results[0].score);
        if results[index].score < bar {
            break;
        }
        selected.remove(&evicted);
        selected.insert(index);
        durable += 1;
    }
    let mut window = Vec::with_capacity(limit);
    let mut promoted = Vec::with_capacity(reserve);
    let mut rest = Vec::with_capacity(results.len() - limit);
    for (index, value) in results.drain(..).enumerate() {
        match (selected.contains(&index), index < limit) {
            (true, true) => window.push(value),
            (true, false) => promoted.push(value),
            _ => rest.push(value),
        }
    }
    let head = usize::from(!window.is_empty());
    let mut output = Vec::with_capacity(window.len() + promoted.len() + rest.len());
    output.extend(window.drain(..head));
    output.extend(promoted);
    output.extend(window);
    output.extend(rest);
    *results = output;
}
fn resolve_visibility(namespace: &str, home: &str, visibility: &str, tier: Tier) -> Result<String> {
    let visibility = visibility.trim();
    if visibility.is_empty() || visibility == "project" || tier.term() == Term::Short {
        return Ok(namespace.into());
    }
    if visibility == "personal" {
        return if home.trim().is_empty() {
            Err(ServiceError::InvalidInput(
                "remember: visibility personal requires a home namespace".into(),
            ))
        } else {
            Ok(home.trim().into())
        };
    }
    let ancestors = readset::ancestors(namespace);
    let matches: Vec<_> = ancestors
        .iter()
        .filter(|v| v.as_str() == visibility || v.rsplit('/').next() == Some(visibility))
        .collect();
    if matches.len() == 1 {
        Ok(matches[0].clone())
    } else {
        Err(ServiceError::InvalidInput(format!(
            "remember: visibility {visibility:?} not in scope"
        )))
    }
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Stats {
    pub namespace: String,
    pub total: usize,
    pub by_tier: HashMap<String, usize>,
    pub by_memory_type: HashMap<String, usize>,
    pub expired: usize,
    pub superseded: usize,
    pub low_confidence_durable: usize,
    pub total_accesses: i64,
    #[serde(rename = "avg_importance")]
    pub average_importance: f64,
    pub last_write_at: Option<DateTime<Utc>>,
}
impl Service {
    pub async fn stats(&self, namespace: &str) -> Result<Stats> {
        let all = self
            .store
            .list(
                namespace,
                &Filter {
                    include_expired: true,
                    include_superseded: true,
                    ..Filter::default()
                },
                None,
            )
            .await?;
        let now = (self.now)();
        let mut out = Stats {
            namespace: namespace.into(),
            ..Stats::default()
        };
        let mut importance = 0.0;
        for memory in all {
            if memory.superseded_by.is_some() {
                out.superseded += 1
            } else if memory.expired(now) {
                out.expired += 1
            } else {
                out.total += 1;
                *out.by_tier
                    .entry(format!("{:?}", memory.tier).to_lowercase())
                    .or_default() += 1;
                if let Some(kind) = memory.metadata.get("memory_type").and_then(Value::as_str) {
                    *out.by_memory_type.entry(kind.into()).or_default() += 1
                }
                out.total_accesses += memory.access_count;
                importance += memory.importance;
                if memory.tier.term() == Term::Long
                    && memory.effective_confidence(now)
                        < memini_core::memory::CONFIDENCE_DEMOTE_FLOOR
                {
                    out.low_confidence_durable += 1;
                }
            }
            out.last_write_at = Some(
                out.last_write_at
                    .map_or(memory.created_at, |v| v.max(memory.created_at)),
            );
        }
        if out.total > 0 {
            out.average_importance = importance / out.total as f64
        }
        Ok(out)
    }

    pub async fn stats_all(&self) -> Result<Stats> {
        let mut out = Stats::default();
        let mut weighted = 0.0;
        for namespace in self.store.list_namespaces().await? {
            let stats = self.stats(&namespace).await?;
            out.total += stats.total;
            out.expired += stats.expired;
            out.superseded += stats.superseded;
            out.low_confidence_durable += stats.low_confidence_durable;
            out.total_accesses += stats.total_accesses;
            weighted += stats.average_importance * stats.total as f64;
            for (key, value) in stats.by_tier {
                *out.by_tier.entry(key).or_default() += value
            }
            for (key, value) in stats.by_memory_type {
                *out.by_memory_type.entry(key).or_default() += value
            }
            out.last_write_at = match (out.last_write_at, stats.last_write_at) {
                (Some(a), Some(b)) => Some(a.max(b)),
                (None, b) => b,
                (a, None) => a,
            };
        }
        if out.total > 0 {
            out.average_importance = weighted / out.total as f64
        }
        for (tier, count) in &out.by_tier {
            self.metrics
                .set("memini_memories_active", &[("tier", tier)], *count as f64);
        }
        Ok(out)
    }

    pub async fn forget_by_tag(&self, namespace: &str, tag: &str) -> Result<u64> {
        if tag.trim().is_empty() {
            return Err(ServiceError::InvalidInput(
                "forget by tag: tag is required".into(),
            ));
        }
        Ok(memini_maintenance::forget_by_tag(self.store.as_ref(), namespace, tag).await?)
    }
    pub async fn fsck(&self, short_term_cap: usize) -> Result<memini_maintenance::Report> {
        let started = std::time::Instant::now();
        let result = memini_maintenance::fsck(self.store.as_ref(), short_term_cap, (self.now)())
            .await
            .map_err(Into::into);
        self.metrics.inc(
            "memini_fsck_results_total",
            &[("result", if result.is_ok() { "ok" } else { "error" })],
        );
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "fsck")],
            started.elapsed().as_secs_f64(),
        );
        if result.is_ok() {
            let _ = self.stats_all().await;
        }
        result
    }
    pub async fn dedup(
        &self,
        mut options: memini_maintenance::DedupOptions,
    ) -> Result<memini_maintenance::DedupReport> {
        let started = std::time::Instant::now();
        if self.dedup_llm_merge && options.merger.is_none() {
            options.merger = self.consolidator.clone();
        }
        let result =
            memini_maintenance::dedup(self.store.as_ref(), self.embedder.as_ref(), options)
                .await
                .map_err(|e| ServiceError::Backend(e.to_string()));
        if let Ok(report) = &result {
            self.metrics.add(
                "memini_dedup_tombstoned_total",
                &[],
                report.tombstoned as f64,
            );
        }
        self.metrics.observe(
            "memini_op_duration_seconds",
            &[("op", "dedup")],
            started.elapsed().as_secs_f64(),
        );
        result
    }
}

pub const DEFAULT_PER_SECTION: usize = 5;
#[derive(Clone, Debug, Default)]
pub struct BriefingOptions {
    pub pinned: Option<usize>,
    pub facts: Option<usize>,
    pub procedures: Option<usize>,
    pub recent: Option<usize>,
    pub namespaces: Vec<String>,
    pub subtree: bool,
    pub home: String,
    pub scope: String,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct ChildSummary {
    pub namespace: String,
    pub total: usize,
    pub pinned: Vec<Memory>,
    pub recent: Vec<Memory>,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct Briefing {
    pub namespace: String,
    pub scope_header: String,
    pub facts: Vec<Memory>,
    pub procedures: Vec<Memory>,
    pub recent: Vec<Memory>,
    pub pinned: Vec<Memory>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub children: Vec<ChildSummary>,
    #[serde(skip_serializing_if = "is_zero")]
    pub children_truncated: usize,
}
const fn is_zero(value: &usize) -> bool {
    *value == 0
}
impl Service {
    pub async fn briefing(
        &self,
        namespace: &str,
        options: BriefingOptions,
    ) -> Result<(Briefing, Vec<ReadSetEntry>)> {
        let input = RecallInput {
            namespace: namespace.into(),
            namespaces: options.namespaces.clone(),
            subtree: options.subtree,
            home: options.home.clone(),
            scope: options.scope.clone(),
            ..RecallInput::default()
        };
        let entries = self.resolve_read_set(&input).await?;
        let now = (self.now)();
        let mut out = Briefing {
            namespace: namespace.into(),
            scope_header: format!("Scope: {namespace}"),
            ..Briefing::default()
        };
        let mut durable_counts = HashMap::new();
        for entry in &entries {
            let memories = self
                .store
                .list(
                    &entry.namespace,
                    &Filter {
                        tiers: entry.tiers.clone().unwrap_or_default(),
                        now: Some(now),
                        ..Filter::default()
                    },
                    None,
                )
                .await?;
            for memory in memories {
                if memory.tier.term() == Term::Long {
                    *durable_counts
                        .entry(entry.namespace.clone())
                        .or_insert(0usize) += 1
                }
                match memory.tier {
                    Tier::Semantic => out.facts.push(memory.clone()),
                    Tier::Procedural => out.procedures.push(memory.clone()),
                    Tier::Episodic if entry.tiers.is_none() => out.recent.push(memory.clone()),
                    _ => {}
                }
                if memory
                    .tags
                    .iter()
                    .any(|v| v == memini_maintenance::PINNED_TAG)
                {
                    out.pinned.push(memory)
                }
            }
        }
        for entry in entries
            .iter()
            .filter(|v| matches!(v.origin, Origin::Ancestor | Origin::Home))
        {
            if let Some(count) = durable_counts.get(&entry.namespace).filter(|v| **v > 0) {
                out.scope_header
                    .push_str(&format!(" ← {}({count})", entry.namespace))
            }
        }
        let links = entries
            .iter()
            .filter(|v| {
                v.origin == Origin::Link && durable_counts.get(&v.namespace).is_some_and(|n| *n > 0)
            })
            .count();
        if links > 0 {
            out.scope_header.push_str(&format!(
                ", +{links} link{}",
                if links == 1 { "" } else { "s" }
            ))
        }
        out.facts
            .sort_by(|a, b| b.durable_score(now).total_cmp(&a.durable_score(now)));
        out.procedures
            .sort_by(|a, b| b.durable_score(now).total_cmp(&a.durable_score(now)));
        out.recent
            .sort_by_key(|memory| std::cmp::Reverse(memory.created_at));
        out.pinned.sort_by(|a, b| {
            b.durable_score(now)
                .total_cmp(&a.durable_score(now))
                .then_with(|| b.created_at.cmp(&a.created_at))
        });
        out.facts
            .truncate(options.facts.unwrap_or(DEFAULT_PER_SECTION));
        out.procedures
            .truncate(options.procedures.unwrap_or(DEFAULT_PER_SECTION));
        out.recent
            .truncate(options.recent.unwrap_or(DEFAULT_PER_SECTION));
        out.pinned
            .truncate(options.pinned.unwrap_or(DEFAULT_PER_SECTION));
        if options.namespaces.is_empty()
            && !options.subtree
            && matches!(options.scope.as_str(), "" | "full")
        {
            let prefix = format!("{namespace}/");
            let mut child_names = HashSet::new();
            for candidate in self.store.list_namespaces().await? {
                if let Some(rest) = candidate.strip_prefix(&prefix)
                    && let Some(segment) = rest.split('/').next()
                    && !segment.is_empty()
                {
                    child_names.insert(format!("{prefix}{segment}"));
                }
            }
            for child in child_names {
                let child_prefix = format!("{child}/");
                let mut memories = Vec::new();
                for candidate in self.store.list_namespaces().await? {
                    if candidate == child || candidate.starts_with(&child_prefix) {
                        memories.extend(
                            self.store
                                .list(
                                    &candidate,
                                    &Filter {
                                        now: Some(now),
                                        ..Filter::default()
                                    },
                                    None,
                                )
                                .await?,
                        );
                    }
                }
                memories.sort_by_key(|memory| std::cmp::Reverse(memory.updated_at));
                let mut pinned = memories
                    .iter()
                    .filter(|memory| {
                        memory
                            .tags
                            .iter()
                            .any(|tag| tag == memini_maintenance::PINNED_TAG)
                    })
                    .take(3)
                    .cloned()
                    .collect::<Vec<_>>();
                pinned.sort_by_key(|memory| std::cmp::Reverse(memory.updated_at));
                let recent = memories
                    .iter()
                    .filter(|memory| memory.tier.term() == Term::Long)
                    .take(3)
                    .cloned()
                    .collect();
                out.children.push(ChildSummary {
                    namespace: child,
                    total: memories.len(),
                    pinned,
                    recent,
                });
            }
            out.children.sort_by(|a, b| {
                let a_time = a.recent.first().map(|memory| memory.updated_at);
                let b_time = b.recent.first().map(|memory| memory.updated_at);
                b_time
                    .cmp(&a_time)
                    .then_with(|| a.namespace.cmp(&b.namespace))
            });
            if out.children.len() > 10 {
                out.children_truncated = out.children.len() - 10;
                out.children.truncate(10);
            }
        }
        Ok((out, entries))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use async_trait::async_trait;
    use memini_embed::{EmbedError, Result as EmbedResult};
    use std::sync::atomic::{AtomicUsize, Ordering};
    struct Fake;
    #[async_trait]
    impl Embedder for Fake {
        async fn embed(&self, texts: &[String]) -> EmbedResult<Vec<Vec<f32>>> {
            Ok(texts.iter().map(|t| vec![t.len() as f32, 1.0]).collect())
        }
        fn dimensions(&self) -> usize {
            2
        }
    }
    struct Constant;
    #[async_trait]
    impl Embedder for Constant {
        async fn embed(&self, texts: &[String]) -> EmbedResult<Vec<Vec<f32>>> {
            Ok(texts.iter().map(|_| vec![1.0, 1.0]).collect())
        }
        fn dimensions(&self) -> usize {
            2
        }
    }
    struct Consolidate;
    #[async_trait]
    impl memini_llm::Client for Consolidate {
        async fn complete(&self, _: &str, _: &str) -> memini_llm::Result<String> {
            Ok(r#"{"action":"update","target":"old","content":"Email now uses SES.","summary":"Email provider","reason":"provider changed"}"#.into())
        }
        async fn chat_tools(
            &self,
            _: &str,
            _: &[memini_llm::ChatTurn],
            _: &[memini_llm::Tool],
            _: memini_llm::ToolChoice,
        ) -> memini_llm::Result<memini_llm::ChatResult> {
            Err(memini_llm::LlmError::Invalid("unused".into()))
        }
    }
    struct DistillBatch(AtomicUsize);
    #[async_trait]
    impl memini_llm::Client for DistillBatch {
        async fn complete(&self, _: &str, user: &str) -> memini_llm::Result<String> {
            let value: Value = serde_json::from_str(user).unwrap();
            self.0.store(
                value["episodes"].as_array().map_or(0, Vec::len),
                Ordering::SeqCst,
            );
            Ok(r#"{"facts":[{"content":"The project uses Rust.","category":"fact","confidence":0.6}]}"#.into())
        }
        async fn chat_tools(
            &self,
            _: &str,
            _: &[memini_llm::ChatTurn],
            _: &[memini_llm::Tool],
            _: memini_llm::ToolChoice,
        ) -> memini_llm::Result<memini_llm::ChatResult> {
            Err(memini_llm::LlmError::Invalid("unused".into()))
        }
    }
    #[allow(dead_code)]
    struct Broken;
    #[async_trait]
    impl Embedder for Broken {
        async fn embed(&self, _: &[String]) -> EmbedResult<Vec<Vec<f32>>> {
            Err(EmbedError::Disabled)
        }
        fn dimensions(&self) -> usize {
            2
        }
    }

    #[tokio::test]
    async fn remember_recall_and_exact_dedup() {
        let dir = tempfile::tempdir().unwrap();
        let store = Arc::new(memini_sqlite::SqliteStore::open(dir.path().join("db"), 2).unwrap());
        let service = Service::new(store, Arc::new(Fake));
        let first = service
            .remember(RememberInput {
                namespace: "acme/app".into(),
                content: "We decided to use Postgres instead of SQLite for concurrent writes."
                    .into(),
                ..RememberInput::default()
            })
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.tier, Tier::Semantic);
        let repeated = service
            .remember(RememberInput {
                namespace: "acme/app".into(),
                content: "  we decided to use postgres instead of sqlite for concurrent writes. "
                    .into(),
                tier: Some(Tier::Semantic),
                ..RememberInput::default()
            })
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.id, repeated.id);
        let listed = service
            .list(ListInput {
                namespace: "acme/app".into(),
                ..ListInput::default()
            })
            .await
            .unwrap();
        assert_eq!(listed.len(), 1);
        let (recalled, degraded, read_set) = service
            .recall(RecallInput {
                namespace: "acme/app".into(),
                query: "Postgres".into(),
                ..RecallInput::default()
            })
            .await
            .unwrap();
        assert!(degraded.is_none());
        assert_eq!(recalled[0].memory.id, first.id);
        assert_eq!(read_set[0].namespace, "acme/app");
    }

    #[tokio::test]
    async fn distill_batch_flushes_session_as_one_completion() {
        let dir = tempfile::tempdir().unwrap();
        let store = Arc::new(memini_sqlite::SqliteStore::open(dir.path().join("db"), 2).unwrap());
        let distiller = Arc::new(DistillBatch(AtomicUsize::new(0)));
        let service = Service::new(store.clone(), Arc::new(Fake))
            .with_distiller(distiller.clone())
            .with_distill_on_write(true)
            .with_distill_batch(100, Duration::minutes(10));
        for content in ["User: choose Rust", "Assistant: Rust was selected"] {
            let mut metadata = Map::new();
            metadata.insert("session_id".into(), Value::String("session-1".into()));
            service
                .remember(RememberInput {
                    namespace: "project".into(),
                    content: content.into(),
                    tier: Some(Tier::Episodic),
                    metadata,
                    ..RememberInput::default()
                })
                .await
                .unwrap();
        }
        assert_eq!(distiller.0.load(Ordering::SeqCst), 0);
        assert_eq!(service.flush_distill_batches(true).await, 1);
        assert_eq!(distiller.0.load(Ordering::SeqCst), 2);
        let durable = memini_store::Store::list(
            store.as_ref(),
            "project",
            &Filter {
                tiers: vec![Tier::Semantic],
                ..Filter::default()
            },
            None,
        )
        .await
        .unwrap();
        assert_eq!(durable.len(), 1);
        assert_eq!(
            durable[0].metadata["source_memory_ids"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
    }
    #[tokio::test]
    async fn consolidation_updates_existing_memory() {
        let dir = tempfile::tempdir().unwrap();
        let store = Arc::new(memini_sqlite::SqliteStore::open(dir.path().join("db"), 2).unwrap());
        let base = Service::new(store.clone(), Arc::new(Constant));
        let old = base
            .remember(RememberInput {
                namespace: "ns".into(),
                id: "old".into(),
                tier: Some(Tier::Semantic),
                content: "Email is sent through Postmark.".into(),
                ..RememberInput::default()
            })
            .await
            .unwrap()
            .unwrap();
        assert_eq!(old.id, "old");
        let service = base.with_consolidator(Arc::new(Consolidate), 0.0);
        let updated = service
            .remember(RememberInput {
                namespace: "ns".into(),
                tier: Some(Tier::Semantic),
                content: "Email is now delivered by SES.".into(),
                ..RememberInput::default()
            })
            .await
            .unwrap()
            .unwrap();
        assert_eq!(updated.id, "old");
        assert_eq!(updated.content, "Email now uses SES.");
        assert_eq!(
            Store::list(store.as_ref(), "ns", &Filter::default(), None)
                .await
                .unwrap()
                .len(),
            1
        );
    }

    #[tokio::test]
    async fn fuzzy_write_dedup_coalesces_and_supersedes() {
        let dir = tempfile::tempdir().unwrap();
        let store = Arc::new(memini_sqlite::SqliteStore::open(dir.path().join("db"), 2).unwrap());
        let coalesce =
            Service::new(store.clone(), Arc::new(Constant)).with_write_dedup(0.1, "coalesce");
        let old = coalesce
            .remember(RememberInput {
                namespace: "n".into(),
                content: "Cache TTL is ten minutes".into(),
                tier: Some(Tier::Semantic),
                ..RememberInput::default()
            })
            .await
            .unwrap()
            .unwrap();
        let same = coalesce
            .remember(RememberInput {
                namespace: "n".into(),
                content: "TTL ten minutes".into(),
                tier: Some(Tier::Semantic),
                ..RememberInput::default()
            })
            .await
            .unwrap()
            .unwrap();
        assert_eq!(same.id, old.id);
        assert_eq!(same.access_count, 1);

        let supersede =
            Service::new(store.clone(), Arc::new(Constant)).with_write_dedup(0.1, "supersede");
        let outcome = supersede
            .remember_with_outcome(RememberInput {
                namespace: "n".into(),
                content: "Cache TTL is thirty minutes".into(),
                tier: Some(Tier::Semantic),
                ..RememberInput::default()
            })
            .await
            .unwrap();
        assert!(outcome.auto_superseded);
        let new = outcome.memory.unwrap();
        assert_eq!(
            Store::get(store.as_ref(), "n", &old.id)
                .await
                .unwrap()
                .superseded_by
                .as_deref(),
            Some(new.id.as_str())
        );
        let hint = Service::new(store, Arc::new(Constant)).with_write_dedup(0.1, "hint");
        let outcome = hint
            .remember_with_outcome(RememberInput {
                namespace: "n".into(),
                content: "Cache TTL remains thirty minutes".into(),
                tier: Some(Tier::Semantic),
                ..RememberInput::default()
            })
            .await
            .unwrap();
        assert!(outcome.merge_hint.is_some());
    }
}
