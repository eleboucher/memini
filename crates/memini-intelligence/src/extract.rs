use std::sync::LazyLock;

use memini_core::memory::Tier;
use regex::Regex;

pub const MIN_CONFIDENCE: f64 = 0.3;
pub const MAX_PER_EXCHANGE: usize = 5;
pub const CLASSIFY_MAX_CHARS: usize = 400;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Kind {
    Decision,
    Preference,
    Problem,
    Fact,
    HowTo,
}

impl Kind {
    pub const fn tier(self) -> Tier {
        match self {
            Self::Preference | Self::HowTo => Tier::Procedural,
            _ => Tier::Semantic,
        }
    }
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Decision => "decision",
            Self::Preference => "preference",
            Self::Problem => "problem",
            Self::Fact => "fact",
            Self::HowTo => "howto",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Result {
    pub kind: Kind,
    pub content: String,
}

struct Markers {
    kind: Kind,
    patterns: Vec<Regex>,
}
fn compile(raw: &[&str]) -> Vec<Regex> {
    raw.iter()
        .map(|v| Regex::new(v).expect("valid marker regex"))
        .collect()
}

static MARKERS: LazyLock<Vec<Markers>> = LazyLock::new(|| {
    vec![
        Markers {
            kind: Kind::Decision,
            patterns: compile(&[
                r"\blet'?s (use|go with|try|pick|choose|switch to)\b",
                r"\bwe (should|decided|chose|went with|picked|settled on)\b",
                r"\bi'?m going (to|with)\b",
                r"\bbetter (to|than|approach|option|choice)\b",
                r"\binstead of\b",
                r"\brather than\b",
                r"\bthe reason (is|was|being)\b",
                r"\btrade-?off\b",
                r"\bpros and cons\b",
            ]),
        },
        Markers {
            kind: Kind::Preference,
            patterns: compile(&[
                r"\bi prefer\b",
                r"\balways use\b",
                r"\bnever use\b",
                r"\bdon'?t (ever |like to )?(use|do|mock|stub|import)\b",
                r"\bi like (to|when|how)\b",
                r"\bi hate (when|how|it when)\b",
                r"\bplease (always|never|don'?t)\b",
                r"\bmy (rule|preference|style|convention) is\b",
                r"\bwe (always|never)\b",
                r"\buse\b.*\binstead of\b",
            ]),
        },
        Markers {
            kind: Kind::Problem,
            patterns: compile(&[
                r"\b(bug|error|crash|fail|broke|broken|issue|problem)\b",
                r"\bdoesn'?t work\b",
                r"\bnot working\b",
                r"\bkeeps? (failing|crashing|breaking|erroring)\b",
                r"\broot cause\b",
                r"\bthe (problem|issue|bug) (is|was)\b",
                r"\bthe fix (is|was)\b",
                r"\bworkaround\b",
                r"\bfixed (it|the|by)\b",
                r"\bsolution (is|was)\b",
                r"\bresolved\b",
                r"\bpatched\b",
            ]),
        },
        Markers {
            kind: Kind::Fact,
            patterns: compile(&[
                r"\bthe (system|app|server|service|backend|store|database|frontend|api|project|repo|codebase|module|package) (uses|runs on|is|requires|relies on|depends on|is built with|is written in)\b",
                r"\bdeploy(ed|s|ment) (to|on|via|using)\b",
                r"\b(tests?|ci|pipeline) run (on|in|via|using)\b",
                r"\bby default[,;]?\s+(the|it|they|we|this)\b",
                r"\bthe (config|configuration) (is|file is|lives in|is at)\b",
                r"\b(environment variable|env var)\b",
            ]),
        },
        Markers {
            kind: Kind::HowTo,
            patterns: compile(&[
                r"\bto (set up|configure|install|deploy|run|test|debug|enable|disable)\b",
                r"\byou (need to|have to|must) (set|configure|run|install|add|create|enable|disable)\b",
                r"\bsteps? (are|to|for)\b",
                r"\brun (the (command|following)|this (command|snippet))\b",
            ]),
        },
    ]
});

static CODE_LINES: LazyLock<Vec<Regex>> = LazyLock::new(|| {
    compile(&[
        r"^[$#]\s",
        r"^(cd|source|echo|export|pip|npm|git|python|bash|curl|wget|mkdir|rm|cp|mv|ls|cat|grep|find|chmod|sudo|brew|docker)\s",
        r"^```",
        r"^(import|from|def|class|function|const|let|var|return)\s",
        r"^[A-Z_]{2,}=",
        r"^(if|for|while|try|except|elif|else:)\b",
        r"^\w+\.\w+\(",
    ])
});
static HEDGES: LazyLock<Vec<Regex>> = LazyLock::new(|| {
    compile(&[
        r"\bmaybe\b",
        r"\bi think\b",
        r"\bnot sure\b",
        r"\bpossibly\b",
        r"\bmight\b",
        r"\bprobably\b",
        r"\breportedly\b",
        r"\bi guess\b",
        r"\bcould be\b",
    ])
});
static ROLE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?mi)^\s*(user|assistant)\s*:").unwrap());

