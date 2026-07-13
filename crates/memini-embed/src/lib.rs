use async_trait::async_trait;
use lru::LruCache;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::HashMap,
    num::NonZeroUsize,
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};
use thiserror::Error;
use tokio::sync::{Mutex, Semaphore};

#[derive(Debug, Error)]
pub enum EmbedError {
    #[error("embeddings endpoint not configured (set MEMINI_EMBED_BASE_URL)")]
    Disabled,
    #[error("embed: {0}")]
    Invalid(String),
    #[error("embed: {0}")]
    Http(#[from] reqwest::Error),
    #[error("embed disk cache: {0}")]
    Io(#[from] std::io::Error),
}
pub type Result<T> = std::result::Result<T, EmbedError>;
#[async_trait]
pub trait Embedder: Send + Sync {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>>;
    fn dimensions(&self) -> usize;
}
pub async fn embed_one(embedder: &dyn Embedder, text: &str) -> Result<Vec<f32>> {
    embedder
        .embed(&[text.into()])
        .await?
        .into_iter()
        .next()
        .ok_or_else(|| EmbedError::Invalid("embedder returned no vector".into()))
}

pub struct Observed {
    inner: Arc<dyn Embedder>,
    backend: String,
    metrics: memini_observability::Registry,
    dependencies: memini_observability::DependencyTracker,
}

pub fn observed(
    inner: Arc<dyn Embedder>,
    backend: impl Into<String>,
    metrics: memini_observability::Registry,
    dependencies: memini_observability::DependencyTracker,
) -> Arc<dyn Embedder> {
    Arc::new(Observed {
        inner,
        backend: backend.into(),
        metrics,
        dependencies,
    })
}

#[async_trait]
impl Embedder for Observed {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
        let started = std::time::Instant::now();
        let result = self.inner.embed(texts).await;
        self.metrics.observe(
            "memini_embed_duration_seconds",
            &[("backend", &self.backend)],
            started.elapsed().as_secs_f64(),
        );
        self.metrics.observe(
            "memini_embed_items",
            &[("backend", &self.backend)],
            texts.len() as f64,
        );
        self.metrics.add(
            "memini_embed_tokens_total",
            &[("backend", &self.backend)],
            0.0,
        );
        if let Err(error) = &result {
            self.metrics
                .inc("memini_embed_errors_total", &[("backend", &self.backend)]);
            self.dependencies
                .record("embedder", Some(&error.to_string()));
        } else {
            self.dependencies.record("embedder", None);
        }
        result
    }

    fn dimensions(&self) -> usize {
        self.inner.dimensions()
    }
}

pub struct Disabled {
    pub dimensions: usize,
}
#[async_trait]
impl Embedder for Disabled {
    async fn embed(&self, _: &[String]) -> Result<Vec<Vec<f32>>> {
        Err(EmbedError::Disabled)
    }
    fn dimensions(&self) -> usize {
        self.dimensions
    }
}

pub struct OpenAiEmbedder {
    client: reqwest::Client,
    url: String,
    api_key: String,
    model: String,
    dimensions: usize,
}
impl OpenAiEmbedder {
    pub fn new(base_url: &str, api_key: &str, model: &str, dimensions: usize) -> Result<Self> {
        if base_url.is_empty() {
            return Err(EmbedError::Invalid("BaseURL is required".into()));
        }
        if model.is_empty() {
            return Err(EmbedError::Invalid("Model is required".into()));
        }
        if dimensions == 0 {
            return Err(EmbedError::Invalid("Dims must be positive".into()));
        }
        let client = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(60))
            .build()?;
        Ok(Self {
            client,
            url: format!("{}/embeddings", base_url.trim_end_matches('/')),
            api_key: api_key.into(),
            model: model.into(),
            dimensions,
        })
    }
}
#[derive(Serialize)]
struct Request<'a> {
    model: &'a str,
    input: &'a [String],
    encoding_format: &'static str,
}
#[derive(Deserialize)]
struct Response {
    data: Vec<ResponseItem>,
}
#[derive(Deserialize)]
struct ResponseItem {
    index: usize,
    embedding: Vec<f32>,
}
#[async_trait]
impl Embedder for OpenAiEmbedder {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
        if texts.is_empty() {
            return Ok(vec![]);
        }
        let mut final_response = None;
        for attempt in 0..=6 {
            let mut request = self.client.post(&self.url).json(&Request {
                model: &self.model,
                input: texts,
                encoding_format: "float",
            });
            if !self.api_key.is_empty() {
                request = request.bearer_auth(&self.api_key);
            }
            let response = request.send().await?;
            let retry = response.status().as_u16() == 429 || response.status().is_server_error();
            if retry && attempt < 6 {
                tokio::time::sleep(std::time::Duration::from_millis(
                    50 * (1_u64 << attempt.min(5)),
                ))
                .await;
                continue;
            }
            final_response = Some(response.error_for_status()?);
            break;
        }
        let response = final_response
            .ok_or_else(|| EmbedError::Invalid("retry budget exhausted".into()))?
            .json::<Response>()
            .await?;
        if response.data.len() != texts.len() {
            return Err(EmbedError::Invalid(format!(
                "expected {} vectors, got {}",
                texts.len(),
                response.data.len()
            )));
        }
        let mut output = vec![vec![]; texts.len()];
        for item in response.data {
            if item.index >= output.len() {
                return Err(EmbedError::Invalid(format!(
                    "vector index {} out of range",
                    item.index
                )));
            }
            if item.embedding.len() != self.dimensions {
                return Err(EmbedError::Invalid(format!(
                    "vector {} has {} dims, configured {}",
                    item.index,
                    item.embedding.len(),
                    self.dimensions
                )));
            }
            output[item.index] = item.embedding;
        }
        Ok(output)
    }
    fn dimensions(&self) -> usize {
        self.dimensions
    }
}

