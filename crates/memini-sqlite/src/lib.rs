use async_trait::async_trait;
use chrono::{DateTime, TimeZone, Utc};
use memini_core::memory::{Level, Memory, Tier, fingerprint};
use memini_core::search::Scored;
use memini_store::{
    ApiKey, ApiKeyStore, Event, EventFilter, EventKind, EventLogStore, Filter, LinkStore,
    NamespaceActivity, NamespaceLink, Result, Store, StoreError, compare_memories, filter_matches,
};
use rusqlite::{Connection, OptionalExtension, Row, ffi::sqlite3_auto_extension, params};
use std::{
    collections::HashSet,
    path::Path,
    sync::{Mutex, Once},
};

const MEMORY_COLUMNS: &str = "id, namespace, tier, content, summary, metadata, tags, importance, created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by, valid_from, valid_to, confidence, level, linked_memory_ids";
static REGISTER_VEC: Once = Once::new();

pub struct SqliteStore {
    connection: Mutex<Connection>,
    dimensions: usize,
}

impl SqliteStore {
    pub fn open(path: impl AsRef<Path>, dimensions: usize) -> Result<Self> {
        if dimensions == 0 {
            return Err(StoreError::Backend(
                "sqlitevec: dims must be positive, got 0".into(),
            ));
        }
        REGISTER_VEC.call_once(|| unsafe {
            type ExtensionEntry = unsafe extern "C" fn(
                *mut rusqlite::ffi::sqlite3,
                *mut *mut std::ffi::c_char,
                *const rusqlite::ffi::sqlite3_api_routines,
            ) -> std::ffi::c_int;
            sqlite3_auto_extension(Some(std::mem::transmute::<*const (), ExtensionEntry>(
                sqlite_vec::sqlite3_vec_init as *const (),
            )));
        });
        let connection = Connection::open(path).map_err(backend)?;
        connection
            .busy_timeout(std::time::Duration::from_secs(10))
            .map_err(backend)?;
        connection
            .pragma_update(None, "journal_mode", "wal")
            .map_err(backend)?;
        connection
            .pragma_update(None, "foreign_keys", true)
            .map_err(backend)?;
        let store = Self {
            connection: Mutex::new(connection),
            dimensions,
        };
        store.migrate()?;
        Ok(store)
    }

