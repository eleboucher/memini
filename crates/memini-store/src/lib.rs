use async_trait::async_trait;
use chrono::{DateTime, Utc};
use memini_core::{
    memory::{Level, Memory, Tier},
    search::Scored,
};
use serde_json::{Map, Value};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("memory not found")]
    NotFound,
    #[error("id exists in a different namespace")]
    Conflict,
    #[error("{0}")]
    Backend(String),
}
pub type Result<T> = std::result::Result<T, StoreError>;
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum SortKey {
    #[default]
    CreatedAt,
    UpdatedAt,
    LastAccessedAt,
    AccessCount,
    Importance,
}
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Sort {
    pub key: SortKey,
    pub ascending: bool,
}
#[derive(Clone, Debug, Default)]
pub struct Filter {
    pub tiers: Vec<Tier>,
    pub levels: Vec<Level>,
    pub tags: Vec<String>,
    pub metadata: Map<String, Value>,
    pub exclude_metadata: Map<String, Value>,
    pub include_expired: bool,
    pub include_superseded: bool,
    pub now: Option<DateTime<Utc>>,
    pub as_of: Option<DateTime<Utc>>,
    pub memory_types: Vec<String>,
    pub created_after: Option<DateTime<Utc>>,
    pub accessed_after: Option<DateTime<Utc>>,
    pub sort: Sort,
}

pub fn filter_matches(memory: &Memory, filter: &Filter) -> bool {
    if !filter.tiers.is_empty() && !filter.tiers.contains(&memory.tier) {
        return false;
    }
    if !filter.levels.is_empty() && !memory.level.is_some_and(|v| filter.levels.contains(&v)) {
        return false;
    }
    if !filter.tags.iter().all(|v| memory.tags.contains(v)) {
        return false;
    }
    if !filter
        .metadata
        .iter()
        .all(|(k, v)| memory.metadata.get(k) == Some(v))
    {
        return false;
    }
    if filter
        .exclude_metadata
        .iter()
        .any(|(k, v)| memory.metadata.get(k) == Some(v))
    {
        return false;
    }
    if !filter.memory_types.is_empty()
        && !memory
            .metadata
            .get("memory_type")
            .and_then(Value::as_str)
            .is_some_and(|v| filter.memory_types.iter().any(|item| item == v))
    {
        return false;
    }
    if filter.created_after.is_some_and(|v| memory.created_at < v)
        || filter
            .accessed_after
            .is_some_and(|v| memory.last_accessed_at < v)
    {
        return false;
    }
    let reference = filter.as_of.or(filter.now).unwrap_or_else(Utc::now);
    if !filter.include_expired && memory.expires_at.is_some_and(|v| v <= reference) {
        return false;
    }
    if let Some(as_of) = filter.as_of {
        if memory.valid_from.is_some_and(|v| v > as_of)
            || memory.valid_to.is_some_and(|v| v <= as_of)
        {
            return false;
        }
    } else if !filter.include_superseded
        && (memory.superseded_by.is_some() || memory.valid_to.is_some_and(|v| v <= reference))
    {
        return false;
    }
    true
}

pub fn compare_memories(a: &Memory, b: &Memory, filter: &Filter) -> std::cmp::Ordering {
    let order = match filter.sort.key {
        SortKey::CreatedAt => a.created_at.cmp(&b.created_at),
        SortKey::UpdatedAt => a.updated_at.cmp(&b.updated_at),
        SortKey::LastAccessedAt => a.last_accessed_at.cmp(&b.last_accessed_at),
        SortKey::AccessCount => a.access_count.cmp(&b.access_count),
        SortKey::Importance => a.importance.total_cmp(&b.importance),
    };
    let order = if filter.sort.ascending {
        order
    } else {
        order.reverse()
    };
    order.then_with(|| a.id.cmp(&b.id))
}