pub struct Batched {
    inner: Arc<dyn Embedder>,
    max_items: usize,
    max_chars: usize,
    max_item_chars: usize,
}
impl Batched {
    pub fn new(
        inner: Arc<dyn Embedder>,
        max_items: usize,
        max_chars: usize,
        max_item_chars: usize,
    ) -> Self {
        Self {
            inner,
            max_items: if max_items == 0 { 16 } else { max_items },
            max_chars,
            max_item_chars,
        }
    }
}
#[async_trait]
impl Embedder for Batched {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
        let mut output = vec![vec![]; texts.len()];
        let mut start = 0;
        while start < texts.len() {
            let mut end = start;
            let mut chars = 0;
            let mut batch = Vec::new();
            while end < texts.len() && batch.len() < self.max_items {
                let text = truncate(&texts[end], self.max_item_chars);
                if !batch.is_empty() && self.max_chars > 0 && chars + text.len() > self.max_chars {
                    break;
                }
                chars += text.len();
                batch.push(text);
                end += 1;
            }
            let vectors = self.inner.embed(&batch).await?;
            if vectors.len() != batch.len() {
                return Err(EmbedError::Invalid(
                    "batched embedder returned wrong vector count".into(),
                ));
            }
            output[start..end].clone_from_slice(&vectors);
            start = end;
        }
        Ok(output)
    }
    fn dimensions(&self) -> usize {
        self.inner.dimensions()
    }
}

pub struct Limited {
    inner: Arc<dyn Embedder>,
    semaphore: Semaphore,
    in_flight: AtomicUsize,
    callback: Option<Arc<dyn Fn(usize) + Send + Sync>>,
    max: usize,
}
impl Limited {
    pub fn new(
        inner: Arc<dyn Embedder>,
        max: usize,
        callback: Option<Arc<dyn Fn(usize) + Send + Sync>>,
    ) -> Self {
        Self {
            inner,
            semaphore: Semaphore::new(max.max(1)),
            in_flight: AtomicUsize::new(0),
            callback,
            max: max.max(1),
        }
    }
    pub fn max(&self) -> usize {
        self.max
    }
}
pub fn limited(
    inner: Arc<dyn Embedder>,
    max: usize,
    callback: Option<Arc<dyn Fn(usize) + Send + Sync>>,
) -> Arc<dyn Embedder> {
    if max == 0 {
        inner
    } else {
        Arc::new(Limited::new(inner, max, callback))
    }
}
#[async_trait]
impl Embedder for Limited {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
        let permit = self
            .semaphore
            .acquire()
            .await
            .map_err(|e| EmbedError::Invalid(e.to_string()))?;
        let count = self.in_flight.fetch_add(1, Ordering::SeqCst) + 1;
        if let Some(cb) = &self.callback {
            cb(count)
        }
        let result = self.inner.embed(texts).await;
        let count = self.in_flight.fetch_sub(1, Ordering::SeqCst) - 1;
        if let Some(cb) = &self.callback {
            cb(count)
        }
        drop(permit);
        result
    }
    fn dimensions(&self) -> usize {
        self.inner.dimensions()
    }
}