    fn migrate(&self) -> Result<()> {
        let connection = self.connection.lock().map_err(poisoned)?;
        connection.execute_batch(&format!(r#"
CREATE TABLE IF NOT EXISTS memories (
 rowid INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, namespace TEXT NOT NULL,
 tier TEXT NOT NULL, content TEXT NOT NULL, summary TEXT NOT NULL DEFAULT '',
 metadata TEXT NOT NULL DEFAULT '{{}}', tags TEXT NOT NULL DEFAULT '[]', importance REAL NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, last_accessed_at INTEGER NOT NULL,
 access_count INTEGER NOT NULL DEFAULT 0, expires_at INTEGER, superseded_by TEXT,
 valid_from INTEGER, valid_to INTEGER, confidence REAL, fingerprint TEXT NOT NULL DEFAULT '',
 level TEXT NOT NULL DEFAULT '', linked_memory_ids TEXT NOT NULL DEFAULT '[]');
CREATE INDEX IF NOT EXISTS idx_memories_namespace ON memories(namespace);
CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at);
CREATE VIRTUAL TABLE IF NOT EXISTS vec_memories USING vec0(namespace TEXT partition key, embedding float[{}]);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_memories USING fts5(content, summary, tags, tokenize='porter unicode61');
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS namespace_links (src_ns TEXT NOT NULL, dst_ns TEXT NOT NULL, tiers TEXT NOT NULL DEFAULT '[]', note TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, PRIMARY KEY(src_ns,dst_ns));
CREATE TABLE IF NOT EXISTS api_keys (name TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, home_ns TEXT NOT NULL DEFAULT '', default_ns TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, disabled INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS memory_events (id INTEGER PRIMARY KEY, op_id TEXT NOT NULL, kind TEXT NOT NULL, namespace TEXT NOT NULL, query TEXT NOT NULL DEFAULT '', memory_id TEXT NOT NULL DEFAULT '', memory_ns TEXT NOT NULL DEFAULT '', memory_tier TEXT NOT NULL DEFAULT '', memory_summary TEXT NOT NULL DEFAULT '', rank INTEGER NOT NULL DEFAULT 0, score REAL, detail TEXT NOT NULL DEFAULT '{{}}', created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_memory_events_ns_time ON memory_events(namespace, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_memory_events_time ON memory_events(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_memory_events_memory ON memory_events(memory_id);
"#, self.dimensions)).map_err(backend)?;
        drop(connection);
        for (name, declaration) in [
            ("valid_from", "INTEGER"),
            ("valid_to", "INTEGER"),
            ("confidence", "REAL"),
            ("fingerprint", "TEXT NOT NULL DEFAULT ''"),
            ("level", "TEXT NOT NULL DEFAULT ''"),
            ("linked_memory_ids", "TEXT NOT NULL DEFAULT '[]'"),
        ] {
            self.add_column_if_missing("memories", name, declaration)?;
        }
        self.add_column_if_missing("api_keys", "default_ns", "TEXT NOT NULL DEFAULT ''")?;
        let connection = self.connection.lock().map_err(poisoned)?;
        connection.execute("CREATE INDEX IF NOT EXISTS idx_memories_fingerprint ON memories(namespace,tier,fingerprint)",[]).map_err(backend)?;
        let ddl: String = connection
            .query_row(
                "SELECT sql FROM sqlite_master WHERE type='table' AND name='vec_memories'",
                [],
                |row| row.get(0),
            )
            .map_err(backend)?;
        let actual = parse_vector_dimensions(&ddl)?;
        if actual != self.dimensions {
            return Err(StoreError::Backend(format!(
                "sqlitevec: store was created with {actual} embedding dims but is configured for {}",
                self.dimensions
            )));
        }
        Ok(())
    }

    fn add_column_if_missing(&self, table: &str, column: &str, declaration: &str) -> Result<()> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement = connection
            .prepare(&format!("PRAGMA table_info({table})"))
            .map_err(backend)?;
        let names = statement
            .query_map([], |row| row.get::<_, String>(1))
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)?;
        drop(statement);
        if !names.iter().any(|name| name == column) {
            connection
                .execute(
                    &format!("ALTER TABLE {table} ADD COLUMN {column} {declaration}"),
                    [],
                )
                .map_err(backend)?;
        }
        Ok(())
    }

    pub fn upsert(&self, memory: &Memory) -> Result<()> {
        if !memory.embedding.is_empty() && memory.embedding.len() != self.dimensions {
            return Err(StoreError::Backend(format!(
                "sqlitevec: embedding has {} dims, store expects {}",
                memory.embedding.len(),
                self.dimensions
            )));
        }
        let mut connection = self.connection.lock().map_err(poisoned)?;
        let transaction = connection.transaction().map_err(backend)?;
        let existing: Option<(i64, String)> = transaction
            .query_row(
                "SELECT rowid,namespace FROM memories WHERE id=?",
                [&memory.id],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()
            .map_err(backend)?;
        let metadata = serde_json::to_string(&memory.metadata).map_err(json_error)?;
        let tags = serde_json::to_string(&memory.tags).map_err(json_error)?;
        let linked = serde_json::to_string(&memory.linked_memory_ids).map_err(json_error)?;
        let tier = tier_text(memory.tier);
        let level = memory.level.map(level_text).unwrap_or("");
        let row_id = if let Some((row_id, namespace)) = existing {
            if namespace != memory.namespace {
                return Err(StoreError::Conflict);
            }
            transaction.execute("UPDATE memories SET tier=?,content=?,summary=?,metadata=?,tags=?,importance=?,updated_at=?,last_accessed_at=?,access_count=?,expires_at=?,superseded_by=?,valid_from=?,valid_to=?,confidence=?,fingerprint=?,level=?,linked_memory_ids=? WHERE rowid=?", params![tier,memory.content,memory.summary,metadata,tags,memory.importance,millis(memory.updated_at),millis(memory.last_accessed_at),memory.access_count,opt_millis(memory.expires_at),memory.superseded_by,opt_millis(memory.valid_from),opt_millis(memory.valid_to),memory.confidence,fingerprint(&memory.content),level,linked,row_id]).map_err(backend)?;
            row_id
        } else {
            transaction.execute("INSERT INTO memories(id,namespace,tier,content,summary,metadata,tags,importance,created_at,updated_at,last_accessed_at,access_count,expires_at,superseded_by,valid_from,valid_to,confidence,fingerprint,level,linked_memory_ids) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", params![memory.id,memory.namespace,tier,memory.content,memory.summary,metadata,tags,memory.importance,millis(memory.created_at),millis(memory.updated_at),millis(memory.last_accessed_at),memory.access_count,opt_millis(memory.expires_at),memory.superseded_by,opt_millis(memory.valid_from),opt_millis(memory.valid_to),memory.confidence,fingerprint(&memory.content),level,linked]).map_err(backend)?;
            transaction.last_insert_rowid()
        };
        transaction
            .execute("DELETE FROM vec_memories WHERE rowid=?", [row_id])
            .map_err(backend)?;
        if !memory.embedding.is_empty() {
            transaction
                .execute(
                    "INSERT INTO vec_memories(rowid,namespace,embedding) VALUES(?,?,?)",
                    params![row_id, memory.namespace, float_bytes(&memory.embedding)],
                )
                .map_err(backend)?;
        }
        transaction
            .execute("DELETE FROM fts_memories WHERE rowid=?", [row_id])
            .map_err(backend)?;
        transaction
            .execute(
                "INSERT INTO fts_memories(rowid,content,summary,tags) VALUES(?,?,?,?)",
                params![
                    row_id,
                    memory.content,
                    memory.summary,
                    memory.tags.join(" ")
                ],
            )
            .map_err(backend)?;
        transaction.commit().map_err(backend)
    }

    pub fn get(&self, namespace: &str, id: &str) -> Result<Memory> {
        self.connection
            .lock()
            .map_err(poisoned)?
            .query_row(
                &format!("SELECT {MEMORY_COLUMNS} FROM memories WHERE id=? AND namespace=?"),
                params![id, namespace],
                read_memory,
            )
            .optional()
            .map_err(backend)?
            .ok_or(StoreError::NotFound)
    }

    pub fn delete(&self, namespace: &str, id: &str) -> Result<()> {
        let mut connection = self.connection.lock().map_err(poisoned)?;
        let transaction = connection.transaction().map_err(backend)?;
        let row_id: Option<i64> = transaction
            .query_row(
                "SELECT rowid FROM memories WHERE id=? AND namespace=?",
                params![id, namespace],
                |r| r.get(0),
            )
            .optional()
            .map_err(backend)?;
        let Some(row_id) = row_id else {
            return Err(StoreError::NotFound);
        };
        transaction
            .execute("DELETE FROM memories WHERE rowid=?", [row_id])
            .map_err(backend)?;
        transaction
            .execute("DELETE FROM vec_memories WHERE rowid=?", [row_id])
            .map_err(backend)?;
        transaction
            .execute("DELETE FROM fts_memories WHERE rowid=?", [row_id])
            .map_err(backend)?;
        transaction.commit().map_err(backend)
    }

    pub fn predecessor_ids(&self, namespace: &str, id: &str) -> Result<Vec<String>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement = connection
            .prepare("SELECT id FROM memories WHERE namespace=? AND superseded_by=? ORDER BY id")
            .map_err(backend)?;
        statement
            .query_map(params![namespace, id], |row| row.get(0))
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)
    }
    pub fn get_by_fingerprint(
        &self,
        namespace: &str,
        tier: Tier,
        value: &str,
        now: Option<DateTime<Utc>>,
    ) -> Result<Memory> {
        if value.is_empty() {
            return Err(StoreError::NotFound);
        }
        let now = now.unwrap_or_else(Utc::now);
        self.connection.lock().map_err(poisoned)?.query_row(&format!("SELECT {MEMORY_COLUMNS} FROM memories WHERE namespace=? AND tier=? AND fingerprint=? AND superseded_by IS NULL AND (expires_at IS NULL OR expires_at>?) AND (valid_to IS NULL OR valid_to>?) ORDER BY created_at DESC LIMIT 1"),params![namespace,tier_text(tier),value,millis(now),millis(now)],read_memory).optional().map_err(backend)?.ok_or(StoreError::NotFound)
    }
    pub fn set_superseded(&self, namespace: &str, id: &str, replacement: &str) -> Result<()> {
        changed(self.connection.lock().map_err(poisoned)?.execute("UPDATE memories SET superseded_by=?,valid_to=COALESCE(valid_to,?) WHERE id=? AND namespace=?",params![replacement,millis(Utc::now()),id,namespace]).map_err(backend)?)
    }
    pub fn restore(&self, namespace: &str, id: &str) -> Result<()> {
        changed(self.connection.lock().map_err(poisoned)?.execute("UPDATE memories SET superseded_by=NULL,valid_to=NULL WHERE id=? AND namespace=?",params![id,namespace]).map_err(backend)?)
    }
    pub fn reinforce(
        &self,
        namespace: &str,
        ids: &[String],
        accessed_at: DateTime<Utc>,
        new_expiry: Option<DateTime<Utc>>,
    ) -> Result<()> {
        let connection = self.connection.lock().map_err(poisoned)?;
        for id in ids {
            connection.execute("UPDATE memories SET access_count=access_count+1,last_accessed_at=?,expires_at=CASE WHEN expires_at IS NOT NULL AND ? IS NOT NULL THEN ? ELSE expires_at END WHERE namespace=? AND id=?",params![millis(accessed_at),opt_millis(new_expiry),opt_millis(new_expiry),namespace,id]).map_err(backend)?;
        }
        Ok(())
    }
    pub fn delete_if_expired_before(
        &self,
        namespace: &str,
        id: &str,
        cutoff: DateTime<Utc>,
    ) -> Result<()> {
        let eligible:Option<i64>=self.connection.lock().map_err(poisoned)?.query_row("SELECT 1 FROM memories WHERE namespace=? AND id=? AND expires_at IS NOT NULL AND expires_at<=?",params![namespace,id,millis(cutoff)],|r|r.get(0)).optional().map_err(backend)?;
        if eligible.is_none() {
            return Err(StoreError::NotFound);
        }
        self.delete(namespace, id)
    }
    pub fn list(
        &self,
        namespace: &str,
        filter: &Filter,
        limit: Option<usize>,
    ) -> Result<Vec<Memory>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement = connection
            .prepare(&format!(
                "SELECT {MEMORY_COLUMNS} FROM memories WHERE namespace=?"
            ))
            .map_err(backend)?;
        let mut values = statement
            .query_map([namespace], read_memory)
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)?;
        values.retain(|memory| filter_matches(memory, filter));
        values.sort_by(|a, b| compare_memories(a, b, filter));
        if let Some(limit) = limit {
            values.truncate(limit)
        }
        Ok(values)
    }
    pub fn list_expired(&self, now: DateTime<Utc>, limit: usize) -> Result<Vec<Memory>> {
        let filter = Filter {
            include_expired: true,
            include_superseded: true,
            ..Filter::default()
        };
        let namespaces = self.list_namespaces()?;
        let mut all = Vec::new();
        for namespace in namespaces {
            all.extend(
                self.list(&namespace, &filter, None)?
                    .into_iter()
                    .filter(|m| m.expires_at.is_some_and(|v| v <= now)),
            );
        }
        if limit > 0 {
            all.truncate(limit)
        }
        Ok(all)
    }
    pub fn list_namespaces(&self) -> Result<Vec<String>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement = connection
            .prepare("SELECT DISTINCT namespace FROM memories ORDER BY namespace")
            .map_err(backend)?;
        statement
            .query_map([], |r| r.get(0))
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)
    }
    pub fn vector_search(
        &self,
        namespace: &str,
        vector: &[f32],
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>> {
        if vector.len() != self.dimensions {
            return Err(StoreError::Backend(format!(
                "sqlitevec: query vector has {} dims, store expects {}",
                vector.len(),
                self.dimensions
            )));
        }
        let connection = self.connection.lock().map_err(poisoned)?;
        let sql = format!(
            "SELECT {} ,v.distance FROM vec_memories v JOIN memories m ON m.rowid=v.rowid WHERE v.namespace=? AND v.embedding MATCH ? AND k=? ORDER BY v.distance",
            prefixed_columns()
        );
        let mut statement = connection.prepare(&sql).map_err(backend)?;
        let mut output = statement
            .query_map(
                params![namespace, float_bytes(vector), (limit.max(1) * 4) as i64],
                |row| {
                    Ok(Scored {
                        memory: read_memory(row)?,
                        score: 1.0 / (1.0 + row.get::<_, f64>(19)?),
                    })
                },
            )
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)?;
        output.retain(|v| filter_matches(&v.memory, filter));
        output.truncate(limit);
        Ok(output)
    }
    pub fn keyword_search(
        &self,
        namespace: &str,
        query: &str,
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>> {
        let query = fts_query(query);
        if query.is_empty() {
            return Ok(vec![]);
        }
        let connection = self.connection.lock().map_err(poisoned)?;
        let sql = format!(
            "SELECT {},bm25(fts_memories) FROM fts_memories JOIN memories m ON m.rowid=fts_memories.rowid WHERE fts_memories MATCH ? AND m.namespace=? ORDER BY bm25(fts_memories) LIMIT ?",
            prefixed_columns()
        );
        let mut statement = connection.prepare(&sql).map_err(backend)?;
        let mut output = statement
            .query_map(
                params![query, namespace, (limit.max(1) * 4) as i64],
                |row| {
                    Ok(Scored {
                        memory: read_memory(row)?,
                        score: -row.get::<_, f64>(19)?,
                    })
                },
            )
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)?;
        output.retain(|v| filter_matches(&v.memory, filter));
        output.truncate(limit);
        Ok(output)
    }
    pub fn namespace_activity(&self, now: Option<DateTime<Utc>>) -> Result<Vec<NamespaceActivity>> {
        let now = now.unwrap_or_else(Utc::now);
        let mut output = Vec::new();
        for namespace in self.list_namespaces()? {
            let filter = Filter {
                now: Some(now),
                ..Filter::default()
            };
            let values = self.list(&namespace, &filter, None)?;
            if let Some(last) = values.iter().map(|m| m.created_at).max() {
                output.push(NamespaceActivity {
                    namespace,
                    total: values.len(),
                    last_write: last,
                });
            }
        }
        Ok(output)
    }
    pub fn delete_namespace(&self, namespace: &str) -> Result<u64> {
        let ids = self
            .list(
                namespace,
                &Filter {
                    include_expired: true,
                    include_superseded: true,
                    ..Filter::default()
                },
                None,
            )?
            .into_iter()
            .map(|m| m.id)
            .collect::<Vec<_>>();
        for id in &ids {
            self.delete(namespace, id)?
        }
        Ok(ids.len() as u64)
    }
    pub fn reassign(&self, from: &str, ids: &[String], to: &str) -> Result<u64> {
        let mut connection = self.connection.lock().map_err(poisoned)?;
        let transaction = connection.transaction().map_err(backend)?;
        let mut count = 0;
        for id in ids {
            let row_id: Option<i64> = transaction
                .query_row(
                    "SELECT rowid FROM memories WHERE namespace=? AND id=?",
                    params![from, id],
                    |r| r.get(0),
                )
                .optional()
                .map_err(backend)?;
            if let Some(row_id) = row_id {
                let embedding: Option<Vec<u8>> = transaction
                    .query_row(
                        "SELECT embedding FROM vec_memories WHERE rowid=?",
                        [row_id],
                        |row| row.get(0),
                    )
                    .optional()
                    .map_err(backend)?;
                transaction
                    .execute("DELETE FROM vec_memories WHERE rowid=?", [row_id])
                    .map_err(backend)?;
                transaction
                    .execute(
                        "UPDATE memories SET namespace=? WHERE rowid=?",
                        params![to, row_id],
                    )
                    .map_err(backend)?;
                if let Some(embedding) = embedding {
                    transaction
                        .execute(
                            "INSERT INTO vec_memories(rowid,namespace,embedding) VALUES(?,?,?)",
                            params![row_id, to, embedding],
                        )
                        .map_err(backend)?;
                }
                count += 1;
            }
        }
        transaction.commit().map_err(backend)?;
        Ok(count)
    }
    pub fn retier(
        &self,
        namespace: &str,
        id: &str,
        tier: Tier,
        expires_at: Option<DateTime<Utc>>,
    ) -> Result<()> {
        changed(
            self.connection
                .lock()
                .map_err(poisoned)?
                .execute(
                    "UPDATE memories SET tier=?,expires_at=? WHERE namespace=? AND id=?",
                    params![tier_text(tier), opt_millis(expires_at), namespace, id],
                )
                .map_err(backend)?,
        )
    }
    pub fn set_confidence(
        &self,
        namespace: &str,
        id: &str,
        confidence: f64,
        now: DateTime<Utc>,
    ) -> Result<()> {
        changed(
            self.connection
                .lock()
                .map_err(poisoned)?
                .execute(
                    "UPDATE memories SET confidence=?,updated_at=? WHERE namespace=? AND id=?",
                    params![confidence, millis(now), namespace, id],
                )
                .map_err(backend)?,
        )
    }
    pub fn mark_contradicted(
        &self,
        namespace: &str,
        id: &str,
        contradicted_by: &str,
        confidence: f64,
        now: DateTime<Utc>,
    ) -> Result<()> {
        let mut memory = self.get(namespace, id)?;
        memory
            .metadata
            .insert("contradicted_by".into(), contradicted_by.into());
        if let Some(old) = memory.confidence {
            memory
                .metadata
                .insert("pre_contradiction_confidence".into(), old.into());
        }
        let metadata = serde_json::to_string(&memory.metadata).map_err(json_error)?;
        changed(self.connection.lock().map_err(poisoned)?.execute("UPDATE memories SET confidence=?,updated_at=?,valid_to=COALESCE(valid_to,?),metadata=? WHERE namespace=? AND id=?",params![confidence,millis(now),millis(now),metadata,namespace,id]).map_err(backend)?)
    }
    pub fn embed_model(&self) -> Result<String> {
        Ok(self
            .connection
            .lock()
            .map_err(poisoned)?
            .query_row("SELECT value FROM meta WHERE key='embed_model'", [], |r| {
                r.get(0)
            })
            .optional()
            .map_err(backend)?
            .unwrap_or_default())
    }
    pub fn set_embed_model(&self, model: &str) -> Result<()> {
        self.connection.lock().map_err(poisoned)?.execute("INSERT INTO meta(key,value) VALUES('embed_model',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",[model]).map_err(backend)?;
        Ok(())
    }
}

