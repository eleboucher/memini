use crate::memory::{Memory, clamp01, normalize_content};
use chrono::{DateTime, Utc};
use std::collections::{HashMap, HashSet};

pub const DEFAULT_RRF_K: f64 = 5.0;
pub const DEFAULT_FUSION_ALPHA: f64 = 0.5;

#[derive(Clone, Debug)]
pub struct Scored {
    pub memory: Memory,
    pub score: f64,
}

#[derive(Clone, Copy, Debug)]
pub struct RerankWeights {
    pub relevance: f64,
    pub recency: f64,
    pub importance: f64,
    pub quality: f64,
}
pub const DEFAULT_RERANK_WEIGHTS: RerankWeights = RerankWeights {
    relevance: 0.80,
    recency: 0.0,
    importance: 0.0,
    quality: 0.20,
};

pub fn rerank_with(
    results: &[Scored],
    now: DateTime<Utc>,
    weights: RerankWeights,
    stability_k: f64,
) -> Vec<Scored> {
    let max_relevance = results.iter().map(|v| v.score).fold(0.0_f64, f64::max);
    let qualities: Vec<_> = results
        .iter()
        .map(|v| v.memory.quality(now, stability_k))
        .collect();
    let max_quality = qualities.iter().copied().fold(0.0_f64, f64::max);
    let mut output: Vec<_> = results
        .iter()
        .zip(qualities)
        .map(|(item, quality)| {
            let relevance = if max_relevance > 0.0 {
                item.score / max_relevance
            } else {
                0.0
            };
            let quality = if max_quality > 0.0 {
                quality / max_quality
            } else {
                0.0
            };
            let score = relevance * (weights.relevance + weights.quality * quality)
                + weights.recency * item.memory.recency(now)
                + weights.importance * clamp01(item.memory.importance);
            Scored {
                memory: item.memory.clone(),
                score,
            }
        })
        .collect();
    output.sort_by(|a, b| b.score.total_cmp(&a.score));
    output
}

pub fn rerank(results: &[Scored], now: DateTime<Utc>, stability_k: f64) -> Vec<Scored> {
    rerank_with(results, now, DEFAULT_RERANK_WEIGHTS, stability_k)
}

pub fn dedup(results: &[Scored], limit: usize) -> Vec<Scored> {
    let mut seen = HashSet::new();
    results
        .iter()
        .filter(|v| seen.insert(normalize_content(&v.memory.content)))
        .take(if limit == 0 { usize::MAX } else { limit })
        .cloned()
        .collect()
}

pub fn fuse(lists: &[Vec<Scored>], limit: usize, rrf_k: f64) -> Vec<Scored> {
    let rrf_k = if rrf_k <= 0.0 { DEFAULT_RRF_K } else { rrf_k };
    let mut entries: Vec<Scored> = vec![];
    let mut indices = HashMap::new();
    for list in lists {
        for (rank, item) in list.iter().enumerate() {
            let index = *indices.entry(item.memory.id.clone()).or_insert_with(|| {
                entries.push(Scored {
                    memory: item.memory.clone(),
                    score: 0.0,
                });
                entries.len() - 1
            });
            entries[index].score += 1.0 / (rrf_k + rank as f64);
        }
    }
    entries.sort_by(|a, b| b.score.total_cmp(&a.score));
    if limit > 0 {
        entries.truncate(limit);
    }
    entries
}

pub fn fuse_scores(lists: &[Vec<Scored>], weights: &[f64], limit: usize) -> Vec<Scored> {
    let mut entries: Vec<Scored> = vec![];
    let mut indices = HashMap::new();
    for (list_index, list) in lists.iter().enumerate() {
        let weight = weights.get(list_index).copied().unwrap_or(1.0);
        let low = list.iter().map(|v| v.score).reduce(f64::min).unwrap_or(0.0);
        let high = list.iter().map(|v| v.score).reduce(f64::max).unwrap_or(0.0);
        let span = high - low;
        for item in list {
            let normalized = if span > 0.0 {
                (item.score - low) / span
            } else {
                1.0
            };
            let index = *indices.entry(item.memory.id.clone()).or_insert_with(|| {
                entries.push(Scored {
                    memory: item.memory.clone(),
                    score: 0.0,
                });
                entries.len() - 1
            });
            entries[index].score += weight * normalized;
        }
    }
    entries.sort_by(|a, b| b.score.total_cmp(&a.score));
    if limit > 0 {
        entries.truncate(limit);
    }
    entries
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::memory::Tier;
    use chrono::Utc;
    use serde_json::Map;

    fn scored(id: &str, content: &str, score: f64) -> Scored {
        let now = Utc::now();
        Scored {
            memory: Memory {
                id: id.into(),
                namespace: "ns".into(),
                tier: Tier::Episodic,
                level: None,
                content: content.into(),
                summary: String::new(),
                metadata: Map::new(),
                tags: vec![],
                importance: 0.5,
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
                embedding: vec![],
            },
            score,
        }
    }

    #[test]
    fn rrf_rewards_agreement_and_deduplicates() {
        let vector = vec![scored("b", "b", 1.0), scored("a", "a", 0.9)];
        let keyword = vec![scored("b", "b", 5.0), scored("c", "c", 4.0)];
        let output = fuse(&[vector, keyword], 10, DEFAULT_RRF_K);
        assert_eq!(
            output
                .iter()
                .map(|v| v.memory.id.as_str())
                .collect::<Vec<_>>(),
            vec!["b", "a", "c"]
        );
    }

    #[test]
    fn dedup_normalizes_and_caps() {
        let input = vec![
            scored("a", "The sky is blue", 1.0),
            scored("b", "the   SKY is blue", 0.9),
            scored("c", "grass is green", 0.8),
        ];
        let output = dedup(&input, 2);
        assert_eq!(
            output
                .iter()
                .map(|v| v.memory.id.as_str())
                .collect::<Vec<_>>(),
            vec!["a", "c"]
        );
    }

    #[test]
    fn rerank_is_stable_for_equal_scores() {
        let now = Utc::now();
        let input = vec![scored("first", "x", 1.0), scored("second", "y", 1.0)];
        let output = rerank(&input, now, 0.0);
        assert_eq!(
            output
                .iter()
                .map(|v| v.memory.id.as_str())
                .collect::<Vec<_>>(),
            vec!["first", "second"]
        );
    }

    #[test]
    fn score_fusion_normalizes_and_weights_each_leg() {
        let vector = vec![scored("v", "v", 0.9), scored("k", "k", 0.1)];
        let keyword = vec![scored("k", "k", 80.0), scored("v", "v", 10.0)];
        let tied = fuse_scores(&[vector.clone(), keyword.clone()], &[0.5, 0.5], 10);
        assert_eq!(tied[0].score, 0.5);
        assert_eq!(tied[1].score, 0.5);
        let weighted = fuse_scores(&[vector, keyword], &[0.9, 0.1], 10);
        assert_eq!(weighted[0].memory.id, "v");
    }

    #[test]
    fn flat_score_leg_maps_every_entry_to_weight() {
        let output = fuse_scores(
            &[vec![scored("a", "a", 7.0), scored("b", "b", 7.0)]],
            &[1.0],
            10,
        );
        assert!(output.iter().all(|v| v.score == 1.0));
    }
}
