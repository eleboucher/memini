use anyhow::{Context, Result, bail};
#[cfg(feature = "gendocs")]
use clap::CommandFactory;
use clap::{Args, Parser, Subcommand};
use memini_config::config::{Backend, Config, deprecation_warnings, fatal_deprecated_vars};
use memini_embed::{Batched, Disabled, Embedder, OpenAiEmbedder};
use memini_service::{ListInput, Service};
use memini_store::{ApiKey, ApiKeyStore, EventLogStore, Filter, LinkStore, NamespaceLink, Store};
use std::{path::PathBuf, sync::Arc};

#[cfg(feature = "gendocs")]
mod docs;

#[derive(serde::Serialize)]
struct ExportRecord {
    id: String,
    namespace: String,
    tier: memini_core::memory::Tier,
    content: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    summary: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    tags: Vec<String>,
    #[serde(skip_serializing_if = "serde_json::Map::is_empty")]
    metadata: serde_json::Map<String, serde_json::Value>,
    importance: f64,
    created_at: chrono::DateTime<chrono::Utc>,
    updated_at: chrono::DateTime<chrono::Utc>,
    #[serde(skip_serializing_if = "Option::is_none")]
    expires_at: Option<chrono::DateTime<chrono::Utc>>,
}
#[derive(serde::Serialize)]
struct ExportLink {
    src: String,
    dst: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    tiers: Vec<memini_core::memory::Tier>,
    #[serde(skip_serializing_if = "String::is_empty")]
    note: String,
    created_at: chrono::DateTime<chrono::Utc>,
}
#[derive(serde::Serialize)]
struct ExportDocument {
    memories: Vec<ExportRecord>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    links: Vec<ExportLink>,
}

#[derive(Parser)]
#[command(
    name = "memini",
    version,
    about = "memini — a memory service for AI agents"
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
}
#[derive(Subcommand)]
enum Command {
    /// Print version information.
    Version,
    /// Diagnose namespace mismatches and store health.
    Doctor {
        /// Remediate diagnosed problems (preview unless --yes).
        #[arg(long)]
        fix: bool,
        /// Remove content junk and exact duplicates (preview unless --yes).
        #[arg(long)]
        scrub: bool,
        /// Apply changes instead of previewing them.
        #[arg(long)]
        yes: bool,
    },
    /// Export memories to memini's portable, re-importable JSON format.
    Export {
        /// Output path; omit to write stdout.
        file: Option<PathBuf>,
        /// Namespace to export (defaults to the resolved default).
        #[arg(long)]
        namespace: Option<String>,
        /// Output path; omit to write stdout.
        #[arg(short, long)]
        output: Option<PathBuf>,
        /// Export every namespace.
        #[arg(long)]
        all_namespaces: bool,
        /// Include memories past their TTL.
        #[arg(long)]
        include_expired: bool,
        /// Include contradiction-tombstoned memories.
        #[arg(long)]
        include_superseded: bool,
        /// Indent the JSON output.
        #[arg(long)]
        pretty: bool,
        /// Restrict to these tiers (repeatable).
        #[arg(long)]
        tier: Vec<String>,
        /// Require this exact tag (repeatable, AND).
        #[arg(long)]
        tag: Vec<String>,
        /// Require metadata key=value (repeatable, AND).
        #[arg(long = "meta")]
        metadata: Vec<String>,
    },
    /// Bulk-load an export from another memory system.
    Import(ImportArgs),
    /// Bulk-delete memories by tag (for example, to undo an import).
    Forget {
        /// Namespace to delete from (defaults to the resolved default).
        #[arg(long)]
        namespace: Option<String>,
        /// Delete memories carrying this exact tag.
        #[arg(long)]
        tag: String,
    },
    /// Manage local API keys in this store's api_keys table.
    Key {
        #[command(subcommand)]
        command: KeyCommand,
    },
    /// Manage cross-namespace durable-tier read links.
    Link {
        #[command(subcommand)]
        command: LinkCommand,
    },
    /// Inspect and repair memory namespaces.
    Namespace {
        #[command(subcommand)]
        command: NamespaceCommand,
    },
    /// Seed confidence on legacy durable memories.
    Backfill {
        /// Apply the backfill (default is a dry run).
        #[arg(long)]
        yes: bool,
    },
    /// Run one-shot data migrations between memini scope models.
    Migrate {
        #[command(subcommand)]
        command: MigrateCommand,
    },
    /// Re-embed memories with the configured embedding model.
    Reembed {
        /// Limit the operation to one namespace.
        #[arg(long)]
        namespace: Option<String>,
        /// Apply the rewrite (default is a dry run).
        #[arg(long)]
        yes: bool,
        /// Memories embedded per request (0 uses the default).
        #[arg(long, default_value_t = 0)]
        batch: usize,
    },
    /// Serve MCP tools over stdio.
    Mcp,
    #[cfg(feature = "gendocs")]
    GenDocs {
        #[arg(long, default_value = "docs/reference/cli.md")]
        out: PathBuf,
    },
}
#[derive(Args)]
struct ImportArgs {
    /// Input path; omit or use - to read stdin.
    file: Option<PathBuf>,
    /// Export format: agentmemory, claude-code, mem0, memini, or mnemory.
    #[arg(long, default_value = "memini")]
    source: String,
    /// Fallback namespace for records whose source carries none.
    #[arg(long)]
    namespace: Option<String>,
    /// Force every record into one namespace.
    #[arg(long)]
    merge_into: Option<String>,
    /// Parse and report without writing.
    #[arg(long)]
    dry_run: bool,
    /// Skip exact and vector-cluster deduplication.
    #[arg(long)]
    no_dedup: bool,
    /// Skip content shorter than this many bytes (0 disables).
    #[arg(long = "min-length", default_value_t = 20)]
    min_content_len: usize,
    /// Skip records below this importance (0 disables).
    #[arg(long, default_value_t = 0.0)]
    min_importance: f64,
    /// Records per batch (0 uses the backend default).
    #[arg(long, default_value_t = 0)]
    batch: usize,
    /// Default importance for records whose source carries none.
    #[arg(long, default_value_t = 0.25)]
    importance: f64,
    /// Durable confidence seed; negative uses the import default.
    #[arg(long, default_value_t = -1.0)]
    confidence: f64,
    /// Similarity threshold for the post-import dedup pass.
    #[arg(long, default_value_t = 0.85)]
    dedup_similarity: f64,
    /// Extract decisions, preferences, and problems from conversations.
    #[arg(long)]
    extract: bool,
    /// Target a running memini server instead of the local store.
    #[arg(long)]
    remote: Option<String>,
    /// Remote bearer token (defaults to MEMINI_API_KEY).
    #[arg(long)]
    token: Option<String>,
    /// Skip merge-into confirmation.
    #[arg(long)]
    yes: bool,
}
#[derive(Subcommand)]
enum KeyCommand {
    /// Create an API key, or rotate an existing key's secret.
    Add {
        name: String,
        /// Bind the key to a home namespace; an explicit empty value clears it.
        #[arg(long)]
        home: Option<String>,
        /// Default namespace when a request supplies no namespace header.
        #[arg(long)]
        default_namespace: Option<String>,
        /// Set or clear the disabled state.
        #[arg(long, num_args=0..=1, default_missing_value="true")]
        disabled: Option<bool>,
    },
    /// Delete an API key.
    Rm { name: String },
    /// List API keys without secrets or hashes.
    Ls,
}
#[derive(Subcommand)]
enum LinkCommand {
    /// Create or replace a read link from source to destination.
    Add {
        source: String,
        destination: String,
        /// Comma-separated admitted tiers (default: durable tiers).
        #[arg(long, value_delimiter = ',')]
        tiers: Vec<String>,
        /// Free-text reason for the link.
        #[arg(long, default_value = "")]
        note: String,
    },
    /// Remove a read link.
    Rm { source: String, destination: String },
    /// List links, optionally from one source namespace.
    Ls { source: Option<String> },
}
#[derive(Subcommand)]
enum NamespaceCommand {
    /// List namespaces and memory counts.
    List,
    /// Move every memory from one namespace to another.
    Move {
        /// Source namespace.
        #[arg(long)]
        from: String,
        /// Destination namespace.
        #[arg(long)]
        to: String,
        /// Report what would move without writing.
        #[arg(long)]
        dry_run: bool,
    },
    /// Regroup a pooled namespace into namespaces derived from metadata.
    Split {
        /// Namespace to split.
        #[arg(long = "from")]
        namespace: String,
        /// Comma-separated metadata keys to group by.
        #[arg(
            long,
            value_delimiter = ',',
            default_value = "import_source_namespace,user_id,agent_id,run_id,project"
        )]
        by: Vec<String>,
        /// Report the split without writing.
        #[arg(long)]
        dry_run: bool,
    },
}
#[derive(Subcommand)]
enum MigrateCommand {
    /// Merge legacy <tenant>/_shared namespaces into their parent cascade layer.
    Scopes {
        /// Apply the migration and post-merge dedup pass.
        #[arg(long)]
        yes: bool,
    },
}