#[async_trait]
impl Store for SqliteStore {
    async fn upsert(&self, memory: &Memory) -> Result<()> {
        SqliteStore::upsert(self, memory)
    }
    async fn get(&self, namespace: &str, id: &str) -> Result<Memory> {
        SqliteStore::get(self, namespace, id)
    }
    async fn predecessor_ids(&self, namespace: &str, id: &str) -> Result<Vec<String>> {
        SqliteStore::predecessor_ids(self, namespace, id)
    }
    async fn get_by_fingerprint(
        &self,
        namespace: &str,
        tier: Tier,
        value: &str,
        now: Option<DateTime<Utc>>,
    ) -> Result<Memory> {
        SqliteStore::get_by_fingerprint(self, namespace, tier, value, now)
    }
    async fn delete(&self, namespace: &str, id: &str) -> Result<()> {
        SqliteStore::delete(self, namespace, id)
    }
    async fn set_superseded(&self, namespace: &str, id: &str, replacement: &str) -> Result<()> {
        SqliteStore::set_superseded(self, namespace, id, replacement)
    }
    async fn restore(&self, namespace: &str, id: &str) -> Result<()> {
        SqliteStore::restore(self, namespace, id)
    }
    async fn vector_search(
        &self,
        namespace: &str,
        vector: &[f32],
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>> {
        SqliteStore::vector_search(self, namespace, vector, filter, limit)
    }
    async fn keyword_search(
        &self,
        namespace: &str,
        query: &str,
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>> {
        SqliteStore::keyword_search(self, namespace, query, filter, limit)
    }
    async fn reinforce(
        &self,
        namespace: &str,
        ids: &[String],
        at: DateTime<Utc>,
        expiry: Option<DateTime<Utc>>,
    ) -> Result<()> {
        SqliteStore::reinforce(self, namespace, ids, at, expiry)
    }
    async fn delete_if_expired_before(
        &self,
        namespace: &str,
        id: &str,
        cutoff: DateTime<Utc>,
    ) -> Result<()> {
        SqliteStore::delete_if_expired_before(self, namespace, id, cutoff)
    }
    async fn list_expired(&self, now: DateTime<Utc>, limit: usize) -> Result<Vec<Memory>> {
        SqliteStore::list_expired(self, now, limit)
    }
    async fn list(
        &self,
        namespace: &str,
        filter: &Filter,
        limit: Option<usize>,
    ) -> Result<Vec<Memory>> {
        SqliteStore::list(self, namespace, filter, limit)
    }
    async fn list_namespaces(&self) -> Result<Vec<String>> {
        SqliteStore::list_namespaces(self)
    }
    async fn namespace_activity(
        &self,
        now: Option<DateTime<Utc>>,
    ) -> Result<Vec<NamespaceActivity>> {
        SqliteStore::namespace_activity(self, now)
    }
    async fn delete_namespace(&self, namespace: &str) -> Result<u64> {
        SqliteStore::delete_namespace(self, namespace)
    }
    async fn reassign(&self, from: &str, ids: &[String], to: &str) -> Result<u64> {
        SqliteStore::reassign(self, from, ids, to)
    }
    async fn retier(
        &self,
        namespace: &str,
        id: &str,
        tier: Tier,
        expiry: Option<DateTime<Utc>>,
    ) -> Result<()> {
        SqliteStore::retier(self, namespace, id, tier, expiry)
    }
    async fn set_confidence(
        &self,
        namespace: &str,
        id: &str,
        value: f64,
        now: DateTime<Utc>,
    ) -> Result<()> {
        SqliteStore::set_confidence(self, namespace, id, value, now)
    }
    async fn mark_contradicted(
        &self,
        namespace: &str,
        id: &str,
        by: &str,
        value: f64,
        now: DateTime<Utc>,
    ) -> Result<()> {
        SqliteStore::mark_contradicted(self, namespace, id, by, value, now)
    }
    async fn embed_model(&self) -> Result<String> {
        SqliteStore::embed_model(self)
    }
    async fn set_embed_model(&self, model: &str) -> Result<()> {
        SqliteStore::set_embed_model(self, model)
    }
    async fn ping(&self) -> Result<()> {
        self.connection
            .lock()
            .map_err(poisoned)?
            .query_row("SELECT 1", [], |r| r.get::<_, i64>(0))
            .map_err(backend)?;
        Ok(())
    }
}

#[async_trait]
impl LinkStore for SqliteStore {
    async fn put_link(&self, link: &NamespaceLink) -> Result<()> {
        let tiers =
            serde_json::to_string(&link.tiers.iter().map(|v| tier_text(*v)).collect::<Vec<_>>())
                .map_err(json_error)?;
        self.connection.lock().map_err(poisoned)?.execute("INSERT INTO namespace_links(src_ns,dst_ns,tiers,note,created_at) VALUES(?,?,?,?,?) ON CONFLICT(src_ns,dst_ns) DO UPDATE SET tiers=excluded.tiers,note=excluded.note,created_at=excluded.created_at",params![link.source,link.destination,tiers,link.note,link.created_at.to_rfc3339()]).map_err(backend)?;
        Ok(())
    }
    async fn delete_link(&self, source: &str, destination: &str) -> Result<bool> {
        Ok(self
            .connection
            .lock()
            .map_err(poisoned)?
            .execute(
                "DELETE FROM namespace_links WHERE src_ns=? AND dst_ns=?",
                params![source, destination],
            )
            .map_err(backend)?
            > 0)
    }
    async fn list_links(&self, source: &str) -> Result<Vec<NamespaceLink>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        read_links(
            &connection,
            "SELECT src_ns,dst_ns,tiers,note,created_at FROM namespace_links WHERE src_ns=? ORDER BY dst_ns",
            [source],
        )
    }
    async fn list_all_links(&self) -> Result<Vec<NamespaceLink>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement=connection.prepare("SELECT src_ns,dst_ns,tiers,note,created_at FROM namespace_links ORDER BY src_ns,dst_ns").map_err(backend)?;
        statement
            .query_map([], read_link)
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)
    }
    async fn rename_link_endpoints(&self, from: &str, to: &str) -> Result<()> {
        if from == to {
            return Ok(());
        }
        let links = self.list_all_links().await?;
        for mut link in links
            .into_iter()
            .filter(|v| v.source == from || v.destination == from)
        {
            self.delete_link(&link.source, &link.destination).await?;
            if link.source == from {
                link.source = to.into()
            }
            if link.destination == from {
                link.destination = to.into()
            }
            let _ = self.put_link_if_absent(&link);
        }
        Ok(())
    }
}

