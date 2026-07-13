use anyhow::Result;
use clap::Parser;
use memini_bench::{
    BenchOptions, Dataset, DocumentMode, FakeEmbedder, IngestMode, gate_markdown, markdown,
    rerank_gate_sweep, run_configured, vector_gate_sweep,
};
use memini_embed::{Batched, DiskCache, Embedder, OpenAiEmbedder};
use memini_llm::{AnthropicClient, Client, OpenAiClient};
use memini_rerank::{CrossEncoder, LlmReranker, Reranker};
use std::path::PathBuf;
use std::sync::Arc;

#[derive(Parser)]
#[command(name = "memini-bench", about = "memini retrieval benchmark harness")]
struct Args {
    #[arg(long, default_value = "sample", value_parser = ["sample", "file", "longmemeval", "locomo", "locomo-sessions"])]
    suite: String,
    #[arg(long)]
    data: Option<PathBuf>,
    #[arg(short = 'k', long, value_delimiter = ',', default_value = "5")]
    k: Vec<usize>,
    #[arg(long, default_value_t = 256)]
    dims: usize,
    #[arg(long, default_value_t = 0)]
    limit: usize,
    #[arg(long, default_value = "bench/results")]
    out: PathBuf,
    #[arg(long, default_value = "all", value_parser = ["all", "tune", "held"])]
    holdout: String,
    #[arg(long, default_value = "full", value_parser = ["full", "user-only", "dated"])]
    session_doc: String,
    #[arg(long, default_value = "upsert", value_parser = ["upsert", "write"])]
    ingest: String,
    #[arg(long, default_value_t = 0.5)]
    fusion: f64,
    #[arg(long, default_value_t = 0)]
    pool_factor: usize,
    #[arg(long, default_value_t = 0)]
    pool_floor: usize,
    #[arg(long)]
    rerank: bool,
    #[arg(long)]
    llm_rerank: bool,
    #[arg(long, default_value = "")]
    rerank_url: String,
    #[arg(long, default_value = "")]
    rerank_model: String,
    #[arg(long, default_value_t = 20)]
    llm_rerank_pool: usize,
    #[arg(long, default_value_t = 2048)]
    rerank_max_doc_chars: usize,
    #[arg(long, default_value_t = 6000)]
    rerank_max_batch_chars: usize,
    #[arg(long)]
    rewrite: bool,
    #[arg(long)]
    distill: bool,
    #[arg(long, default_value_t = 8)]
    concurrency: usize,
    #[arg(long, value_delimiter = ',')]
    vec_gate: Vec<f64>,
    #[arg(long, value_delimiter = ',')]
    rerank_gate: Vec<f64>,
    #[arg(long, default_value_t = 20)]
    rerank_gate_pool: usize,
}