struct Stack {
    service: Arc<Service>,
    store: Arc<dyn Store>,
    keys: Arc<dyn ApiKeyStore>,
    links: Arc<dyn LinkStore>,
    embedder: Arc<dyn Embedder>,
    embed_enabled: bool,
    metrics: memini_observability::Registry,
    dependencies: memini_observability::DependencyTracker,
}
type Stores = (
    Arc<dyn Store>,
    Arc<dyn ApiKeyStore>,
    Arc<dyn LinkStore>,
    Arc<dyn EventLogStore>,
);
async fn stack(config: &Config, allow_model_mismatch: bool) -> Result<Stack> {
    let metrics = memini_observability::Registry::default();
    let dependencies = memini_observability::DependencyTracker::default();
    let dims = config.embed_dims as usize;
    let (store, keys, links, events): Stores = match config.backend {
        Backend::Sqlite => {
            let value = Arc::new(memini_sqlite::SqliteStore::open(&config.sqlite_path, dims)?);
            (value.clone(), value.clone(), value.clone(), value)
        }
        Backend::Postgres => {
            let value =
                Arc::new(memini_postgres::PostgresStore::open(&config.postgres_dsn, dims).await?);
            (value.clone(), value.clone(), value.clone(), value)
        }
    };
    let embedder: Arc<dyn Embedder> = if config.embed_base_url.is_empty() {
        memini_embed::observed(
            Arc::new(Disabled { dimensions: dims }),
            "disabled",
            metrics.clone(),
            dependencies.clone(),
        )
    } else {
        let client: Arc<dyn Embedder> = memini_embed::observed(
            Arc::new(OpenAiEmbedder::new(
                &config.embed_base_url,
                &config.embed_api_key,
                &config.embed_model,
                dims,
            )?),
            "openai",
            metrics.clone(),
            dependencies.clone(),
        );
        let gauge = metrics.clone();
        let limited = memini_embed::limited(
            client,
            config.embed_max_concurrency.max(0) as usize,
            Some(Arc::new(move |count| {
                gauge.set("memini_embed_in_flight", &[], count as f64)
            })),
        );
        let batched = memini_embed::observed(
            Arc::new(Batched::new(
                limited,
                config.embed_max_batch as usize,
                config.embed_max_batch_chars as usize,
                8000,
            )),
            "batched",
            metrics.clone(),
            dependencies.clone(),
        );
        memini_embed::observed(
            Arc::new(memini_embed::Cached::new(batched, 4096)?),
            "cached",
            metrics.clone(),
            dependencies.clone(),
        )
    };
    let recorded_model = store.embed_model().await?;
    if recorded_model.is_empty() {
        store.set_embed_model(&config.embed_model).await?;
    } else if recorded_model != config.embed_model && !allow_model_mismatch {
        if !config.reembed_on_model_change {
            bail!(
                "store was created with embedding model {:?} but is configured for {:?}; vectors from different models are not comparable. Set MEMINI_EMBED_MODEL={} to match, run `memini reembed`, or set MEMINI_REEMBED_ON_MODEL_CHANGE=true",
                recorded_model,
                config.embed_model,
                recorded_model
            );
        }
        if config.embed_base_url.is_empty() {
            bail!(
                "MEMINI_REEMBED_ON_MODEL_CHANGE is set but no MEMINI_EMBED_BASE_URL is configured"
            )
        }
        reembed_store(store.as_ref(), embedder.as_ref(), None, 64).await?;
        store.set_embed_model(&config.embed_model).await?;
    }
    let mut service = Service::new(store.clone(), embedder.clone())
        .with_link_store(links.clone())
        .with_api_key_store(keys.clone())
        .with_cascade(config.cascade)
        .with_corroboration(0.70)
        .with_contradiction_downrank(if config.contradiction_downrank {
            0.625
        } else {
            0.0
        })
        .with_temporal_boost(0.40)
        .with_stability(config.stability_k)
        .with_split_dedup_llm_merge(config.split_dedup_llm_merge)
        .with_dedup_llm_merge(config.dedup_llm_merge)
        .with_turn_echo_window(chrono::Duration::from_std(config.turn_echo_window).unwrap())
        .with_episodic_min_chars(config.episodic_min_chars.max(0) as usize)
        .with_semantic_reserve(config.recall_semantic_reserve.max(0) as usize)
        .with_write_dedup(config.write_dedup_score, config.write_dedup_action.clone())
        .with_distill_on_write(config.llm_enabled())
        .with_distill_batch(
            config.distill_batch_tokens.max(0) as usize,
            chrono::Duration::from_std(config.distill_batch_max_age).unwrap_or_default(),
        )
        .with_extract_on_write(true)
        .with_promote_min_access(config.promote_min_access)
        .with_query_prefix(config.embed_query_prefix.clone())
        .with_min_scores(config.recall_min_score, 0.0)
        .with_embed_timeouts(
            Some(config.recall_embed_timeout),
            Some(config.write_embed_timeout),
        )
        .with_rewrite_timeout(Some(config.recall_rewrite_timeout));
    service = service.with_metrics(metrics.clone());
    if config.activity_log {
        service = service.with_event_store(events.clone());
    }
    let llm_client: Option<Arc<dyn memini_llm::Client>> = if config.llm_enabled() {
        let raw: Arc<dyn memini_llm::Client> = if config.llm_api == "anthropic" {
            Arc::new(memini_llm::AnthropicClient::new(
                &config.llm_base_url,
                &config.llm_api_key,
                &config.llm_model,
                4096,
            )?)
        } else {
            Arc::new(memini_llm::OpenAiClient::new(
                &config.llm_base_url,
                &config.llm_api_key,
                &config.llm_model,
                4096,
            )?)
        };
        Some(memini_llm::observed(raw, dependencies.clone()))
    } else {
        None
    };
    if let Some(client) = &llm_client {
        service = service
            .with_answerer(client.clone())
            .with_distiller(client.clone())
            .with_consolidator(client.clone(), config.consolidate_min_score)
            .with_consolidate_mode(config.consolidate_mode.clone())
    }
    if config.rerank_enabled() {
        let reranker: Arc<dyn memini_rerank::Reranker> = if config.rerank_is_llm() {
            let client = llm_client.clone().ok_or_else(|| {
                anyhow::anyhow!("MEMINI_RERANK=llm but no LLM is configured; set MEMINI_LLM_BASE_URL or unset MEMINI_RERANK")
            })?;
            Arc::new(memini_rerank::LlmReranker::new(client))
        } else {
            Arc::new(memini_rerank::CrossEncoder::new(
                &config.rerank,
                &config.rerank_model,
                &config.rerank_api_key,
                2048,
                config.rerank_max_batch_chars.max(0) as usize,
            )?)
        };
        let gauge = metrics.clone();
        let reranker = memini_rerank::limited(
            reranker,
            config.rerank_max_concurrency.max(0) as usize,
            Some(Arc::new(move |count| {
                gauge.set("memini_rerank_in_flight", &[], count as f64)
            })),
        );
        service = service
            .with_reranker(memini_rerank::timed(reranker, config.rerank_timeout))
            .with_rerank_pool(config.rerank_pool.max(0) as usize);
    }
    Ok(Stack {
        service: Arc::new(service),
        store,
        keys,
        links,
        embedder,
        embed_enabled: !config.embed_base_url.is_empty(),
        metrics,
        dependencies,
    })
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("fatal: {error:#}");
        std::process::exit(1)
    }
}
async fn run() -> Result<()> {
    let mut cli = Cli::parse();
    if matches!(cli.command, Some(Command::Version)) {
        println!("{}", version_string());
        return Ok(());
    }
    #[cfg(feature = "gendocs")]
    if let Some(Command::GenDocs { out }) = &cli.command {
        return gen_docs(out);
    }
    if matches!(cli.command, None | Some(Command::Mcp)) {
        let fatal = fatal_deprecated_vars();
        if !fatal.is_empty() {
            bail!(fatal.join("\n"));
        }
    }
    let config = Config::load()?;
    init_logging(&config);
    if matches!(cli.command, None | Some(Command::Mcp)) {
        for warning in deprecation_warnings() {
            eprintln!("warning: deprecated configuration: {warning}");
        }
    }
    if matches!(cli.command, Some(Command::Import(_)))
        && let Some(Command::Import(mut args)) = cli.command.take()
    {
        args.namespace
            .get_or_insert(config.default_namespace.clone());
        if args.remote.is_some() {
            return import_remote_command(args).await;
        }
        cli.command = Some(Command::Import(args));
    }
    let allow_model_mismatch = matches!(cli.command, Some(Command::Reembed { .. }));
    let stack = stack(&config, allow_model_mismatch).await?;
    match cli.command {
        None => serve(config, stack).await,
        Some(Command::Doctor { fix, scrub, yes }) => doctor(&config, &stack, fix, scrub, yes).await,
        Some(Command::Export {
            file,
            namespace,
            output,
            all_namespaces,
            include_expired,
            include_superseded,
            pretty,
            tier,
            tag,
            metadata,
        }) => {
            export(
                &stack,
                file,
                output,
                namespace.or_else(|| (!all_namespaces).then(|| config.default_namespace.clone())),
                all_namespaces,
                include_expired,
                include_superseded,
                pretty,
                tier,
                tag,
                metadata,
            )
            .await
        }
        Some(Command::Import(mut args)) => {
            args.namespace
                .get_or_insert(config.default_namespace.clone());
            import(&stack, args).await
        }
        Some(Command::Forget { namespace, tag }) => {
            let namespace = namespace.unwrap_or_else(|| config.default_namespace.clone());
            let deleted = stack.service.forget_by_tag(&namespace, &tag).await?;
            println!("deleted {deleted}");
            Ok(())
        }
        Some(Command::Key { command }) => key(&stack, command).await,
        Some(Command::Link { command }) => link(&stack, command).await,
        Some(Command::Namespace { command }) => namespace(&stack, command).await,
        Some(Command::Backfill { yes }) => {
            println!(
                "{}",
                serde_json::to_string_pretty(
                    &memini_maintenance::backfill_confidence(
                        stack.store.as_ref(),
                        chrono::Utc::now(),
                        yes
                    )
                    .await?
                )?
            );
            Ok(())
        }
        Some(Command::Migrate { command }) => migrate(&stack, command).await,
        Some(Command::Reembed {
            namespace,
            yes,
            batch,
        }) => reembed(&stack, namespace, yes, batch, &config.embed_model).await,
        Some(Command::Mcp) => mcp_stdio(&config, stack).await,
        #[cfg(feature = "gendocs")]
        Some(Command::GenDocs { .. }) => unreachable!(),
        Some(Command::Version) => unreachable!(),
    }
}
fn version_string() -> String {
    format!(
        "{} ({}, {})",
        option_env!("MEMINI_BUILD_VERSION").unwrap_or(env!("CARGO_PKG_VERSION")),
        option_env!("MEMINI_BUILD_REVISION").unwrap_or("none"),
        option_env!("MEMINI_BUILD_DATE").unwrap_or("unknown")
    )
}
fn init_logging(config: &Config) {
    use tracing_subscriber::EnvFilter;
    let filter = EnvFilter::try_new(&config.log_level).unwrap_or_else(|_| EnvFilter::new("info"));
    if config.log_format == "json" {
        let _ = tracing_subscriber::fmt()
            .with_env_filter(filter)
            .json()
            .try_init();
    } else {
        let _ = tracing_subscriber::fmt().with_env_filter(filter).try_init();
    }
}

