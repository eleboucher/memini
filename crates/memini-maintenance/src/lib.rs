use chrono::{DateTime, Utc};
use memini_core::memory::{
    CONFIDENCE_DEMOTE_FLOOR, CONFIDENCE_SEED_IMPORTED, Tier, normalize_content,
};
use memini_store::{Filter, Store, StoreError};
use serde::{Deserialize, Serialize};
use std::{
    collections::{HashMap, HashSet},
    sync::Arc,
    time::Duration as StdDuration,
};

pub const PINNED_TAG: &str = "pinned";
const BATCH: usize = 500;
pub async fn purge_expired(store: &dyn Store, now: DateTime<Utc>) -> memini_store::Result<usize> {
    let mut total = 0;
    loop {
        let expired = store.list_expired(now, BATCH).await?;
        for memory in &expired {
            match store
                .delete_if_expired_before(&memory.namespace, &memory.id, now)
                .await
            {
                Ok(()) => total += 1,
                Err(StoreError::NotFound) => {}
                Err(e) => return Err(e),
            }
        }
        if expired.len() < BATCH {
            return Ok(total);
        }
    }
}
pub async fn enforce_short_term_cap(
    store: &dyn Store,
    cap: usize,
    now: DateTime<Utc>,
) -> memini_store::Result<usize> {
    if cap == 0 {
        return Ok(0);
    }
    let mut total = 0;
    for namespace in store.list_namespaces().await? {
        let mut memories = store
            .list(
                &namespace,
                &Filter {
                    tiers: vec![Tier::Working, Tier::Episodic],
                    ..Filter::default()
                },
                None,
            )
            .await?;
        if memories.len() <= cap {
            continue;
        }
        memories.sort_by(|a, b| a.retention_score(now).total_cmp(&b.retention_score(now)));
        let remove = memories.len() - cap;
        for memory in memories.into_iter().take(remove) {
            match store.delete(&namespace, &memory.id).await {
                Ok(()) => total += 1,
                Err(StoreError::NotFound) => {}
                Err(e) => return Err(e),
            }
        }
    }
    Ok(total)
}
#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Report {
    pub expired_purged: usize,
    pub short_term_evicted: usize,
    pub namespaces: usize,
    pub duplicate_groups: Vec<Vec<String>>,
}
pub async fn fsck(
    store: &dyn Store,
    cap: usize,
    now: DateTime<Utc>,
) -> memini_store::Result<Report> {
    let mut out = Report {
        expired_purged: purge_expired(store, now).await?,
        short_term_evicted: enforce_short_term_cap(store, cap, now).await?,
        ..Report::default()
    };
    let namespaces = store.list_namespaces().await?;
    out.namespaces = namespaces.len();
    for ns in namespaces {
        let mut groups: HashMap<String, Vec<String>> = HashMap::new();
        for memory in store.list(&ns, &Filter::default(), None).await? {
            groups
                .entry(normalize_content(&memory.content))
                .or_default()
                .push(memory.id)
        }
        out.duplicate_groups
            .extend(groups.into_values().filter(|v| v.len() > 1));
    }
    Ok(out)
}
pub async fn forget_by_tag(
    store: &dyn Store,
    namespace: &str,
    tag: &str,
) -> memini_store::Result<u64> {
    let mut total = 0;
    for memory in store
        .list(
            namespace,
            &Filter {
                include_expired: true,
                include_superseded: true,
                ..Filter::default()
            },
            None,
        )
        .await?
    {
        if memory.tags.iter().any(|v| v == tag) {
            match store.delete(namespace, &memory.id).await {
                Ok(()) => total += 1,
                Err(StoreError::NotFound) => {}
                Err(e) => return Err(e),
            }
        }
    }
    Ok(total)
}
pub async fn purge_tombstones(
    store: &dyn Store,
    older_than: DateTime<Utc>,
) -> memini_store::Result<usize> {
    let mut total = 0;
    for ns in store.list_namespaces().await? {
        for memory in store
            .list(
                &ns,
                &Filter {
                    include_expired: true,
                    include_superseded: true,
                    ..Filter::default()
                },
                None,
            )
            .await?
        {
            if memory.superseded_by.is_some()
                && memory.valid_to.unwrap_or(memory.updated_at) < older_than
            {
                match store.delete(&ns, &memory.id).await {
                    Ok(()) => total += 1,
                    Err(StoreError::NotFound) => {}
                    Err(e) => return Err(e),
                }
            }
        }
    }
    Ok(total)
}
pub async fn demote_stale(
    store: &dyn Store,
    older_than: DateTime<Utc>,
    now: DateTime<Utc>,
) -> memini_store::Result<usize> {
    let mut total = 0;
    let expiry = now + Tier::Episodic.default_ttl().unwrap();
    for ns in store.list_namespaces().await? {
        for memory in store
            .list(
                &ns,
                &Filter {
                    tiers: vec![Tier::Semantic, Tier::Procedural],
                    now: Some(now),
                    ..Filter::default()
                },
                None,
            )
            .await?
        {
            if memory.access_count == 0
                && memory.importance < 0.5
                && memory.updated_at < older_than
                && memory.effective_confidence(now) < CONFIDENCE_DEMOTE_FLOOR
                && !memory.tags.iter().any(|v| v == PINNED_TAG)
            {
                match store
                    .retier(&ns, &memory.id, Tier::Episodic, Some(expiry))
                    .await
                {
                    Ok(()) => total += 1,
                    Err(StoreError::NotFound) => {}
                    Err(e) => return Err(e),
                }
            }
        }
    }
    Ok(total)
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct ConfidenceReport {
    pub inspected: usize,
    pub seeded: usize,
    pub skipped: usize,
}
pub async fn backfill_confidence(
    store: &dyn Store,
    now: DateTime<Utc>,
    apply: bool,
) -> memini_store::Result<ConfidenceReport> {
    let mut out = ConfidenceReport::default();
    for ns in store.list_namespaces().await? {
        for memory in store
            .list(
                &ns,
                &Filter {
                    tiers: vec![Tier::Semantic, Tier::Procedural],
                    ..Filter::default()
                },
                None,
            )
            .await?
        {
            out.inspected += 1;
            if memory.confidence.is_some() {
                out.skipped += 1
            } else if !apply {
                out.seeded += 1
            } else {
                match store
                    .set_confidence(&ns, &memory.id, CONFIDENCE_SEED_IMPORTED, now)
                    .await
                {
                    Ok(()) => out.seeded += 1,
                    Err(StoreError::NotFound) => out.skipped += 1,
                    Err(e) => return Err(e),
                }
            }
        }
    }
    Ok(out)
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct ScrubReport {
    pub lifecycle_noise: usize,
    pub exact_duplicates: usize,
    pub namespaces: usize,
}
impl ScrubReport {
    pub fn total(&self) -> usize {
        self.lifecycle_noise + self.exact_duplicates
    }
}
pub async fn scrub(store: &dyn Store, apply: bool) -> memini_store::Result<ScrubReport> {
    let namespaces = store.list_namespaces().await?;
    let mut out = ScrubReport {
        namespaces: namespaces.len(),
        ..ScrubReport::default()
    };
    for ns in namespaces {
        let mut memories = store.list(&ns, &Filter::default(), None).await?;
        memories.sort_by(|a, b| {
            a.created_at
                .cmp(&b.created_at)
                .then_with(|| a.id.cmp(&b.id))
        });
        let mut seen = HashSet::new();
        for memory in memories {
            let normalized = normalize_content(&memory.content);
            let noise = normalized.starts_with("session ended in ")
                || normalized.starts_with("stop checkpoint in ");
            let duplicate = !noise && !seen.insert((memory.tier as u8, normalized));
            if noise {
                out.lifecycle_noise += 1
            } else if duplicate {
                out.exact_duplicates += 1
            } else {
                continue;
            }
            if apply {
                match store.delete(&ns, &memory.id).await {
                    Ok(()) | Err(StoreError::NotFound) => {}
                    Err(e) => return Err(e),
                }
            }
        }
    }
    Ok(out)
}

pub const DEFAULT_SPLIT_KEYS: &[&str] = &[
    "import_source_namespace",
    "user_id",
    "agent_id",
    "run_id",
    "project",
];
#[derive(Clone, Debug, Default, Serialize)]
pub struct RenamespaceReport {
    pub moved: u64,
    pub targets: HashMap<String, u64>,
    pub skipped: usize,
    pub dry_run: bool,
}
pub async fn move_namespace(
    store: &dyn Store,
    from: &str,
    to: &str,
    dry_run: bool,
) -> memini_store::Result<RenamespaceReport> {
    let mut out = RenamespaceReport {
        dry_run,
        ..RenamespaceReport::default()
    };
    if from == to {
        return Ok(out);
    }
    let memories = store
        .list(
            from,
            &Filter {
                include_expired: true,
                include_superseded: true,
                ..Filter::default()
            },
            None,
        )
        .await?;
    if memories.is_empty() {
        return Ok(out);
    }
    let ids = memories.into_iter().map(|v| v.id).collect::<Vec<_>>();
    let count = if dry_run {
        ids.len() as u64
    } else {
        store.reassign(from, &ids, to).await?
    };
    out.moved = count;
    out.targets.insert(to.into(), count);
    Ok(out)
}
pub async fn split_namespace(
    store: &dyn Store,
    from: &str,
    keys: &[String],
    dry_run: bool,
) -> memini_store::Result<RenamespaceReport> {
    let keys: Vec<&str> = if keys.is_empty() {
        DEFAULT_SPLIT_KEYS.to_vec()
    } else {
        keys.iter().map(String::as_str).collect()
    };
    let mut groups: HashMap<String, Vec<String>> = HashMap::new();
    let mut out = RenamespaceReport {
        dry_run,
        ..RenamespaceReport::default()
    };
    for memory in store
        .list(
            from,
            &Filter {
                include_expired: true,
                include_superseded: true,
                ..Filter::default()
            },
            None,
        )
        .await?
    {
        let target = keys
            .iter()
            .find_map(|key| {
                memory
                    .metadata
                    .get(*key)
                    .and_then(serde_json::Value::as_str)
                    .map(str::trim)
                    .filter(|v| !v.is_empty())
            })
            .unwrap_or("")
            .trim_matches('/')
            .replace("//", "/");
        if target.is_empty() || target == from || target.len() > 256 || target.contains(['\0', '*'])
        {
            out.skipped += 1
        } else {
            groups.entry(target).or_default().push(memory.id)
        }
    }
    let mut targets: Vec<_> = groups.into_iter().collect();
    targets.sort_by(|a, b| a.0.cmp(&b.0));
    for (target, ids) in targets {
        let count = if dry_run {
            ids.len() as u64
        } else {
            store.reassign(from, &ids, &target).await?
        };
        out.moved += count;
        out.targets.insert(target, count);
    }
    Ok(out)
}

#[derive(Clone, Debug)]
pub struct SweeperConfig {
    pub interval: StdDuration,
    pub short_term_cap: usize,
    pub tombstone_ttl: Option<chrono::Duration>,
    pub demote_after: Option<chrono::Duration>,
}
pub async fn run_sweeper(store: Arc<dyn Store>, config: SweeperConfig) {
    if config.interval.is_zero() {
        return;
    }
    let mut ticker = tokio::time::interval(config.interval);
    loop {
        ticker.tick().await;
        let now = Utc::now();
        let _ = purge_expired(store.as_ref(), now).await;
        let _ = enforce_short_term_cap(store.as_ref(), config.short_term_cap, now).await;
        if let Some(ttl) = config.tombstone_ttl {
            let _ = purge_tombstones(store.as_ref(), now - ttl).await;
        }
        if let Some(age) = config.demote_after {
            let _ = demote_stale(store.as_ref(), now - age, now).await;
        }
    }
}

#[derive(Clone)]
pub struct DedupOptions {
    pub similarity: f64,
    pub min_cluster_size: usize,
    pub tiers: Vec<Tier>,
    pub namespaces: Vec<String>,
    pub neighbours_per_anchor: usize,
    pub dry_run: bool,
    pub now: DateTime<Utc>,
    pub merger: Option<Arc<dyn memini_llm::Client>>,
    pub max_merge_cluster_size: usize,
}
impl Default for DedupOptions {
    fn default() -> Self {
        Self {
            similarity: 0.85,
            min_cluster_size: 2,
            tiers: vec![],
            namespaces: vec![],
            neighbours_per_anchor: 20,
            dry_run: false,
            now: Utc::now(),
            merger: None,
            max_merge_cluster_size: 10,
        }
    }
}
#[derive(Clone, Debug, Serialize)]
pub struct ClusterAction {
    pub representative_id: String,
    pub tombstoned_ids: Vec<String>,
    pub size: usize,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct DedupReport {
    pub namespaces: usize,
    pub memories_seen: usize,
    pub clusters_found: usize,
    pub tombstoned: usize,
    pub dry_run: bool,
    pub actions: Vec<ClusterAction>,
}
pub async fn dedup(
    store: &dyn Store,
    embedder: &dyn memini_embed::Embedder,
    options: DedupOptions,
) -> Result<DedupReport, Box<dyn std::error::Error + Send + Sync>> {
    if options.similarity < 0.0 {
        return Ok(DedupReport::default());
    }
    let namespaces = if options.namespaces.is_empty() {
        store.list_namespaces().await?
    } else {
        options.namespaces.clone()
    };
    let mut report = DedupReport {
        dry_run: options.dry_run,
        ..DedupReport::default()
    };
    for namespace in namespaces {
        let memories = store
            .list(
                &namespace,
                &Filter {
                    tiers: options.tiers.clone(),
                    now: Some(options.now),
                    ..Filter::default()
                },
                None,
            )
            .await?;
        if memories.len() < options.min_cluster_size.max(2) {
            continue;
        }
        report.namespaces += 1;
        report.memories_seen += memories.len();
        let texts = memories
            .iter()
            .map(|v| v.content.clone())
            .collect::<Vec<_>>();
        let vectors = embedder.embed(&texts).await?;
        let mut parent: HashMap<String, String> = memories
            .iter()
            .map(|v| (v.id.clone(), v.id.clone()))
            .collect();
        for (memory, vector) in memories.iter().zip(vectors) {
            for hit in store
                .vector_search(
                    &namespace,
                    &vector,
                    &Filter {
                        tiers: options.tiers.clone(),
                        now: Some(options.now),
                        ..Filter::default()
                    },
                    options.neighbours_per_anchor.max(20),
                )
                .await?
            {
                if hit.memory.id != memory.id
                    && hit.score >= options.similarity
                    && parent.contains_key(&hit.memory.id)
                {
                    union(&mut parent, &memory.id, &hit.memory.id)
                }
            }
        }
        let mut components: HashMap<String, Vec<memini_core::memory::Memory>> = HashMap::new();
        for memory in memories {
            let root = find(&mut parent, &memory.id);
            components.entry(root).or_default().push(memory)
        }
        for mut component in components
            .into_values()
            .filter(|v| v.len() >= options.min_cluster_size.max(2))
        {
            component.sort_by(|a, b| {
                b.retention_score(options.now)
                    .total_cmp(&a.retention_score(options.now))
                    .then_with(|| b.updated_at.cmp(&a.updated_at))
                    .then_with(|| b.created_at.cmp(&a.created_at))
            });
            let mut representative = component.remove(0);
            let ids = component.iter().map(|v| v.id.clone()).collect::<Vec<_>>();
            report.actions.push(ClusterAction {
                representative_id: representative.id.clone(),
                tombstoned_ids: ids.clone(),
                size: ids.len() + 1,
            });
            report.clusters_found += 1;
            if let Some(merger) = &options.merger {
                let mut contents = vec![representative.content.clone()];
                contents.extend(
                    component
                        .iter()
                        .take(options.max_merge_cluster_size.saturating_sub(1))
                        .map(|v| v.content.clone()),
                );
                if let Ok(merged) = merger.merge_memories(&contents).await
                    && !merged.is_empty()
                    && merged != representative.content
                    && let Ok(vector) = memini_embed::embed_one(embedder, &merged).await
                {
                    representative.content = merged;
                    representative.embedding = vector;
                    representative.updated_at = options.now;
                    if !options.dry_run {
                        store.upsert(&representative).await?
                    }
                }
            }
            for id in ids {
                if !options.dry_run {
                    match store
                        .set_superseded(&namespace, &id, &representative.id)
                        .await
                    {
                        Ok(()) => {}
                        Err(StoreError::NotFound) => continue,
                        Err(e) => return Err(Box::new(e)),
                    }
                }
                report.tombstoned += 1
            }
        }
    }
    Ok(report)
}
fn find(parent: &mut HashMap<String, String>, id: &str) -> String {
    let value = parent.get(id).cloned().unwrap_or_else(|| id.into());
    if value == id {
        return value;
    }
    let root = find(parent, &value);
    parent.insert(id.into(), root.clone());
    root
}
fn union(parent: &mut HashMap<String, String>, a: &str, b: &str) {
    let ar = find(parent, a);
    let br = find(parent, b);
    if ar != br {
        parent.insert(ar, br);
    }
}

#[derive(Clone, Debug, Default, Serialize)]
pub struct RepairReport {
    pub restored: usize,
    pub namespaces: usize,
    pub dry_run: bool,
}
pub async fn repair_supersession(
    store: &dyn Store,
    namespaces: &[String],
    dry_run: bool,
) -> memini_store::Result<RepairReport> {
    let namespaces = if namespaces.is_empty() {
        store.list_namespaces().await?
    } else {
        namespaces.to_vec()
    };
    let mut report = RepairReport {
        dry_run,
        ..RepairReport::default()
    };
    for namespace in namespaces {
        let memories = store
            .list(
                &namespace,
                &Filter {
                    include_expired: true,
                    include_superseded: true,
                    ..Filter::default()
                },
                None,
            )
            .await?;
        let by_id: HashMap<_, _> = memories.iter().map(|v| (v.id.clone(), v.clone())).collect();
        let mut count = 0;
        for memory in &memories {
            if memory.superseded_by.is_some() && !reaches_live(memory, &by_id) {
                count += 1;
                if !dry_run {
                    match store.restore(&namespace, &memory.id).await {
                        Ok(()) | Err(StoreError::NotFound) => {}
                        Err(e) => return Err(e),
                    }
                }
            }
        }
        if count > 0 {
            report.namespaces += 1;
            report.restored += count
        }
    }
    Ok(report)
}
fn reaches_live(
    memory: &memini_core::memory::Memory,
    by_id: &HashMap<String, memini_core::memory::Memory>,
) -> bool {
    let mut current = memory;
    let mut seen = HashSet::new();
    while let Some(next) = &current.superseded_by {
        if !seen.insert(current.id.clone()) {
            return false;
        }
        let Some(value) = by_id.get(next) else {
            return false;
        };
        current = value
    }
    true
}
#[derive(Clone, Debug, Serialize)]
pub struct ScopeMerge {
    pub from: String,
    pub to: String,
    pub moved: u64,
    pub dedup_clusters: usize,
    pub dedup_tombstoned: usize,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct ScopesReport {
    pub merges: Vec<ScopeMerge>,
    pub bare_shared: Vec<String>,
    pub global_namespace_env: String,
    pub dry_run: bool,
}
pub async fn migrate_scopes(
    store: &dyn Store,
    embedder: Option<&dyn memini_embed::Embedder>,
    dry_run: bool,
    dedup_options: DedupOptions,
) -> Result<ScopesReport, Box<dyn std::error::Error + Send + Sync>> {
    let mut report = ScopesReport {
        dry_run,
        global_namespace_env: std::env::var("MEMINI_GLOBAL_NAMESPACE").unwrap_or_default(),
        ..ScopesReport::default()
    };
    let mut names = store.list_namespaces().await?;
    names.sort();
    for from in names {
        if from == "_shared" {
            report.bare_shared.push(from);
            continue;
        }
        let Some(parent) = from.strip_suffix("/_shared") else {
            continue;
        };
        let moved = move_namespace(store, &from, parent, dry_run).await?.moved;
        let mut merge = ScopeMerge {
            from: from.clone(),
            to: parent.into(),
            moved,
            dedup_clusters: 0,
            dedup_tombstoned: 0,
        };
        if !dry_run && moved > 0 {
            let embedder = embedder.ok_or("migrate scopes requires an embedder")?;
            let mut options = dedup_options.clone();
            options.namespaces = vec![parent.into()];
            options.dry_run = false;
            let result = dedup(store, embedder, options).await?;
            merge.dedup_clusters = result.clusters_found;
            merge.dedup_tombstoned = result.tombstoned;
        }
        report.merges.push(merge)
    }
    Ok(report)
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Duration;
    use memini_core::memory::Memory;
    use serde_json::Map;

    fn memory(id: &str, content: &str, tier: Tier, now: DateTime<Utc>) -> Memory {
        Memory {
            id: id.into(),
            namespace: "ns".into(),
            tier,
            level: None,
            content: content.into(),
            summary: String::new(),
            metadata: Map::new(),
            tags: vec![],
            importance: 0.1,
            created_at: now,
            updated_at: now,
            last_accessed_at: now,
            access_count: 0,
            expires_at: None,
            superseded_by: None,
            valid_from: None,
            valid_to: None,
            confidence: None,
            linked_memory_ids: vec![],
            embedding: vec![0.0, 0.0],
        }
    }
    #[tokio::test]
    async fn expiry_capacity_and_scrub_contract() {
        let dir = tempfile::tempdir().unwrap();
        let store = memini_sqlite::SqliteStore::open(dir.path().join("db"), 2).unwrap();
        let now = Utc::now();
        let mut expired = memory("expired", "old", Tier::Working, now - Duration::days(10));
        expired.expires_at = Some(now - Duration::seconds(1));
        Store::upsert(&store, &expired).await.unwrap();
        Store::upsert(
            &store,
            &memory("a", "Session ended in /tmp", Tier::Episodic, now),
        )
        .await
        .unwrap();
        Store::upsert(&store, &memory("b", "same content", Tier::Episodic, now))
            .await
            .unwrap();
        Store::upsert(
            &store,
            &memory(
                "c",
                "SAME   CONTENT",
                Tier::Episodic,
                now + Duration::seconds(1),
            ),
        )
        .await
        .unwrap();
        assert_eq!(purge_expired(&store, now).await.unwrap(), 1);
        let preview = scrub(&store, false).await.unwrap();
        assert_eq!(preview.lifecycle_noise, 1);
        assert_eq!(preview.exact_duplicates, 1);
        assert_eq!(scrub(&store, true).await.unwrap().total(), 2);
    }
}