#[async_trait]
pub trait Store: Send + Sync {
    async fn upsert(&self, memory: &Memory) -> Result<()>;
    async fn get(&self, namespace: &str, id: &str) -> Result<Memory>;
    async fn predecessor_ids(&self, namespace: &str, id: &str) -> Result<Vec<String>>;
    async fn get_by_fingerprint(
        &self,
        namespace: &str,
        tier: Tier,
        fingerprint: &str,
        now: Option<DateTime<Utc>>,
    ) -> Result<Memory>;
    async fn delete(&self, namespace: &str, id: &str) -> Result<()>;
    async fn set_superseded(&self, namespace: &str, id: &str, replacement: &str) -> Result<()>;
    async fn restore(&self, namespace: &str, id: &str) -> Result<()>;
    async fn vector_search(
        &self,
        namespace: &str,
        vector: &[f32],
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>>;
    async fn keyword_search(
        &self,
        namespace: &str,
        query: &str,
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>>;
    async fn reinforce(
        &self,
        namespace: &str,
        ids: &[String],
        accessed_at: DateTime<Utc>,
        new_expiry: Option<DateTime<Utc>>,
    ) -> Result<()>;
    async fn delete_if_expired_before(
        &self,
        namespace: &str,
        id: &str,
        cutoff: DateTime<Utc>,
    ) -> Result<()>;
    async fn list_expired(&self, now: DateTime<Utc>, limit: usize) -> Result<Vec<Memory>>;
    async fn list(
        &self,
        namespace: &str,
        filter: &Filter,
        limit: Option<usize>,
    ) -> Result<Vec<Memory>>;
    async fn list_namespaces(&self) -> Result<Vec<String>>;
    async fn namespace_activity(
        &self,
        now: Option<DateTime<Utc>>,
    ) -> Result<Vec<NamespaceActivity>>;
    async fn delete_namespace(&self, namespace: &str) -> Result<u64>;
    async fn reassign(&self, from: &str, ids: &[String], to: &str) -> Result<u64>;
    async fn retier(
        &self,
        namespace: &str,
        id: &str,
        tier: Tier,
        expires_at: Option<DateTime<Utc>>,
    ) -> Result<()>;
    async fn set_confidence(
        &self,
        namespace: &str,
        id: &str,
        confidence: f64,
        now: DateTime<Utc>,
    ) -> Result<()>;
    async fn mark_contradicted(
        &self,
        namespace: &str,
        id: &str,
        contradicted_by: &str,
        confidence: f64,
        now: DateTime<Utc>,
    ) -> Result<()>;
    async fn embed_model(&self) -> Result<String>;
    async fn set_embed_model(&self, model: &str) -> Result<()>;
    async fn ping(&self) -> Result<()>;
}

#[derive(Clone, Debug, PartialEq)]
pub struct NamespaceLink {
    pub source: String,
    pub destination: String,
    pub tiers: Vec<Tier>,
    pub note: String,
    pub created_at: DateTime<Utc>,
}
#[derive(Clone, Debug, PartialEq)]
pub struct NamespaceActivity {
    pub namespace: String,
    pub total: usize,
    pub last_write: DateTime<Utc>,
}
#[derive(Clone, Debug, PartialEq)]
pub struct ApiKey {
    pub name: String,
    pub hash: String,
    pub home_namespace: String,
    pub default_namespace: String,
    pub created_at: Option<DateTime<Utc>>,
    pub disabled: bool,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EventKind {
    Recall,
    Get,
    Briefing,
    Remember,
    Update,
    Forget,
    Supersede,
}
#[derive(Clone, Debug, PartialEq)]
pub struct Event {
    pub id: i64,
    pub operation_id: String,
    pub kind: EventKind,
    pub namespace: String,
    pub query: String,
    pub memory_id: String,
    pub memory_namespace: String,
    pub memory_tier: Tier,
    pub memory_summary: String,
    pub rank: usize,
    pub score: Option<f64>,
    pub detail: Map<String, Value>,
    pub created_at: DateTime<Utc>,
}
#[derive(Clone, Debug, Default)]
pub struct EventFilter {
    pub namespace: String,
    pub namespaces: Vec<String>,
    pub kinds: Vec<EventKind>,
    pub tiers: Vec<Tier>,
    pub text: String,
    pub since: Option<DateTime<Utc>>,
    pub before: Option<DateTime<Utc>>,
    pub before_id: i64,
    pub limit: Option<usize>,
}

#[async_trait]
pub trait LinkStore: Send + Sync {
    async fn put_link(&self, link: &NamespaceLink) -> Result<()>;
    async fn delete_link(&self, source: &str, destination: &str) -> Result<bool>;
    async fn list_links(&self, source: &str) -> Result<Vec<NamespaceLink>>;
    async fn list_all_links(&self) -> Result<Vec<NamespaceLink>>;
    async fn rename_link_endpoints(&self, from: &str, to: &str) -> Result<()>;
}
#[async_trait]
pub trait ApiKeyStore: Send + Sync {
    async fn put_api_key(&self, key: &ApiKey) -> Result<()>;
    async fn delete_api_key(&self, name: &str) -> Result<bool>;
    async fn list_api_keys(&self) -> Result<Vec<ApiKey>>;
    async fn get_api_key_by_hash(&self, hash: &str) -> Result<Option<ApiKey>>;
    async fn rename_api_key_namespaces(&self, from: &str, to: &str) -> Result<()>;
}
#[async_trait]
pub trait EventLogStore: Send + Sync {
    async fn append_events(&self, events: &[Event]) -> Result<()>;
    async fn list_events(&self, filter: &EventFilter) -> Result<Vec<Event>>;
    async fn prune_events(
        &self,
        older_than: Option<DateTime<Utc>>,
        keep_max: Option<usize>,
    ) -> Result<u64>;
}