#[cfg(feature = "gendocs")]
fn gen_docs(path: &std::path::Path) -> Result<()> {
    docs::generate(Cli::command(), path)
}
async fn serve(config: Config, stack: Stack) -> Result<()> {
    probe_embedder(&stack).await;
    spawn_workers(&config, &stack);
    let service_for_shutdown = stack.service.clone();
    let file_keys = if config.api_keys_file.is_empty() {
        None
    } else {
        Some(Arc::new(memini_auth::load_file_keys(
            &config.api_keys_file,
        )?))
    };
    let auth = memini_auth::Config::new(config.api_key.clone(), Some(stack.keys.clone()))
        .with_file_keys(file_keys);
    let api_config = |ui_enabled, metrics_enabled| memini_api::ApiConfig {
        auth: auth.clone(),
        namespace_header: "X-Memini-Namespace".into(),
        home_header: "X-Memini-Home".into(),
        default_namespace: config.default_namespace.clone(),
        request_timeout: config.request_timeout,
        key_store: Some(stack.keys.clone()),
        link_store: Some(stack.links.clone()),
        ui_enabled,
        ui_api_key: config.api_key.clone(),
        metrics_enabled,
        llm_configured: config.llm_enabled(),
        embedder_configured: !config.embed_base_url.is_empty(),
        metrics: stack.metrics.clone(),
        dependencies: stack.dependencies.clone(),
    };
    let address = if config.http_addr.starts_with(':') {
        format!("0.0.0.0{}", config.http_addr)
    } else {
        config.http_addr.clone()
    };
    let metrics_address = normalize_address(&config.metrics_addr);
    let ui_address = normalize_address(&config.ui_addr);
    let dedicated_metrics = metrics_address.as_deref().is_some_and(|v| v != address);
    let dedicated_ui = config.ui_enabled && ui_address.as_deref().is_some_and(|v| v != address);
    let app = memini_api::router(
        stack.service.clone(),
        api_config(config.ui_enabled && !dedicated_ui, !dedicated_metrics),
    );
    let listener = tokio::net::TcpListener::bind(&address)
        .await
        .with_context(|| format!("listen {address}"))?;
    if let Some(metrics_address) = metrics_address.filter(|_| dedicated_metrics) {
        let listener = tokio::net::TcpListener::bind(&metrics_address)
            .await
            .with_context(|| format!("listen {metrics_address}"))?;
        eprintln!("memini metrics listening on {metrics_address}");
        let metrics = stack.metrics.clone();
        tokio::spawn(async move {
            let _ = axum::serve(listener, memini_api::metrics_router(metrics)).await;
        });
    }
    if let Some(ui_address) = ui_address.filter(|_| dedicated_ui) {
        let listener = tokio::net::TcpListener::bind(&ui_address)
            .await
            .with_context(|| format!("listen {ui_address}"))?;
        let ui_app = memini_api::router(stack.service, api_config(true, false));
        eprintln!("memini UI listening on {ui_address}");
        tokio::spawn(async move {
            let _ = axum::serve(listener, ui_app).await;
        });
    }
    eprintln!("memini listening on {address}");
    let stopping = Arc::new(tokio::sync::Notify::new());
    let shutdown_notice = stopping.clone();
    let server = async move {
        axum::serve(listener, app)
            .with_graceful_shutdown(async move {
                shutdown().await;
                shutdown_notice.notify_waiters();
            })
            .await
    };
    tokio::pin!(server);
    tokio::select! {
        result = &mut server => result?,
        () = async {
            stopping.notified().await;
            tokio::time::sleep(config.shutdown_timeout).await;
        } => eprintln!("warning: graceful shutdown timed out after {:?}", config.shutdown_timeout),
    }
    service_for_shutdown.flush_distill_batches(true).await;
    Ok(())
}
fn normalize_address(value: &str) -> Option<String> {
    (!value.is_empty()).then(|| {
        if value.starts_with(':') {
            format!("0.0.0.0{value}")
        } else {
            value.to_owned()
        }
    })
}
fn spawn_workers(config: &Config, stack: &Stack) {
    let sweep = stack.store.clone();
    tokio::spawn(memini_maintenance::run_sweeper(
        sweep,
        memini_maintenance::SweeperConfig {
            interval: config.sweep_interval,
            short_term_cap: config.short_term_cap.max(0) as usize,
            tombstone_ttl: (!config.tombstone_ttl.is_zero())
                .then(|| chrono::Duration::from_std(config.tombstone_ttl).unwrap()),
            demote_after: (!config.demote_after.is_zero())
                .then(|| chrono::Duration::from_std(config.demote_after).unwrap()),
        },
    ));
    if !config.promote_interval.is_zero() {
        let service = stack.service.clone();
        let interval = config.promote_interval;
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            loop {
                ticker.tick().await;
                let _ = service.promote().await;
            }
        });
    }
    if !config.backfill_interval.is_zero() && !config.embed_base_url.is_empty() {
        let service = stack.service.clone();
        let interval = config.backfill_interval;
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            loop {
                ticker.tick().await;
                let _ = service.backfill_embeddings().await;
            }
        });
    }
    if !config.dedup_interval.is_zero() && !config.embed_base_url.is_empty() {
        let service = stack.service.clone();
        let interval = config.dedup_interval;
        let similarity = config.dedup_similarity;
        let tiers = config.dedup_tier_list().unwrap_or_default();
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            loop {
                ticker.tick().await;
                let _ = service
                    .dedup(memini_maintenance::DedupOptions {
                        similarity,
                        tiers: tiers.clone(),
                        ..memini_maintenance::DedupOptions::default()
                    })
                    .await;
            }
        });
    }
    if config.activity_log && (config.activity_max_rows > 0 || !config.activity_retention.is_zero())
    {
        let service = stack.service.clone();
        let interval = config.sweep_interval;
        let keep = (config.activity_max_rows > 0).then_some(config.activity_max_rows as usize);
        let age = chrono::Duration::from_std(config.activity_retention).unwrap_or_default();
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            loop {
                ticker.tick().await;
                let older = (!age.is_zero()).then_some(chrono::Utc::now() - age);
                let _ = service.prune_events(older, keep).await;
            }
        });
    }
    if config.distill_batch_tokens > 0 && config.llm_enabled() {
        let service = stack.service.clone();
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(std::time::Duration::from_secs(30));
            loop {
                ticker.tick().await;
                service.flush_distill_batches(false).await;
            }
        });
    }
}
async fn shutdown() {
    let ctrl = tokio::signal::ctrl_c();
    #[cfg(unix)]
    let term = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .unwrap()
            .recv()
            .await;
    };
    #[cfg(not(unix))]
    let term = std::future::pending::<()>();
    tokio::select! {_ = ctrl=>{},_ = term=>{}}
}
async fn doctor(config: &Config, stack: &Stack, fix: bool, scrub: bool, yes: bool) -> Result<()> {
    stack.store.ping().await?;
    let cwd = std::env::current_dir().unwrap_or_default();
    let (plugin_namespace, plugin_source) =
        memini_config::namespace::resolve_plugin_namespace(Some(&cwd));
    println!("Namespace resolution (cwd: {})", cwd.display());
    println!(
        "  server default:  {:?} ({:?})",
        config.default_namespace, config.namespace_source
    );
    println!(
        "  plugin resolves: {:?} ({plugin_source:?})",
        plugin_namespace
    );
    println!(
        "  home namespace:  {}",
        if config.home.is_empty() {
            "<unset>"
        } else {
            &config.home
        }
    );
    if plugin_namespace != config.default_namespace {
        println!(
            "WARN: plugin namespace {:?} differs from the header-less server default {:?}",
            plugin_namespace, config.default_namespace
        );
    }
    let namespaces = stack.service.namespaces().await?;
    println!(
        "Store ({:?}): reachable, {} namespace(s)",
        config.backend,
        namespaces.len()
    );
    for namespace in &namespaces {
        let count = stack
            .store
            .list(namespace, &Filter::default(), None)
            .await?
            .len();
        println!("  {namespace}\t{count}");
        if matches!(namespace.as_str(), "default" | "openclaw") && count > 500 {
            println!("WARN: catch-all namespace {namespace:?} contains {count} memories");
        }
    }
    println!("Embedding dimensions: {}", stack.embedder.dimensions());
    if fix || scrub {
        let report = memini_maintenance::scrub(stack.store.as_ref(), yes).await?;
        println!("Scrub ({}):", if yes { "applied" } else { "preview" });
        println!("{}", serde_json::to_string_pretty(&report)?);
    }
    if fix {
        let confidence =
            memini_maintenance::backfill_confidence(stack.store.as_ref(), chrono::Utc::now(), yes)
                .await?;
        println!("{}", serde_json::to_string_pretty(&confidence)?);
        for namespace in namespaces
            .iter()
            .filter(|namespace| matches!(namespace.as_str(), "default" | "openclaw"))
        {
            let report = stack
                .service
                .split_namespace(
                    namespace,
                    &[
                        "import_source_namespace".into(),
                        "user_id".into(),
                        "agent_id".into(),
                        "run_id".into(),
                        "project".into(),
                    ],
                    !yes,
                )
                .await?;
            println!("split {namespace}: {}", serde_json::to_string(&report)?);
        }
        if yes {
            let purged =
                memini_maintenance::purge_expired(stack.store.as_ref(), chrono::Utc::now()).await?;
            println!("purged_expired: {purged}");
            if !config.demote_after.is_zero() {
                let now = chrono::Utc::now();
                let demoted = memini_maintenance::demote_stale(
                    stack.store.as_ref(),
                    now - chrono::Duration::from_std(config.demote_after).unwrap_or_default(),
                    now,
                )
                .await?;
                println!("demoted_stale: {demoted}");
            }
        }
        if !config.embed_base_url.is_empty() {
            let report = stack
                .service
                .dedup(memini_maintenance::DedupOptions {
                    similarity: config.dedup_similarity,
                    dry_run: !yes,
                    tiers: config.dedup_tier_list().unwrap_or_default(),
                    ..memini_maintenance::DedupOptions::default()
                })
                .await?;
            println!("dedup: {}", serde_json::to_string(&report)?);
        }
    }
    Ok(())
}
#[allow(clippy::too_many_arguments)]
async fn export(
    stack: &Stack,
    file: Option<PathBuf>,
    output: Option<PathBuf>,
    namespace: Option<String>,
    all_namespaces: bool,
    include_expired: bool,
    include_superseded: bool,
    pretty: bool,
    tiers: Vec<String>,
    tags: Vec<String>,
    metadata_args: Vec<String>,
) -> Result<()> {
    let tiers = tiers
        .iter()
        .map(|value| parse_tier(value))
        .collect::<Result<Vec<_>>>()?;
    let mut metadata = serde_json::Map::new();
    for item in metadata_args {
        let (key, value) = item
            .split_once('=')
            .ok_or_else(|| anyhow::anyhow!("--meta must be key=value, got {item:?}"))?;
        metadata.insert(key.into(), serde_json::Value::String(value.into()));
    }
    let selected_namespace = namespace.clone();
    let memories = stack
        .service
        .list(ListInput {
            namespace: namespace.unwrap_or_default(),
            all_namespaces,
            include_expired,
            include_superseded,
            tiers,
            tags,
            metadata,
            ..ListInput::default()
        })
        .await?;
    let records = memories
        .into_iter()
        .map(|memory| ExportRecord {
            id: memory.id,
            namespace: memory.namespace,
            tier: memory.tier,
            content: memory.content,
            summary: memory.summary,
            tags: memory.tags,
            metadata: memory.metadata,
            importance: memory.importance,
            created_at: memory.created_at,
            updated_at: memory.updated_at,
            expires_at: memory.expires_at,
        })
        .collect::<Vec<_>>();
    let links = stack
        .links
        .list_all_links()
        .await?
        .into_iter()
        .filter(|link| {
            all_namespaces
                || selected_namespace.as_deref().is_some_and(|namespace| {
                    link.source == namespace || link.destination == namespace
                })
        })
        .map(|link| ExportLink {
            src: link.source,
            dst: link.destination,
            tiers: link.tiers,
            note: link.note,
            created_at: link.created_at,
        })
        .collect::<Vec<_>>();
    let document = ExportDocument {
        memories: records,
        links,
    };
    let contents = if pretty {
        serde_json::to_string_pretty(&document)?
    } else {
        serde_json::to_string(&document)?
    };
    if let Some(path) = output.or(file) {
        std::fs::write(path, contents)?
    } else {
        println!("{contents}")
    }
    Ok(())
}
async fn import(stack: &Stack, args: ImportArgs) -> Result<()> {
    let (source, records, links) = load_import_records(&args).await?;
    import_records(Some(stack), args, source, records, links).await
}

