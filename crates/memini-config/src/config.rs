use crate::namespace::{NamespaceSource, resolve_default_namespace};
use memini_core::memory::Tier;
use std::{env, time::Duration};
use thiserror::Error;

struct DeprecatedVar {
    name: &'static str,
    guidance: &'static str,
    fatal: bool,
}

const DEPRECATED_VARS: &[DeprecatedVar] = &[
    DeprecatedVar {
        name: "MEMINI_WRITE_DEDUP_MIN_SCORE",
        guidance: "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=coalesce",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_MERGE_HINT_MIN_SCORE",
        guidance: "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=hint (the default)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_AUTO_SUPERSEDE_MIN_SCORE",
        guidance: "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=supersede",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_DEDUP_MIN_CLUSTER_SIZE",
        guidance: "now a fixed internal default (2)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_DEDUP_NEIGHBOURS",
        guidance: "now a fixed internal default (20)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_EMBED_MAX_ITEM_CHARS",
        guidance: "now a fixed internal default (8000); batch-char budgets stay configurable",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_CONSOLIDATE_QUEUE_CAP",
        guidance: "now a fixed internal default (1024)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_NAMESPACE_HEADER",
        guidance: "the header name is fixed to X-Memini-Namespace",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_FUSION_ALPHA",
        guidance: "now a baked retrieval default (0.5); tune via the benchmark harness, not env",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_RECALL_MIN_SEMANTIC_SCORE",
        guidance: "now a baked retrieval default (0, off)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_TEMPORAL_BOOST",
        guidance: "now a baked retrieval default (0.40)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_RERANK_MAX_DOC_CHARS",
        guidance: "now a fixed internal default (2048); MEMINI_RERANK_MAX_BATCH_CHARS remains configurable",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_REDACT_SECRETS",
        guidance: "secret redaction is always on",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_REINFORCE_SKIP_MARKERS",
        guidance: "always on",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_WRITE_DEDUP_FINGERPRINT",
        guidance: "exact-restatement dedup is always on",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_QUARANTINE_GARBLED",
        guidance: "removed; garbled-content downranking is no longer configurable",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_DISTILL_ON_WRITE",
        guidance: "write-time fact building is automatic (LLM when configured, heuristic extractor otherwise)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_EXTRACT_ON_WRITE",
        guidance: "write-time fact building is automatic (LLM when configured, heuristic extractor otherwise)",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_DISTILL_DROP_NO_FACT",
        guidance: "removed; episodic captures are always kept",
        fatal: false,
    },
    DeprecatedVar {
        name: "MEMINI_GLOBAL_NAMESPACE",
        guidance: "the scope model changed: namespaces are now always merged via the ancestor cascade, replacing the old opt-in global namespace. Run `memini migrate scopes` to fold any <tenant>/_shared data forward, and adopt the old global namespace via MEMINI_HOME (single-operator) or `memini link add <ns> <old-global>` (team-wide, per namespace that needs it) — see docs/scopes.md#knobs",
        fatal: true,
    },
    DeprecatedVar {
        name: "MEMINI_TENANT_SHARED",
        guidance: "the scope model changed: namespaces are now always merged via the ancestor cascade, replacing the old opt-in tenant-shared merge. Run `memini migrate scopes` to fold each <tenant>/_shared namespace into <tenant>, and adopt MEMINI_HOME or `memini link add` if you also relied on a global namespace — see docs/scopes.md#knobs",
        fatal: true,
    },
];

fn deprecated_messages(is_set: impl Fn(&str) -> bool, fatal: bool) -> Vec<String> {
    DEPRECATED_VARS
        .iter()
        .filter(|item| item.fatal == fatal && is_set(item.name))
        .map(|item| {
            if fatal {
                format!("{} is set; {}", item.name, item.guidance)
            } else {
                format!("{} is removed and ignored; {}", item.name, item.guidance)
            }
        })
        .collect()
}

pub fn deprecation_warnings() -> Vec<String> {
    deprecated_messages(|name| env::var_os(name).is_some(), false)
}

