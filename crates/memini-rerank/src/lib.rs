use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use std::{
    collections::{HashMap, HashSet},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};
use thiserror::Error;
use tokio::sync::Semaphore;

#[derive(Debug, Error)]
pub enum RerankError {
    #[error("rerank: {0}")]
    Invalid(String),
    #[error("rerank: {0}")]
    Http(#[from] reqwest::Error),
    #[error("rerank: llm: {0}")]
    Llm(#[from] memini_llm::LlmError),
}
pub type Result<T> = std::result::Result<T, RerankError>;
#[derive(Clone, Debug)]
pub struct Candidate {
    pub id: String,
    pub content: String,
}
#[async_trait]
pub trait Reranker: Send + Sync {
    async fn rerank(&self, query: &str, candidates: &[Candidate]) -> Result<Vec<String>>;
    fn backend(&self) -> &str {
        "unknown"
    }
}

pub struct CrossEncoder {
    http: reqwest::Client,
    url: String,
    model: String,
    key: String,
    max_doc_chars: usize,
    max_batch_chars: usize,
}
impl CrossEncoder {
    pub fn new(
        base_url: &str,
        model: &str,
        key: &str,
        max_doc_chars: usize,
        max_batch_chars: usize,
    ) -> Result<Self> {
        if base_url.is_empty() {
            return Err(RerankError::Invalid("base url is required".into()));
        }
        Ok(Self {
            http: reqwest::Client::builder()
                .timeout(std::time::Duration::from_secs(60))
                .build()?,
            url: format!("{}/rerank", base_url.trim_end_matches('/')),
            model: model.into(),
            key: key.into(),
            max_doc_chars,
            max_batch_chars,
        })
    }
    pub async fn scores(
        &self,
        query: &str,
        candidates: &[Candidate],
    ) -> Result<HashMap<String, f64>> {
        let max_doc = if self.max_batch_chars > 0
            && (self.max_doc_chars == 0 || self.max_doc_chars > self.max_batch_chars)
        {
            self.max_batch_chars
        } else {
            self.max_doc_chars
        };
        let docs = candidates
            .iter()
            .map(|v| truncate_chars(&v.content, max_doc))
            .collect::<Vec<_>>();
        let mut output = HashMap::new();
        for (start, end) in split_batches(&docs, query, self.max_batch_chars, self.model.len()) {
            output.extend(
                self.score_batch(query, &candidates[start..end], &docs[start..end])
                    .await?,
            );
        }
        Ok(output)
    }
    async fn score_batch(
        &self,
        query: &str,
        candidates: &[Candidate],
        documents: &[String],
    ) -> Result<HashMap<String, f64>> {
        let mut request = self.http.post(&self.url).json(&Request {
            model: &self.model,
            query,
            documents,
        });
        if !self.key.is_empty() {
            request = request.bearer_auth(&self.key)
        }
        let response = request
            .send()
            .await?
            .error_for_status()?
            .json::<Response>()
            .await?;
        if let Some(error) = response.error {
            return Err(RerankError::Invalid(format!(
                "server returned 200 with error: {error}"
            )));
        }
        if response.results.is_empty() {
            return Err(RerankError::Invalid(format!(
                "empty results for {} documents",
                documents.len()
            )));
        }
        let mut output = HashMap::new();
        for result in response.results {
            if let Some(candidate) = candidates.get(result.index) {
                output.insert(candidate.id.clone(), result.relevance_score);
            }
        }
        Ok(output)
    }
}
#[derive(Serialize)]
struct Request<'a> {
    #[serde(skip_serializing_if = "str::is_empty")]
    model: &'a str,
    query: &'a str,
    documents: &'a [String],
}
#[derive(Deserialize)]
struct Response {
    #[serde(default)]
    results: Vec<Score>,
    error: Option<String>,
}
#[derive(Deserialize)]
struct Score {
    index: usize,
    relevance_score: f64,
}
#[async_trait]
impl Reranker for CrossEncoder {
    async fn rerank(&self, query: &str, candidates: &[Candidate]) -> Result<Vec<String>> {
        if candidates.len() <= 1 {
            return Ok(candidates.iter().map(|v| v.id.clone()).collect());
        }
        let scores = self.scores(query, candidates).await?;
        let mut values = scores.into_iter().collect::<Vec<_>>();
        values.sort_by(|a, b| b.1.total_cmp(&a.1).then_with(|| a.0.cmp(&b.0)));
        Ok(values.into_iter().map(|v| v.0).collect())
    }
    fn backend(&self) -> &str {
        "cross_encoder"
    }
}

pub struct LlmReranker {
    client: Arc<dyn memini_llm::Client>,
    max_chars: usize,
}
impl LlmReranker {
    pub fn new(client: Arc<dyn memini_llm::Client>) -> Self {
        Self {
            client,
            max_chars: 300,
        }
    }
}
#[async_trait]
impl Reranker for LlmReranker {
    async fn rerank(&self, query: &str, candidates: &[Candidate]) -> Result<Vec<String>> {
        if candidates.len() <= 1 {
            return Ok(candidates.iter().map(|v| v.id.clone()).collect());
        }
        let mut prompt = format!("Question: {query}\n\nCandidates:\n");
        for (index, candidate) in candidates.iter().enumerate() {
            prompt.push_str(&format!(
                "[{}] {}\n",
                index + 1,
                truncate_chars(&candidate.content, self.max_chars)
            ));
        }
        prompt
            .push_str("\nMost relevant candidate numbers (comma-separated, most relevant first):");
        let output=self.client.complete("You re-rank candidate memories by how well each helps answer the question. Output only relevant candidate numbers, most relevant first, comma-separated, or none.",&prompt).await?;
        Ok(apply_order(&output, candidates))
    }
    fn backend(&self) -> &str {
        "llm"
    }
}

pub struct Limited {
    inner: Arc<dyn Reranker>,
    semaphore: Semaphore,
    in_flight: AtomicUsize,
    callback: Option<Arc<dyn Fn(usize) + Send + Sync>>,
}
impl Limited {
    pub fn new(
        inner: Arc<dyn Reranker>,
        max: usize,
        callback: Option<Arc<dyn Fn(usize) + Send + Sync>>,
    ) -> Self {
        Self {
            inner,
            semaphore: Semaphore::new(max.max(1)),
            in_flight: AtomicUsize::new(0),
            callback,
        }
    }
}
#[async_trait]
impl Reranker for Limited {
    async fn rerank(&self, query: &str, candidates: &[Candidate]) -> Result<Vec<String>> {
        let permit = self
            .semaphore
            .acquire()
            .await
            .map_err(|e| RerankError::Invalid(e.to_string()))?;
        let count = self.in_flight.fetch_add(1, Ordering::SeqCst) + 1;
        if let Some(cb) = &self.callback {
            cb(count)
        }
        let result = self.inner.rerank(query, candidates).await;
        let count = self.in_flight.fetch_sub(1, Ordering::SeqCst) - 1;
        if let Some(cb) = &self.callback {
            cb(count)
        }
        drop(permit);
        result
    }
    fn backend(&self) -> &str {
        self.inner.backend()
    }
}
pub fn limited(
    inner: Arc<dyn Reranker>,
    max: usize,
    callback: Option<Arc<dyn Fn(usize) + Send + Sync>>,
) -> Arc<dyn Reranker> {
    if max == 0 {
        inner
    } else {
        Arc::new(Limited::new(inner, max, callback))
    }
}

pub struct Timed {
    inner: Arc<dyn Reranker>,
    timeout: std::time::Duration,
}
#[async_trait]
impl Reranker for Timed {
    async fn rerank(&self, query: &str, candidates: &[Candidate]) -> Result<Vec<String>> {
        tokio::time::timeout(self.timeout, self.inner.rerank(query, candidates))
            .await
            .map_err(|_| RerankError::Invalid("request timed out".into()))?
    }
    fn backend(&self) -> &str {
        self.inner.backend()
    }
}
pub fn timed(inner: Arc<dyn Reranker>, timeout: std::time::Duration) -> Arc<dyn Reranker> {
    if timeout.is_zero() {
        inner
    } else {
        Arc::new(Timed { inner, timeout })
    }
}

fn apply_order(value: &str, candidates: &[Candidate]) -> Vec<String> {
    let mut output = Vec::new();
    let mut seen = HashSet::new();
    for token in value
        .split(|c: char| !c.is_ascii_digit())
        .filter(|v| !v.is_empty())
    {
        if let Ok(index) = token.parse::<usize>()
            && index > 0
            && index <= candidates.len()
            && seen.insert(index)
        {
            output.push(candidates[index - 1].id.clone());
        }
    }
    output
}

#[cfg(test)]
#[allow(clippy::items_after_test_module)]
mod tests {
    use super::*;
    use axum::{Json, Router, routing::post};
    use serde_json::{Value, json};
    async fn server(app: Router) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{address}")
    }
    fn candidates() -> Vec<Candidate> {
        vec![
            Candidate {
                id: "a".into(),
                content: "alpha".into(),
            },
            Candidate {
                id: "b".into(),
                content: "beta".into(),
            },
            Candidate {
                id: "c".into(),
                content: "gamma".into(),
            },
        ]
    }
    #[tokio::test]
    async fn cross_encoder_orders_and_drops() {
        let app=Router::new().route("/rerank",post(|Json(body):Json<Value>|async move{assert_eq!(body["query"],"q");Json(json!({"results":[{"index":0,"relevance_score":0.1},{"index":2,"relevance_score":0.9}]}))}));
        let base = server(app).await;
        let encoder = CrossEncoder::new(&base, "model", "", 0, 0).unwrap();
        assert_eq!(
            encoder.rerank("q", &candidates()).await.unwrap(),
            vec!["c", "a"]
        );
    }
    #[tokio::test]
    async fn cross_encoder_truncates() {
        let app=Router::new().route("/rerank",post(|Json(body):Json<Value>|async move{assert_eq!(body["documents"][0],"abcde");Json(json!({"results":[{"index":0,"relevance_score":0.9},{"index":1,"relevance_score":0.1}]}))}));
        let base = server(app).await;
        let encoder = CrossEncoder::new(&base, "", "", 5, 0).unwrap();
        let values = vec![
            Candidate {
                id: "a".into(),
                content: "abcdefghij".into(),
            },
            Candidate {
                id: "b".into(),
                content: "ok".into(),
            },
        ];
        encoder.rerank("q", &values).await.unwrap();
    }
    #[test]
    fn model_order_parser_deduplicates_and_drops() {
        assert_eq!(
            apply_order("garbage 3, 1, 99, 3", &candidates()),
            vec!["c", "a"]
        );
        assert!(apply_order("none", &candidates()).is_empty());
    }
}
fn truncate_chars(value: &str, max: usize) -> String {
    if max == 0 {
        return value.into();
    }
    value.chars().take(max).collect()
}
fn split_batches(
    documents: &[String],
    query: &str,
    cap: usize,
    model_len: usize,
) -> Vec<(usize, usize)> {
    if cap == 0 {
        return vec![(0, documents.len())];
    }
    let budget = cap
        .saturating_sub(query.chars().count() + 38 + model_len)
        .max(1);
    let (mut output, mut start, mut used) = (Vec::new(), 0, 0);
    for (index, document) in documents.iter().enumerate() {
        let count = document.chars().count();
        if index > start && used + count > budget {
            output.push((start, index));
            start = index;
            used = 0;
        }
        used += count;
    }
    output.push((start, documents.len()));
    output
}