impl SqliteStore {
    fn put_link_if_absent(&self, link: &NamespaceLink) -> Result<()> {
        let tiers =
            serde_json::to_string(&link.tiers.iter().map(|v| tier_text(*v)).collect::<Vec<_>>())
                .map_err(json_error)?;
        self.connection.lock().map_err(poisoned)?.execute("INSERT OR IGNORE INTO namespace_links(src_ns,dst_ns,tiers,note,created_at) VALUES(?,?,?,?,?)",params![link.source,link.destination,tiers,link.note,link.created_at.to_rfc3339()]).map_err(backend)?;
        Ok(())
    }
}

#[async_trait]
impl ApiKeyStore for SqliteStore {
    async fn put_api_key(&self, key: &ApiKey) -> Result<()> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let existing: Option<String> = connection
            .query_row(
                "SELECT created_at FROM api_keys WHERE name=?",
                [&key.name],
                |r| r.get(0),
            )
            .optional()
            .map_err(backend)?;
        let created = key
            .created_at
            .map(|v| v.to_rfc3339())
            .or(existing)
            .unwrap_or_else(|| Utc::now().to_rfc3339());
        connection.execute("INSERT INTO api_keys(name,key_hash,home_ns,default_ns,created_at,disabled) VALUES(?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET key_hash=excluded.key_hash,home_ns=excluded.home_ns,default_ns=excluded.default_ns,created_at=excluded.created_at,disabled=excluded.disabled",params![key.name,key.hash,key.home_namespace,key.default_namespace,created,key.disabled as i64]).map_err(backend)?;
        Ok(())
    }
    async fn delete_api_key(&self, name: &str) -> Result<bool> {
        Ok(self
            .connection
            .lock()
            .map_err(poisoned)?
            .execute("DELETE FROM api_keys WHERE name=?", [name])
            .map_err(backend)?
            > 0)
    }
    async fn list_api_keys(&self) -> Result<Vec<ApiKey>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement=connection.prepare("SELECT name,key_hash,home_ns,default_ns,created_at,disabled FROM api_keys ORDER BY name").map_err(backend)?;
        statement
            .query_map([], read_api_key)
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)
    }
    async fn get_api_key_by_hash(&self, hash: &str) -> Result<Option<ApiKey>> {
        self.connection.lock().map_err(poisoned)?.query_row("SELECT name,key_hash,home_ns,default_ns,created_at,disabled FROM api_keys WHERE key_hash=?",[hash],read_api_key).optional().map_err(backend)
    }
    async fn rename_api_key_namespaces(&self, from: &str, to: &str) -> Result<()> {
        self.connection.lock().map_err(poisoned)?.execute("UPDATE api_keys SET home_ns=CASE WHEN home_ns=? THEN ? ELSE home_ns END,default_ns=CASE WHEN default_ns=? THEN ? ELSE default_ns END WHERE home_ns=? OR default_ns=?",params![from,to,from,to,from,from]).map_err(backend)?;
        Ok(())
    }
}