pub fn fatal_deprecated_vars() -> Vec<String> {
    deprecated_messages(|name| env::var_os(name).is_some(), true)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Backend {
    Sqlite,
    Postgres,
}

/// Environment variables consumed by the server and their documented defaults.
///
/// The docs generator validates the configuration reference against this list,
/// making additions and default changes fail the docs drift check.
pub const CONFIG_VARIABLES: &[(&str, &str)] = &[
    ("MEMINI_HTTP_ADDR", ":8080"),
    ("MEMINI_SHUTDOWN_TIMEOUT", "15s"),
    ("MEMINI_REQUEST_TIMEOUT", "60s"),
    ("MEMINI_METRICS_ADDR", ""),
    ("MEMINI_UI_ADDR", ""),
    ("MEMINI_UI_ENABLED", "true"),
    ("MEMINI_LOG_LEVEL", "info"),
    ("MEMINI_LOG_FORMAT", "json"),
    ("MEMINI_BACKEND", "sqlite"),
    ("MEMINI_SQLITE_PATH", "memini.db"),
    ("MEMINI_POSTGRES_DSN", ""),
    ("MEMINI_EMBED_BASE_URL", ""),
    ("MEMINI_EMBED_API_KEY", ""),
    ("MEMINI_EMBED_MODEL", "text-embedding-3-small"),
    ("MEMINI_EMBED_DIMS", "1536"),
    ("MEMINI_EMBED_QUERY_PREFIX", ""),
    ("MEMINI_EMBED_MAX_BATCH", "20"),
    ("MEMINI_EMBED_MAX_BATCH_CHARS", "24000"),
    ("MEMINI_EMBED_MAX_CONCURRENCY", "0"),
    ("MEMINI_REEMBED_ON_MODEL_CHANGE", "false"),
    ("MEMINI_LLM_BASE_URL", ""),
    ("MEMINI_LLM_API_KEY", ""),
    ("MEMINI_LLM_MODEL", "gpt-4o-mini"),
    ("MEMINI_LLM_API", "openai"),
    ("MEMINI_RERANK", "off"),
    ("MEMINI_RERANK_MODEL", ""),
    ("MEMINI_RERANK_API_KEY", ""),
    ("MEMINI_RERANK_POOL", "0"),
    ("MEMINI_RERANK_TIMEOUT", "10s"),
    ("MEMINI_RERANK_MAX_BATCH_CHARS", "6000"),
    ("MEMINI_RERANK_MAX_CONCURRENCY", "0"),
    ("MEMINI_ACTIVITY_LOG", "true"),
    ("MEMINI_ACTIVITY_RETENTION", "720h"),
    ("MEMINI_ACTIVITY_MAX_ROWS", "100000"),
    ("MEMINI_RECALL_MIN_SCORE", "0.1"),
    ("MEMINI_RECALL_SEMANTIC_RESERVE", "2"),
    ("MEMINI_RECALL_EMBED_TIMEOUT", "2s"),
    ("MEMINI_RECALL_REWRITE_TIMEOUT", "3s"),
    ("MEMINI_STABILITY_K", "1"),
    ("MEMINI_TURN_ECHO_WINDOW", "5m"),
    ("MEMINI_CASCADE", "true"),
    ("MEMINI_WRITE_EMBED_TIMEOUT", "5s"),
    ("MEMINI_WRITE_DEDUP_SCORE", "0.625"),
    ("MEMINI_WRITE_DEDUP_ACTION", "hint"),
    ("MEMINI_SPLIT_DEDUP_LLM_MERGE", "false"),
    ("MEMINI_CONTRADICT_DOWNRANK", "true"),
    ("MEMINI_EPISODIC_MIN_CHARS", "120"),
    ("MEMINI_CONSOLIDATE_MODE", "async"),
    ("MEMINI_CONSOLIDATE_MIN_SCORE", "0.3"),
    ("MEMINI_DISTILL_BATCH_TOKENS", "1024"),
    ("MEMINI_DISTILL_BATCH_MAX_AGE", "10m"),
    ("MEMINI_PROMOTE_INTERVAL", "24h"),
    ("MEMINI_PROMOTE_MIN_ACCESS", "3"),
    ("MEMINI_BACKFILL_INTERVAL", "1m"),
    ("MEMINI_SWEEP_INTERVAL", "1h"),
    ("MEMINI_SHORT_TERM_CAP", "1000"),
    ("MEMINI_DEMOTE_AFTER", "168h"),
    ("MEMINI_TOMBSTONE_TTL", "0"),
    ("MEMINI_DEDUP_INTERVAL", "24h"),
    ("MEMINI_DEDUP_SIMILARITY", "0.85"),
    ("MEMINI_DEDUP_TIERS", ""),
    ("MEMINI_DEDUP_LLM_MERGE", "false"),
    ("MEMINI_API_KEY", ""),
    ("MEMINI_API_KEYS_FILE", ""),
    ("MEMINI_DEFAULT_NAMESPACE", "auto"),
    ("MEMINI_NAMESPACE", ""),
    ("MEMINI_AGENT", ""),
    ("MEMINI_HOME", ""),
];

#[derive(Clone, Debug)]
pub struct Config {
    pub http_addr: String,
    pub shutdown_timeout: Duration,
    pub request_timeout: Duration,
    pub metrics_addr: String,
    pub ui_addr: String,
    pub log_level: String,
    pub log_format: String,
    pub backend: Backend,
    pub sqlite_path: String,
    pub postgres_dsn: String,
    pub embed_base_url: String,
    pub embed_api_key: String,
    pub embed_model: String,
    pub embed_dims: i64,
    pub embed_query_prefix: String,
    pub embed_max_batch: i64,
    pub embed_max_batch_chars: i64,
    pub embed_max_concurrency: i64,
    pub reembed_on_model_change: bool,
    pub write_dedup_score: f64,
    pub write_dedup_action: String,
    pub split_dedup_llm_merge: bool,
    pub contradiction_downrank: bool,
    pub cascade: bool,
    pub llm_base_url: String,
    pub llm_api_key: String,
    pub llm_model: String,
    pub llm_api: String,
    pub rerank: String,
    pub rerank_model: String,
    pub rerank_api_key: String,
    pub rerank_pool: i64,
    pub rerank_max_batch_chars: i64,
    pub rerank_timeout: Duration,
    pub rerank_max_concurrency: i64,
    pub recall_embed_timeout: Duration,
    pub recall_rewrite_timeout: Duration,
    pub write_embed_timeout: Duration,
    pub recall_min_score: f64,
    pub recall_semantic_reserve: i64,
    pub stability_k: f64,
    pub turn_echo_window: Duration,
    pub episodic_min_chars: i64,
    pub distill_batch_tokens: i64,
    pub distill_batch_max_age: Duration,
    pub consolidate_mode: String,
    pub consolidate_min_score: f64,
    pub promote_interval: Duration,
    pub promote_min_access: i64,
    pub backfill_interval: Duration,
    pub sweep_interval: Duration,
    pub short_term_cap: i64,
    pub tombstone_ttl: Duration,
    pub activity_log: bool,
    pub activity_retention: Duration,
    pub activity_max_rows: i64,
    pub demote_after: Duration,
    pub dedup_interval: Duration,
    pub dedup_similarity: f64,
    pub dedup_tiers: String,
    pub dedup_llm_merge: bool,
    pub ui_enabled: bool,
    pub api_key: String,
    pub api_keys_file: String,
    pub home: String,
    pub default_namespace: String,
    pub namespace_source: NamespaceSource,
}

#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("parsing config from environment: {name}: {message}")]
    Parse { name: &'static str, message: String },
    #[error("{0}")]
    Validation(String),
}

impl Config {
    pub fn load() -> Result<Self, ConfigError> {
        let (namespace, source) = resolve_default_namespace();
        Self::from_lookup(|name| env::var(name).ok(), namespace, source)
    }

    pub fn from_lookup(
        get: impl Fn(&str) -> Option<String>,
        default_namespace: String,
        namespace_source: NamespaceSource,
    ) -> Result<Self, ConfigError> {
        macro_rules! s {
            ($n:literal,$d:literal) => {
                get($n).unwrap_or_else(|| $d.into())
            };
        }
        macro_rules! p {
            ($n:literal,$d:literal,$t:ty) => {{
                let v = s!($n, $d);
                v.parse::<$t>().map_err(|e| ConfigError::Parse {
                    name: $n,
                    message: e.to_string(),
                })?
            }};
        }
        macro_rules! d {
            ($n:literal,$v:literal) => {{
                let v = s!($n, $v);
                humantime::parse_duration(&v).map_err(|e| ConfigError::Parse {
                    name: $n,
                    message: e.to_string(),
                })?
            }};
        }
        let backend = match s!("MEMINI_BACKEND", "sqlite").as_str() {
            "sqlite" => Backend::Sqlite,
            "postgres" => Backend::Postgres,
            v => {
                return Err(ConfigError::Validation(format!(
                    "unknown MEMINI_BACKEND {v:?} (want sqlite|postgres)"
                )));
            }
        };
        let result = Self {
            http_addr: s!("MEMINI_HTTP_ADDR", ":8080"),
            shutdown_timeout: d!("MEMINI_SHUTDOWN_TIMEOUT", "15s"),
            request_timeout: d!("MEMINI_REQUEST_TIMEOUT", "60s"),
            metrics_addr: s!("MEMINI_METRICS_ADDR", ""),
            ui_addr: s!("MEMINI_UI_ADDR", ""),
            log_level: s!("MEMINI_LOG_LEVEL", "info"),
            log_format: s!("MEMINI_LOG_FORMAT", "json"),
            backend,
            sqlite_path: s!("MEMINI_SQLITE_PATH", "memini.db"),
            postgres_dsn: s!("MEMINI_POSTGRES_DSN", ""),
            embed_base_url: s!("MEMINI_EMBED_BASE_URL", ""),
            embed_api_key: s!("MEMINI_EMBED_API_KEY", ""),
            embed_model: s!("MEMINI_EMBED_MODEL", "text-embedding-3-small"),
            embed_dims: p!("MEMINI_EMBED_DIMS", "1536", i64),
            embed_query_prefix: s!("MEMINI_EMBED_QUERY_PREFIX", ""),
            embed_max_batch: p!("MEMINI_EMBED_MAX_BATCH", "20", i64),
            embed_max_batch_chars: p!("MEMINI_EMBED_MAX_BATCH_CHARS", "24000", i64),
            embed_max_concurrency: p!("MEMINI_EMBED_MAX_CONCURRENCY", "0", i64),
            reembed_on_model_change: p!("MEMINI_REEMBED_ON_MODEL_CHANGE", "false", bool),
            write_dedup_score: p!("MEMINI_WRITE_DEDUP_SCORE", "0.625", f64),
            write_dedup_action: s!("MEMINI_WRITE_DEDUP_ACTION", "hint"),
            split_dedup_llm_merge: p!("MEMINI_SPLIT_DEDUP_LLM_MERGE", "false", bool),
            contradiction_downrank: p!("MEMINI_CONTRADICT_DOWNRANK", "true", bool),
            cascade: p!("MEMINI_CASCADE", "true", bool),
            llm_base_url: s!("MEMINI_LLM_BASE_URL", ""),
            llm_api_key: s!("MEMINI_LLM_API_KEY", ""),
            llm_model: s!("MEMINI_LLM_MODEL", "gpt-4o-mini"),
            llm_api: s!("MEMINI_LLM_API", "openai"),
            rerank: s!("MEMINI_RERANK", "off"),
            rerank_model: s!("MEMINI_RERANK_MODEL", ""),
            rerank_api_key: s!("MEMINI_RERANK_API_KEY", ""),
            rerank_pool: p!("MEMINI_RERANK_POOL", "0", i64),
            rerank_max_batch_chars: p!("MEMINI_RERANK_MAX_BATCH_CHARS", "6000", i64),
            rerank_timeout: d!("MEMINI_RERANK_TIMEOUT", "10s"),
            rerank_max_concurrency: p!("MEMINI_RERANK_MAX_CONCURRENCY", "0", i64),
            recall_embed_timeout: d!("MEMINI_RECALL_EMBED_TIMEOUT", "2s"),
            recall_rewrite_timeout: d!("MEMINI_RECALL_REWRITE_TIMEOUT", "3s"),
            write_embed_timeout: d!("MEMINI_WRITE_EMBED_TIMEOUT", "5s"),
            recall_min_score: p!("MEMINI_RECALL_MIN_SCORE", "0.1", f64),
            recall_semantic_reserve: p!("MEMINI_RECALL_SEMANTIC_RESERVE", "2", i64),
            stability_k: p!("MEMINI_STABILITY_K", "1", f64),
            turn_echo_window: d!("MEMINI_TURN_ECHO_WINDOW", "5m"),
            episodic_min_chars: p!("MEMINI_EPISODIC_MIN_CHARS", "120", i64),
            distill_batch_tokens: p!("MEMINI_DISTILL_BATCH_TOKENS", "1024", i64),
            distill_batch_max_age: d!("MEMINI_DISTILL_BATCH_MAX_AGE", "10m"),
            consolidate_mode: s!("MEMINI_CONSOLIDATE_MODE", "async"),
            consolidate_min_score: p!("MEMINI_CONSOLIDATE_MIN_SCORE", "0.3", f64),
            promote_interval: d!("MEMINI_PROMOTE_INTERVAL", "24h"),
            promote_min_access: p!("MEMINI_PROMOTE_MIN_ACCESS", "3", i64),
            backfill_interval: d!("MEMINI_BACKFILL_INTERVAL", "1m"),
            sweep_interval: d!("MEMINI_SWEEP_INTERVAL", "1h"),
            short_term_cap: p!("MEMINI_SHORT_TERM_CAP", "1000", i64),
            tombstone_ttl: d!("MEMINI_TOMBSTONE_TTL", "0"),
            activity_log: p!("MEMINI_ACTIVITY_LOG", "true", bool),
            activity_retention: d!("MEMINI_ACTIVITY_RETENTION", "720h"),
            activity_max_rows: p!("MEMINI_ACTIVITY_MAX_ROWS", "100000", i64),
            demote_after: d!("MEMINI_DEMOTE_AFTER", "168h"),
            dedup_interval: d!("MEMINI_DEDUP_INTERVAL", "24h"),
            dedup_similarity: p!("MEMINI_DEDUP_SIMILARITY", "0.85", f64),
            dedup_tiers: s!("MEMINI_DEDUP_TIERS", ""),
            dedup_llm_merge: p!("MEMINI_DEDUP_LLM_MERGE", "false", bool),
            ui_enabled: p!("MEMINI_UI_ENABLED", "true", bool),
            api_key: s!("MEMINI_API_KEY", ""),
            api_keys_file: s!("MEMINI_API_KEYS_FILE", ""),
            home: s!("MEMINI_HOME", ""),
            default_namespace,
            namespace_source,
        };
        result.validate()?;
        Ok(result)
    }
    pub fn llm_enabled(&self) -> bool {
        !self.llm_base_url.is_empty()
    }
    pub fn rerank_enabled(&self) -> bool {
        !self.rerank.is_empty() && self.rerank != "off"
    }
    pub fn rerank_is_llm(&self) -> bool {
        self.rerank == "llm"
    }
    pub fn dedup_tier_list(&self) -> Result<Vec<Tier>, ConfigError> {
        self.dedup_tiers.split(',').filter_map(|v|{let v=v.trim();(!v.is_empty()).then_some(v)}).map(|v|match v{"working"=>Ok(Tier::Working),"episodic"=>Ok(Tier::Episodic),"semantic"=>Ok(Tier::Semantic),"procedural"=>Ok(Tier::Procedural),_=>Err(ConfigError::Validation(format!("unknown tier {v:?} in MEMINI_DEDUP_TIERS (want working|episodic|semantic|procedural)")))}).collect()
    }
    fn validate(&self) -> Result<(), ConfigError> {
        let err = |v| Err(ConfigError::Validation(v));
        if self.backend == Backend::Postgres && self.postgres_dsn.is_empty() {
            return err("MEMINI_POSTGRES_DSN is required when MEMINI_BACKEND=postgres".into());
        }
        if self.embed_dims <= 0 {
            return err(format!(
                "MEMINI_EMBED_DIMS must be positive, got {}",
                self.embed_dims
            ));
        }
        if self.sweep_interval.is_zero() {
            return err("MEMINI_SWEEP_INTERVAL must be positive, got 0s".into());
        }
        if !matches!(self.consolidate_mode.as_str(), "async" | "sync" | "off") {
            return err(format!(
                "unknown MEMINI_CONSOLIDATE_MODE {:?} (want async|sync|off)",
                self.consolidate_mode
            ));
        }
        for (n, v) in [
            ("MEMINI_DEDUP_SIMILARITY", self.dedup_similarity),
            ("MEMINI_WRITE_DEDUP_SCORE", self.write_dedup_score),
            ("MEMINI_RECALL_MIN_SCORE", self.recall_min_score),
            ("MEMINI_CONSOLIDATE_MIN_SCORE", self.consolidate_min_score),
        ] {
            if !(0.0..=1.0).contains(&v) {
                return err(format!("{n} must be in [0,1], got {v}"));
            }
        }
        self.dedup_tier_list()?;
        for (n, v) in [
            (
                "MEMINI_RECALL_SEMANTIC_RESERVE",
                self.recall_semantic_reserve,
            ),
            ("MEMINI_EPISODIC_MIN_CHARS", self.episodic_min_chars),
            ("MEMINI_DISTILL_BATCH_TOKENS", self.distill_batch_tokens),
        ] {
            if v < 0 {
                return err(format!("{n} must be >= 0, got {v}"));
            }
        }
        if self.stability_k < 0.0 {
            return err(format!(
                "MEMINI_STABILITY_K must be >= 0, got {}",
                self.stability_k
            ));
        }
        if self.distill_batch_tokens > 0 && self.distill_batch_max_age.is_zero() {
            return err(
                "MEMINI_DISTILL_BATCH_MAX_AGE must be positive when batching is on, got 0s".into(),
            );
        }
        if !matches!(
            self.write_dedup_action.as_str(),
            "off" | "hint" | "coalesce" | "supersede"
        ) {
            return err(format!(
                "unknown MEMINI_WRITE_DEDUP_ACTION {:?} (want off|hint|coalesce|supersede)",
                self.write_dedup_action
            ));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    fn load(v: &[(&str, &str)]) -> Result<Config, ConfigError> {
        let m: HashMap<_, _> = v
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        Config::from_lookup(
            |n| m.get(n).cloned(),
            "test".into(),
            NamespaceSource::Fallback,
        )
    }
    #[test]
    fn defaults_match_go() {
        let c = load(&[]).unwrap();
        assert_eq!(c.http_addr, ":8080");
        assert_eq!(c.backend, Backend::Sqlite);
        assert_eq!(c.embed_dims, 1536);
        assert_eq!(c.write_dedup_score, 0.625);
        assert_eq!(c.sweep_interval, Duration::from_secs(3600));
        assert_eq!(c.consolidate_mode, "async");
        assert_eq!(c.rerank_max_batch_chars, 6000);
        assert!(c.ui_enabled);
        assert!(!c.llm_enabled())
    }
    #[test]
    fn overrides_match_go() {
        let c = load(&[
            ("MEMINI_HTTP_ADDR", ":9999"),
            ("MEMINI_EMBED_DIMS", "256"),
            ("MEMINI_SWEEP_INTERVAL", "5m"),
            ("MEMINI_LLM_BASE_URL", "http://localhost"),
        ])
        .unwrap();
        assert_eq!(c.http_addr, ":9999");
        assert_eq!(c.embed_dims, 256);
        assert_eq!(c.sweep_interval, Duration::from_secs(300));
        assert!(c.llm_enabled())
    }
    #[test]
    fn validation_rejects_reference_failures() {
        for v in [
            vec![("MEMINI_BACKEND", "postgres")],
            vec![("MEMINI_EMBED_DIMS", "0")],
            vec![("MEMINI_CONSOLIDATE_MODE", "eventually")],
            vec![("MEMINI_DEDUP_SIMILARITY", "1.5")],
            vec![("MEMINI_DEDUP_TIERS", "semantic,bogus")],
            vec![("MEMINI_WRITE_DEDUP_ACTION", "merge")],
        ] {
            assert!(load(&v).is_err(), "accepted {v:?}")
        }
    }
    #[test]
    fn tier_lists_match_go() {
        assert!(load(&[]).unwrap().dedup_tier_list().unwrap().is_empty());
        assert_eq!(
            load(&[("MEMINI_DEDUP_TIERS", " semantic , episodic, ")])
                .unwrap()
                .dedup_tier_list()
                .unwrap(),
            vec![Tier::Semantic, Tier::Episodic]
        )
    }

    #[test]
    fn deprecated_variable_contract_matches_go() {
        let set = ["MEMINI_GLOBAL_NAMESPACE", "MEMINI_WRITE_DEDUP_MIN_SCORE"];
        let fatal = deprecated_messages(|name| set.contains(&name), true);
        let warnings = deprecated_messages(|name| set.contains(&name), false);
        assert_eq!(fatal.len(), 1);
        assert!(fatal[0].contains("MEMINI_GLOBAL_NAMESPACE"));
        assert!(fatal[0].contains("ancestor cascade"));
        assert!(fatal[0].contains("memini migrate scopes"));
        assert!(fatal[0].contains("docs/scopes.md#knobs"));
        assert_eq!(warnings.len(), 1);
        assert!(warnings[0].contains("removed and ignored"));
    }
}