async fn import_remote_command(args: ImportArgs) -> Result<()> {
    let (source, records, links) = load_import_records(&args).await?;
    import_records(None, args, source, records, links).await
}

async fn load_import_records(
    args: &ImportArgs,
) -> Result<(
    memini_import::Source,
    Vec<memini_import::Record>,
    Vec<NamespaceLink>,
)> {
    use tokio::io::AsyncReadExt;
    let mut data = Vec::new();
    let file_path = args.file.clone();
    if let Some(path) = &file_path {
        if !path.is_dir() {
            data = std::fs::read(path)?;
        }
    } else {
        tokio::io::stdin().read_to_end(&mut data).await?;
    }
    let source = memini_import::Source::parse(&args.source)
        .ok_or_else(|| anyhow::anyhow!("unknown import source {:?}", args.source))?;
    let records = if source == memini_import::Source::ClaudeCode
        && file_path.as_ref().is_some_and(|v| v.is_dir())
    {
        memini_import::load_claude_path(file_path.as_ref().unwrap())?
    } else {
        memini_import::parse(source, &data)?
    };
    let links = if source == memini_import::Source::Memini && !data.is_empty() {
        memini_import::parse_links(&data)?
    } else {
        Vec::new()
    };
    Ok((source, records, links))
}

async fn import_records(
    stack: Option<&Stack>,
    args: ImportArgs,
    source: memini_import::Source,
    mut records: Vec<memini_import::Record>,
    links: Vec<NamespaceLink>,
) -> Result<()> {
    if !(args.confidence < 0.0 || (0.0..=1.0).contains(&args.confidence)) {
        bail!("--confidence must be in [0,1] or negative for the default")
    }
    if !(0.0..=1.0).contains(&args.importance) {
        bail!("--importance must be in [0,1]")
    }
    if args.merge_into.is_some() && !args.yes {
        let namespaces = records
            .iter()
            .filter(|record| !record.namespace.is_empty())
            .map(|record| record.namespace.as_str())
            .collect::<std::collections::HashSet<_>>();
        if namespaces.len() > 1 {
            use std::io::IsTerminal;
            if !std::io::stdin().is_terminal() {
                bail!("--merge-into collapses multiple namespaces; pass --yes to confirm")
            }
            eprint!(
                "Merge {} source namespaces into {}? [y/N] ",
                namespaces.len(),
                args.merge_into.as_deref().unwrap_or_default()
            );
            let mut answer = String::new();
            std::io::stdin().read_line(&mut answer)?;
            if !matches!(answer.trim().to_ascii_lowercase().as_str(), "y" | "yes") {
                bail!("import cancelled")
            }
        }
    }
    if args.extract {
        let mut extracted = Vec::new();
        for record in &records {
            for fact in memini_intelligence::extract::typed(&record.content) {
                let mut derived = record.clone();
                derived.id.clear();
                derived.content = fact.content;
                derived.summary.clear();
                derived.tier = Some(fact.kind.tier());
                derived.importance = derived.importance.max(0.4);
                derived.metadata.insert(
                    "extracted_kind".into(),
                    serde_json::Value::String(fact.kind.as_str().into()),
                );
                extracted.push(derived);
            }
        }
        records.extend(extracted);
    }
    let options = memini_import::Options {
        default_namespace: args.namespace.unwrap_or_default(),
        force_namespace: args.merge_into.unwrap_or_default(),
        source,
        dry_run: args.dry_run,
        default_importance: args.importance,
        confidence: (args.confidence >= 0.0).then_some(args.confidence),
        skip_existing: !args.no_dedup,
        dedup_content: !args.no_dedup,
        batch_size: if args.batch == 0 { 64 } else { args.batch },
        min_content_len: args.min_content_len,
        min_importance: args.min_importance,
    };
    if let Some(remote) = args.remote {
        let (records, mut report) = memini_import::prepare(records, &options);
        if !options.dry_run {
            let token = args
                .token
                .unwrap_or_else(|| std::env::var("MEMINI_API_KEY").unwrap_or_default());
            remote_import(
                &remote,
                &token,
                records,
                &options,
                args.dedup_similarity,
                &mut report,
            )
            .await?;
            remote_import_links(&remote, &token, &links).await?;
        }
        println!("{}", serde_json::to_string_pretty(&report)?);
        return Ok(());
    }
    let stack = stack.ok_or_else(|| anyhow::anyhow!("local import requires a store"))?;
    let report = memini_import::import(
        stack.store.as_ref(),
        stack.embedder.as_ref(),
        records,
        &options,
    )
    .await?;
    if !options.dry_run {
        for link in links {
            stack.links.put_link(&link).await?;
        }
    }
    if !args.no_dedup && stack.embed_enabled && report.imported > 0 {
        let dedup = stack
            .service
            .dedup(memini_maintenance::DedupOptions {
                similarity: args.dedup_similarity,
                namespaces: report.namespaces.keys().cloned().collect(),
                dry_run: false,
                ..memini_maintenance::DedupOptions::default()
            })
            .await?;
        println!(
            "{}",
            serde_json::to_string_pretty(&serde_json::json!({"import":report,"dedup":dedup}))?
        );
    } else {
        println!("{}", serde_json::to_string_pretty(&report)?);
    }
    Ok(())
}