pub struct Cached {
    inner: Arc<dyn Embedder>,
    cache: Mutex<LruCache<[u8; 32], Vec<f32>>>,
}
impl Cached {
    pub fn new(inner: Arc<dyn Embedder>, size: usize) -> Result<Self> {
        let size = NonZeroUsize::new(size)
            .ok_or_else(|| EmbedError::Invalid("cache size must be positive".into()))?;
        Ok(Self {
            inner,
            cache: Mutex::new(LruCache::new(size)),
        })
    }
}
#[async_trait]
impl Embedder for Cached {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
        let mut output = vec![vec![]; texts.len()];
        let (mut positions, mut misses) = (Vec::new(), Vec::new());
        {
            let mut cache = self.cache.lock().await;
            for (i, text) in texts.iter().enumerate() {
                if let Some(vector) = cache.get(&key(text)) {
                    output[i] = vector.clone()
                } else {
                    positions.push(i);
                    misses.push(text.clone())
                }
            }
        }
        if !misses.is_empty() {
            let vectors = self.inner.embed(&misses).await?;
            let mut cache = self.cache.lock().await;
            for ((position, text), vector) in positions.into_iter().zip(misses).zip(vectors) {
                output[position] = vector.clone();
                cache.put(key(&text), vector);
            }
        }
        Ok(output)
    }
    fn dimensions(&self) -> usize {
        self.inner.dimensions()
    }
}

pub struct DiskCache {
    inner: Arc<dyn Embedder>,
    path: PathBuf,
    cache: Mutex<HashMap<[u8; 32], Vec<f32>>>,
}
impl DiskCache {
    pub fn new(inner: Arc<dyn Embedder>, path: impl AsRef<Path>) -> Result<Self> {
        let path = path.as_ref().to_owned();
        let cache = std::fs::read(&path)
            .ok()
            .and_then(|bytes| {
                bincode::serde::decode_from_slice(&bytes, bincode::config::standard())
                    .ok()
                    .map(|v: (HashMap<[u8; 32], Vec<f32>>, usize)| v.0)
            })
            .unwrap_or_default();
        Ok(Self {
            inner,
            path,
            cache: Mutex::new(cache),
        })
    }
    pub async fn len(&self) -> usize {
        self.cache.lock().await.len()
    }
    pub async fn is_empty(&self) -> bool {
        self.cache.lock().await.is_empty()
    }
    pub async fn save(&self) -> Result<()> {
        let cache = self.cache.lock().await;
        let bytes = bincode::serde::encode_to_vec(&*cache, bincode::config::standard())
            .map_err(|e| EmbedError::Invalid(e.to_string()))?;
        let temporary = self.path.with_extension("tmp");
        std::fs::write(&temporary, bytes)?;
        std::fs::rename(temporary, &self.path)?;
        Ok(())
    }
}
#[async_trait]
impl Embedder for DiskCache {
    async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
        let mut output = vec![vec![]; texts.len()];
        let (mut positions, mut misses) = (Vec::new(), Vec::new());
        {
            let cache = self.cache.lock().await;
            for (i, text) in texts.iter().enumerate() {
                if let Some(vector) = cache.get(&key(text)) {
                    output[i] = vector.clone()
                } else {
                    positions.push(i);
                    misses.push(text.clone())
                }
            }
        }
        if !misses.is_empty() {
            let vectors = self.inner.embed(&misses).await?;
            let mut cache = self.cache.lock().await;
            for ((position, text), vector) in positions.into_iter().zip(misses).zip(vectors) {
                output[position] = vector.clone();
                cache.insert(key(&text), vector);
            }
        }
        Ok(output)
    }
    fn dimensions(&self) -> usize {
        self.inner.dimensions()
    }
}