#[async_trait]
impl EventLogStore for SqliteStore {
    async fn append_events(&self, events: &[Event]) -> Result<()> {
        let mut connection = self.connection.lock().map_err(poisoned)?;
        let transaction = connection.transaction().map_err(backend)?;
        for event in events {
            transaction.execute("INSERT INTO memory_events(op_id,kind,namespace,query,memory_id,memory_ns,memory_tier,memory_summary,rank,score,detail,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",params![event.operation_id,event_kind_text(event.kind),event.namespace,event.query,event.memory_id,event.memory_namespace,tier_text(event.memory_tier),event.memory_summary,event.rank as i64,event.score,serde_json::to_string(&event.detail).map_err(json_error)?,millis(event.created_at)]).map_err(backend)?;
        }
        transaction.commit().map_err(backend)
    }
    async fn list_events(&self, filter: &EventFilter) -> Result<Vec<Event>> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut statement=connection.prepare("SELECT id,op_id,kind,namespace,query,memory_id,memory_ns,memory_tier,memory_summary,rank,score,detail,created_at FROM memory_events ORDER BY created_at DESC,id DESC").map_err(backend)?;
        let mut events = statement
            .query_map([], read_event)
            .map_err(backend)?
            .collect::<rusqlite::Result<Vec<_>>>()
            .map_err(backend)?;
        events.retain(|e| event_matches(e, filter));
        if !filter.tiers.is_empty() || !filter.text.is_empty() {
            let needle = filter.text.to_lowercase();
            let operations: HashSet<_> = events
                .iter()
                .filter(|e| {
                    (filter.tiers.is_empty() || filter.tiers.contains(&e.memory_tier))
                        && (needle.is_empty()
                            || e.query.to_lowercase().contains(&needle)
                            || e.memory_summary.to_lowercase().contains(&needle))
                })
                .map(|e| e.operation_id.clone())
                .collect();
            events.retain(|e| operations.contains(&e.operation_id));
        }
        if let Some(limit) = filter.limit {
            events.truncate(limit)
        }
        Ok(events)
    }
    async fn prune_events(
        &self,
        older_than: Option<DateTime<Utc>>,
        keep_max: Option<usize>,
    ) -> Result<u64> {
        let connection = self.connection.lock().map_err(poisoned)?;
        let mut count = 0;
        if let Some(before) = older_than {
            count += connection
                .execute(
                    "DELETE FROM memory_events WHERE created_at<?",
                    [millis(before)],
                )
                .map_err(backend)?;
        }
        if let Some(max) = keep_max.filter(|v| *v > 0) {
            count+=connection.execute("DELETE FROM memory_events WHERE id NOT IN (SELECT id FROM memory_events ORDER BY created_at DESC,id DESC LIMIT ?)",[max as i64]).map_err(backend)?;
        }
        Ok(count as u64)
    }
}