async fn remote_import(
    base_url: &str,
    token: &str,
    records: Vec<memini_import::Record>,
    options: &memini_import::Options,
    dedup_similarity: f64,
    report: &mut memini_import::Report,
) -> Result<()> {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(30))
        .build()?;
    let base_url = base_url.trim_end_matches('/');
    for record in records {
        let id = record.id.clone();
        let tier = record.tier.unwrap_or(memini_core::memory::Tier::Episodic);
        let mut body = serde_json::json!({
            "id": id,
            "content": record.content.clone(),
            "summary": record.summary.clone(),
            "tier": tier,
            "tags": record.tags.clone(),
            "metadata": record.metadata.clone(),
            "importance": record.importance,
        });
        if tier.term() == memini_core::memory::Term::Long {
            body["confidence"] = serde_json::Value::from(
                options
                    .confidence
                    .unwrap_or(memini_core::memory::CONFIDENCE_SEED_IMPORTED),
            );
        }
        if let Some(expiry) = record.expires_at {
            let seconds = (expiry - chrono::Utc::now()).num_seconds();
            if seconds > 0 {
                body["ttl_seconds"] = serde_json::Value::from(seconds);
            }
        }
        let mut request = client
            .post(format!("{base_url}/v1/memories"))
            .header("X-Memini-Namespace", &record.namespace)
            .json(&body);
        if !token.is_empty() {
            request = request.bearer_auth(token);
        }
        match request.send().await {
            Ok(response) if response.status().is_success() => report.imported += 1,
            Ok(response)
                if response.status() == reqwest::StatusCode::UNAUTHORIZED
                    || response.status() == reqwest::StatusCode::FORBIDDEN =>
            {
                bail!(
                    "import: remote rejected credentials ({})",
                    response.status()
                )
            }
            Ok(response) => {
                let status = response.status();
                let message = response.text().await.unwrap_or_default();
                report.errors.push(format!("{id}: {status}: {message}"));
            }
            Err(error) => report.errors.push(format!("{id}: {error}")),
        }
    }
    if options.dedup_content && report.imported > 0 {
        for namespace in report.namespaces.keys() {
            let mut request = client
                .post(format!("{base_url}/v1/dedup"))
                .header("X-Memini-Namespace", namespace)
                .json(&serde_json::json!({"similarity": dedup_similarity}));
            if !token.is_empty() {
                request = request.bearer_auth(token);
            }
            let response = request.send().await?;
            if !response.status().is_success() {
                report.errors.push(format!(
                    "dedup {namespace}: {}: {}",
                    response.status(),
                    response.text().await.unwrap_or_default()
                ));
            }
        }
    }
    Ok(())
}

