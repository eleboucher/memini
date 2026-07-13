use async_trait::async_trait;
use chrono::{DateTime, NaiveDateTime, Utc};
use futures_util::{StreamExt, stream};
use memini_core::memory::{Memory, Tier};
use memini_embed::{EmbedError, Embedder};
use memini_service::{AnswerInput, RecallInput, RememberInput, Service};
use memini_store::{Filter, Store};
use serde::{Deserialize, Serialize};
use std::{
    collections::{HashMap, HashSet},
    sync::Arc,
    time::Instant,
};

const SAMPLE: &str = include_str!("../../../bench/data/sample.json");

#[derive(Clone, Debug, Deserialize)]
pub struct Item {
    pub id: String,
    pub content: String,
    #[serde(default)]
    pub group: String,
    #[serde(default)]
    pub session: String,
    #[serde(default)]
    pub source: String,
    #[serde(skip)]
    pub time: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct Question {
    pub query: String,
    pub gold: Vec<String>,
    #[serde(default)]
    pub group: String,
    #[serde(default)]
    pub answer: String,
    #[serde(default)]
    pub category: String,
    #[serde(default)]
    pub gold_all: Vec<String>,
    #[serde(default)]
    pub provenance: String,
    #[serde(skip)]
    pub now: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct Dataset {
    pub name: String,
    pub items: Vec<Item>,
    pub questions: Vec<Question>,
}

impl Dataset {
    pub fn sample() -> anyhow::Result<Self> {
        Ok(serde_json::from_str(SAMPLE)?)
    }

    pub fn load(path: impl AsRef<std::path::Path>) -> anyhow::Result<Self> {
        let value: Self = serde_json::from_slice(&std::fs::read(path.as_ref())?)?;
        anyhow::ensure!(
            !value.items.is_empty() && !value.questions.is_empty(),
            "dataset {:?} has no items or questions",
            path.as_ref()
        );
        Ok(value)
    }

    /// Evenly sample questions across the corpus and discard unrelated items.
    pub fn limit_questions(&mut self, limit: usize) {
        if limit == 0 || limit >= self.questions.len() {
            return;
        }
        let step = self.questions.len() as f64 / limit as f64;
        self.questions = (0..limit)
            .map(|index| self.questions[(index as f64 * step) as usize].clone())
            .collect();
        let groups = self
            .questions
            .iter()
            .map(|question| question.group.as_str())
            .collect::<HashSet<_>>();
        self.items
            .retain(|item| groups.contains(item.group.as_str()));
    }

    pub fn load_longmemeval(
        path: impl AsRef<std::path::Path>,
        mode: DocumentMode,
    ) -> anyhow::Result<Self> {
        let rows: Vec<LongMemEvalRow> = serde_json::from_slice(&std::fs::read(path)?)?;
        let mut dataset = Self {
            name: "longmemeval".into(),
            items: Vec::new(),
            questions: Vec::new(),
        };
        for (index, row) in rows.into_iter().enumerate() {
            let group = if row.question_id.is_empty() {
                format!("q-{index}")
            } else {
                row.question_id
            };
            for (session_index, turns) in row.haystack_sessions.iter().enumerate() {
                let id = row
                    .haystack_session_ids
                    .get(session_index)
                    .filter(|id| !id.is_empty())
                    .cloned()
                    .unwrap_or_else(|| format!("session-{session_index}"));
                dataset.items.push(Item {
                    id: format!("{group}/{id}"),
                    content: session_document(turns, &row.haystack_dates, session_index, mode),
                    group: group.clone(),
                    session: String::new(),
                    source: String::new(),
                    time: parse_lme_date(row.haystack_dates.get(session_index)),
                });
            }
            let category = if group.ends_with("_abs") {
                format!("{}_abs", row.question_type)
            } else {
                row.question_type
            };
            dataset.questions.push(Question {
                query: row.question,
                gold: row
                    .answer_session_ids
                    .into_iter()
                    .map(|id| format!("{group}/{id}"))
                    .collect(),
                group,
                answer: scalar(&row.answer),
                category,
                gold_all: Vec::new(),
                provenance: String::new(),
                now: parse_lme_date(Some(&row.question_date)),
            });
        }
        Ok(dataset)
    }

    pub fn load_locomo(path: impl AsRef<std::path::Path>, sessions: bool) -> anyhow::Result<Self> {
        let rows: Vec<LocomoRow> = serde_json::from_slice(&std::fs::read(path)?)?;
        let evidence = regex::Regex::new(r"D\d+:\d+").unwrap();
        let mut dataset = Self {
            name: if sessions {
                "locomo-sessions".into()
            } else {
                "locomo".into()
            },
            items: Vec::new(),
            questions: Vec::new(),
        };
        for (index, row) in rows.into_iter().enumerate() {
            let group = if row.sample_id.is_empty() {
                format!("conv-{index}")
            } else {
                row.sample_id
            };
            let mut turn_sessions = std::collections::HashMap::new();
            let mut group_now = None;
            for (key, value) in &row.conversation {
                if key
                    .strip_prefix("session_")
                    .is_none_or(|suffix| suffix.parse::<usize>().is_err())
                {
                    continue;
                }
                let Ok(turns) = serde_json::from_value::<Vec<LocomoTurn>>(value.clone()) else {
                    continue;
                };
                let date = row
                    .conversation
                    .get(&format!("{key}_date_time"))
                    .map(scalar)
                    .unwrap_or_default();
                let timestamp = parse_locomo_date(&date);
                if timestamp > group_now {
                    group_now = timestamp;
                }
                if sessions {
                    let id = format!("{group}/{key}");
                    let mut lines = if date.is_empty() {
                        Vec::new()
                    } else {
                        vec![format!("[{date}]")]
                    };
                    for turn in turns {
                        if turn.dia_id.is_empty() {
                            continue;
                        }
                        turn_sessions.insert(turn.dia_id, id.clone());
                        lines.push(format!("{}: {}", turn.speaker.trim(), turn.text.trim()));
                    }
                    dataset.items.push(Item {
                        id,
                        content: lines.join("\n").trim().into(),
                        group: group.clone(),
                        session: String::new(),
                        source: String::new(),
                        time: timestamp,
                    });
                } else {
                    for turn in turns {
                        if turn.dia_id.is_empty() {
                            continue;
                        }
                        let id = format!("{group}/{}", turn.dia_id);
                        let content = format!(
                            "{}{}: {}",
                            if date.is_empty() {
                                String::new()
                            } else {
                                format!("[{date}] ")
                            },
                            turn.speaker.trim(),
                            turn.text.trim()
                        );
                        dataset.items.push(Item {
                            id,
                            content,
                            group: group.clone(),
                            session: String::new(),
                            source: String::new(),
                            time: timestamp,
                        });
                    }
                }
            }
            for qa in row.qa {
                let matches = evidence
                    .find_iter(&qa.evidence.to_string())
                    .map(|value| value.as_str().to_owned())
                    .collect::<Vec<_>>();
                if matches.is_empty() {
                    continue;
                }
                let gold: Vec<String> = if sessions {
                    let mut seen = HashSet::new();
                    matches
                        .into_iter()
                        .filter_map(|id| turn_sessions.get(&id).cloned())
                        .filter(|id| seen.insert(id.clone()))
                        .collect()
                } else {
                    matches
                        .into_iter()
                        .map(|id| format!("{group}/{id}"))
                        .collect()
                };
                if gold.is_empty() {
                    continue;
                }
                dataset.questions.push(Question {
                    query: qa.question,
                    gold,
                    group: group.clone(),
                    answer: scalar(&qa.answer),
                    category: scalar(&qa.category),
                    gold_all: Vec::new(),
                    provenance: String::new(),
                    now: group_now,
                });
            }
        }
        Ok(dataset)
    }

    pub fn load_coding_agent(path: impl AsRef<std::path::Path>) -> anyhow::Result<Self> {
        let value: serde_json::Value = serde_json::from_slice(&std::fs::read(path.as_ref())?)?;
        let name = value
            .get("name")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("codingagent")
            .to_owned();
        let raw_items = value
            .get("items")
            .and_then(serde_json::Value::as_array)
            .ok_or_else(|| anyhow::anyhow!("coding-agent dataset has no items"))?;
        let raw_questions = value
            .get("questions")
            .and_then(serde_json::Value::as_array)
            .ok_or_else(|| anyhow::anyhow!("coding-agent dataset has no questions"))?;
        anyhow::ensure!(
            !raw_items.is_empty() && !raw_questions.is_empty(),
            "coding-agent dataset has no items or questions"
        );
        let mut items = Vec::with_capacity(raw_items.len());
        for raw in raw_items {
            let id = string_field(raw, "id");
            let time = DateTime::parse_from_rfc3339(&string_field(raw, "time"))
                .map_err(|error| anyhow::anyhow!("item {id:?}: invalid time: {error}"))?
                .with_timezone(&Utc);
            items.push(Item {
                id,
                content: string_field(raw, "content"),
                group: string_field(raw, "group"),
                session: string_field(raw, "session"),
                source: string_field(raw, "source"),
                time: Some(time),
            });
        }
        items.sort_by(|a, b| a.time.cmp(&b.time).then_with(|| a.id.cmp(&b.id)));
        let mut questions = Vec::with_capacity(raw_questions.len());
        for (index, raw) in raw_questions.iter().enumerate() {
            let now = DateTime::parse_from_rfc3339(&string_field(raw, "now"))
                .map_err(|error| anyhow::anyhow!("question {index}: invalid now: {error}"))?
                .with_timezone(&Utc);
            questions.push(Question {
                query: string_field(raw, "query"),
                gold: string_array(raw, "gold"),
                group: string_field(raw, "group"),
                answer: string_field(raw, "answer"),
                category: string_field(raw, "category"),
                gold_all: string_array(raw, "gold_all"),
                provenance: string_field(raw, "provenance"),
                now: Some(now),
            });
        }
        Ok(Self {
            name,
            items,
            questions,
        })
    }

    pub fn split_holdout(mut self, mode: &str) -> anyhow::Result<Self> {
        if mode.is_empty() || mode == "all" {
            return Ok(self);
        }
        anyhow::ensure!(
            matches!(mode, "tune" | "held"),
            "unknown holdout {mode:?} (want tune|held|all)"
        );
        let held = mode == "held";
        self.questions = self
            .questions
            .into_iter()
            .enumerate()
            .filter_map(|(index, question)| ((index % 10 == 9) == held).then_some(question))
            .collect();
        let groups = self
            .questions
            .iter()
            .map(|question| &question.group)
            .collect::<HashSet<_>>();
        self.items.retain(|item| groups.contains(&item.group));
        self.name.push('-');
        self.name.push_str(mode);
        Ok(self)
    }
}

#[derive(Clone, Copy, Debug)]
pub enum DocumentMode {
    Full,
    UserOnly,
    Dated,
}
#[derive(Deserialize)]
struct Turn {
    role: String,
    content: String,
}
#[derive(Deserialize)]
struct LongMemEvalRow {
    #[serde(default)]
    question_id: String,
    #[serde(default)]
    question_type: String,
    question: String,
    #[serde(default)]
    question_date: String,
    #[serde(default)]
    answer: serde_json::Value,
    #[serde(default)]
    answer_session_ids: Vec<String>,
    #[serde(default)]
    haystack_session_ids: Vec<String>,
    #[serde(default)]
    haystack_dates: Vec<String>,
    #[serde(default)]
    haystack_sessions: Vec<Vec<Turn>>,
}
#[derive(Deserialize)]
struct LocomoRow {
    #[serde(default)]
    sample_id: String,
    conversation: serde_json::Map<String, serde_json::Value>,
    #[serde(default)]
    qa: Vec<LocomoQuestion>,
}
#[derive(Deserialize)]
struct LocomoQuestion {
    question: String,
    #[serde(default)]
    answer: serde_json::Value,
    #[serde(default)]
    evidence: serde_json::Value,
    #[serde(default)]
    category: serde_json::Value,
}
#[derive(Deserialize)]
struct LocomoTurn {
    #[serde(default)]
    speaker: String,
    #[serde(default)]
    dia_id: String,
    #[serde(default)]
    text: String,
}
fn scalar(value: &serde_json::Value) -> String {
    value
        .as_str()
        .map(str::to_owned)
        .unwrap_or_else(|| value.to_string().trim_matches('"').into())
}
fn string_field(value: &serde_json::Value, name: &str) -> String {
    value
        .get(name)
        .and_then(serde_json::Value::as_str)
        .unwrap_or("")
        .into()
}
fn string_array(value: &serde_json::Value, name: &str) -> Vec<String> {
    value
        .get(name)
        .and_then(serde_json::Value::as_array)
        .map_or_else(Vec::new, |values| {
            values
                .iter()
                .filter_map(serde_json::Value::as_str)
                .map(str::to_owned)
                .collect()
        })
}
fn parse_lme_date(value: Option<&String>) -> Option<DateTime<Utc>> {
    NaiveDateTime::parse_from_str(value?.trim(), "%Y/%m/%d (%a) %H:%M")
        .ok()
        .map(|value| value.and_utc())
}
fn parse_locomo_date(value: &str) -> Option<DateTime<Utc>> {
    NaiveDateTime::parse_from_str(
        value.trim().to_lowercase().as_str(),
        "%l:%M %P on %-d %B, %Y",
    )
    .ok()
    .map(|value| value.and_utc())
}
fn session_text(turns: &[Turn]) -> String {
    turns
        .iter()
        .map(|turn| format!("{}: {}\n", turn.role, turn.content))
        .collect()
}
fn session_document(turns: &[Turn], dates: &[String], index: usize, mode: DocumentMode) -> String {
    match mode {
        DocumentMode::Full => session_text(turns),
        DocumentMode::UserOnly => {
            let value = turns
                .iter()
                .filter(|turn| turn.role == "user")
                .map(|turn| format!("{}\n", turn.content))
                .collect::<String>();
            if value.is_empty() {
                session_text(turns)
            } else {
                value
            }
        }
        DocumentMode::Dated => dates
            .get(index)
            .filter(|date| !date.trim().is_empty())
            .map_or_else(
                || session_text(turns),
                |date| format!("[{date}]\n{}", session_text(turns)),
            ),
    }
}

#[derive(Clone)]
pub struct FakeEmbedder {
    dimensions: usize,
}
impl FakeEmbedder {
    pub const fn new(dimensions: usize) -> Self {
        Self { dimensions }
    }
    fn vector(&self, text: &str) -> Vec<f32> {
        let mut vector = vec![0.0; self.dimensions];
        for token in text.to_lowercase().split_whitespace() {
            let mut hash = 2_166_136_261_u32;
            for byte in token.as_bytes() {
                hash ^= u32::from(*byte);
                hash = hash.wrapping_mul(16_777_619);
            }
            vector[hash as usize % self.dimensions] += 1.0;
        }
        let norm = vector.iter().map(|value| value * value).sum::<f32>().sqrt();
        if norm == 0.0 {
            vector[0] = 1.0
        } else {
            vector.iter_mut().for_each(|value| *value /= norm)
        }
        vector
    }
}
#[async_trait]
impl Embedder for FakeEmbedder {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>, EmbedError> {
        Ok(texts.iter().map(|text| self.vector(text)).collect())
    }
    fn dimensions(&self) -> usize {
        self.dimensions
    }
}

#[derive(Clone, Debug, Serialize)]
pub struct ResultRow {
    pub system: String,
    pub dataset: String,
    pub k: usize,
    pub questions: usize,
    pub recall_at_k: f64,
    pub mrr: f64,
    pub p50_ms: f64,
    pub p95_ms: f64,
    pub ingest_ms: f64,
    pub tokens_injected_mean: f64,
    pub token_efficiency: f64,
}

#[derive(Clone, Copy)]
pub enum Strategy {
    Vector,
    Keyword,
    Hybrid,
}
#[derive(Clone, Copy, Debug, Default)]
pub enum IngestMode {
    #[default]
    Upsert,
    Write,
}
#[derive(Clone)]
pub struct BenchOptions {
    pub mode: IngestMode,
    pub query_prefix: String,
    pub document_prefix: String,
    pub fusion_alpha: f64,
    pub pool_factor: usize,
    pub pool_floor: usize,
    pub reranker: Option<Arc<dyn memini_rerank::Reranker>>,
    pub rerank_pool: usize,
    pub answerer: Option<Arc<dyn memini_llm::Client>>,
    pub query_rewrite: bool,
    pub temporal_boost: f64,
    pub distill: bool,
}
impl Default for BenchOptions {
    fn default() -> Self {
        Self {
            mode: IngestMode::Upsert,
            query_prefix: String::new(),
            document_prefix: String::new(),
            fusion_alpha: 0.5,
            pool_factor: 0,
            pool_floor: 0,
            reranker: None,
            rerank_pool: 0,
            answerer: None,
            query_rewrite: false,
            temporal_boost: memini_core::temporal::DEFAULT_TEMPORAL_BOOST,
            distill: false,
        }
    }
}
impl Strategy {
    fn name(self) -> &'static str {
        match self {
            Self::Vector => "memini-vector",
            Self::Keyword => "memini-keyword",
            Self::Hybrid => "memini-hybrid",
        }
    }
}

pub async fn run(
    dataset: &Dataset,
    dimensions: usize,
    ks: &[usize],
) -> anyhow::Result<Vec<ResultRow>> {
    run_with_options(dataset, dimensions, ks, IngestMode::Upsert).await
}

pub async fn run_with_options(
    dataset: &Dataset,
    dimensions: usize,
    ks: &[usize],
    mode: IngestMode,
) -> anyhow::Result<Vec<ResultRow>> {
    run_with_embedder(dataset, Arc::new(FakeEmbedder::new(dimensions)), ks, mode).await
}

pub async fn run_with_embedder(
    dataset: &Dataset,
    embedder: Arc<dyn Embedder>,
    ks: &[usize],
    mode: IngestMode,
) -> anyhow::Result<Vec<ResultRow>> {
    run_configured(
        dataset,
        embedder,
        ks,
        BenchOptions {
            mode,
            ..Default::default()
        },
    )
    .await
}

pub async fn run_configured(
    dataset: &Dataset,
    embedder: Arc<dyn Embedder>,
    ks: &[usize],
    options: BenchOptions,
) -> anyhow::Result<Vec<ResultRow>> {
    anyhow::ensure!(
        !ks.is_empty() && ks.iter().all(|value| *value > 0),
        "k must be positive"
    );
    let directory = tempfile::tempdir()?;
    let store: Arc<dyn Store> = Arc::new(memini_sqlite::SqliteStore::open(
        directory.path().join("bench.db"),
        embedder.dimensions(),
    )?);
    let mut service = Service::new(store.clone(), embedder.clone())
        .with_clock(Arc::new(|| {
            DateTime::from_timestamp(1_700_000_000, 0).unwrap()
        }))
        .with_query_prefix(options.query_prefix.clone())
        .with_score_fusion(options.fusion_alpha)
        .with_recall_pool(options.pool_factor, options.pool_floor)
        .with_temporal_boost(options.temporal_boost)
        .with_min_scores(0.0, 0.0)
        .with_write_dedup(0.625, "hint")
        .with_corroboration(0.70)
        .with_contradiction_downrank(0.625)
        .with_episodic_min_chars(120)
        .with_extract_on_write(true);
    if let Some(reranker) = options.reranker.clone() {
        service = service
            .with_reranker(reranker)
            .with_rerank_pool(options.rerank_pool);
    }
    if let Some(answerer) = options.answerer.clone() {
        service = service.with_answerer(answerer.clone());
        if options.distill {
            service = service.with_distiller(answerer).with_distill_on_write(true);
        }
    }
    let started = Instant::now();
    let aliases = match options.mode {
        IngestMode::Upsert => {
            ingest(
                store.as_ref(),
                embedder.as_ref(),
                &dataset.items,
                &options.document_prefix,
            )
            .await?;
            HashMap::new()
        }
        IngestMode::Write => ingest_write(&service, &dataset.items).await?,
    };
    if options.distill {
        service.flush_distill_batches(true).await;
    }
    let ingest_ms = started.elapsed().as_secs_f64() * 1000.0;
    let context = ScoreContext {
        store: store.as_ref(),
        embedder: embedder.as_ref(),
        service: &service,
        dataset,
        ks,
        aliases: &aliases,
        query_prefix: &options.query_prefix,
        query_rewrite: options.query_rewrite,
    };
    let mut rows = Vec::new();
    for (index, strategy) in [Strategy::Hybrid, Strategy::Vector, Strategy::Keyword]
        .into_iter()
        .enumerate()
    {
        rows.extend(score(&context, strategy, if index == 0 { ingest_ms } else { 0.0 }).await?);
    }
    Ok(rows)
}

async fn ingest_write(
    service: &Service,
    items: &[Item],
) -> anyhow::Result<HashMap<String, Vec<String>>> {
    let mut aliases: HashMap<String, Vec<String>> = HashMap::new();
    for item in items {
        let memory = service
            .remember(RememberInput {
                namespace: namespace(&item.group),
                content: item.content.clone(),
                valid_from: item.time,
                ttl: item.time.map(|_| chrono::Duration::seconds(-1)),
                metadata: if item.session.is_empty() {
                    Default::default()
                } else {
                    [(
                        "session_id".into(),
                        serde_json::Value::String(item.session.clone()),
                    )]
                    .into_iter()
                    .collect()
                },
                ..Default::default()
            })
            .await?;
        if let Some(memory) = memory {
            aliases.entry(memory.id).or_default().push(item.id.clone());
        }
    }
    Ok(aliases)
}

async fn ingest(
    store: &dyn Store,
    embedder: &dyn Embedder,
    items: &[Item],
    document_prefix: &str,
) -> anyhow::Result<()> {
    let now = DateTime::from_timestamp(1_700_000_000, 0).unwrap();
    let texts = items
        .iter()
        .map(|item| format!("{document_prefix}{}", item.content))
        .collect::<Vec<_>>();
    for (item, embedding) in items.iter().zip(embedder.embed(&texts).await?) {
        store
            .upsert(&Memory {
                id: item.id.clone(),
                namespace: namespace(&item.group),
                tier: Tier::Semantic,
                level: None,
                content: item.content.clone(),
                summary: String::new(),
                metadata: Default::default(),
                tags: Vec::new(),
                importance: 0.0,
                created_at: item.time.unwrap_or(now),
                updated_at: item.time.unwrap_or(now),
                last_accessed_at: item.time.unwrap_or(now),
                access_count: 0,
                expires_at: None,
                superseded_by: None,
                valid_from: item.time,
                valid_to: None,
                confidence: None,
                linked_memory_ids: Vec::new(),
                embedding,
            })
            .await?;
    }
    Ok(())
}

struct ScoreContext<'a> {
    store: &'a dyn Store,
    embedder: &'a dyn Embedder,
    service: &'a Service,
    dataset: &'a Dataset,
    ks: &'a [usize],
    aliases: &'a HashMap<String, Vec<String>>,
    query_prefix: &'a str,
    query_rewrite: bool,
}
async fn score(
    context: &ScoreContext<'_>,
    strategy: Strategy,
    ingest_ms: f64,
) -> anyhow::Result<Vec<ResultRow>> {
    let ScoreContext {
        store,
        embedder,
        service,
        dataset,
        ks,
        aliases,
        query_prefix,
        query_rewrite,
    } = context;
    let max_k = *ks.iter().max().unwrap();
    let corpus_tokens = dataset
        .items
        .iter()
        .map(|item| tokens(&item.content))
        .sum::<usize>() as f64;
    let mut hits = vec![0.0; ks.len()];
    let mut injected = vec![0.0; ks.len()];
    let mut reciprocal = 0.0;
    let mut latencies = Vec::new();
    for question in &dataset.questions {
        let started = Instant::now();
        let results = match strategy {
            Strategy::Vector => {
                let vector = embedder
                    .embed(&[format!("{query_prefix}{}", question.query)])
                    .await?
                    .pop()
                    .unwrap();
                store
                    .vector_search(
                        &namespace(&question.group),
                        &vector,
                        &Filter::default(),
                        max_k,
                    )
                    .await?
            }
            Strategy::Keyword => {
                store
                    .keyword_search(
                        &namespace(&question.group),
                        &question.query,
                        &Filter::default(),
                        max_k,
                    )
                    .await?
            }
            Strategy::Hybrid => {
                service
                    .recall(RecallInput {
                        namespace: namespace(&question.group),
                        query: question.query.clone(),
                        limit: max_k,
                        query_rewrite: *query_rewrite,
                        ..Default::default()
                    })
                    .await?
                    .0
            }
        };
        latencies.push(started.elapsed().as_secs_f64() * 1000.0);
        let gold = question.gold.iter().collect::<HashSet<_>>();
        let rank = results.iter().position(|result| {
            aliases.get(&result.memory.id).map_or_else(
                || gold.contains(&result.memory.id),
                |ids| ids.iter().any(|id| gold.contains(id)),
            )
        });
        if let Some(rank) = rank {
            reciprocal += 1.0 / (rank + 1) as f64;
        }
        for (index, k) in ks.iter().enumerate() {
            if rank.is_some_and(|rank| rank < *k) {
                hits[index] += 1.0;
            }
            injected[index] += results
                .iter()
                .take(*k)
                .map(|result| tokens(&result.memory.content))
                .sum::<usize>() as f64;
        }
    }
    latencies.sort_by(f64::total_cmp);
    let count = dataset.questions.len() as f64;
    Ok(ks
        .iter()
        .enumerate()
        .map(|(index, k)| ResultRow {
            system: strategy.name().into(),
            dataset: dataset.name.clone(),
            k: *k,
            questions: dataset.questions.len(),
            recall_at_k: hits[index] / count,
            mrr: reciprocal / count,
            p50_ms: percentile(&latencies, 50),
            p95_ms: percentile(&latencies, 95),
            ingest_ms,
            tokens_injected_mean: injected[index] / count,
            token_efficiency: if corpus_tokens == 0.0 {
                0.0
            } else {
                injected[index] / count / corpus_tokens
            },
        })
        .collect())
}
fn namespace(group: &str) -> String {
    if group.is_empty() {
        "bench".into()
    } else {
        group.into()
    }
}
fn tokens(value: &str) -> usize {
    value.len().div_ceil(4)
}
fn percentile(values: &[f64], p: usize) -> f64 {
    values
        .get(p * values.len().saturating_sub(1) / 100)
        .copied()
        .unwrap_or(0.0)
}

