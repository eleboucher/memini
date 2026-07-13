use anyhow::Result;
use clap::Parser;
use memini_bench::{Dataset, DocumentMode, IngestMode, QaOptions, QaResult, run_qa};
use memini_embed::{Batched, Cached, Embedder, OpenAiEmbedder};
use memini_llm::{AnthropicClient, Client, OpenAiClient};
use std::{
    collections::HashSet,
    io::{BufRead, Write},
    path::{Path, PathBuf},
    sync::Arc,
};

#[derive(Parser)]
#[command(name = "memini-qa", about = "LLM-judged answer-quality benchmark")]
struct Args {
    #[arg(long, default_value = "locomo", value_parser = ["locomo", "longmemeval", "codingagent"])]
    suite: String,
    #[arg(long)]
    data: PathBuf,
    #[arg(long, default_value = "all", value_parser = ["all", "tune", "held"])]
    holdout: String,
    #[arg(long, default_value = "full", value_parser = ["full", "user-only", "dated"])]
    session_doc: String,
    #[arg(long, default_value = "upsert", value_parser = ["upsert", "write"])]
    ingest: String,
    #[arg(short = 'k', long, default_value_t = 10)]
    k: usize,
    #[arg(long, default_value_t = 0)]
    limit: usize,
    #[arg(long)]
    checkpoint: Option<PathBuf>,
    #[arg(long, default_value = "")]
    reasoning: String,
    #[arg(long, default_value_t = 6)]
    workers: usize,
    #[arg(long, default_value_t = 0.40)]
    temporal_boost: f64,
    #[arg(long)]
    distill: bool,
    #[arg(long)]
    debug: bool,
}

#[tokio::main]
async fn main() {
    if let Err(error) = execute().await {
        eprintln!("qa: {error:#}");
        std::process::exit(1);
    }
}
async fn execute() -> Result<()> {
    let args = Args::parse();
    let mode = match args.session_doc.as_str() {
        "user-only" => DocumentMode::UserOnly,
        "dated" => DocumentMode::Dated,
        _ => DocumentMode::Full,
    };
    let mut dataset = match args.suite.as_str() {
        "longmemeval" => {
            Dataset::load_longmemeval(&args.data, mode)?.split_holdout(&args.holdout)?
        }
        "codingagent" => Dataset::load_coding_agent(&args.data)?,
        _ => Dataset::load_locomo(&args.data, false)?,
    };
    dataset.limit_questions(args.limit);
    let dimensions = std::env::var("MEMINI_EMBED_DIMS")
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(4096);
    let embedder: Arc<dyn Embedder> = Arc::new(Cached::new(
        Arc::new(Batched::new(
            Arc::new(OpenAiEmbedder::new(
                &std::env::var("MEMINI_EMBED_BASE_URL")?,
                &std::env::var("MEMINI_EMBED_API_KEY").unwrap_or_default(),
                &std::env::var("MEMINI_EMBED_MODEL")
                    .unwrap_or_else(|_| "text-embedding-3-small".into()),
                dimensions,
            )?),
            20,
            24_000,
            8_000,
        )),
        16_384,
    )?);
    let base = std::env::var("MEMINI_LLM_BASE_URL")?;
    let key = std::env::var("MEMINI_LLM_API_KEY").unwrap_or_default();
    let model = std::env::var("MEMINI_LLM_MODEL")?;
    let client: Arc<dyn Client> =
        if std::env::var("MEMINI_LLM_API").unwrap_or_default() == "anthropic" {
            Arc::new(AnthropicClient::new(&base, &key, &model, 4096)?)
        } else {
            Arc::new(OpenAiClient::new(&base, &key, &model, 4096)?)
        };
    eprintln!(
        "suite {} ({} ingest): {} items, {} questions, model {}",
        dataset.name,
        args.ingest,
        dataset.items.len(),
        dataset.questions.len(),
        model
    );
    anyhow::ensure!(
        !args.distill || args.ingest == "write",
        "--distill requires --ingest=write"
    );
    let checkpoint = args.checkpoint.unwrap_or_else(|| {
        default_checkpoint(&args.suite, &args.ingest, &args.reasoning, args.distill)
    });
    let existing = load_checkpoint(&checkpoint)?;
    let done = existing
        .iter()
        .map(|result| result.index)
        .collect::<HashSet<_>>();
    eprintln!(
        "resuming: {}/{} already done",
        done.len(),
        dataset.questions.len()
    );
    let selected = (0..dataset.questions.len())
        .filter(|index| !done.contains(index))
        .collect();
    let results = run_qa(
        &dataset,
        embedder,
        client,
        QaOptions {
            k: args.k,
            mode: if args.ingest == "write" {
                IngestMode::Write
            } else {
                IngestMode::Upsert
            },
            reasoning: args.reasoning,
            workers: args.workers,
            temporal_boost: args.temporal_boost,
            distill: args.distill,
            selected,
        },
    )
    .await?;
    if let Some(parent) = checkpoint.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&checkpoint)?;
    for result in &results {
        if args.debug {
            let question = &dataset.questions[result.index];
            eprintln!(
                "\n[Q] {}\n[group={} cat={}]\n[gold] {}\n[answer] {}\n[correct] {}",
                question.query,
                question.group,
                question.category,
                question.answer,
                result.answer,
                result.correct
            );
        }
        serde_json::to_writer(&mut file, result)?;
        writeln!(file)?;
    }
    let mut all = existing;
    all.extend(results);
    report(&all, dataset.questions.len());
    Ok(())
}
fn default_checkpoint(suite: &str, ingest: &str, reasoning: &str, distill: bool) -> PathBuf {
    let mut suffix = if distill { "_distill" } else { "" }.to_owned();
    if !reasoning.is_empty() && reasoning != "minimal" {
        suffix.push('_');
        suffix.push_str(reasoning);
    }
    format!("bench/results/qa_{suite}_{ingest}{suffix}.jsonl").into()
}

fn load_checkpoint(path: &Path) -> Result<Vec<QaResult>> {
    let file = match std::fs::File::open(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(error.into()),
    };
    let mut output = Vec::new();
    for line in std::io::BufReader::new(file).lines() {
        let line = line?;
        if let Ok(result) = serde_json::from_str(&line) {
            output.push(result);
        }
    }
    Ok(output)
}

fn report(results: &[QaResult], total: usize) {
    let correct = results.iter().filter(|result| result.correct).count();
    println!(
        "QA accuracy (LLM-judge): {:.1}% ({}/{} answered; {} total questions)",
        100.0 * correct as f64 / results.len().max(1) as f64,
        correct,
        results.len(),
        total,
    );
    let mut categories = std::collections::BTreeMap::<&str, (usize, usize)>::new();
    for result in results {
        let value = categories.entry(&result.category).or_default();
        value.1 += 1;
        value.0 += usize::from(result.correct);
    }
    for (category, (correct, total)) in categories {
        println!(
            "{category}: {correct}/{total} ({:.1}%)",
            100.0 * correct as f64 / total as f64
        );
    }
}