async fn remote_import_links(base_url: &str, token: &str, links: &[NamespaceLink]) -> Result<()> {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(30))
        .build()?;
    let base_url = base_url.trim_end_matches('/');
    for link in links {
        let mut request = client
            .post(format!("{base_url}/v1/links"))
            .header("X-Memini-Namespace", &link.source)
            .json(&serde_json::json!({
                "dst":link.destination,
                "tiers":link.tiers,
                "note":link.note,
            }));
        if !token.is_empty() {
            request = request.bearer_auth(token);
        }
        let response = request.send().await?;
        if !response.status().is_success() {
            let status = response.status();
            bail!(
                "import link {} -> {}: {status}: {}",
                link.source,
                link.destination,
                response.text().await.unwrap_or_default()
            );
        }
    }
    Ok(())
}
async fn key(stack: &Stack, command: KeyCommand) -> Result<()> {
    match command {
        KeyCommand::Add {
            name,
            home,
            default_namespace,
            disabled,
        } => {
            let existing = stack
                .keys
                .list_api_keys()
                .await?
                .into_iter()
                .find(|v| v.name == name);
            let secret = memini_auth::generate_secret()?;
            stack
                .keys
                .put_api_key(&ApiKey {
                    name: name.clone(),
                    hash: memini_auth::hash_token(&secret),
                    home_namespace: home.unwrap_or_else(|| {
                        existing
                            .as_ref()
                            .map_or_else(String::new, |v| v.home_namespace.clone())
                    }),
                    default_namespace: default_namespace.unwrap_or_else(|| {
                        existing
                            .as_ref()
                            .map_or_else(String::new, |v| v.default_namespace.clone())
                    }),
                    created_at: existing.as_ref().and_then(|v| v.created_at),
                    disabled: disabled
                        .unwrap_or_else(|| existing.as_ref().is_some_and(|v| v.disabled)),
                })
                .await?;
            println!("name: {name}\nsecret: {secret}")
        }
        KeyCommand::Rm { name } => {
            if !stack.keys.delete_api_key(&name).await? {
                bail!("no api key named {name:?}")
            }
            println!("deleted {name}")
        }
        KeyCommand::Ls => {
            for key in stack.keys.list_api_keys().await? {
                println!(
                    "{}\thome={}\tdefault={}\tdisabled={}",
                    key.name, key.home_namespace, key.default_namespace, key.disabled
                )
            }
        }
    }
    Ok(())
}
async fn link(stack: &Stack, command: LinkCommand) -> Result<()> {
    match command {
        LinkCommand::Add {
            source,
            destination,
            tiers,
            note,
        } => {
            let tiers = tiers
                .iter()
                .map(|v| parse_tier(v))
                .collect::<Result<Vec<_>>>()?;
            stack
                .links
                .put_link(&NamespaceLink {
                    source,
                    destination,
                    tiers,
                    note,
                    created_at: chrono::Utc::now(),
                })
                .await?
        }
        LinkCommand::Rm {
            source,
            destination,
        } => {
            if !stack.links.delete_link(&source, &destination).await? {
                bail!("link not found")
            }
        }
        LinkCommand::Ls { source } => {
            let links = if let Some(source) = source {
                stack.links.list_links(&source).await?
            } else {
                stack.links.list_all_links().await?
            };
            for link in links {
                println!(
                    "{} -> {} {:?} {}",
                    link.source, link.destination, link.tiers, link.note
                )
            }
        }
    }
    Ok(())
}
fn parse_tier(value: &str) -> Result<memini_core::memory::Tier> {
    Ok(match value {
        "working" => memini_core::memory::Tier::Working,
        "episodic" => memini_core::memory::Tier::Episodic,
        "semantic" => memini_core::memory::Tier::Semantic,
        "procedural" => memini_core::memory::Tier::Procedural,
        _ => bail!("invalid tier {value:?}"),
    })
}
async fn namespace(stack: &Stack, command: NamespaceCommand) -> Result<()> {
    match command {
        NamespaceCommand::List => {
            for ns in stack.service.namespaces().await? {
                let count = stack.store.list(&ns, &Filter::default(), None).await?.len();
                println!("{ns}\t{count}")
            }
        }
        NamespaceCommand::Move { from, to, dry_run } => println!(
            "{}",
            serde_json::to_string_pretty(
                &stack.service.move_namespace(&from, &to, dry_run).await?
            )?
        ),
        NamespaceCommand::Split {
            namespace,
            by,
            dry_run,
        } => println!(
            "{}",
            serde_json::to_string_pretty(
                &stack
                    .service
                    .split_namespace(&namespace, &by, dry_run)
                    .await?
            )?
        ),
    }
    Ok(())
}
async fn reembed(
    stack: &Stack,
    namespace: Option<String>,
    apply: bool,
    batch: usize,
    model: &str,
) -> Result<()> {
    let updated = if !apply {
        let namespaces = if let Some(value) = &namespace {
            vec![value.clone()]
        } else {
            stack.store.list_namespaces().await?
        };
        let mut count = 0;
        for ns in namespaces {
            count += stack
                .store
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
                .len();
        }
        count
    } else {
        reembed_store(
            stack.store.as_ref(),
            stack.embedder.as_ref(),
            namespace.as_deref(),
            batch,
        )
        .await?
    };
    if apply && namespace.is_none() {
        stack.store.set_embed_model(model).await?;
    }
    println!(
        "{} {updated} memories",
        if apply {
            "re-embedded"
        } else {
            "would re-embed"
        }
    );
    Ok(())
}

