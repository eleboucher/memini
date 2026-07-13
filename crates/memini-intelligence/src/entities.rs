use std::collections::HashSet;

pub const MAX_ENTITIES: usize = 12;
const MAX_SPAN: usize = 4;

#[derive(Default)]
struct Span {
    words: Vec<String>,
    raw: Vec<String>,
    initial: bool,
}

pub fn entities(text: &str) -> Vec<String> {
    let spans = entity_spans(text);
    let mut mid = HashSet::new();
    for span in &spans {
        for (i, word) in span.words.iter().enumerate() {
            if !span.initial || i > 0 {
                mid.insert(word.clone());
            }
        }
    }
    let mut seen = HashSet::new();
    let mut out = Vec::new();
    for span in spans {
        if span.initial
            && span.words.len() < 2
            && !mid.contains(&span.words[0])
            && !self_evident(&span.raw[0])
        {
            continue;
        }
        let key = span.words.join(" ");
        if seen.insert(key.clone()) {
            out.push(key);
            if out.len() == MAX_ENTITIES {
                break;
            }
        }
    }
    out
}

fn entity_spans(text: &str) -> Vec<Span> {
    let mut spans = Vec::new();
    let mut current = Span::default();
    let flush = |current: &mut Span, spans: &mut Vec<Span>| {
        if !current.words.is_empty()
            && current.words.len() <= MAX_SPAN
            && !current.words.iter().all(|w| calendar(w))
        {
            spans.push(std::mem::take(current));
        } else {
            *current = Span::default();
        }
    };
    for line in text.lines() {
        let mut sentence_start = true;
        let mut bracket = false;
        for token in line.split_whitespace() {
            if bracket || token.starts_with('[') {
                bracket = !token.contains(']');
                flush(&mut current, &mut spans);
                continue;
            }
            let (trimmed, ends) = trim_token(token);
            if trimmed.is_empty() {
                flush(&mut current, &mut spans);
                if ends {
                    sentence_start = true;
                }
                continue;
            }
            let normalized = normalize(&trimmed);
            if !starts_upper(&trimmed)
                || stopword(&normalized)
                || normalized.is_empty()
                || !normalized.chars().any(char::is_alphabetic)
                || normalized.chars().count() < 2
            {
                flush(&mut current, &mut spans);
            } else {
                if current.words.is_empty() {
                    current.initial = sentence_start;
                }
                current.words.push(normalized);
                current.raw.push(trimmed);
            }
            if ends {
                flush(&mut current, &mut spans);
            }
            sentence_start = ends;
        }
        flush(&mut current, &mut spans);
    }
    flush(&mut current, &mut spans);
    spans
}
fn trim_token(token: &str) -> (String, bool) {
    let trimmed = token
        .trim_matches(|c: char| !c.is_alphanumeric() && c != '\'' && c != '’' && c != '-')
        .to_owned();
    let trailing = trimmed
        .rfind(|_: char| true)
        .and_then(|_| token.rfind(&trimmed).map(|i| &token[i + trimmed.len()..]))
        .unwrap_or(token);
    (trimmed, trailing.chars().any(|c| ".!?;:".contains(c)))
}
fn starts_upper(s: &str) -> bool {
    s.chars().next().is_some_and(char::is_uppercase)
}
fn self_evident(s: &str) -> bool {
    s.chars()
        .skip(1)
        .any(|c| c.is_uppercase() || c.is_ascii_digit())
}
fn normalize(s: &str) -> String {
    s.to_lowercase()
        .replace('’', "'")
        .trim_end_matches("'s")
        .trim_end_matches('\'')
        .to_owned()
}
fn calendar(w: &str) -> bool {
    matches!(
        w,
        "monday"
            | "tuesday"
            | "wednesday"
            | "thursday"
            | "friday"
            | "saturday"
            | "sunday"
            | "january"
            | "february"
            | "march"
            | "april"
            | "may"
            | "june"
            | "july"
            | "august"
            | "september"
            | "october"
            | "november"
            | "december"
    )
}