#[tokio::main]
async fn main() {
    if let Err(error) = execute().await {
        eprintln!("bench: {error:#}");
        std::process::exit(1);
    }
}
async fn execute() -> Result<()> {
    let args = Args::parse();
    let data = || {
        args.data
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("--data is required for --suite {}", args.suite))
    };
    let mode = match args.session_doc.as_str() {
        "user-only" => DocumentMode::UserOnly,
        "dated" => DocumentMode::Dated,
        _ => DocumentMode::Full,
    };
    let mut dataset = match args.suite.as_str() {
        "sample" => Dataset::sample()?,
        "file" => Dataset::load(data()?)?,
        "longmemeval" => Dataset::load_longmemeval(data()?, mode)?.split_holdout(&args.holdout)?,
        "locomo" => Dataset::load_locomo(data()?, false)?,
        "locomo-sessions" => Dataset::load_locomo(data()?, true)?,
        _ => unreachable!(),
    };
    dataset.limit_questions(args.limit);
    let mode = if args.ingest == "write" {
        IngestMode::Write
    } else {
        IngestMode::Upsert
    };
    let query_prefix = std::env::var("MEMINI_EMBED_QUERY_PREFIX").unwrap_or_default();
    let (embedder, disk_cache): (Arc<dyn Embedder>, Option<Arc<DiskCache>>) = if let Some(base) =
        std::env::var("MEMINI_EMBED_BASE_URL")
            .ok()
            .filter(|value| !value.is_empty())
    {
        let dimensions = std::env::var("MEMINI_EMBED_DIMS")
            .ok()
            .and_then(|value| value.parse().ok())
            .unwrap_or(args.dims);
        let model =
            std::env::var("MEMINI_EMBED_MODEL").unwrap_or_else(|_| "text-embedding-3-small".into());
        let client = Arc::new(OpenAiEmbedder::new(
            &base,
            &std::env::var("MEMINI_EMBED_API_KEY").unwrap_or_default(),
            &model,
            dimensions,
        )?);
        let cache_path = std::env::temp_dir().join(format!(
            "memini-embcache-{}-{dimensions}.bin",
            model.replace(['/', ':'], "_")
        ));
        let cache = Arc::new(DiskCache::new(
            Arc::new(Batched::new(client, 20, 24_000, 8_000)),
            &cache_path,
        )?);
        eprintln!(
            "using embeddings endpoint {base} (model={model} dims={dimensions}); embedding cache {} ({} cached)",
            cache_path.display(),
            cache.len().await
        );
        (cache.clone(), Some(cache))
    } else {
        eprintln!(
            "using deterministic local embedder (dims={}) — set MEMINI_EMBED_BASE_URL for a real model",
            args.dims
        );
        (Arc::new(FakeEmbedder::new(args.dims)), None)
    };
    if !args.vec_gate.is_empty() {
        let rows = vector_gate_sweep(
            &dataset,
            embedder.clone(),
            args.k[0],
            &args.vec_gate,
            &query_prefix,
        )
        .await?;
        println!(
            "{}",
            gate_markdown("vector relevance gate sweep", &rows, args.k[0])
        );
        if let Some(cache) = &disk_cache {
            cache.save().await?;
        }
        return Ok(());
    }
    if !args.rerank_gate.is_empty() {
        let encoder = CrossEncoder::new(
            &args.rerank_url,
            &args.rerank_model,
            &std::env::var("MEMINI_RERANK_API_KEY").unwrap_or_default(),
            args.rerank_max_doc_chars,
            args.rerank_max_batch_chars,
        )?;
        let rows = rerank_gate_sweep(
            &dataset,
            embedder.clone(),
            &encoder,
            args.k[0],
            args.rerank_gate_pool,
            &args.rerank_gate,
            &query_prefix,
        )
        .await?;
        println!(
            "{}",
            gate_markdown("cross-encoder rerank gate sweep", &rows, args.k[0])
        );
        if let Some(cache) = &disk_cache {
            cache.save().await?;
        }
        return Ok(());
    }
    anyhow::ensure!(
        !args.distill || args.ingest == "write",
        "--distill requires --ingest=write"
    );
    let needs_llm = args.llm_rerank || args.rewrite || args.distill;
    let llm: Option<Arc<dyn Client>> = if needs_llm {
        let base = std::env::var("MEMINI_LLM_BASE_URL")?;
        let key = std::env::var("MEMINI_LLM_API_KEY").unwrap_or_default();
        let model = std::env::var("MEMINI_LLM_MODEL")?;
        Some(
            if std::env::var("MEMINI_LLM_API").unwrap_or_default() == "anthropic" {
                Arc::new(AnthropicClient::new(&base, &key, &model, 4096)?) as Arc<dyn Client>
            } else {
                Arc::new(OpenAiClient::new(&base, &key, &model, 4096)?) as Arc<dyn Client>
            },
        )
    } else {
        None
    };
    let reranker: Option<Arc<dyn Reranker>> = if !args.rerank_url.is_empty() {
        Some(Arc::new(CrossEncoder::new(
            &args.rerank_url,
            &args.rerank_model,
            &std::env::var("MEMINI_RERANK_API_KEY").unwrap_or_default(),
            args.rerank_max_doc_chars,
            args.rerank_max_batch_chars,
        )?))
    } else if args.llm_rerank {
        Some(Arc::new(LlmReranker::new(llm.clone().unwrap())))
    } else {
        None
    };
    let options = BenchOptions {
        mode,
        query_prefix,
        document_prefix: std::env::var("MEMINI_EMBED_DOC_PREFIX").unwrap_or_default(),
        fusion_alpha: args.fusion,
        pool_factor: args.pool_factor,
        pool_floor: args.pool_floor,
        reranker,
        rerank_pool: args.llm_rerank_pool,
        answerer: llm,
        query_rewrite: args.rewrite,
        temporal_boost: if args.rerank { 0.0 } else { 0.40 },
        distill: args.distill,
    };
    let mut rows = run_configured(&dataset, embedder.clone(), &args.k, options.clone()).await?;
    if args.rerank {
        for row in &mut rows {
            row.system.push_str("-pure");
        }
        let mut temporal = run_configured(
            &dataset,
            embedder,
            &args.k,
            BenchOptions {
                temporal_boost: 0.40,
                ..options
            },
        )
        .await?;
        for row in &mut temporal {
            row.system.push_str("-temporal");
        }
        rows.extend(temporal);
    }
    if let Some(cache) = disk_cache {
        cache.save().await?;
    }
    for k in &args.k {
        println!(
            "{}",
            markdown(
                &rows
                    .iter()
                    .filter(|row| row.k == *k)
                    .cloned()
                    .collect::<Vec<_>>()
            )
        );
    }
    std::fs::create_dir_all(&args.out)?;
    let path = args.out.join(format!("{}.json", dataset.name));
    std::fs::write(
        &path,
        serde_json::to_vec_pretty(
            &serde_json::json!({"config":{"suite":args.suite,"holdout":args.holdout,"session_doc":args.session_doc,"ingest_mode":args.ingest,"distill":args.distill,"ks":args.k,"limit":args.limit,"concurrency":args.concurrency,"fusion_alpha":args.fusion,"pool_factor":args.pool_factor,"pool_floor":args.pool_floor},"results":rows}),
        )?,
    )?;
    eprintln!("wrote {}", path.display());
    Ok(())
}