async fn reembed_store(
    store: &dyn Store,
    embedder: &dyn Embedder,
    namespace: Option<&str>,
    batch: usize,
) -> Result<usize> {
    let mut updated = 0;
    let namespaces = if let Some(value) = namespace {
        vec![value.to_owned()]
    } else {
        store.list_namespaces().await?
    };
    for ns in namespaces {
        let memories = store
            .list(
                &ns,
                &Filter {
                    include_expired: true,
                    include_superseded: true,
                    ..Filter::default()
                },
                None,
            )
            .await?;
        for chunk in memories.chunks(batch.max(1).max(if batch == 0 { 64 } else { batch })) {
            let texts = chunk.iter().map(|v| v.content.clone()).collect::<Vec<_>>();
            let vectors = embedder.embed(&texts).await?;
            for (memory, vector) in chunk.iter().cloned().zip(vectors) {
                let mut memory = memory;
                memory.embedding = vector;
                store.upsert(&memory).await?;
                updated += 1
            }
        }
    }
    Ok(updated)
}
async fn migrate(stack: &Stack, command: MigrateCommand) -> Result<()> {
    match command {
        MigrateCommand::Scopes { yes } => {
            let report = memini_maintenance::migrate_scopes(
                stack.store.as_ref(),
                yes.then_some(stack.embedder.as_ref()),
                !yes,
                memini_maintenance::DedupOptions::default(),
            )
            .await
            .map_err(|e| anyhow::anyhow!(e.to_string()))?;
            println!("{}", serde_json::to_string_pretty(&report)?)
        }
    }
    Ok(())
}