pub fn stopword(word: &str) -> bool {
    let stem = word.split_once('\'').map_or(word, |v| v.0);
    STOPWORDS.contains(&word) || STOPWORDS.contains(&stem)
}
static STOPWORDS: &[&str] = &[
    "i",
    "me",
    "my",
    "mine",
    "we",
    "us",
    "our",
    "ours",
    "you",
    "your",
    "yours",
    "he",
    "him",
    "his",
    "she",
    "her",
    "hers",
    "it",
    "its",
    "they",
    "them",
    "their",
    "theirs",
    "this",
    "that",
    "these",
    "those",
    "the",
    "a",
    "an",
    "some",
    "any",
    "each",
    "every",
    "all",
    "both",
    "few",
    "many",
    "much",
    "one",
    "two",
    "other",
    "another",
    "such",
    "same",
    "own",
    "no",
    "none",
    "what",
    "when",
    "where",
    "who",
    "whom",
    "whose",
    "why",
    "how",
    "which",
    "is",
    "are",
    "was",
    "were",
    "be",
    "been",
    "being",
    "am",
    "do",
    "does",
    "did",
    "don",
    "didn",
    "doesn",
    "isn",
    "aren",
    "wasn",
    "weren",
    "have",
    "has",
    "had",
    "haven",
    "hasn",
    "hadn",
    "will",
    "would",
    "wouldn",
    "should",
    "shouldn",
    "could",
    "couldn",
    "can",
    "cannot",
    "may",
    "might",
    "must",
    "shall",
    "won",
    "let",
    "get",
    "got",
    "go",
    "going",
    "gonna",
    "come",
    "make",
    "see",
    "know",
    "think",
    "want",
    "need",
    "try",
    "keep",
    "and",
    "or",
    "but",
    "nor",
    "so",
    "yet",
    "if",
    "then",
    "than",
    "as",
    "at",
    "by",
    "for",
    "from",
    "in",
    "into",
    "of",
    "off",
    "on",
    "onto",
    "out",
    "over",
    "to",
    "under",
    "up",
    "down",
    "with",
    "without",
    "about",
    "after",
    "before",
    "during",
    "while",
    "since",
    "until",
    "because",
    "though",
    "although",
    "however",
    "also",
    "too",
    "very",
    "not",
    "there",
    "here",
    "again",
    "once",
    "just",
    "still",
    "even",
    "only",
    "now",
    "hey",
    "hi",
    "hello",
    "bye",
    "goodbye",
    "wow",
    "oh",
    "ah",
    "ok",
    "okay",
    "yes",
    "yeah",
    "yep",
    "nope",
    "thanks",
    "thank",
    "please",
    "sorry",
    "sure",
    "great",
    "good",
    "cool",
    "nice",
    "well",
    "right",
    "awesome",
    "amazing",
    "perfect",
    "exactly",
    "definitely",
    "absolutely",
    "maybe",
    "perhaps",
    "anyway",
    "alright",
    "congrats",
    "congratulations",
    "cheers",
    "dear",
    "happy",
    "glad",
    "sounds",
    "haha",
    "lol",
    "hmm",
    "today",
    "tomorrow",
    "yesterday",
    "tonight",
];

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn basics() {
        let cases: [(&str, &[&str]); 12] = [
            (
                "We decided to use Postgres instead of SQLite for the vector store.",
                &["postgres", "sqlite"],
            ),
            (
                "The freeze starts the week before Black Friday according to Matt Patterson.",
                &["black friday", "matt patterson"],
            ),
            (
                "She has been reading Charlotte's Web to the kids.",
                &["charlotte web"],
            ),
            (
                "Previously, this foundation used paper records for everything they tracked.",
                &[],
            ),
            (
                "Sweden was cold. She moved away from Sweden four years ago.",
                &["sweden"],
            ),
            ("LGBTQ support groups meet weekly here.", &["lgbtq"]),
            (
                "[8:00 pm on 8 May, 2023] Caroline: I went to a support group yesterday.",
                &[],
            ),
            (
                "[8:00 pm on 8 May, 2023] Melanie: Hey Caroline! Good to see you!",
                &["caroline"],
            ),
            (
                "Someone mentioned that Maria took the last two weeks of December off.",
                &["maria"],
            ),
            (
                "We also watch \"Elf\" during the holidays with the kids.",
                &["elf"],
            ),
            (
                "Decision: for the database engine, the team standardized on Postgres with pgvector.",
                &["postgres"],
            ),
            (
                "The dump ran 90 minutes over and finished at 4:30 in the morning.",
                &[],
            ),
        ];
        for (text, want) in cases {
            assert_eq!(entities(text), want, "{text}");
        }
    }
}
