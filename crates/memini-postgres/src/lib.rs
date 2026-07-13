use async_trait::async_trait;
use chrono::{DateTime, Utc};
use memini_core::{
    memory::{Level, Memory, Tier, fingerprint},
    search::Scored,
};
use memini_store::{
    ApiKey, ApiKeyStore, Event, EventFilter, EventKind, EventLogStore, Filter, LinkStore,
    NamespaceActivity, NamespaceLink, Result, Store, StoreError, compare_memories, filter_matches,
};
use pgvector::Vector;
use serde_json::Value;
use tokio::sync::Mutex;
use tokio_postgres::{Client, NoTls, Row};

const COLUMNS: &str = "id,namespace,tier,content,summary,metadata,tags,importance,created_at,updated_at,last_accessed_at,access_count,expires_at,superseded_by,valid_from,valid_to,confidence,level,linked_memory_ids";
pub struct PostgresStore {
    client: Mutex<Client>,
    dimensions: usize,
}

impl PostgresStore {
    pub async fn open(dsn: &str, dimensions: usize) -> Result<Self> {
        if dimensions == 0 {
            return Err(StoreError::Backend(
                "postgres: dims must be positive, got 0".into(),
            ));
        }
        let (client, connection) = tokio_postgres::connect(dsn, NoTls).await.map_err(pg)?;
        tokio::spawn(async move {
            let _ = connection.await;
        });
        let store = Self {
            client: Mutex::new(client),
            dimensions,
        };
        store.migrate().await?;
        Ok(store)
    }
    async fn migrate(&self) -> Result<()> {
        let client = self.client.lock().await;
        let statements = [
            "CREATE EXTENSION IF NOT EXISTS vchord CASCADE",
            "CREATE OR REPLACE FUNCTION memini_tags_to_text(text[]) RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$ SELECT array_to_string($1, ' ') $$",
            &format!(
                "CREATE TABLE IF NOT EXISTS memories(id text PRIMARY KEY,namespace text NOT NULL,tier text NOT NULL,content text NOT NULL,summary text NOT NULL DEFAULT '',metadata jsonb NOT NULL DEFAULT '{{}}',tags text[] NOT NULL DEFAULT '{{}}',importance double precision NOT NULL DEFAULT 0,created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,last_accessed_at timestamptz NOT NULL,access_count integer NOT NULL DEFAULT 0,expires_at timestamptz,superseded_by text,valid_from timestamptz,valid_to timestamptz,confidence double precision,fingerprint text NOT NULL DEFAULT '',level text NOT NULL DEFAULT '',linked_memory_ids text NOT NULL DEFAULT '[]',embedding vector({}),fts tsvector GENERATED ALWAYS AS(to_tsvector('english',content||' '||summary||' '||memini_tags_to_text(tags))) STORED)",
                self.dimensions
            ),
            "ALTER TABLE memories ADD COLUMN IF NOT EXISTS valid_from timestamptz",
            "ALTER TABLE memories ADD COLUMN IF NOT EXISTS valid_to timestamptz",
            "ALTER TABLE memories ADD COLUMN IF NOT EXISTS confidence double precision",
            "ALTER TABLE memories ADD COLUMN IF NOT EXISTS fingerprint text NOT NULL DEFAULT ''",
            "ALTER TABLE memories ADD COLUMN IF NOT EXISTS level text NOT NULL DEFAULT ''",
            "ALTER TABLE memories ALTER COLUMN embedding DROP NOT NULL",
            "ALTER TABLE memories ADD COLUMN IF NOT EXISTS linked_memory_ids text NOT NULL DEFAULT '[]'",
            "CREATE INDEX IF NOT EXISTS idx_memories_namespace ON memories(namespace)",
            "CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at)",
            "CREATE INDEX IF NOT EXISTS idx_memories_fts ON memories USING gin(fts)",
            "CREATE INDEX IF NOT EXISTS idx_memories_vec ON memories USING vchordrq (embedding vector_l2_ops)",
            "CREATE INDEX IF NOT EXISTS idx_memories_fingerprint ON memories(namespace,tier,fingerprint)",
            "CREATE TABLE IF NOT EXISTS meta(key text PRIMARY KEY,value text NOT NULL)",
            "CREATE TABLE IF NOT EXISTS namespace_links(src_ns text NOT NULL,dst_ns text NOT NULL,tiers jsonb NOT NULL DEFAULT '[]',note text NOT NULL DEFAULT '',created_at timestamptz NOT NULL,PRIMARY KEY(src_ns,dst_ns))",
            "CREATE TABLE IF NOT EXISTS api_keys(name text PRIMARY KEY,key_hash text NOT NULL UNIQUE,home_ns text NOT NULL DEFAULT '',default_ns text NOT NULL DEFAULT '',created_at timestamptz NOT NULL,disabled boolean NOT NULL DEFAULT false)",
            "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS default_ns text NOT NULL DEFAULT ''",
            "CREATE TABLE IF NOT EXISTS memory_events(id bigserial PRIMARY KEY,op_id text NOT NULL,kind text NOT NULL,namespace text NOT NULL,query text NOT NULL DEFAULT '',memory_id text NOT NULL DEFAULT '',memory_ns text NOT NULL DEFAULT '',memory_tier text NOT NULL DEFAULT '',memory_summary text NOT NULL DEFAULT '',rank integer NOT NULL DEFAULT 0,score double precision,detail jsonb NOT NULL DEFAULT '{}',created_at timestamptz NOT NULL)",
            "CREATE INDEX IF NOT EXISTS idx_memory_events_ns_time ON memory_events(namespace,created_at DESC,id DESC)",
            "CREATE INDEX IF NOT EXISTS idx_memory_events_time ON memory_events(created_at DESC,id DESC)",
            "CREATE INDEX IF NOT EXISTS idx_memory_events_memory ON memory_events(memory_id)",
        ];
        for statement in statements {
            client.batch_execute(statement).await.map_err(pg)?;
        }
        let vector_type:String=client.query_one("SELECT format_type(a.atttypid,a.atttypmod) FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid WHERE c.relname='memories' AND a.attname='embedding' AND a.attnum>0",&[]).await.map_err(pg)?.get(0);
        let actual = parse_vector_dimensions(&vector_type)?;
        if actual != self.dimensions {
            return Err(StoreError::Backend(format!(
                "postgres: store was created with {actual} embedding dims but is configured for {}; set MEMINI_EMBED_DIMS={actual} to match the existing data, or migrate to a new database",
                self.dimensions
            )));
        }
        Ok(())
    }
    async fn all(&self, namespace: &str) -> Result<Vec<Memory>> {
        let rows = self
            .client
            .lock()
            .await
            .query(
                &format!("SELECT {COLUMNS} FROM memories WHERE namespace=$1"),
                &[&namespace],
            )
            .await
            .map_err(pg)?;
        rows.iter().map(read_memory).collect()
    }
}