pub fn markdown(rows: &[ResultRow]) -> String {
    let mut rows = rows.to_vec();
    rows.sort_by(|a, b| b.recall_at_k.total_cmp(&a.recall_at_k));
    let Some(first) = rows.first() else {
        return String::new();
    };
    let mut output = format!(
        "## {} — {} questions, recall_any@{}\n\n| System | Recall@K | MRR | inj tok | tok-eff | p50 (ms) | p95 (ms) | ingest (ms) |\n|--------|---------:|----:|--------:|--------:|---------:|---------:|------------:|\n",
        first.dataset, first.questions, first.k
    );
    for row in rows {
        output.push_str(&format!(
            "| {} | {:.1}% | {:.1}% | {:.0} | {:.3}% | {:.2} | {:.2} | {:.1} |\n",
            row.system,
            row.recall_at_k * 100.0,
            row.mrr * 100.0,
            row.tokens_injected_mean,
            row.token_efficiency * 100.0,
            row.p50_ms,
            row.p95_ms,
            row.ingest_ms
        ));
    }
    output
}

#[derive(Clone, Debug, Serialize)]
pub struct GateResult {
    pub threshold: f64,
    pub pos_recall_at_k: f64,
    pub neg_injection_rate: f64,
}

pub async fn vector_gate_sweep(
    dataset: &Dataset,
    embedder: Arc<dyn Embedder>,
    k: usize,
    thresholds: &[f64],
    query_prefix: &str,
) -> anyhow::Result<Vec<GateResult>> {
    anyhow::ensure!(
        dataset.questions.len() >= 2,
        "gate sweep needs at least two questions"
    );
    let directory = tempfile::tempdir()?;
    let store: Arc<dyn Store> = Arc::new(memini_sqlite::SqliteStore::open(
        directory.path().join("gate.db"),
        embedder.dimensions(),
    )?);
    ingest(store.as_ref(), embedder.as_ref(), &dataset.items, "").await?;
    let service = Service::new(store.clone(), embedder.clone()).with_min_scores(0.0, 0.0);
    let mut probes = Vec::new();
    for (index, question) in dataset.questions.iter().enumerate() {
        let vector = embedder
            .embed(&[format!("{query_prefix}{}", question.query)])
            .await?
            .pop()
            .unwrap_or_default();
        let own = store
            .vector_search(&namespace(&question.group), &vector, &Filter::default(), k)
            .await?;
        let foreign = store
            .vector_search(
                &namespace(&dataset.questions[(index + 1) % dataset.questions.len()].group),
                &vector,
                &Filter::default(),
                k,
            )
            .await?;
        let recalled = service
            .recall(RecallInput {
                namespace: namespace(&question.group),
                query: question.query.clone(),
                limit: k,
                ..Default::default()
            })
            .await?
            .0;
        let gold = question.gold.iter().collect::<HashSet<_>>();
        probes.push((
            own.first().map_or(0.0, |hit| hit.score),
            foreign.first().map_or(0.0, |hit| hit.score),
            recalled.iter().any(|hit| gold.contains(&hit.memory.id)),
        ));
    }
    Ok(render_gate_results(&probes, thresholds))
}

