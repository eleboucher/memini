use chrono::{DateTime, Duration, NaiveDate, NaiveDateTime, Utc};
use memini_core::memory::{CONFIDENCE_SEED_IMPORTED, Memory, Term, Tier, fingerprint};
use memini_embed::Embedder;
use memini_intelligence::redact;
use memini_store::{NamespaceLink, Store, StoreError};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use std::collections::{HashMap, HashSet};
use thiserror::Error;
use uuid::Uuid;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Source {
    Memini,
    AgentMemory,
    Mem0,
    Mnemory,
    ClaudeCode,
}
impl Source {
    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "memini" => Some(Self::Memini),
            "agentmemory" => Some(Self::AgentMemory),
            "mem0" => Some(Self::Mem0),
            "mnemory" => Some(Self::Mnemory),
            "claude-code" => Some(Self::ClaudeCode),
            _ => None,
        }
    }
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Memini => "memini",
            Self::AgentMemory => "agentmemory",
            Self::Mem0 => "mem0",
            Self::Mnemory => "mnemory",
            Self::ClaudeCode => "claude-code",
        }
    }
}
#[derive(Clone, Debug, Default)]
pub struct Record {
    pub id: String,
    pub namespace: String,
    pub tier: Option<Tier>,
    pub content: String,
    pub summary: String,
    pub tags: Vec<String>,
    pub metadata: Map<String, Value>,
    pub importance: f64,
    pub created_at: Option<DateTime<Utc>>,
    pub updated_at: Option<DateTime<Utc>>,
    pub expires_at: Option<DateTime<Utc>>,
}
#[derive(Debug, Error)]
pub enum ImportError {
    #[error("import: {0}")]
    Parse(String),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Embed(#[from] memini_embed::EmbedError),
}
pub type Result<T> = std::result::Result<T, ImportError>;
fn list(data: &[u8], key: &str) -> Result<Vec<Value>> {
    let value: Value = serde_json::from_slice(data)?;
    Ok(if let Some(v) = value.get(key).and_then(Value::as_array) {
        v.clone()
    } else {
        value
            .as_array()
            .cloned()
            .ok_or_else(|| ImportError::Parse("expected JSON array".into()))?
    })
}
fn string(value: &Value, key: &str) -> String {
    value
        .get(key)
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .into()
}
fn metadata(value: &Value) -> Map<String, Value> {
    value
        .get("metadata")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default()
}
fn strings(value: Option<&Value>) -> Vec<String> {
    value.and_then(Value::as_array).map_or_else(Vec::new, |v| {
        v.iter()
            .filter_map(Value::as_str)
            .map(str::to_owned)
            .collect()
    })
}
fn time(value: &str) -> Option<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(value)
        .ok()
        .map(|v| v.with_timezone(&Utc))
        .or_else(|| {
            NaiveDateTime::parse_from_str(value, "%Y-%m-%d %H:%M:%S")
                .ok()
                .map(|v| v.and_utc())
        })
        .or_else(|| {
            NaiveDate::parse_from_str(value, "%Y-%m-%d")
                .ok()
                .and_then(|v| v.and_hms_opt(0, 0, 0))
                .map(|v| v.and_utc())
        })
}
fn tier(value: &str) -> Option<Tier> {
    match value {
        "working" | "context" => Some(Tier::Working),
        "episodic" | "episodic_memory" => Some(Tier::Episodic),
        "procedural" | "preference" | "procedural_memory" | "pattern" | "architecture"
        | "workflow" => Some(Tier::Procedural),
        "semantic" | "semantic_memory" | "fact" | "bug" => Some(Tier::Semantic),
        _ => None,
    }
}
pub fn parse(source: Source, data: &[u8]) -> Result<Vec<Record>> {
    match source {
        Source::Memini => parse_memini(data),
        Source::Mem0 => parse_mem0(data),
        Source::AgentMemory => parse_agent(data),
        Source::Mnemory => parse_mnemory(data),
        Source::ClaudeCode => parse_claude(data),
    }
}
pub fn parse_links(data: &[u8]) -> Result<Vec<NamespaceLink>> {
    let value: Value = serde_json::from_slice(data)?;
    Ok(value
        .get("links")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|value| {
            let source = string(value, "src");
            let destination = string(value, "dst");
            (!source.is_empty() && !destination.is_empty()).then(|| NamespaceLink {
                source,
                destination,
                tiers: strings(value.get("tiers"))
                    .iter()
                    .filter_map(|value| tier(value))
                    .collect(),
                note: string(value, "note"),
                created_at: time(&string(value, "created_at")).unwrap_or_else(Utc::now),
            })
        })
        .collect())
}
fn parse_memini(data: &[u8]) -> Result<Vec<Record>> {
    Ok(list(data, "memories")?
        .into_iter()
        .map(|v| Record {
            id: string(&v, "id"),
            namespace: string(&v, "namespace"),
            tier: tier(&string(&v, "tier")),
            content: string(&v, "content"),
            summary: string(&v, "summary"),
            tags: strings(v.get("tags")),
            metadata: metadata(&v),
            importance: v.get("importance").and_then(Value::as_f64).unwrap_or(0.0),
            created_at: time(&string(&v, "created_at")),
            updated_at: time(&string(&v, "updated_at")),
            expires_at: time(&string(&v, "expires_at")),
        })
        .collect())
}
fn parse_mem0(data: &[u8]) -> Result<Vec<Record>> {
    Ok(list(data, "results")?
        .into_iter()
        .map(|v| {
            let meta = metadata(&v);
            let namespace = ["user_id", "agent_id", "run_id"]
                .iter()
                .find_map(|key| {
                    v.get(key)
                        .or_else(|| meta.get(*key))
                        .and_then(Value::as_str)
                        .filter(|v| !v.trim().is_empty())
                })
                .unwrap_or("")
                .into();
            let tags = if v.get("categories").is_some() {
                strings(v.get("categories"))
            } else {
                strings(meta.get("categories"))
            };
            let memory_type = meta
                .get("memory_type")
                .and_then(Value::as_str)
                .unwrap_or("semantic");
            Record {
                id: string(&v, "id"),
                namespace,
                tier: tier(memory_type).or(Some(Tier::Semantic)),
                content: string(&v, "memory"),
                tags,
                metadata: meta,
                importance: 0.5,
                created_at: time(&string(&v, "created_at")),
                updated_at: time(&string(&v, "updated_at")),
                ..Record::default()
            }
        })
        .collect())
}
fn parse_agent(data: &[u8]) -> Result<Vec<Record>> {
    Ok(list(data, "memories")?
        .into_iter()
        .map(|v| Record {
            id: string(&v, "id"),
            namespace: string(&v, "project"),
            tier: tier(&string(&v, "type")).or(Some(Tier::Semantic)),
            content: string(&v, "content"),
            summary: string(&v, "title"),
            tags: strings(v.get("concepts")),
            importance: v.get("strength").and_then(Value::as_f64).unwrap_or(0.0),
            created_at: time(&string(&v, "createdAt")),
            updated_at: time(&string(&v, "updatedAt")),
            expires_at: time(&string(&v, "forgetAfter")),
            ..Record::default()
        })
        .collect())
}
fn parse_mnemory(data: &[u8]) -> Result<Vec<Record>> {
    Ok(list(data, "memories")?
        .into_iter()
        .map(|v| {
            let created = time(&string(&v, "created_at"));
            let expires = v
                .get("ttl_days")
                .and_then(Value::as_i64)
                .filter(|v| *v > 0)
                .and_then(|days| created.map(|v| v + Duration::days(days)));
            let importance = match string(&v, "importance").as_str() {
                "high" | "critical" => 0.9,
                "low" | "minor" => 0.2,
                _ => 0.5,
            };
            Record {
                id: string(&v, "id"),
                namespace: {
                    let n = string(&v, "namespace");
                    if n.is_empty() {
                        string(&v, "tenant")
                    } else {
                        n
                    }
                },
                tier: tier(&string(&v, "memory_type")).or(Some(Tier::Semantic)),
                content: {
                    let c = string(&v, "content");
                    if c.is_empty() {
                        string(&v, "memory")
                    } else {
                        c
                    }
                },
                tags: strings(v.get("tags")),
                metadata: metadata(&v),
                importance,
                created_at: created,
                updated_at: created,
                expires_at: expires,
                ..Record::default()
            }
        })
        .collect())
}
#[derive(Deserialize)]
struct ClaudeLine {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default, rename = "isSidechain")]
    sidechain: bool,
    #[serde(default, rename = "isMeta")]
    meta: bool,
    #[serde(default)]
    timestamp: String,
    #[serde(default)]
    cwd: String,
    #[serde(default, rename = "sessionId")]
    session_id: String,
    message: ClaudeMessage,
}
#[derive(Deserialize)]
struct ClaudeMessage {
    content: Value,
}
pub fn parse_claude(data: &[u8]) -> Result<Vec<Record>> {
    let mut records = Vec::new();
    let mut pending: Option<(String, ClaudeLine)> = None;
    let mut emitted = 0;
    for line in data.split(|v| *v == b'\n').filter(|v| !v.is_empty()) {
        let Ok(item) = serde_json::from_slice::<ClaudeLine>(line) else {
            continue;
        };
        match item.kind.as_str() {
            "user" if !item.sidechain && !item.meta && item.message.content.is_string() => {
                pending = Some((item.message.content.as_str().unwrap().into(), item))
            }
            "assistant" if !item.sidechain => {
                let Some((user, base)) = pending.take() else {
                    continue;
                };
                let answer = item
                    .message
                    .content
                    .as_array()
                    .map(|blocks| {
                        blocks
                            .iter()
                            .filter(|v| v.get("type").and_then(Value::as_str) == Some("text"))
                            .filter_map(|v| v.get("text").and_then(Value::as_str))
                            .collect::<Vec<_>>()
                            .join("\n")
                    })
                    .unwrap_or_default();
                if answer.trim().is_empty() || answer.trim().starts_with("API Error:") {
                    continue;
                }
                let session = if base.session_id.is_empty() {
                    "unknown"
                } else {
                    &base.session_id
                };
                records.push(Record {
                    id: format!("cc:{session}:{emitted:04}"),
                    namespace: base.cwd.rsplit('/').next().unwrap_or("").into(),
                    tier: Some(Tier::Episodic),
                    content: format!(
                        "user: {}\nassistant: {}",
                        truncate(&user, 4000),
                        truncate(&answer, 4000)
                    ),
                    tags: vec!["claude-code".into()],
                    metadata: Map::from_iter([
                        ("session_id".into(), Value::String(session.into())),
                        ("timestamp".into(), Value::String(base.timestamp.clone())),
                        ("source".into(), Value::String("claude-code".into())),
                    ]),
                    created_at: time(&base.timestamp),
                    expires_at: Some(Utc::now() + Tier::Episodic.default_ttl().unwrap()),
                    ..Record::default()
                });
                emitted += 1
            }
            _ => {}
        }
    }
    Ok(records)
}
pub fn load_claude_path(path: &std::path::Path) -> Result<Vec<Record>> {
    if path.is_file() {
        return parse_claude(&std::fs::read(path).map_err(|e| ImportError::Parse(e.to_string()))?);
    }
    let mut records = Vec::new();
    let entries = std::fs::read_dir(path).map_err(|e| ImportError::Parse(e.to_string()))?;
    for entry in entries {
        let entry = entry.map_err(|e| ImportError::Parse(e.to_string()))?;
        let path = entry.path();
        if path.is_dir() {
            records.extend(load_claude_path(&path)?)
        } else if path.extension().and_then(|v| v.to_str()) == Some("jsonl") {
            records.extend(parse_claude(
                &std::fs::read(path).map_err(|e| ImportError::Parse(e.to_string()))?,
            )?)
        }
    }
    Ok(records)
}
fn truncate(value: &str, max: usize) -> String {
    value.chars().take(max).collect()
}

