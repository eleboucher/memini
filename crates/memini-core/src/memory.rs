use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};

pub const CONFIDENCE_SEED_FRESH: f64 = 0.4;
pub const CONFIDENCE_SEED_IMPORTED: f64 = 0.25;
pub const CONFIDENCE_DEMOTE_FLOOR: f64 = 0.35;
const CONFIDENCE_DECAY_PER_WEEK: f64 = 0.05;
const CONFIDENCE_FLOOR: f64 = 0.05;
const RETENTION_HALF_LIFE_SECS: f64 = 7.0 * 24.0 * 60.0 * 60.0;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Tier {
    Working,
    Episodic,
    Semantic,
    Procedural,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Term {
    Short,
    Long,
}

impl Tier {
    pub const fn term(self) -> Term {
        match self {
            Self::Working | Self::Episodic => Term::Short,
            _ => Term::Long,
        }
    }

    pub const fn default_ttl(self) -> Option<Duration> {
        match self {
            Self::Working => Some(Duration::hours(72)),
            Self::Episodic => Some(Duration::days(30)),
            Self::Semantic | Self::Procedural => None,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Level {
    Explicit,
    Deduced,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Memory {
    pub id: String,
    pub namespace: String,
    pub tier: Tier,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub level: Option<Level>,
    pub content: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub summary: String,
    #[serde(default, skip_serializing_if = "Map::is_empty")]
    pub metadata: Map<String, Value>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tags: Vec<String>,
    pub importance: f64,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub last_accessed_at: DateTime<Utc>,
    pub access_count: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub superseded_by: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub valid_from: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub valid_to: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub confidence: Option<f64>,
    // Internal graph-expansion state. Public REST/MCP/export DTOs in the Go
    // implementation never exposed this storage field.
    #[serde(default, skip_serializing)]
    pub linked_memory_ids: Vec<String>,
    #[serde(skip)]
    pub embedding: Vec<f32>,
}

impl Memory {
    pub fn expired(&self, now: DateTime<Utc>) -> bool {
        self.expires_at.is_some_and(|v| v <= now)
    }

    pub fn retention_score(&self, now: DateTime<Utc>) -> f64 {
        self.quality(now, 0.0)
    }

    pub fn salience(&self) -> f64 {
        let weight = match self.tier {
            Tier::Procedural => 0.95,
            Tier::Semantic => 0.90,
            Tier::Episodic => 0.55,
            Tier::Working => 0.30,
        };
        clamp01(weight * (0.5 + 0.5 * clamp01(self.importance)))
    }

    pub fn effective_confidence(&self, now: DateTime<Utc>) -> f64 {
        if self.tier.term() != Term::Long {
            return 1.0;
        }
        let Some(confidence) = self.confidence else {
            return 1.0;
        };
        let base = self.updated_at.max(self.last_accessed_at);
        let weeks = (now - base).num_milliseconds() as f64 / (7.0 * 24.0 * 60.0 * 60.0 * 1000.0);
        decay_confidence(clamp01(confidence), weeks)
    }

    pub fn quality(&self, now: DateTime<Utc>, stability_k: f64) -> f64 {
        if self.tier.term() == Term::Long {
            return self.durable_score(now);
        }
        let age = (now - self.last_accessed_at).num_milliseconds().max(0) as f64 / 1000.0;
        let usage = 1.0 + (self.access_count as f64).ln_1p();
        let stability =
            RETENTION_HALF_LIFE_SECS * (1.0 + stability_k * (self.access_count as f64).ln_1p());
        self.salience() * self.effective_confidence(now) * usage * (-age / stability).exp()
    }

    pub fn durable_score(&self, now: DateTime<Utc>) -> f64 {
        self.salience()
            * self.effective_confidence(now)
            * (1.0 + (self.access_count as f64).ln_1p())
    }

    pub fn recency(&self, now: DateTime<Utc>) -> f64 {
        let age = (now - self.last_accessed_at).num_milliseconds().max(0) as f64 / 1000.0;
        (-age / RETENTION_HALF_LIFE_SECS).exp()
    }
}

pub fn grow_confidence(value: f64) -> f64 {
    clamp01(value + 0.1 * (1.0 - value))
}
fn decay_confidence(value: f64, weeks: f64) -> f64 {
    if weeks <= 1.0 {
        value
    } else {
        CONFIDENCE_FLOOR.max(value - CONFIDENCE_DECAY_PER_WEEK * (weeks - 1.0))
    }
}
pub fn normalize_content(value: &str) -> String {
    value
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
        .to_lowercase()
}
pub fn fingerprint(value: &str) -> String {
    format!("{:x}", Sha256::digest(normalize_content(value).as_bytes()))
}
pub(crate) fn clamp01(value: f64) -> f64 {
    value.clamp(0.0, 1.0)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn memory(tier: Tier, now: DateTime<Utc>) -> Memory {
        Memory {
            id: "id".into(),
            namespace: "ns".into(),
            tier,
            level: None,
            content: "The  SKY is blue".into(),
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
        }
    }

    #[test]
    fn tier_contract() {
        assert_eq!(Tier::Working.term(), Term::Short);
        assert_eq!(Tier::Semantic.term(), Term::Long);
        assert_eq!(Tier::Working.default_ttl(), Some(Duration::hours(72)));
        assert_eq!(Tier::Episodic.default_ttl(), Some(Duration::days(30)));
        assert_eq!(Tier::Semantic.default_ttl(), None);
    }
    #[test]
    fn expiry_boundary_matches_go() {
        let now = Utc::now();
        let mut m = memory(Tier::Working, now);
        assert!(!m.expired(now));
        m.expires_at = Some(now);
        assert!(m.expired(now));
    }
    #[test]
    fn durable_skips_recency_decay() {
        let now = Utc::now();
        let stale = now - Duration::days(60);
        let mut semantic = memory(Tier::Semantic, now);
        semantic.last_accessed_at = stale;
        let mut episodic = memory(Tier::Episodic, now);
        episodic.last_accessed_at = stale;
        assert_eq!(semantic.quality(now, 0.0), semantic.durable_score(now));
        assert!(semantic.quality(now, 0.0) > episodic.quality(now, 0.0));
    }
    #[test]
    fn normalization_and_fingerprint_match_go() {
        assert_eq!(normalize_content(" The  SKY\n is blue "), "the sky is blue");
        assert_eq!(
            fingerprint("The sky is blue"),
            fingerprint(" the  SKY is blue ")
        );
    }
    #[test]
    fn confidence_growth_is_logistic() {
        assert!((grow_confidence(0.4) - 0.46).abs() < 1e-12);
    }
}