pub async fn rerank_gate_sweep(
    dataset: &Dataset,
    embedder: Arc<dyn Embedder>,
    reranker: &memini_rerank::CrossEncoder,
    k: usize,
    pool: usize,
    thresholds: &[f64],
    query_prefix: &str,
) -> anyhow::Result<Vec<GateResult>> {
    anyhow::ensure!(
        dataset.questions.len() >= 2,
        "gate sweep needs at least two questions"
    );
    let directory = tempfile::tempdir()?;
    let store: Arc<dyn Store> = Arc::new(memini_sqlite::SqliteStore::open(
        directory.path().join("rerank-gate.db"),
        embedder.dimensions(),
    )?);
    ingest(store.as_ref(), embedder.as_ref(), &dataset.items, "").await?;
    let mut probes = Vec::new();
    for (index, question) in dataset.questions.iter().enumerate() {
        let vector = embedder
            .embed(&[format!("{query_prefix}{}", question.query)])
            .await?
            .pop()
            .unwrap_or_default();
        let own = rerank_probe(
            store.as_ref(),
            reranker,
            &namespace(&question.group),
            question,
            &vector,
            k,
            pool,
        )
        .await?;
        let foreign_group = &dataset.questions[(index + 1) % dataset.questions.len()].group;
        let foreign = rerank_probe(
            store.as_ref(),
            reranker,
            &namespace(foreign_group),
            question,
            &vector,
            k,
            pool,
        )
        .await?;
        probes.push((own.0, foreign.0, own.1));
    }
    Ok(render_gate_results(&probes, thresholds))
}