fn prose_only(segment: &str) -> String {
    let mut prose = Vec::new();
    let mut fenced = false;
    for line in segment.lines() {
        let trimmed = line.trim();
        if trimmed.starts_with("```") {
            fenced = !fenced;
            continue;
        }
        if !fenced && !CODE_LINES.iter().any(|p| p.is_match(trimmed)) {
            prose.push(line);
        }
    }
    let joined = prose.join("\n").trim().to_owned();
    if joined.is_empty() {
        segment.to_owned()
    } else {
        joined
    }
}
fn length_bonus(s: &str) -> usize {
    if s.len() > 500 {
        2
    } else if s.len() > 200 {
        1
    } else {
        0
    }
}
fn best(prose: &str) -> Option<(Kind, usize)> {
    MARKERS
        .iter()
        .map(|set| {
            (
                set.kind,
                set.patterns.iter().filter(|p| p.is_match(prose)).count(),
            )
        })
        .max_by_key(|v| v.1)
        .filter(|v| v.1 > 0)
}
pub fn typed(text: &str) -> Vec<Result> {
    let mut out = Vec::new();
    for segment in text.split("\n\n").map(str::trim).filter(|s| s.len() >= 20) {
        let prose = prose_only(segment).to_lowercase();
        let Some((kind, score)) = best(&prose) else {
            continue;
        };
        if (score + length_bonus(segment)).min(5) as f64 / 5.0 < MIN_CONFIDENCE {
            continue;
        }
        out.push(Result {
            kind,
            content: segment.to_owned(),
        });
        if out.len() == MAX_PER_EXCHANGE {
            break;
        }
    }
    out
}
pub fn classify(text: &str) -> Option<Kind> {
    let segment = text.trim();
    if !(20..=CLASSIFY_MAX_CHARS).contains(&segment.len()) || ROLE.is_match(segment) {
        return None;
    }
    let prose = prose_only(segment).to_lowercase();
    if HEDGES.iter().any(|p| p.is_match(&prose)) {
        return None;
    }
    let (kind, score) = best(&prose)?;
    ((score + length_bonus(segment)).min(5) as f64 / 5.0 >= MIN_CONFIDENCE).then_some(kind)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn classifies_contract() {
        let cases = [
            (
                "We decided to use Postgres instead of SQLite because we need concurrent writes from multiple replicas.",
                Kind::Decision,
            ),
            (
                "My convention is: always use snake_case for table columns. Please never use camelCase here.",
                Kind::Preference,
            ),
            (
                "The bug was a nil deref in the auth middleware. The fix was to guard the token lookup before dereferencing it.",
                Kind::Problem,
            ),
            (
                "The store uses sqlite-vec for hybrid vector search because it keeps the index in the same SQLite database as the metadata. By default the vector index is built at write time.",
                Kind::Fact,
            ),
            (
                "To configure the echo guard, you need to set MEMINI_TURN_ECHO_WINDOW to a duration like 5m. Then you must restart the server for the change to take effect.",
                Kind::HowTo,
            ),
        ];
        for (text, want) in cases {
            assert_eq!(typed(text)[0].kind, want);
        }
    }
    #[test]
    fn noise_and_cap_contract() {
        for text in [
            "ok thanks",
            "Here is the file you asked about. Let me know what you think of it overall.",
            "Hit a bug, then another bug, then a third bug, bugs everywhere today.",
            "```go\nfunc main() {}\n```",
        ] {
            assert!(typed(text).is_empty(), "{text}");
        }
        assert_eq!(typed(&"We decided to use a queue because it decouples writers, instead of synchronous calls.\n\n".repeat(10)).len(), 5);
    }
    #[test]
    fn whole_write_contract() {
        assert_eq!(
            classify("We decided to use Postgres instead of MySQL for the vector store."),
            Some(Kind::Decision)
        );
        for text in [
            "Maybe we should go with Postgres instead of MySQL.",
            "User: which db?\nAssistant: we decided to use postgres instead of mysql",
            "I prefer tabs.",
        ] {
            assert_eq!(classify(text), None);
        }
    }
}