#[derive(Clone, Debug)]
pub struct Options {
    pub default_namespace: String,
    pub force_namespace: String,
    pub source: Source,
    pub default_importance: f64,
    pub confidence: Option<f64>,
    pub skip_existing: bool,
    pub dedup_content: bool,
    pub dry_run: bool,
    pub batch_size: usize,
    pub min_content_len: usize,
    pub min_importance: f64,
}
impl Default for Options {
    fn default() -> Self {
        Self {
            default_namespace: "default".into(),
            force_namespace: String::new(),
            source: Source::Memini,
            default_importance: 0.2,
            confidence: None,
            skip_existing: true,
            dedup_content: true,
            dry_run: false,
            batch_size: 64,
            min_content_len: 0,
            min_importance: 0.0,
        }
    }
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct Report {
    pub total: usize,
    pub imported: usize,
    pub skipped: usize,
    pub duplicates: usize,
    pub namespaces: HashMap<String, usize>,
    pub errors: Vec<String>,
}
const IMPORT_NAMESPACE: Uuid = Uuid::from_u128(0x6f9e4d2a7c3b4e159a8d2b1c0f6e5a44);
pub async fn import(
    store: &dyn Store,
    embedder: &dyn Embedder,
    records: Vec<Record>,
    options: &Options,
) -> Result<Report> {
    let (records, mut report) = prepare(records, options);
    let now = Utc::now();
    if options.dry_run {
        return Ok(report);
    }
    let mut seen = HashSet::new();
    for batch in records.chunks(options.batch_size.max(1)) {
        let mut pending = Vec::new();
        for record in batch {
            let effective = record.tier.unwrap_or(Tier::Episodic);
            if options.skip_existing {
                match store.get(&record.namespace, &record.id).await {
                    Ok(_) => {
                        report.duplicates += 1;
                        continue;
                    }
                    Err(StoreError::NotFound) => {}
                    Err(e) => {
                        report.errors.push(format!("{}: {e}", record.id));
                        continue;
                    }
                }
            }
            if options.dedup_content {
                let fp = fingerprint(&redact::secrets(&record.content));
                let key = (
                    record.namespace.clone(),
                    format!("{effective:?}"),
                    fp.clone(),
                );
                if !seen.insert(key)
                    || store
                        .get_by_fingerprint(&record.namespace, effective, &fp, Some(now))
                        .await
                        .is_ok()
                {
                    report.duplicates += 1;
                    continue;
                }
            }
            pending.push(record)
        }
        if pending.is_empty() {
            continue;
        }
        let texts = pending
            .iter()
            .map(|v| redact::secrets(&v.content))
            .collect::<Vec<_>>();
        let vectors = match embedder.embed(&texts).await {
            Ok(vectors) => vectors,
            Err(_) => vec![Vec::new(); pending.len()],
        };
        for (record, embedding) in pending.into_iter().zip(vectors) {
            let created = record.created_at.unwrap_or(now);
            let tier = record.tier.unwrap_or(Tier::Episodic);
            let mut metadata = record.metadata.clone();
            if embedding.is_empty() {
                metadata.insert("pending_embed".into(), Value::String("true".into()));
            }
            let memory = Memory {
                id: record.id.clone(),
                namespace: record.namespace.clone(),
                tier,
                level: None,
                content: redact::secrets(&record.content),
                summary: redact::secrets(&record.summary),
                metadata,
                tags: record.tags.clone(),
                importance: record.importance,
                created_at: created,
                updated_at: record.updated_at.unwrap_or(created),
                last_accessed_at: created,
                access_count: 0,
                expires_at: record
                    .expires_at
                    .or_else(|| tier.default_ttl().map(|v| created + v)),
                superseded_by: None,
                valid_from: (tier.term() == Term::Long).then_some(created),
                valid_to: None,
                confidence: (tier.term() == Term::Long)
                    .then_some(options.confidence.unwrap_or(CONFIDENCE_SEED_IMPORTED)),
                linked_memory_ids: vec![],
                embedding,
            };
            match store.upsert(&memory).await {
                Ok(()) => report.imported += 1,
                Err(e) => report.errors.push(format!("{}: {e}", record.id)),
            }
        }
    }
    Ok(report)
}

pub fn prepare(mut records: Vec<Record>, options: &Options) -> (Vec<Record>, Report) {
    let now = Utc::now();
    let mut report = Report {
        total: records.len(),
        ..Report::default()
    };
    records.retain(|r| {
        let keep = r.content.trim().len() >= options.min_content_len.max(1)
            && r.importance >= options.min_importance;
        if !keep {
            report.skipped += 1
        }
        keep
    });
    for record in &mut records {
        let original = record.namespace.clone();
        record.namespace = if !options.force_namespace.is_empty() {
            options.force_namespace.clone()
        } else if record.namespace.is_empty() {
            options.default_namespace.clone()
        } else {
            record.namespace.clone()
        };
        *report
            .namespaces
            .entry(record.namespace.clone())
            .or_default() += 1;
        record.tags.push(format!(
            "import:{}:{}",
            options.source.as_str(),
            now.format("%Y-%m-%d")
        ));
        record.metadata.insert(
            "import_source".into(),
            Value::String(options.source.as_str().into()),
        );
        if original != record.namespace && !original.is_empty() {
            record
                .metadata
                .insert("import_source_namespace".into(), Value::String(original));
        }
        if record.importance == 0.0 {
            record.importance = options.default_importance
        }
        if record.id.is_empty() {
            record.id = Uuid::new_v5(
                &IMPORT_NAMESPACE,
                format!(
                    "{}\0{}\0{}",
                    options.source.as_str(),
                    record.namespace,
                    record.content
                )
                .as_bytes(),
            )
            .to_string()
        }
    }
    (records, report)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn adapters() {
        let mem0 = br#"{"results":[{"id":"1","memory":"likes rust","user_id":"u"}]}"#;
        let records = parse(Source::Mem0, mem0).unwrap();
        assert_eq!(records[0].namespace, "u");
        assert_eq!(records[0].tier, Some(Tier::Semantic));
        let own = br#"[{"id":"x","namespace":"n","tier":"procedural","content":"run cargo test"}]"#;
        assert_eq!(
            parse(Source::Memini, own).unwrap()[0].tier,
            Some(Tier::Procedural)
        );
        let links = parse_links(br#"{"memories":[],"links":[{"src":"a","dst":"b","tiers":["semantic"],"note":"shared"}]}"#).unwrap();
        assert_eq!(links.len(), 1);
        assert_eq!(links[0].source, "a");
        assert_eq!(links[0].destination, "b");
        assert_eq!(links[0].tiers, vec![Tier::Semantic]);
    }
}