fn changed(count: usize) -> Result<()> {
    if count == 0 {
        Err(StoreError::NotFound)
    } else {
        Ok(())
    }
}
fn prefixed_columns() -> String {
    MEMORY_COLUMNS
        .split(',')
        .map(|v| format!("m.{}", v.trim()))
        .collect::<Vec<_>>()
        .join(",")
}
fn fts_query(value: &str) -> String {
    value
        .to_lowercase()
        .split(|c: char| !c.is_ascii_alphanumeric())
        .filter(|v| v.len() >= 2)
        .map(|v| format!("\"{v}\""))
        .collect::<Vec<_>>()
        .join(" OR ")
}
fn read_links(connection: &Connection, sql: &str, params: [&str; 1]) -> Result<Vec<NamespaceLink>> {
    let mut statement = connection.prepare(sql).map_err(backend)?;
    statement
        .query_map(params, read_link)
        .map_err(backend)?
        .collect::<rusqlite::Result<Vec<_>>>()
        .map_err(backend)
}
fn read_link(row: &Row<'_>) -> rusqlite::Result<NamespaceLink> {
    let raw: String = row.get(2)?;
    let tiers = serde_json::from_str::<Vec<String>>(&raw)
        .unwrap_or_default()
        .into_iter()
        .map(|v| parse_tier(&v))
        .collect::<rusqlite::Result<Vec<_>>>()?;
    let created: String = row.get(4)?;
    Ok(NamespaceLink {
        source: row.get(0)?,
        destination: row.get(1)?,
        tiers,
        note: row.get(3)?,
        created_at: DateTime::parse_from_rfc3339(&created)
            .map_err(|_| rusqlite::Error::InvalidQuery)?
            .with_timezone(&Utc),
    })
}
fn read_api_key(row: &Row<'_>) -> rusqlite::Result<ApiKey> {
    let created: String = row.get(4)?;
    Ok(ApiKey {
        name: row.get(0)?,
        hash: row.get(1)?,
        home_namespace: row.get(2)?,
        default_namespace: row.get(3)?,
        created_at: Some(
            DateTime::parse_from_rfc3339(&created)
                .map_err(|_| rusqlite::Error::InvalidQuery)?
                .with_timezone(&Utc),
        ),
        disabled: row.get::<_, i64>(5)? != 0,
    })
}
fn event_kind_text(value: EventKind) -> &'static str {
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
fn parse_event_kind(value: &str) -> rusqlite::Result<EventKind> {
    match value {
        "recall" => Ok(EventKind::Recall),
        "get" => Ok(EventKind::Get),
        "briefing" => Ok(EventKind::Briefing),
        "remember" => Ok(EventKind::Remember),
        "update" => Ok(EventKind::Update),
        "forget" => Ok(EventKind::Forget),
        "supersede" => Ok(EventKind::Supersede),
        _ => Err(rusqlite::Error::InvalidQuery),
    }
}
fn read_event(row: &Row<'_>) -> rusqlite::Result<Event> {
    let kind: String = row.get(2)?;
    let tier: String = row.get(7)?;
    Ok(Event {
        id: row.get(0)?,
        operation_id: row.get(1)?,
        kind: parse_event_kind(&kind)?,
        namespace: row.get(3)?,
        query: row.get(4)?,
        memory_id: row.get(5)?,
        memory_namespace: row.get(6)?,
        memory_tier: parse_tier(&tier)?,
        memory_summary: row.get(8)?,
        rank: row.get::<_, i64>(9)? as usize,
        score: row.get(10)?,
        detail: serde_json::from_str(&row.get::<_, String>(11)?).unwrap_or_default(),
        created_at: timestamp(row.get(12)?)?,
    })
}
fn event_matches(event: &Event, filter: &EventFilter) -> bool {
    if !filter.namespace.is_empty() && event.namespace != filter.namespace {
        return false;
    }
    if filter.namespace.is_empty()
        && !filter.namespaces.is_empty()
        && !filter.namespaces.contains(&event.namespace)
    {
        return false;
    }
    if !filter.kinds.is_empty() && !filter.kinds.contains(&event.kind) {
        return false;
    }
    if filter.since.is_some_and(|v| event.created_at < v) {
        return false;
    }
    if let Some(before) = filter.before
        && (event.created_at > before
            || (event.created_at == before && event.id >= filter.before_id))
    {
        return false;
    }
    true
}