fn truncate(value: &str, limit: usize) -> String {
    if limit == 0 {
        return value.into();
    }
    value.chars().take(limit).collect()
}
fn key(value: &str) -> [u8; 32] {
    Sha256::digest(value.as_bytes()).into()
}

#[cfg(test)]
mod tests {
    use super::*;
    struct Fake {
        dimensions: usize,
        calls: AtomicUsize,
        seen: Mutex<Vec<String>>,
    }
    impl Fake {
        fn new(dimensions: usize) -> Self {
            Self {
                dimensions,
                calls: AtomicUsize::new(0),
                seen: Mutex::new(vec![]),
            }
        }
    }
    #[async_trait]
    impl Embedder for Fake {
        async fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.seen.lock().await.extend_from_slice(texts);
            Ok(texts
                .iter()
                .map(|text| {
                    let mut vector = vec![0.0; self.dimensions];
                    vector[0] = text.chars().count() as f32;
                    vector
                })
                .collect())
        }
        fn dimensions(&self) -> usize {
            self.dimensions
        }
    }
    #[tokio::test]
    async fn disabled_and_batching_contract() {
        let disabled = Disabled { dimensions: 16 };
        assert_eq!(disabled.dimensions(), 16);
        assert!(matches!(
            disabled.embed(&["x".into()]).await,
            Err(EmbedError::Disabled)
        ));
        let fake = Arc::new(Fake::new(4));
        let same = limited(fake.clone(), 0, None);
        assert!(Arc::ptr_eq(&(fake.clone() as Arc<dyn Embedder>), &same));
        let batch = Batched::new(fake.clone(), 2, 0, 3);
        let output = batch
            .embed(&["abcdef".into(), "yy".into(), "zzz".into()])
            .await
            .unwrap();
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
        assert_eq!(
            output.iter().map(|v| v[0]).collect::<Vec<_>>(),
            vec![3.0, 2.0, 3.0]
        );
        assert_eq!(&*fake.seen.lock().await, &["abc", "yy", "zzz"]);
    }
    #[tokio::test]
    async fn memory_and_disk_cache_contract() {
        let fake = Arc::new(Fake::new(3));
        let cached = Cached::new(fake.clone(), 8).unwrap();
        cached.embed(&["a".into(), "b".into()]).await.unwrap();
        cached.embed(&["a".into(), "c".into()]).await.unwrap();
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
        assert_eq!(&fake.seen.lock().await[2..], &["c"]);
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("vectors.bin");
        let disk = DiskCache::new(fake.clone(), &path).unwrap();
        disk.embed(&["hello".into()]).await.unwrap();
        disk.save().await.unwrap();
        assert_eq!(disk.len().await, 1);
        let fresh = Arc::new(Fake::new(3));
        let loaded = DiskCache::new(fresh.clone(), &path).unwrap();
        loaded.embed(&["hello".into()]).await.unwrap();
        assert_eq!(fresh.calls.load(Ordering::SeqCst), 0);
    }
    #[tokio::test]
    async fn openai_reorders_by_index() {
        use axum::{Json, Router, routing::post};
        let app=Router::new().route("/v1/embeddings",post(||async{Json(serde_json::json!({"data":[{"index":1,"embedding":[0.1,0.2]},{"index":0,"embedding":[0.3,0.4]}]}))}));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        let client = OpenAiEmbedder::new(&format!("http://{address}/v1"), "", "model", 2).unwrap();
        let vectors = client
            .embed(&["first".into(), "second".into()])
            .await
            .unwrap();
        assert_eq!(vectors[0][0], 0.3);
        assert_eq!(vectors[1][0], 0.1);
    }
}
