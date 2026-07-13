use chrono::{Duration, Utc};
use memini_core::memory::{Level, Memory, Tier, fingerprint};
use memini_store::{Filter, Sort, SortKey, Store, StoreError};

pub fn fixture(id: &str, namespace: &str, tier: Tier, embedding: Vec<f32>) -> Memory {
    let now = Utc::now();
    Memory {
        id: id.into(),
        namespace: namespace.into(),
        tier,
        level: Some(Level::Explicit),
        content: format!("memory content {id}"),
        summary: id.into(),
        metadata: serde_json::Map::new(),
        tags: vec!["shared".into()],
        importance: 0.5,
        created_at: now,
        updated_at: now,
        last_accessed_at: now,
        access_count: 0,
        expires_at: None,
        superseded_by: None,
        valid_from: None,
        valid_to: None,
        confidence: (tier == Tier::Semantic || tier == Tier::Procedural).then_some(0.4),
        linked_memory_ids: vec![],
        embedding,
    }
}

pub async fn run_store_conformance<S: Store>(store: &S, namespace: &str) {
    let _ = store.delete_namespace(namespace).await;
    let mut first = fixture(
        "conformance-first",
        namespace,
        Tier::Semantic,
        vec![1.0, 0.0, 0.0],
    );
    first.content = "The sky is blue".into();
    first.metadata.insert("memory_type".into(), "fact".into());
    let mut second = fixture(
        "conformance-second",
        namespace,
        Tier::Episodic,
        vec![0.0, 1.0, 0.0],
    );
    second.content = "Grass is green".into();
    second.created_at -= Duration::seconds(1);
    store.upsert(&first).await.unwrap();
    store.upsert(&second).await.unwrap();
    assert_eq!(
        store.get(namespace, &first.id).await.unwrap().content,
        first.content
    );
    assert!(matches!(
        store.get("foreign", &first.id).await,
        Err(StoreError::NotFound)
    ));
    let mut hijack = first.clone();
    hijack.namespace = "foreign".into();
    assert!(matches!(
        store.upsert(&hijack).await,
        Err(StoreError::Conflict)
    ));
    assert_eq!(
        store
            .get_by_fingerprint(
                namespace,
                Tier::Semantic,
                &fingerprint(&first.content),
                None
            )
            .await
            .unwrap()
            .id,
        first.id
    );
    let filter = Filter {
        tiers: vec![Tier::Semantic],
        memory_types: vec!["fact".into()],
        ..Filter::default()
    };
    assert_eq!(store.list(namespace, &filter, None).await.unwrap().len(), 1);
    assert_eq!(
        store
            .vector_search(namespace, &[1.0, 0.0, 0.0], &filter, 5)
            .await
            .unwrap()[0]
            .memory
            .id,
        first.id
    );
    assert_eq!(
        store
            .keyword_search(namespace, "sky", &Filter::default(), 5)
            .await
            .unwrap()[0]
            .memory
            .id,
        first.id
    );
    let at = Utc::now();
    store
        .reinforce(namespace, std::slice::from_ref(&first.id), at, None)
        .await
        .unwrap();
    assert_eq!(
        store.get(namespace, &first.id).await.unwrap().access_count,
        1
    );
    store
        .set_superseded(namespace, &first.id, &second.id)
        .await
        .unwrap();
    assert!(
        store
            .list(namespace, &Filter::default(), None)
            .await
            .unwrap()
            .iter()
            .all(|v| v.id != first.id)
    );
    assert_eq!(
        store.predecessor_ids(namespace, &second.id).await.unwrap(),
        vec![first.id.clone()]
    );
    store.restore(namespace, &first.id).await.unwrap();
    store
        .retier(
            namespace,
            &second.id,
            Tier::Working,
            Some(at - Duration::seconds(1)),
        )
        .await
        .unwrap();
    assert_eq!(
        store
            .list_expired(at, 10)
            .await
            .unwrap()
            .iter()
            .filter(|v| v.namespace == namespace)
            .count(),
        1
    );
    store
        .set_confidence(namespace, &first.id, 0.8, at)
        .await
        .unwrap();
    assert_eq!(
        store.get(namespace, &first.id).await.unwrap().confidence,
        Some(0.8)
    );
    store
        .mark_contradicted(namespace, &first.id, &second.id, 0.1, at)
        .await
        .unwrap();
    assert!(
        store
            .get(namespace, &first.id)
            .await
            .unwrap()
            .valid_to
            .is_some()
    );
    store.set_embed_model("conformance-model").await.unwrap();
    assert_eq!(store.embed_model().await.unwrap(), "conformance-model");
    let sorted = store
        .list(
            namespace,
            &Filter {
                include_expired: true,
                include_superseded: true,
                sort: Sort {
                    key: SortKey::CreatedAt,
                    ascending: true,
                },
                ..Filter::default()
            },
            None,
        )
        .await
        .unwrap();
    assert_eq!(sorted[0].id, second.id);
    assert_eq!(
        store
            .reassign(
                namespace,
                std::slice::from_ref(&second.id),
                &format!("{namespace}-moved")
            )
            .await
            .unwrap(),
        1
    );
    assert!(
        store
            .list_namespaces()
            .await
            .unwrap()
            .contains(&format!("{namespace}-moved"))
    );
    assert_eq!(store.delete_namespace(namespace).await.unwrap(), 1);
    assert_eq!(
        store
            .delete_namespace(&format!("{namespace}-moved"))
            .await
            .unwrap(),
        1
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn sqlite_conformance() {
        let dir = tempfile::tempdir().unwrap();
        let store = memini_sqlite::SqliteStore::open(dir.path().join("test.db"), 3).unwrap();
        run_store_conformance(&store, "sqlite-conformance").await;
    }
    #[tokio::test]
    #[ignore = "requires the compose VectorChord database"]
    async fn postgres_conformance() {
        let dsn = std::env::var("MEMINI_TEST_POSTGRES_DSN")
            .unwrap_or_else(|_| "postgres://postgres:memini@localhost:5432/memini".into());
        let store = memini_postgres::PostgresStore::open(&dsn, 3).await.unwrap();
        run_store_conformance(&store, "postgres-conformance").await;
    }
}