fn parse_vector_dimensions(ddl: &str) -> Result<usize> {
    let tail = ddl.split("float[").nth(1).ok_or_else(|| {
        StoreError::Backend(
            "sqlitevec: cannot find embedding dimension in vec_memories schema".into(),
        )
    })?;
    tail.split(']')
        .next()
        .unwrap_or_default()
        .trim()
        .parse()
        .map_err(|e| StoreError::Backend(format!("sqlitevec: parse vec_memories dimension: {e}")))
}
fn read_memory(row: &Row<'_>) -> rusqlite::Result<Memory> {
    let tier: String = row.get(2)?;
    let level: String = row.get(17)?;
    Ok(Memory {
        id: row.get(0)?,
        namespace: row.get(1)?,
        tier: parse_tier(&tier)?,
        level: parse_level(&level),
        content: row.get(3)?,
        summary: row.get(4)?,
        metadata: serde_json::from_str(&row.get::<_, String>(5)?).unwrap_or_default(),
        tags: serde_json::from_str(&row.get::<_, String>(6)?).unwrap_or_default(),
        importance: row.get(7)?,
        created_at: timestamp(row.get(8)?)?,
        updated_at: timestamp(row.get(9)?)?,
        last_accessed_at: timestamp(row.get(10)?)?,
        access_count: row.get(11)?,
        expires_at: row.get::<_, Option<i64>>(12)?.map(timestamp).transpose()?,
        superseded_by: row.get(13)?,
        valid_from: row.get::<_, Option<i64>>(14)?.map(timestamp).transpose()?,
        valid_to: row.get::<_, Option<i64>>(15)?.map(timestamp).transpose()?,
        confidence: row.get(16)?,
        linked_memory_ids: serde_json::from_str(&row.get::<_, String>(18)?).unwrap_or_default(),
        embedding: vec![],
    })
}
fn timestamp(value: i64) -> rusqlite::Result<DateTime<Utc>> {
    Utc.timestamp_millis_opt(value)
        .single()
        .ok_or_else(|| rusqlite::Error::IntegralValueOutOfRange(0, value))
}
fn millis(value: DateTime<Utc>) -> i64 {
    value.timestamp_millis()
}
fn opt_millis(value: Option<DateTime<Utc>>) -> Option<i64> {
    value.map(millis)
}
fn tier_text(value: Tier) -> &'static str {
    match value {
        Tier::Working => "working",
        Tier::Episodic => "episodic",
        Tier::Semantic => "semantic",
        Tier::Procedural => "procedural",
    }
}
fn level_text(value: Level) -> &'static str {
    match value {
        Level::Explicit => "explicit",
        Level::Deduced => "deduced",
    }
}
fn parse_tier(value: &str) -> rusqlite::Result<Tier> {
    match value {
        "working" => Ok(Tier::Working),
        "episodic" => Ok(Tier::Episodic),
        "semantic" => Ok(Tier::Semantic),
        "procedural" => Ok(Tier::Procedural),
        _ => Err(rusqlite::Error::InvalidQuery),
    }
}
fn parse_level(value: &str) -> Option<Level> {
    match value {
        "explicit" => Some(Level::Explicit),
        "deduced" => Some(Level::Deduced),
        _ => None,
    }
}
fn float_bytes(values: &[f32]) -> Vec<u8> {
    values.iter().flat_map(|v| v.to_le_bytes()).collect()
}
fn backend(error: rusqlite::Error) -> StoreError {
    StoreError::Backend(format!("sqlitevec: {error}"))
}
fn json_error(error: serde_json::Error) -> StoreError {
    StoreError::Backend(format!("sqlitevec: {error}"))
}
fn poisoned<T>(error: std::sync::PoisonError<T>) -> StoreError {
    StoreError::Backend(format!("sqlitevec: lock poisoned: {error}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Map;
    fn memory(id: &str, namespace: &str, embedding: Vec<f32>) -> Memory {
        let now = Utc::now();
        Memory {
            id: id.into(),
            namespace: namespace.into(),
            tier: Tier::Semantic,
            level: Some(Level::Explicit),
            content: "The sky is blue".into(),
            summary: "sky".into(),
            metadata: Map::new(),
            tags: vec!["fact".into()],
            importance: 0.8,
            created_at: now,
            updated_at: now,
            last_accessed_at: now,
            access_count: 0,
            expires_at: None,
            superseded_by: None,
            valid_from: None,
            valid_to: None,
            confidence: Some(0.4),
            linked_memory_ids: vec![],
            embedding,
        }
    }
    #[test]
    fn opens_schema_and_round_trips() {
        let dir = tempfile::tempdir().unwrap();
        let store = SqliteStore::open(dir.path().join("memory.db"), 3).unwrap();
        let item = memory("one", "project", vec![0.1, 0.2, 0.3]);
        store.upsert(&item).unwrap();
        let got = store.get("project", "one").unwrap();
        assert_eq!(got.id, "one");
        assert_eq!(got.content, item.content);
        assert!(got.embedding.is_empty());
        store.delete("project", "one").unwrap();
        assert!(matches!(
            store.get("project", "one"),
            Err(StoreError::NotFound)
        ));
    }
    #[test]
    fn rejects_dimension_and_namespace_conflicts() {
        let store = SqliteStore::open(":memory:", 3).unwrap();
        assert!(store.upsert(&memory("bad", "a", vec![1.0])).is_err());
        store.upsert(&memory("same", "a", vec![])).unwrap();
        assert!(matches!(
            store.upsert(&memory("same", "b", vec![])),
            Err(StoreError::Conflict)
        ));
    }
    #[test]
    fn detects_existing_dimension_mismatch() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("memory.db");
        drop(SqliteStore::open(&path, 3).unwrap());
        assert!(SqliteStore::open(&path, 4).is_err());
    }
    #[test]
    fn migrates_legacy_memory_and_key_columns() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("legacy.db");
        let connection = Connection::open(&path).unwrap();
        connection.execute_batch("CREATE TABLE memories(rowid INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE,namespace TEXT NOT NULL,tier TEXT NOT NULL,content TEXT NOT NULL,summary TEXT NOT NULL DEFAULT '',metadata TEXT NOT NULL DEFAULT '{}',tags TEXT NOT NULL DEFAULT '[]',importance REAL NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,last_accessed_at INTEGER NOT NULL,access_count INTEGER NOT NULL DEFAULT 0,expires_at INTEGER,superseded_by TEXT);CREATE TABLE api_keys(name TEXT PRIMARY KEY,key_hash TEXT NOT NULL UNIQUE,home_ns TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,disabled INTEGER NOT NULL DEFAULT 0);").unwrap();
        drop(connection);
        let store = SqliteStore::open(&path, 3).unwrap();
        let connection = store.connection.lock().unwrap();
        let memory_columns: Vec<String> = connection
            .prepare("PRAGMA table_info(memories)")
            .unwrap()
            .query_map([], |row| row.get(1))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap();
        for expected in [
            "valid_from",
            "valid_to",
            "confidence",
            "fingerprint",
            "level",
            "linked_memory_ids",
        ] {
            assert!(memory_columns.contains(&expected.into()));
        }
        let key_columns: Vec<String> = connection
            .prepare("PRAGMA table_info(api_keys)")
            .unwrap()
            .query_map([], |row| row.get(1))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap();
        assert!(key_columns.contains(&"default_ns".into()));
    }
    #[test]
    fn search_filter_and_lifecycle_contract() {
        let store = SqliteStore::open(":memory:", 3).unwrap();
        let mut first = memory("first", "project", vec![1.0, 0.0, 0.0]);
        first.tags.push("blue".into());
        first.metadata.insert("memory_type".into(), "fact".into());
        let mut second = memory("second", "project", vec![0.0, 1.0, 0.0]);
        second.content = "Grass is green".into();
        second.summary = "grass".into();
        second.tier = Tier::Episodic;
        store.upsert(&first).unwrap();
        store.upsert(&second).unwrap();
        let filter = Filter {
            tiers: vec![Tier::Semantic],
            tags: vec!["blue".into()],
            memory_types: vec!["fact".into()],
            ..Filter::default()
        };
        assert_eq!(store.list("project", &filter, None).unwrap().len(), 1);
        assert_eq!(
            store
                .vector_search("project", &[1.0, 0.0, 0.0], &filter, 5)
                .unwrap()[0]
                .memory
                .id,
            "first"
        );
        assert_eq!(
            store
                .keyword_search("project", "sky", &Filter::default(), 5)
                .unwrap()[0]
                .memory
                .id,
            "first"
        );
        store
            .reinforce("project", &["first".into()], Utc::now(), None)
            .unwrap();
        assert_eq!(store.get("project", "first").unwrap().access_count, 1);
        store.set_superseded("project", "first", "second").unwrap();
        assert!(
            store
                .list("project", &Filter::default(), None)
                .unwrap()
                .iter()
                .all(|m| m.id != "first")
        );
        store.restore("project", "first").unwrap();
        assert!(
            store
                .list("project", &Filter::default(), None)
                .unwrap()
                .iter()
                .any(|m| m.id == "first")
        );
    }
    #[test]
    fn namespace_confidence_and_model_contract() {
        let store = SqliteStore::open(":memory:", 3).unwrap();
        store.upsert(&memory("one", "from", vec![])).unwrap();
        store.upsert(&memory("two", "from", vec![])).unwrap();
        assert_eq!(store.list_namespaces().unwrap(), vec!["from"]);
        assert_eq!(store.reassign("from", &["one".into()], "to").unwrap(), 1);
        assert_eq!(store.get("to", "one").unwrap().id, "one");
        let now = Utc::now();
        store.set_confidence("to", "one", 0.8, now).unwrap();
        assert_eq!(store.get("to", "one").unwrap().confidence, Some(0.8));
        store
            .mark_contradicted("to", "one", "new", 0.1, now)
            .unwrap();
        assert!(store.get("to", "one").unwrap().valid_to.is_some());
        assert_eq!(store.embed_model().unwrap(), "");
        store.set_embed_model("model-a").unwrap();
        assert_eq!(store.embed_model().unwrap(), "model-a");
        assert_eq!(store.delete_namespace("from").unwrap(), 1);
    }
    #[test]
    fn links_keys_and_events_contract() {
        pollster::block_on(async {
            let store = SqliteStore::open(":memory:", 3).unwrap();
            let now = Utc::now();
            let link = NamespaceLink {
                source: "a".into(),
                destination: "b".into(),
                tiers: vec![Tier::Semantic],
                note: "shared".into(),
                created_at: now,
            };
            store.put_link(&link).await.unwrap();
            assert_eq!(store.list_links("a").await.unwrap()[0].destination, "b");
            store.rename_link_endpoints("a", "c").await.unwrap();
            assert_eq!(store.list_links("c").await.unwrap().len(), 1);
            let key = ApiKey {
                name: "agent".into(),
                hash: "abc".into(),
                home_namespace: "home".into(),
                default_namespace: "project".into(),
                created_at: Some(now),
                disabled: false,
            };
            store.put_api_key(&key).await.unwrap();
            let rotated = ApiKey {
                hash: "def".into(),
                created_at: None,
                ..key.clone()
            };
            store.put_api_key(&rotated).await.unwrap();
            let got = store.get_api_key_by_hash("def").await.unwrap().unwrap();
            assert_eq!(got.created_at, Some(now));
            store
                .rename_api_key_namespaces("home", "personal")
                .await
                .unwrap();
            assert_eq!(
                store.list_api_keys().await.unwrap()[0].home_namespace,
                "personal"
            );
            let event = Event {
                id: 0,
                operation_id: "op".into(),
                kind: EventKind::Recall,
                namespace: "project".into(),
                query: "sky".into(),
                memory_id: "one".into(),
                memory_namespace: "project".into(),
                memory_tier: Tier::Semantic,
                memory_summary: "blue sky".into(),
                rank: 1,
                score: Some(0.9),
                detail: Map::new(),
                created_at: now,
            };
            store.append_events(&[event]).await.unwrap();
            assert_eq!(
                store
                    .list_events(&EventFilter {
                        tiers: vec![Tier::Semantic],
                        text: "sky".into(),
                        ..EventFilter::default()
                    })
                    .await
                    .unwrap()
                    .len(),
                1
            );
            assert_eq!(store.prune_events(None, Some(0)).await.unwrap(), 0);
            assert_eq!(
                store
                    .prune_events(Some(now + chrono::Duration::milliseconds(1)), None)
                    .await
                    .unwrap(),
                1
            );
        })
    }
}