async fn rerank_probe(
    store: &dyn Store,
    reranker: &memini_rerank::CrossEncoder,
    namespace_value: &str,
    question: &Question,
    vector: &[f32],
    k: usize,
    pool: usize,
) -> anyhow::Result<(f64, bool)> {
    let vector_hits = store
        .vector_search(namespace_value, vector, &Filter::default(), pool)
        .await?;
    let keyword_hits = store
        .keyword_search(namespace_value, &question.query, &Filter::default(), pool)
        .await?;
    let mut fused = memini_core::search::fuse_scores(&[vector_hits, keyword_hits], &[0.5, 0.5], 0);
    fused.truncate(pool);
    let candidates = fused
        .iter()
        .map(|hit| memini_rerank::Candidate {
            id: hit.memory.id.clone(),
            content: hit.memory.content.clone(),
        })
        .collect::<Vec<_>>();
    let scores = reranker.scores(&question.query, &candidates).await?;
    fused.sort_by(|a, b| {
        scores
            .get(&b.memory.id)
            .unwrap_or(&0.0)
            .total_cmp(scores.get(&a.memory.id).unwrap_or(&0.0))
    });
    let gold = question.gold.iter().collect::<HashSet<_>>();
    Ok((
        scores
            .values()
            .copied()
            .max_by(f64::total_cmp)
            .unwrap_or(0.0),
        fused
            .iter()
            .take(k)
            .any(|hit| gold.contains(&hit.memory.id)),
    ))
}

