use crate::{Result, Service};
use chrono::{DateTime, Utc};
use memini_core::{memory::Tier, search::Scored};
use memini_store::{Event, EventFilter, EventKind};
use serde::Serialize;
use serde_json::{Map, Value};

#[derive(Clone, Debug, Serialize)]
pub struct ActivityMemory {
    pub id: String,
    pub namespace: String,
    pub summary: String,
    pub tier: String,
    pub rank: usize,
    pub score: Option<f64>,
    pub section: String,
}
#[derive(Clone, Debug, Serialize)]
pub struct ActivityEvent {
    pub operation_id: String,
    pub kind: String,
    pub time: DateTime<Utc>,
    pub namespace: String,
    pub query: String,
    pub detail: Map<String, Value>,
    pub memories: Vec<ActivityMemory>,
}
#[derive(Clone, Debug, Default)]
pub struct EventsInput {
    pub namespace: String,
    pub namespaces: Vec<String>,
    pub kinds: Vec<EventKind>,
    pub tiers: Vec<Tier>,
    pub text: String,
    pub since: Option<DateTime<Utc>>,
    pub before: Option<DateTime<Utc>>,
    pub before_id: i64,
    pub limit: usize,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct EventsPage {
    pub events: Vec<ActivityEvent>,
    pub next_before: Option<DateTime<Utc>>,
    pub next_before_id: i64,
    pub has_more: bool,
}

impl Service {
    pub(crate) async fn log_memory(
        &self,
        kind: EventKind,
        namespace: &str,
        memory: Option<&memini_core::memory::Memory>,
        detail: Map<String, Value>,
    ) {
        let Some(store) = &self.events else { return };
        let mut event = Event {
            id: 0,
            operation_id: (self.id)(),
            kind,
            namespace: namespace.into(),
            query: String::new(),
            memory_id: String::new(),
            memory_namespace: String::new(),
            memory_tier: Tier::Working,
            memory_summary: String::new(),
            rank: 0,
            score: None,
            detail,
            created_at: (self.now)(),
        };
        if let Some(memory) = memory {
            apply(&mut event, memory)
        }
        let _ = store.append_events(&[event]).await;
    }
    pub(crate) async fn log_recall(
        &self,
        namespace: &str,
        query: &str,
        results: &[Scored],
        degraded: Option<&str>,
    ) {
        let Some(store) = &self.events else { return };
        let operation = (self.id)();
        let mut detail = Map::new();
        if let Some(value) = degraded {
            detail.insert("degraded".into(), Value::String(value.into()));
        }
        let mut events = if results.is_empty() {
            vec![Event {
                id: 0,
                operation_id: operation,
                kind: EventKind::Recall,
                namespace: namespace.into(),
                query: query.into(),
                memory_id: String::new(),
                memory_namespace: String::new(),
                memory_tier: Tier::Working,
                memory_summary: String::new(),
                rank: 0,
                score: None,
                detail,
                created_at: (self.now)(),
            }]
        } else {
            results
                .iter()
                .enumerate()
                .map(|(rank, result)| {
                    let mut event = Event {
                        id: 0,
                        operation_id: operation.clone(),
                        kind: EventKind::Recall,
                        namespace: namespace.into(),
                        query: query.into(),
                        memory_id: String::new(),
                        memory_namespace: String::new(),
                        memory_tier: Tier::Working,
                        memory_summary: String::new(),
                        rank: rank + 1,
                        score: Some(result.score),
                        detail: detail.clone(),
                        created_at: (self.now)(),
                    };
                    apply(&mut event, &result.memory);
                    event
                })
                .collect()
        };
        let _ = store.append_events(&events).await;
        events.clear();
    }
    pub async fn events(&self, input: EventsInput) -> Result<EventsPage> {
        let Some(store) = &self.events else {
            return Ok(EventsPage::default());
        };
        let limit = if input.limit == 0 {
            50
        } else {
            input.limit.min(200)
        };
        let row_limit = limit * 8;
        let rows = store
            .list_events(&EventFilter {
                namespace: input.namespace,
                namespaces: input.namespaces,
                kinds: input.kinds,
                tiers: input.tiers,
                text: input.text,
                since: input.since,
                before: input.before,
                before_id: input.before_id,
                limit: Some(row_limit),
            })
            .await?;
        let mut groups = group(&rows);
        let mut page = EventsPage::default();
        let truncated = groups.len() > limit || (rows.len() == row_limit && groups.len() > 1);
        if groups.len() > limit {
            groups.truncate(limit)
        } else if rows.len() == row_limit && groups.len() > 1 {
            groups.pop();
        }
        page.events = groups;
        if truncated
            && let Some(last) = page.events.last()
            && let Some(row) = rows
                .iter()
                .rev()
                .find(|v| v.operation_id == last.operation_id)
        {
            page.next_before = Some(row.created_at);
            page.next_before_id = row.id;
            page.has_more = true
        }
        Ok(page)
    }
    pub async fn prune_events(
        &self,
        older_than: Option<DateTime<Utc>>,
        keep_max: Option<usize>,
    ) -> Result<u64> {
        let Some(store) = &self.events else {
            return Ok(0);
        };
        Ok(store.prune_events(older_than, keep_max).await?)
    }
}
fn apply(event: &mut Event, memory: &memini_core::memory::Memory) {
    event.memory_id = memory.id.clone();
    event.memory_namespace = memory.namespace.clone();
    event.memory_tier = memory.tier;
    let value = if memory.summary.is_empty() {
        &memory.content
    } else {
        &memory.summary
    };
    event.memory_summary = value.chars().take(200).collect();
}
fn kind(value: EventKind) -> &'static str {
    match value {
        EventKind::Recall => "recall",
        EventKind::Get => "get",
        EventKind::Briefing => "briefing",
        EventKind::Remember => "remember",
        EventKind::Update => "update",
        EventKind::Forget => "forget",
        EventKind::Supersede => "supersede",
    }
}
fn group(rows: &[Event]) -> Vec<ActivityEvent> {
    let mut out = Vec::new();
    let mut index = 0;
    while index < rows.len() {
        let mut end = index + 1;
        while end < rows.len() && rows[end].operation_id == rows[index].operation_id {
            end += 1
        }
        let first = &rows[index];
        let mut memories = rows[index..end]
            .iter()
            .filter(|v| !v.memory_id.is_empty())
            .map(|v| ActivityMemory {
                id: v.memory_id.clone(),
                namespace: v.memory_namespace.clone(),
                summary: v.memory_summary.clone(),
                tier: format!("{:?}", v.memory_tier).to_lowercase(),
                rank: v.rank,
                score: v.score,
                section: v
                    .detail
                    .get("section")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .into(),
            })
            .collect::<Vec<_>>();
        memories.sort_by_key(|v| v.rank);
        out.push(ActivityEvent {
            operation_id: first.operation_id.clone(),
            kind: kind(first.kind).into(),
            time: first.created_at,
            namespace: first.namespace.clone(),
            query: first.query.clone(),
            detail: first.detail.clone(),
            memories,
        });
        index = end
    }
    out
}