#[async_trait]
impl Store for PostgresStore {
    async fn upsert(&self, m: &Memory) -> Result<()> {
        if !m.embedding.is_empty() && m.embedding.len() != self.dimensions {
            return Err(StoreError::Backend(format!(
                "postgres: embedding has {} dims, store expects {}",
                m.embedding.len(),
                self.dimensions
            )));
        }
        let vector = (!m.embedding.is_empty()).then(|| Vector::from(m.embedding.clone()));
        let metadata = Value::Object(m.metadata.clone());
        let linked = serde_json::to_string(&m.linked_memory_ids).map_err(json)?;
        let tier = tier_text(m.tier);
        let level = m.level.map(level_text).unwrap_or("");
        let rows=self.client.lock().await.query("INSERT INTO memories(id,namespace,tier,content,summary,metadata,tags,importance,created_at,updated_at,last_accessed_at,access_count,expires_at,superseded_by,valid_from,valid_to,confidence,fingerprint,level,linked_memory_ids,embedding) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) ON CONFLICT(id) DO UPDATE SET tier=excluded.tier,content=excluded.content,summary=excluded.summary,metadata=excluded.metadata,tags=excluded.tags,importance=excluded.importance,updated_at=excluded.updated_at,last_accessed_at=excluded.last_accessed_at,access_count=excluded.access_count,expires_at=excluded.expires_at,superseded_by=excluded.superseded_by,valid_from=excluded.valid_from,valid_to=excluded.valid_to,confidence=excluded.confidence,fingerprint=excluded.fingerprint,level=excluded.level,linked_memory_ids=excluded.linked_memory_ids,embedding=excluded.embedding WHERE memories.namespace=excluded.namespace RETURNING id",&[&m.id,&m.namespace,&tier,&m.content,&m.summary,&metadata,&m.tags,&m.importance,&m.created_at,&m.updated_at,&m.last_accessed_at,&(m.access_count as i32),&m.expires_at,&m.superseded_by,&m.valid_from,&m.valid_to,&m.confidence,&fingerprint(&m.content),&level,&linked,&vector]).await.map_err(pg)?;
        if rows.is_empty() {
            Err(StoreError::Conflict)
        } else {
            Ok(())
        }
    }
    async fn get(&self, namespace: &str, id: &str) -> Result<Memory> {
        self.client
            .lock()
            .await
            .query_opt(
                &format!("SELECT {COLUMNS} FROM memories WHERE namespace=$1 AND id=$2"),
                &[&namespace, &id],
            )
            .await
            .map_err(pg)?
            .as_ref()
            .map(read_memory)
            .transpose()?
            .ok_or(StoreError::NotFound)
    }
    async fn predecessor_ids(&self, namespace: &str, id: &str) -> Result<Vec<String>> {
        Ok(self
            .client
            .lock()
            .await
            .query(
                "SELECT id FROM memories WHERE namespace=$1 AND superseded_by=$2 ORDER BY id",
                &[&namespace, &id],
            )
            .await
            .map_err(pg)?
            .iter()
            .map(|r| r.get(0))
            .collect())
    }
    async fn get_by_fingerprint(
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
        self.client.lock().await.query_opt(&format!("SELECT {COLUMNS} FROM memories WHERE namespace=$1 AND tier=$2 AND fingerprint=$3 AND superseded_by IS NULL AND(expires_at IS NULL OR expires_at>$4)AND(valid_to IS NULL OR valid_to>$4)ORDER BY created_at DESC LIMIT 1"),&[&namespace,&tier_text(tier),&value,&now]).await.map_err(pg)?.as_ref().map(read_memory).transpose()?.ok_or(StoreError::NotFound)
    }
    async fn delete(&self, namespace: &str, id: &str) -> Result<()> {
        changed(
            self.client
                .lock()
                .await
                .execute(
                    "DELETE FROM memories WHERE namespace=$1 AND id=$2",
                    &[&namespace, &id],
                )
                .await
                .map_err(pg)?,
        )
    }
    async fn set_superseded(&self, namespace: &str, id: &str, replacement: &str) -> Result<()> {
        changed(self.client.lock().await.execute("UPDATE memories SET superseded_by=$1,valid_to=COALESCE(valid_to,now()) WHERE namespace=$2 AND id=$3",&[&replacement,&namespace,&id]).await.map_err(pg)?)
    }
    async fn restore(&self, namespace: &str, id: &str) -> Result<()> {
        changed(self.client.lock().await.execute("UPDATE memories SET superseded_by=NULL,valid_to=NULL WHERE namespace=$1 AND id=$2",&[&namespace,&id]).await.map_err(pg)?)
    }
    async fn vector_search(
        &self,
        namespace: &str,
        vector: &[f32],
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>> {
        if vector.len() != self.dimensions {
            return Err(StoreError::Backend(format!(
                "postgres: query vector has {} dims, store expects {}",
                vector.len(),
                self.dimensions
            )));
        }
        let vector = Vector::from(vector.to_vec());
        let rows=self.client.lock().await.query(&format!("SELECT {COLUMNS},embedding<->$2 AS distance FROM memories WHERE namespace=$1 AND embedding IS NOT NULL ORDER BY distance LIMIT $3"),&[&namespace,&vector,&((limit.max(1)*4)as i64)]).await.map_err(pg)?;
        let mut out = rows
            .iter()
            .map(|r| {
                Ok(Scored {
                    memory: read_memory(r)?,
                    score: 1.0 / (1.0 + r.get::<_, f64>(19)),
                })
            })
            .collect::<Result<Vec<_>>>()?;
        out.retain(|v| filter_matches(&v.memory, filter));
        out.truncate(limit);
        Ok(out)
    }
    async fn keyword_search(
        &self,
        namespace: &str,
        query: &str,
        filter: &Filter,
        limit: usize,
    ) -> Result<Vec<Scored>> {
        let rows=self.client.lock().await.query(&format!("SELECT {COLUMNS},ts_rank_cd(fts,websearch_to_tsquery('english',$2)) AS rank FROM memories WHERE namespace=$1 AND fts@@websearch_to_tsquery('english',$2) ORDER BY rank DESC LIMIT $3"),&[&namespace,&query,&((limit.max(1)*4)as i64)]).await.map_err(pg)?;
        let mut out = rows
            .iter()
            .map(|r| {
                Ok(Scored {
                    memory: read_memory(r)?,
                    score: r.get::<_, f32>(19) as f64,
                })
            })
            .collect::<Result<Vec<_>>>()?;
        out.retain(|v| filter_matches(&v.memory, filter));
        out.truncate(limit);
        Ok(out)
    }
    async fn reinforce(
        &self,
        namespace: &str,
        ids: &[String],
        at: DateTime<Utc>,
        expiry: Option<DateTime<Utc>>,
    ) -> Result<()> {
        self.client.lock().await.execute("UPDATE memories SET access_count=access_count+1,last_accessed_at=$1,expires_at=CASE WHEN expires_at IS NOT NULL AND $2::timestamptz IS NOT NULL THEN $2 ELSE expires_at END WHERE namespace=$3 AND id=ANY($4)",&[&at,&expiry,&namespace,&ids]).await.map_err(pg)?;
        Ok(())
    }
    async fn delete_if_expired_before(
        &self,
        namespace: &str,
        id: &str,
        cutoff: DateTime<Utc>,
    ) -> Result<()> {
        changed(self.client.lock().await.execute("DELETE FROM memories WHERE namespace=$1 AND id=$2 AND expires_at IS NOT NULL AND expires_at<=$3",&[&namespace,&id,&cutoff]).await.map_err(pg)?)
    }
    async fn list_expired(&self, now: DateTime<Utc>, limit: usize) -> Result<Vec<Memory>> {
        let rows=self.client.lock().await.query(&format!("SELECT {COLUMNS} FROM memories WHERE expires_at IS NOT NULL AND expires_at<=$1 LIMIT $2"),&[&now,&(limit as i64)]).await.map_err(pg)?;
        rows.iter().map(read_memory).collect()
    }
    async fn list(
        &self,
        namespace: &str,
        filter: &Filter,
        limit: Option<usize>,
    ) -> Result<Vec<Memory>> {
        let mut out = self.all(namespace).await?;
        out.retain(|v| filter_matches(v, filter));
        out.sort_by(|a, b| compare_memories(a, b, filter));
        if let Some(limit) = limit {
            out.truncate(limit)
        }
        Ok(out)
    }
    async fn list_namespaces(&self) -> Result<Vec<String>> {
        Ok(self
            .client
            .lock()
            .await
            .query(
                "SELECT DISTINCT namespace FROM memories ORDER BY namespace",
                &[],
            )
            .await
            .map_err(pg)?
            .iter()
            .map(|r| r.get(0))
            .collect())
    }
    async fn namespace_activity(
        &self,
        now: Option<DateTime<Utc>>,
    ) -> Result<Vec<NamespaceActivity>> {
        let now = now.unwrap_or_else(Utc::now);
        let rows=self.client.lock().await.query("SELECT namespace,count(*),max(created_at) FROM memories WHERE(expires_at IS NULL OR expires_at>$1)AND superseded_by IS NULL AND(valid_to IS NULL OR valid_to>$1)GROUP BY namespace ORDER BY namespace",&[&now]).await.map_err(pg)?;
        Ok(rows
            .iter()
            .map(|r| NamespaceActivity {
                namespace: r.get(0),
                total: r.get::<_, i64>(1) as usize,
                last_write: r.get(2),
            })
            .collect())
    }
    async fn delete_namespace(&self, namespace: &str) -> Result<u64> {
        self.client
            .lock()
            .await
            .execute("DELETE FROM memories WHERE namespace=$1", &[&namespace])
            .await
            .map_err(pg)
    }
    async fn reassign(&self, from: &str, ids: &[String], to: &str) -> Result<u64> {
        self.client
            .lock()
            .await
            .execute(
                "UPDATE memories SET namespace=$1 WHERE namespace=$2 AND id=ANY($3)",
                &[&to, &from, &ids],
            )
            .await
            .map_err(pg)
    }
    async fn retier(
        &self,
        namespace: &str,
        id: &str,
        tier: Tier,
        expiry: Option<DateTime<Utc>>,
    ) -> Result<()> {
        changed(
            self.client
                .lock()
                .await
                .execute(
                    "UPDATE memories SET tier=$1,expires_at=$2 WHERE namespace=$3 AND id=$4",
                    &[&tier_text(tier), &expiry, &namespace, &id],
                )
                .await
                .map_err(pg)?,
        )
    }
    async fn set_confidence(
        &self,
        namespace: &str,
        id: &str,
        value: f64,
        now: DateTime<Utc>,
    ) -> Result<()> {
        changed(
            self.client
                .lock()
                .await
                .execute(
                    "UPDATE memories SET confidence=$1,updated_at=$2 WHERE namespace=$3 AND id=$4",
                    &[&value, &now, &namespace, &id],
                )
                .await
                .map_err(pg)?,
        )
    }
    async fn mark_contradicted(
        &self,
        namespace: &str,
        id: &str,
        by: &str,
        value: f64,
        now: DateTime<Utc>,
    ) -> Result<()> {
        let old = self.get(namespace, id).await?;
        let mut metadata = old.metadata;
        metadata.insert("contradicted_by".into(), by.into());
        if let Some(v) = old.confidence {
            metadata.insert("pre_contradiction_confidence".into(), v.into());
        }
        changed(self.client.lock().await.execute("UPDATE memories SET confidence=$1,updated_at=$2,valid_to=COALESCE(valid_to,$2),metadata=$3 WHERE namespace=$4 AND id=$5",&[&value,&now,&Value::Object(metadata),&namespace,&id]).await.map_err(pg)?)
    }
    async fn embed_model(&self) -> Result<String> {
        Ok(self
            .client
            .lock()
            .await
            .query_opt("SELECT value FROM meta WHERE key='embed_model'", &[])
            .await
            .map_err(pg)?
            .map(|r| r.get(0))
            .unwrap_or_default())
    }
    async fn set_embed_model(&self, model: &str) -> Result<()> {
        self.client.lock().await.execute("INSERT INTO meta(key,value)VALUES('embed_model',$1)ON CONFLICT(key)DO UPDATE SET value=excluded.value",&[&model]).await.map_err(pg)?;
        Ok(())
    }
    async fn ping(&self) -> Result<()> {
        self.client
            .lock()
            .await
            .simple_query("SELECT 1")
            .await
            .map_err(pg)?;
        Ok(())
    }
}

#[async_trait]
impl LinkStore for PostgresStore {
    async fn put_link(&self, link: &NamespaceLink) -> Result<()> {
        let tiers = Value::Array(link.tiers.iter().map(|v| tier_text(*v).into()).collect());
        self.client.lock().await.execute("INSERT INTO namespace_links(src_ns,dst_ns,tiers,note,created_at)VALUES($1,$2,$3,$4,$5)ON CONFLICT(src_ns,dst_ns)DO UPDATE SET tiers=excluded.tiers,note=excluded.note,created_at=excluded.created_at",&[&link.source,&link.destination,&tiers,&link.note,&link.created_at]).await.map_err(pg)?;
        Ok(())
    }
    async fn delete_link(&self, source: &str, destination: &str) -> Result<bool> {
        Ok(self
            .client
            .lock()
            .await
            .execute(
                "DELETE FROM namespace_links WHERE src_ns=$1 AND dst_ns=$2",
                &[&source, &destination],
            )
            .await
            .map_err(pg)?
            > 0)
    }
    async fn list_links(&self, source: &str) -> Result<Vec<NamespaceLink>> {
        let rows=self.client.lock().await.query("SELECT src_ns,dst_ns,tiers,note,created_at FROM namespace_links WHERE src_ns=$1 ORDER BY dst_ns",&[&source]).await.map_err(pg)?;
        rows.iter().map(read_link).collect()
    }
    async fn list_all_links(&self) -> Result<Vec<NamespaceLink>> {
        let rows=self.client.lock().await.query("SELECT src_ns,dst_ns,tiers,note,created_at FROM namespace_links ORDER BY src_ns,dst_ns",&[]).await.map_err(pg)?;
        rows.iter().map(read_link).collect()
    }
    async fn rename_link_endpoints(&self, from: &str, to: &str) -> Result<()> {
        if from == to {
            return Ok(());
        }
        let links = self.list_all_links().await?;
        let mut client = self.client.lock().await;
        let transaction = client.transaction().await.map_err(pg)?;
        for mut link in links
            .into_iter()
            .filter(|v| v.source == from || v.destination == from)
        {
            transaction
                .execute(
                    "DELETE FROM namespace_links WHERE src_ns=$1 AND dst_ns=$2",
                    &[&link.source, &link.destination],
                )
                .await
                .map_err(pg)?;
            if link.source == from {
                link.source = to.into()
            }
            if link.destination == from {
                link.destination = to.into()
            }
            let tiers = Value::Array(link.tiers.iter().map(|v| tier_text(*v).into()).collect());
            transaction.execute("INSERT INTO namespace_links(src_ns,dst_ns,tiers,note,created_at)VALUES($1,$2,$3,$4,$5)ON CONFLICT DO NOTHING",&[&link.source,&link.destination,&tiers,&link.note,&link.created_at]).await.map_err(pg)?;
        }
        transaction.commit().await.map_err(pg)
    }
}

#[async_trait]
impl ApiKeyStore for PostgresStore {
    async fn put_api_key(&self, key: &ApiKey) -> Result<()> {
        let created = key.created_at.unwrap_or_else(Utc::now);
        self.client.lock().await.execute("INSERT INTO api_keys(name,key_hash,home_ns,default_ns,created_at,disabled)VALUES($1,$2,$3,$4,$5,$6)ON CONFLICT(name)DO UPDATE SET key_hash=excluded.key_hash,home_ns=excluded.home_ns,default_ns=excluded.default_ns,created_at=CASE WHEN $7 THEN api_keys.created_at ELSE excluded.created_at END,disabled=excluded.disabled",&[&key.name,&key.hash,&key.home_namespace,&key.default_namespace,&created,&key.disabled,&key.created_at.is_none()]).await.map_err(pg)?;
        Ok(())
    }
    async fn delete_api_key(&self, name: &str) -> Result<bool> {
        Ok(self
            .client
            .lock()
            .await
            .execute("DELETE FROM api_keys WHERE name=$1", &[&name])
            .await
            .map_err(pg)?
            > 0)
    }
    async fn list_api_keys(&self) -> Result<Vec<ApiKey>> {
        let rows=self.client.lock().await.query("SELECT name,key_hash,home_ns,default_ns,created_at,disabled FROM api_keys ORDER BY name",&[]).await.map_err(pg)?;
        Ok(rows.iter().map(read_api_key).collect())
    }
    async fn get_api_key_by_hash(&self, hash: &str) -> Result<Option<ApiKey>> {
        Ok(self.client.lock().await.query_opt("SELECT name,key_hash,home_ns,default_ns,created_at,disabled FROM api_keys WHERE key_hash=$1",&[&hash]).await.map_err(pg)?.as_ref().map(read_api_key))
    }
    async fn rename_api_key_namespaces(&self, from: &str, to: &str) -> Result<()> {
        self.client.lock().await.execute("UPDATE api_keys SET home_ns=CASE WHEN home_ns=$1 THEN $2 ELSE home_ns END,default_ns=CASE WHEN default_ns=$1 THEN $2 ELSE default_ns END WHERE home_ns=$1 OR default_ns=$1",&[&from,&to]).await.map_err(pg)?;
        Ok(())
    }
}

#[async_trait]
impl EventLogStore for PostgresStore {
    async fn append_events(&self, events: &[Event]) -> Result<()> {
        let mut client = self.client.lock().await;
        let transaction = client.transaction().await.map_err(pg)?;
        for event in events {
            transaction.execute("INSERT INTO memory_events(op_id,kind,namespace,query,memory_id,memory_ns,memory_tier,memory_summary,rank,score,detail,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)",&[&event.operation_id,&event_kind_text(event.kind),&event.namespace,&event.query,&event.memory_id,&event.memory_namespace,&tier_text(event.memory_tier),&event.memory_summary,&(event.rank as i32),&event.score,&Value::Object(event.detail.clone()),&event.created_at]).await.map_err(pg)?;
        }
        transaction.commit().await.map_err(pg)
    }
    async fn list_events(&self, filter: &EventFilter) -> Result<Vec<Event>> {
        let rows=self.client.lock().await.query("SELECT id,op_id,kind,namespace,query,memory_id,memory_ns,memory_tier,memory_summary,rank,score,detail,created_at FROM memory_events ORDER BY created_at DESC,id DESC",&[]).await.map_err(pg)?;
        let mut events = rows.iter().map(read_event).collect::<Result<Vec<_>>>()?;
        events.retain(|event| event_base_matches(event, filter));
        if !filter.tiers.is_empty() || !filter.text.is_empty() {
            let needle = filter.text.to_lowercase();
            let operations: std::collections::HashSet<_> = events
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
    async fn prune_events(&self, older: Option<DateTime<Utc>>, keep: Option<usize>) -> Result<u64> {
        let client = self.client.lock().await;
        let mut count = 0;
        if let Some(older) = older {
            count += client
                .execute("DELETE FROM memory_events WHERE created_at<$1", &[&older])
                .await
                .map_err(pg)?;
        }
        if let Some(keep) = keep.filter(|v| *v > 0) {
            count+=client.execute("DELETE FROM memory_events WHERE id NOT IN(SELECT id FROM memory_events ORDER BY created_at DESC,id DESC LIMIT $1)",&[&(keep as i64)]).await.map_err(pg)?;
        }
        Ok(count)
    }
}

fn read_link(row: &Row) -> Result<NamespaceLink> {
    let tiers: Value = row.get(2);
    let tiers = tiers
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .map(parse_tier)
        .collect::<Result<Vec<_>>>()?;
    Ok(NamespaceLink {
        source: row.get(0),
        destination: row.get(1),
        tiers,
        note: row.get(3),
        created_at: row.get(4),
    })
}
fn parse_vector_dimensions(value: &str) -> Result<usize> {
    value
        .strip_prefix("vector(")
        .and_then(|v| v.strip_suffix(')'))
        .ok_or_else(|| {
            StoreError::Backend(format!(
                "postgres: cannot determine embedding dimension from {value:?}"
            ))
        })?
        .parse()
        .map_err(|error| {
            StoreError::Backend(format!("postgres: parse embedding dimension: {error}"))
        })
}
fn read_api_key(row: &Row) -> ApiKey {
    ApiKey {
        name: row.get(0),
        hash: row.get(1),
        home_namespace: row.get(2),
        default_namespace: row.get(3),
        created_at: Some(row.get(4)),
        disabled: row.get(5),
    }
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
fn parse_event_kind(value: &str) -> Result<EventKind> {
    match value {
        "recall" => Ok(EventKind::Recall),
        "get" => Ok(EventKind::Get),
        "briefing" => Ok(EventKind::Briefing),
        "remember" => Ok(EventKind::Remember),
        "update" => Ok(EventKind::Update),
        "forget" => Ok(EventKind::Forget),
        "supersede" => Ok(EventKind::Supersede),
        _ => Err(StoreError::Backend(format!(
            "postgres: unknown event kind {value}"
        ))),
    }
}
fn read_event(row: &Row) -> Result<Event> {
    let kind: String = row.get(2);
    let tier: String = row.get(7);
    let detail: Value = row.get(11);
    Ok(Event {
        id: row.get(0),
        operation_id: row.get(1),
        kind: parse_event_kind(&kind)?,
        namespace: row.get(3),
        query: row.get(4),
        memory_id: row.get(5),
        memory_namespace: row.get(6),
        memory_tier: parse_tier(&tier)?,
        memory_summary: row.get(8),
        rank: row.get::<_, i32>(9) as usize,
        score: row.get(10),
        detail: detail.as_object().cloned().unwrap_or_default(),
        created_at: row.get(12),
    })
}
fn event_base_matches(event: &Event, filter: &EventFilter) -> bool {
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

fn read_memory(row: &Row) -> Result<Memory> {
    let metadata: Value = row.get(5);
    let tier: String = row.get(2);
    let level: String = row.get(17);
    Ok(Memory {
        id: row.get(0),
        namespace: row.get(1),
        tier: parse_tier(&tier)?,
        level: parse_level(&level),
        content: row.get(3),
        summary: row.get(4),
        metadata: metadata.as_object().cloned().unwrap_or_default(),
        tags: row.get(6),
        importance: row.get(7),
        created_at: row.get(8),
        updated_at: row.get(9),
        last_accessed_at: row.get(10),
        access_count: row.get::<_, i32>(11) as i64,
        expires_at: row.get(12),
        superseded_by: row.get(13),
        valid_from: row.get(14),
        valid_to: row.get(15),
        confidence: row.get(16),
        linked_memory_ids: serde_json::from_str(&row.get::<_, String>(18)).unwrap_or_default(),
        embedding: vec![],
    })
}
fn tier_text(v: Tier) -> &'static str {
    match v {
        Tier::Working => "working",
        Tier::Episodic => "episodic",
        Tier::Semantic => "semantic",
        Tier::Procedural => "procedural",
    }
}
fn level_text(v: Level) -> &'static str {
    match v {
        Level::Explicit => "explicit",
        Level::Deduced => "deduced",
    }
}
fn parse_tier(v: &str) -> Result<Tier> {
    match v {
        "working" => Ok(Tier::Working),
        "episodic" => Ok(Tier::Episodic),
        "semantic" => Ok(Tier::Semantic),
        "procedural" => Ok(Tier::Procedural),
        _ => Err(StoreError::Backend(format!("postgres: unknown tier {v}"))),
    }
}
fn parse_level(v: &str) -> Option<Level> {
    match v {
        "explicit" => Some(Level::Explicit),
        "deduced" => Some(Level::Deduced),
        _ => None,
    }
}
fn changed(n: u64) -> Result<()> {
    if n == 0 {
        Err(StoreError::NotFound)
    } else {
        Ok(())
    }
}
fn pg(e: tokio_postgres::Error) -> StoreError {
    StoreError::Backend(format!("postgres: {e}"))
}
fn json(e: serde_json::Error) -> StoreError {
    StoreError::Backend(format!("postgres: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    fn memory(id: &str, vector: Vec<f32>) -> Memory {
        let now = Utc::now();
        Memory {
            id: id.into(),
            namespace: "rust-test".into(),
            tier: Tier::Semantic,
            level: Some(Level::Explicit),
            content: "The sky is blue".into(),
            summary: "sky".into(),
            metadata: serde_json::Map::new(),
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
            embedding: vector,
        }
    }
    #[tokio::test]
    #[ignore = "requires the compose VectorChord database"]
    async fn postgres_round_trip() {
        let dsn = std::env::var("MEMINI_TEST_POSTGRES_DSN")
            .unwrap_or_else(|_| "postgres://postgres:memini@localhost:5432/memini".into());
        let store = PostgresStore::open(&dsn, 3).await.unwrap();
        store.client.lock().await.batch_execute("DELETE FROM namespace_links WHERE src_ns LIKE 'rust-%' OR dst_ns LIKE 'rust-%';DELETE FROM api_keys WHERE name LIKE 'rust-%';DELETE FROM memory_events WHERE namespace LIKE 'rust-%'").await.unwrap();
        let _ = store.delete_namespace("rust-test").await;
        let item = memory("one", vec![1.0, 0.0, 0.0]);
        store.upsert(&item).await.unwrap();
        assert_eq!(
            store.get("rust-test", "one").await.unwrap().content,
            item.content
        );
        assert_eq!(
            store
                .vector_search("rust-test", &[1.0, 0.0, 0.0], &Filter::default(), 5)
                .await
                .unwrap()[0]
                .memory
                .id,
            "one"
        );
        assert_eq!(
            store
                .keyword_search("rust-test", "sky", &Filter::default(), 5)
                .await
                .unwrap()[0]
                .memory
                .id,
            "one"
        );
        let now = Utc::now();
        let link = NamespaceLink {
            source: "rust-a".into(),
            destination: "rust-b".into(),
            tiers: vec![Tier::Semantic],
            note: "shared".into(),
            created_at: now,
        };
        store.put_link(&link).await.unwrap();
        assert_eq!(
            store.list_links("rust-a").await.unwrap()[0].destination,
            "rust-b"
        );
        store
            .rename_link_endpoints("rust-a", "rust-c")
            .await
            .unwrap();
        assert_eq!(store.list_links("rust-c").await.unwrap().len(), 1);
        let key = ApiKey {
            name: "rust-agent".into(),
            hash: "rust-abc".into(),
            home_namespace: "rust-home".into(),
            default_namespace: "rust-test".into(),
            created_at: Some(now),
            disabled: false,
        };
        store.put_api_key(&key).await.unwrap();
        store
            .put_api_key(&ApiKey {
                hash: "rust-def".into(),
                created_at: None,
                ..key
            })
            .await
            .unwrap();
        assert_eq!(
            store
                .get_api_key_by_hash("rust-def")
                .await
                .unwrap()
                .unwrap()
                .created_at,
            Some(now)
        );
        let event = Event {
            id: 0,
            operation_id: "rust-op".into(),
            kind: EventKind::Recall,
            namespace: "rust-test".into(),
            query: "sky".into(),
            memory_id: "one".into(),
            memory_namespace: "rust-test".into(),
            memory_tier: Tier::Semantic,
            memory_summary: "blue sky".into(),
            rank: 1,
            score: Some(0.9),
            detail: serde_json::Map::new(),
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
        assert_eq!(
            store
                .prune_events(Some(now + chrono::Duration::milliseconds(1)), None)
                .await
                .unwrap(),
            1
        );
        store.delete_namespace("rust-test").await.unwrap();
        assert!(
            PostgresStore::open("postgres://postgres:memini@localhost:5432/memini", 4)
                .await
                .is_err()
        );
    }
}