fn render_gate_results(probes: &[(f64, f64, bool)], thresholds: &[f64]) -> Vec<GateResult> {
    let count = probes.len() as f64;
    thresholds
        .iter()
        .map(|threshold| GateResult {
            threshold: *threshold,
            pos_recall_at_k: probes
                .iter()
                .filter(|(own, _, hit)| *hit && own >= threshold)
                .count() as f64
                / count,
            neg_injection_rate: probes
                .iter()
                .filter(|(_, foreign, _)| foreign >= threshold)
                .count() as f64
                / count,
        })
        .collect()
}

pub fn gate_markdown(title: &str, rows: &[GateResult], k: usize) -> String {
    let mut output = format!(
        "## {title} — recall_any@{k} vs injection\n\n| threshold | pos R@K | neg inject % |\n|----------:|--------:|-------------:|\n"
    );
    for row in rows {
        output.push_str(&format!(
            "| {:.3} | {:.1}% | {:.1}% |\n",
            row.threshold,
            row.pos_recall_at_k * 100.0,
            row.neg_injection_rate * 100.0
        ));
    }
    output
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct QaResult {
    #[serde(rename = "i", alias = "index")]
    pub index: usize,
    pub category: String,
    pub correct: bool,
    #[serde(default)]
    pub answer: String,
}

#[derive(Clone, Debug)]
pub struct QaOptions {
    pub k: usize,
    pub mode: IngestMode,
    pub reasoning: String,
    pub workers: usize,
    pub temporal_boost: f64,
    pub distill: bool,
    /// Original question indexes to run. Empty means all questions.
    pub selected: Vec<usize>,
}

impl Default for QaOptions {
    fn default() -> Self {
        Self {
            k: 10,
            mode: IngestMode::Upsert,
            reasoning: String::new(),
            workers: 6,
            temporal_boost: 0.40,
            distill: false,
            selected: Vec::new(),
        }
    }
}

pub async fn run_qa(
    dataset: &Dataset,
    embedder: Arc<dyn Embedder>,
    client: Arc<dyn memini_llm::Client>,
    options: QaOptions,
) -> anyhow::Result<Vec<QaResult>> {
    let directory = tempfile::tempdir()?;
    let store = Arc::new(memini_sqlite::SqliteStore::open(
        directory.path().join("qa.db"),
        embedder.dimensions(),
    )?);
    match options.mode {
        IngestMode::Upsert => ingest(store.as_ref(), embedder.as_ref(), &dataset.items, "").await?,
        IngestMode::Write => {
            let clock = Arc::new(std::sync::RwLock::new(
                DateTime::from_timestamp(1_700_000_000, 0).unwrap(),
            ));
            let clock_reader = clock.clone();
            let writer = Service::new(store.clone(), embedder.clone())
                .with_clock(Arc::new(move || *clock_reader.read().unwrap()))
                .with_write_dedup(0.625, "hint")
                .with_corroboration(0.70)
                .with_contradiction_downrank(0.625)
                .with_episodic_min_chars(120)
                .with_extract_on_write(true)
                .with_consolidator(client.clone(), 0.3)
                .with_consolidate_mode("async");
            let writer = if options.distill {
                writer
                    .with_distiller(client.clone())
                    .with_distill_on_write(true)
            } else {
                writer
            };
            for item in &dataset.items {
                if item.content.is_empty() {
                    continue;
                }
                if let Some(time) = item.time {
                    *clock.write().unwrap() = time;
                }
                let _ = writer
                    .remember(RememberInput {
                        namespace: namespace(&item.group),
                        content: item.content.clone(),
                        valid_from: item.time,
                        ttl: Some(chrono::Duration::seconds(-1)),
                        metadata: if item.session.is_empty() {
                            Default::default()
                        } else {
                            [(
                                "session_id".into(),
                                serde_json::Value::String(item.session.clone()),
                            )]
                            .into_iter()
                            .collect()
                        },
                        ..Default::default()
                    })
                    .await?;
            }
            writer.flush_distill_batches(true).await;
        }
    }
    let selected = if options.selected.is_empty() {
        (0..dataset.questions.len()).collect::<Vec<_>>()
    } else {
        options.selected.clone()
    };
    let workers = options.workers.max(1);
    let k = options.k;
    let temporal_boost = options.temporal_boost;
    let reasoning = options.reasoning.clone();
    let mut output = stream::iter(selected.into_iter().map(|index| {
        let question = dataset.questions[index].clone();
        let store = store.clone();
        let embedder = embedder.clone();
        let client = client.clone();
        let reasoning = reasoning.clone();
        async move {
            let now = question
                .now
                .unwrap_or_else(|| DateTime::from_timestamp(1_700_000_000, 0).unwrap());
            let service = Service::new(store, embedder)
                .with_clock(Arc::new(move || now))
                .with_answerer(client.clone())
                .with_temporal_boost(temporal_boost)
                .with_min_scores(0.1, 0.0)
                .with_semantic_reserve(2);
            let answer = service
                .answer(AnswerInput {
                    namespace: namespace(&question.group),
                    query: question.query.clone(),
                    limit: k,
                    reasoning,
                    ..Default::default()
                })
                .await?
                .answer;
            let reference = if question.answer.is_empty() {
                "(no reference; unanswerable)"
            } else {
                &question.answer
            };
            let prompt = format!(
                "Question: {}\nReference: {}\nCandidate: {}\nGrade:",
                question.query, reference, answer
            );
            let grade = client
                .complete(judge_system(&question.category), &prompt)
                .await?
                .to_uppercase();
            anyhow::Ok(QaResult {
                index,
                category: question.category,
                correct: grade.contains("CORRECT") && !grade.contains("INCORRECT"),
                answer,
            })
        }
    }))
    .buffer_unordered(workers)
    .collect::<Vec<anyhow::Result<QaResult>>>()
    .await
    .into_iter()
    .collect::<anyhow::Result<Vec<_>>>()?;
    output.sort_by_key(|result| result.index);
    Ok(output)
}

fn judge_system(category: &str) -> &'static str {
    const BASE: &str = "You grade answers. Given a question, the reference answer, and a candidate answer, reply with exactly CORRECT or INCORRECT. The candidate is CORRECT if it conveys the same key fact(s) as the reference, even if phrased differently or with extra words. The candidate may be a distilled or summarized paraphrase of the reference; grade on the key fact(s), not the wording.";
    const UPDATE: &str = "You grade answers. Given a question, the reference answer, and a candidate answer, reply with exactly CORRECT or INCORRECT. The candidate is CORRECT if it conveys the same key fact(s) as the reference, even if phrased differently or with extra words. The candidate may be a distilled or summarized paraphrase of the reference; grade on the key fact(s), not the wording. The reference is the UPDATED value of a fact that changed over time: the candidate is CORRECT if it states the updated value (even if it also mentions the earlier value as outdated), and INCORRECT if it gives only the earlier, superseded value.";
    const TEMPORAL: &str = "You grade answers. Given a question, the reference answer, and a candidate answer, reply with exactly CORRECT or INCORRECT. The candidate is CORRECT if it conveys the same key fact(s) as the reference, even if phrased differently or with extra words. The candidate may be a distilled or summarized paraphrase of the reference; grade on the key fact(s), not the wording. Dates within one day of the reference are CORRECT (timezone and relative-date arithmetic slack).";
    const ABSTAIN: &str = "You grade answers to questions that are NOT answerable from the memories the candidate saw. Reply with exactly CORRECT or INCORRECT. The candidate is CORRECT only if it declines to answer — says it doesn't know, the information wasn't mentioned, or the question can't be answered. Any substantive invented answer is INCORRECT.";
    if category == "abstention" || category.ends_with("_abs") {
        ABSTAIN
    } else if matches!(category, "knowledge-update" | "temporal-update") {
        UPDATE
    } else if category == "temporal-reasoning" {
        TEMPORAL
    } else {
        BASE
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn offline_sample_acceptance() {
        let dataset = Dataset::sample().unwrap();
        let rows = run(&dataset, 256, &[5]).await.unwrap();
        assert_eq!(rows.len(), 3);
        let row = |name| rows.iter().find(|row| row.system == name).unwrap();
        assert_eq!(row("memini-hybrid").recall_at_k, 1.0);
        assert_eq!(row("memini-hybrid").mrr, 1.0);
        assert_eq!(row("memini-vector").recall_at_k, 1.0);
        assert_eq!(row("memini-vector").mrr, 0.90625);
        assert_eq!(row("memini-keyword").recall_at_k, 1.0);
        assert_eq!(row("memini-keyword").mrr, 1.0);

        let write = run_with_options(&dataset, 256, &[5], IngestMode::Write)
            .await
            .unwrap();
        let row = |name| write.iter().find(|row| row.system == name).unwrap();
        assert_eq!(row("memini-hybrid").recall_at_k, 1.0);
        assert_eq!(row("memini-vector").recall_at_k, 0.0);
        assert_eq!(row("memini-keyword").recall_at_k, 0.0);
    }

    #[test]
    fn external_dataset_adapters_match_reference_shapes() {
        let directory = tempfile::tempdir().unwrap();
        let long = directory.path().join("long.json");
        std::fs::write(&long, r#"[{"question_id":"q1","question_type":"update","question":"what changed","question_date":"2023/06/01 (Thu) 12:00","answer":"new","answer_session_ids":["s1"],"haystack_session_ids":["s1"],"haystack_dates":["2023/05/30 (Tue) 23:40"],"haystack_sessions":[[{"role":"user","content":"new value"}]]}]"#).unwrap();
        let dataset = Dataset::load_longmemeval(&long, DocumentMode::Dated).unwrap();
        assert_eq!(dataset.items[0].id, "q1/s1");
        assert!(dataset.items[0].content.starts_with("[2023/05/30"));
        assert_eq!(dataset.questions[0].gold, ["q1/s1"]);

        let locomo = directory.path().join("locomo.json");
        std::fs::write(&locomo, r#"[{"sample_id":"c1","conversation":{"session_1":[{"speaker":"Sam","dia_id":"D1:1","text":"uses Rust"}],"session_1_date_time":"1:56 pm on 8 May, 2023"},"qa":[{"question":"language?","answer":"Rust","evidence":["D1:1"],"category":1}]}]"#).unwrap();
        let turns = Dataset::load_locomo(&locomo, false).unwrap();
        assert_eq!(turns.items[0].id, "c1/D1:1");
        assert_eq!(turns.questions[0].gold, ["c1/D1:1"]);
        let sessions = Dataset::load_locomo(&locomo, true).unwrap();
        assert_eq!(sessions.questions[0].gold, ["c1/session_1"]);

        let coding = Dataset::load_coding_agent(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../bench/data/codingagent_v1.json"),
        )
        .unwrap();
        assert!(!coding.items.is_empty());
        assert!(
            coding
                .items
                .windows(2)
                .all(|pair| pair[0].time <= pair[1].time)
        );
    }
}