async fn mcp_stdio(config: &Config, stack: Stack) -> Result<()> {
    use axum::{body::Body, http::Request};
    use http_body_util::BodyExt;
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
    use tower::ServiceExt;
    probe_embedder(&stack).await;
    let app = memini_api::router(
        stack.service,
        memini_api::ApiConfig {
            auth: memini_auth::Config::new("", None),
            namespace_header: "X-Memini-Namespace".into(),
            home_header: "X-Memini-Home".into(),
            default_namespace: config.default_namespace.clone(),
            request_timeout: config.request_timeout,
            key_store: Some(stack.keys),
            link_store: Some(stack.links),
            ui_enabled: false,
            ui_api_key: String::new(),
            metrics_enabled: false,
            llm_configured: config.llm_enabled(),
            embedder_configured: !config.embed_base_url.is_empty(),
            metrics: stack.metrics.clone(),
            dependencies: stack.dependencies.clone(),
        },
    );
    let mut lines = BufReader::new(tokio::io::stdin()).lines();
    let mut stdout = tokio::io::stdout();
    while let Some(line) = lines.next_line().await? {
        if line.trim().is_empty() {
            continue;
        }
        let response = app
            .clone()
            .oneshot(
                Request::post("/mcp")
                    .header("content-type", "application/json")
                    .header("X-Memini-Namespace", &config.default_namespace)
                    .header("X-Memini-Home", &config.home)
                    .body(Body::from(line))
                    .unwrap(),
            )
            .await?;
        let bytes = response.into_body().collect().await?.to_bytes();
        stdout.write_all(&bytes).await?;
        stdout.write_all(b"\n").await?;
        stdout.flush().await?;
    }
    Ok(())
}

async fn probe_embedder(stack: &Stack) {
    if !stack.embed_enabled {
        return;
    }
    let result = tokio::time::timeout(
        std::time::Duration::from_secs(3),
        memini_embed::embed_one(stack.embedder.as_ref(), "ping"),
    )
    .await;
    if !matches!(result, Ok(Ok(_))) {
        let message = match result {
            Ok(Err(error)) => error.to_string(),
            Err(_) => "embed probe timed out".to_owned(),
            Ok(Ok(_)) => unreachable!(),
        };
        stack.dependencies.record("embedder", Some(&message));
        eprintln!(
            "warning: embeddings endpoint unreachable at startup — writes will degrade to keyword-only: {message}"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_surface_matches_reference() {
        for args in [
            vec!["memini", "version"],
            vec!["memini", "doctor", "--fix", "--yes"],
            vec!["memini", "export", "--all-namespaces", "--pretty"],
            vec!["memini", "forget", "--tag", "bad-import"],
            vec![
                "memini",
                "namespace",
                "move",
                "--from",
                "old",
                "--to",
                "new",
            ],
            vec!["memini", "namespace", "split", "--from", "pool"],
            vec!["memini", "migrate", "scopes"],
            vec!["memini", "reembed", "--yes"],
            vec!["memini", "mcp"],
        ] {
            assert!(Cli::try_parse_from(&args).is_ok(), "rejected {args:?}");
        }
        assert!(Cli::try_parse_from(["memini", "dedup"]).is_err());
        assert!(Cli::try_parse_from(["memini", "forget"]).is_err());
    }

    #[test]
    fn version_contains_build_triplet() {
        let value = version_string();
        assert!(value.contains('('));
        assert!(value.contains(','));
    }
}
