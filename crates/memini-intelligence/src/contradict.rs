use std::{collections::HashSet, sync::LazyLock};

use memini_core::memory::fingerprint;
use regex::{Captures, Regex};

use crate::entities::{entities, stopword};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Class {
    Distinct,
    Restatement,
    Update,
}
impl Class {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Distinct => "distinct",
            Self::Restatement => "restatement",
            Self::Update => "update",
        }
    }
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Result {
    pub class: Class,
    pub reason: String,
}
#[derive(Clone, Copy, Debug)]
pub struct Config {
    pub overlap_floor: f64,
    pub residue_max: usize,
    pub neg_overlap_floor: f64,
    pub alias_prefix_min: usize,
}
pub const DEFAULT: Config = Config {
    overlap_floor: 0.5,
    residue_max: 2,
    neg_overlap_floor: 0.6,
    alias_prefix_min: 4,
};

#[derive(Default)]
struct Features {
    tokens: HashSet<String>,
    values: HashSet<String>,
    entities: HashSet<String>,
    negated: bool,
}
static TOKEN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"[a-z]+|[0-9]+(?:[.:][0-9]+)*").unwrap());
static AMPM: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b").unwrap());
static CLOCK: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\b0(\d):(\d{2})\b").unwrap());
static QUOTED: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#""([^\"]{1,80})"|`([^`]{1,80})`|'([^']{1,80})'"#).unwrap());

pub fn classify(new_text: &str, old_text: &str, cfg: Config) -> Result {
    if fingerprint(new_text) == fingerprint(old_text) {
        return result(Class::Restatement, "identical content");
    }
    let new = analyze(new_text);
    let old = analyze(old_text);
    if new.tokens.is_empty() || old.tokens.is_empty() {
        return result(Class::Distinct, "no content tokens");
    }
    let shared = intersection(&new.tokens, &old.tokens);
    let only_new = difference(&new.tokens, &old.tokens);
    let only_old = difference(&old.tokens, &new.tokens);
    let overlap = shared.len() as f64 / new.tokens.len().min(old.tokens.len()) as f64;
    let shared_entity = !intersection(&new.entities, &old.entities).is_empty();
    if overlap < cfg.overlap_floor || (!shared_entity && shared.len() < 2) {
        return result(
            Class::Distinct,
            &format!("overlap {overlap:.2} below floor"),
        );
    }
    let (value_new, value_old) = drop_aliases(
        intersection(&only_new, &new.values),
        intersection(&only_old, &old.values),
        cfg.alias_prefix_min,
    );
    let residue_new = difference(&difference(&only_new, &new.values), cue_words());
    let residue_old = difference(&difference(&only_old, &old.values), cue_words());
    if !value_new.is_empty()
        && !value_old.is_empty()
        && residue_new.len() <= cfg.residue_max
        && residue_old.len() <= cfg.residue_max
    {
        return result(
            Class::Update,
            &format!(
                "value swap: {:?} -> {:?}",
                first(&value_old),
                first(&value_new)
            ),
        );
    }
    if shared_entity || shared.len() >= 3 {
        let cue = retro_cue(new_text);
        let neg_flip = new.negated && !old.negated;
        let new_content = difference(&new.tokens, cue_words());
        let containment =
            difference(&shared, cue_words()).len() as f64 / new_content.len().max(1) as f64;
        if let Some(cue) = cue
            && containment >= cfg.neg_overlap_floor
            && residue_new.len() <= cfg.residue_max
        {
            return result(Class::Update, &format!("polarity: {cue}"));
        }
        if neg_flip && containment >= cfg.neg_overlap_floor.max(0.75) && residue_new.len() <= 1 {
            return result(Class::Update, "polarity: negation flip");
        }
    }
    result(
        Class::Restatement,
        &format!("overlap {overlap:.2}, no decisive change"),
    )
}
fn result(class: Class, reason: &str) -> Result {
    Result {
        class,
        reason: reason.to_owned(),
    }
}

fn analyze(text: &str) -> Features {
    let mut f = Features::default();
    let mut entity_words = HashSet::new();
    for entity in entities(text) {
        entity_words.extend(entity.split_whitespace().map(str::to_owned));
        f.entities.insert(entity);
    }
    let lower = normalize_times(&text.to_lowercase());
    let mut quoted = HashSet::new();
    for caps in QUOTED.captures_iter(&lower) {
        for span in caps.iter().skip(1).flatten() {
            quoted.extend(
                TOKEN
                    .find_iter(span.as_str())
                    .map(|v| v.as_str().to_owned()),
            );
        }
    }
    for found in TOKEN.find_iter(&lower) {
        let mut token = found.as_str().to_owned();
        let mut is_value = quoted.contains(&token)
            || entity_words.contains(&token)
            || token.starts_with(|c: char| c.is_ascii_digit());
        if token.starts_with(|c: char| c.is_ascii_alphabetic()) {
            if negators().contains(token.as_str()) {
                f.negated = true;
            }
            if let Some(number) = number_word(&token) {
                token = number.to_owned();
                is_value = true;
            } else {
                token = normalize_word(&token);
                is_value |= entity_words.contains(&token);
            }
        }
        if token.len() < 2
            || stopword(&token)
            || matches!(token.as_str(), "per" | "via" | "through" | "throughout")
        {
            continue;
        }
        if is_value {
            f.values.insert(token.clone());
        }
        f.tokens.insert(token);
    }
    f
}
fn normalize_times(text: &str) -> String {
    let result = AMPM
        .replace_all(text, |c: &Captures<'_>| {
            let mut hour: u32 = c[1].parse::<u32>().unwrap_or(0) % 12;
            if &c[3] == "pm" {
                hour += 12;
            }
            format!("{hour}:{}", c.get(2).map_or("00", |v| v.as_str()))
        })
        .into_owned();
    CLOCK.replace_all(&result, "$1:$2").into_owned()
}
fn normalize_word(word: &str) -> String {
    let mut word = match word {
        "sent" => "send",
        "ran" => "run",
        "kept" => "keep",
        "built" => "build",
        "wrote" => "write",
        "took" => "take",
        "gave" => "give",
        v => v,
    }
    .to_owned();
    for suffix in ["ies", "ing", "ed", "es", "s"] {
        if word.ends_with(suffix) && word.len() - suffix.len() >= 3 {
            word.truncate(word.len() - suffix.len());
            if suffix == "ies" {
                word.push('y');
            }
            break;
        }
    }
    let bytes = word.as_bytes();
    if bytes.len() >= 4
        && bytes[bytes.len() - 1] == bytes[bytes.len() - 2]
        && !b"aeiou".contains(&bytes[bytes.len() - 1])
    {
        word.pop();
    }
    if word.len() >= 4 && word.ends_with('e') {
        word.pop();
    }
    match word.as_str() {
        "megabyte" => "mb",
        "gigabyte" => "gb",
        "kilobyte" => "kb",
        "terabyte" => "tb",
        "second" => "sec",
        "minute" => "min",
        "hour" => "hr",
        "millisecond" => "ms",
        "request" => "req",
        _ => return word,
    }
    .to_owned()
}
fn number_word(word: &str) -> Option<&'static str> {
    Some(match word {
        "zero" => "0",
        "one" => "1",
        "two" => "2",
        "three" => "3",
        "four" => "4",
        "five" => "5",
        "six" => "6",
        "seven" => "7",
        "eight" => "8",
        "nine" => "9",
        "ten" => "10",
        "eleven" => "11",
        "twelve" => "12",
        "thirteen" => "13",
        "fourteen" => "14",
        "fifteen" => "15",
        "sixteen" => "16",
        "seventeen" => "17",
        "eighteen" => "18",
        "nineteen" => "19",
        "twenty" => "20",
        "thirty" => "30",
        "forty" => "40",
        "fifty" => "50",
        "sixty" => "60",
        "seventy" => "70",
        "eighty" => "80",
        "ninety" => "90",
        "hundred" => "100",
        "thousand" => "1000",
        _ => return None,
    })
}
fn retro_cue(text: &str) -> Option<&'static str> {
    let lower = text.to_lowercase();
    [
        "no longer",
        "anymore",
        "any more",
        "switched from",
        "switched to",
        "switched off",
        "switched away",
        "migrated from",
        "migrated to",
        "migrated off",
        "migrated away",
        "moved off",
        "moved away from",
        "stopped using",
        "stopped doing",
        "we stopped",
        "deprecated",
        "used to",
        "retired",
    ]
    .into_iter()
    .find(|cue| lower.contains(cue))
}
fn negators() -> &'static HashSet<&'static str> {
    static V: LazyLock<HashSet<&str>> = LazyLock::new(|| {
        [
            "not", "no", "never", "cannot", "don", "doesn", "didn", "won", "isn", "aren", "wasn",
            "weren",
        ]
        .into()
    });
    &V
}
fn cue_words() -> &'static HashSet<String> {
    static V: LazyLock<HashSet<String>> = LazyLock::new(|| {
        [
            "longer",
            "anymore",
            "switched",
            "switch",
            "migrated",
            "migrate",
            "moved",
            "mov",
            "deprecated",
            "deprecat",
            "stopped",
            "stopp",
            "stop",
            "used",
            "using",
            "us",
            "doing",
            "retired",
            "retir",
            "dropped",
            "dropp",
            "drop",
            "instead",
            "currently",
        ]
        .into_iter()
        .map(str::to_owned)
        .collect()
    });
    &V
}
fn intersection(a: &HashSet<String>, b: &HashSet<String>) -> HashSet<String> {
    a.intersection(b).cloned().collect()
}
fn difference(a: &HashSet<String>, b: &HashSet<String>) -> HashSet<String> {
    a.difference(b).cloned().collect()
}
fn drop_aliases(
    mut new: HashSet<String>,
    mut old: HashSet<String>,
    min: usize,
) -> (HashSet<String>, HashSet<String>) {
    for a in new.clone() {
        for b in old.clone() {
            if new.contains(&a)
                && old.contains(&b)
                && (num_equal(&a, &b) || prefix_alias(&a, &b, min))
            {
                new.remove(&a);
                old.remove(&b);
            }
        }
    }
    (new, old)
}
fn num_equal(a: &str, b: &str) -> bool {
    a.starts_with(|c: char| c.is_ascii_digit())
        && b.starts_with(|c: char| c.is_ascii_digit())
        && a.trim_end_matches(".0").trim_end_matches('.')
            == b.trim_end_matches(".0").trim_end_matches('.')
}
fn prefix_alias(a: &str, b: &str, min: usize) -> bool {
    let (short, long) = if a.len() <= b.len() { (a, b) } else { (b, a) };
    short.len() >= min && long.starts_with(short)
}
fn first(values: &HashSet<String>) -> &str {
    values.iter().map(String::as_str).min().unwrap_or("")
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn contract() {
        let cases = [
            ("We use Postgres.", "we use  postgres.", Class::Restatement),
            (
                "The project stores vectors in Postgres with the pgvector extension.",
                "Vectors are stored in PostgreSQL with the pgvector extension.",
                Class::Restatement,
            ),
            (
                "Retries use exponential backoff.",
                "Retries use exponential backoff capped at five attempts.",
                Class::Restatement,
            ),
            (
                "Image uploads are limited to 10 megabytes.",
                "Image uploads are limited to 10 MB.",
                Class::Restatement,
            ),
            (
                "The search index is rebuilt nightly at 02:00 UTC.",
                "The search index is rebuilt nightly at 2am UTC.",
                Class::Restatement,
            ),
            (
                "Cache entries expire after a 10 minute TTL.",
                "Cache entries expire after a ten minute TTL.",
                Class::Restatement,
            ),
            (
                "The project stores vectors in Postgres with the pgvector extension.",
                "The project runs database migrations against Postgres with golang-migrate.",
                Class::Distinct,
            ),
            (
                "Retries use exponential backoff capped at five attempts.",
                "Retries are not enabled for POST requests.",
                Class::Distinct,
            ),
            (
                "Cache entries expire after a 10 minute TTL.",
                "Cache entries expire after a 30 minute TTL.",
                Class::Update,
            ),
            (
                "Email is sent through Postmark.",
                "Email is sent through SES.",
                Class::Update,
            ),
            (
                "Background jobs run on NATS JetStream.",
                "We no longer run background jobs on NATS JetStream.",
                Class::Update,
            ),
            (
                "Background jobs run on NATS JetStream.",
                "Background jobs do not run on NATS JetStream.",
                Class::Update,
            ),
            (
                "The admin UI is restricted to the internal network only.",
                "The admin UI is no longer restricted to the internal network.",
                Class::Update,
            ),
            (
                "The reranker is Qwen3-Reranker-0.6B served on port 8002.",
                "The reranker is Qwen3-Reranker-0.6B served on port 9002.",
                Class::Update,
            ),
        ];
        for (old, new, want) in cases {
            let got = classify(new, old, DEFAULT);
            assert_eq!(got.class, want, "{}: {}", got.reason, new);
        }
    }
    #[test]
    fn go_golang_is_not_update() {
        assert_ne!(
            classify(
                "The primary language for services is Golang.",
                "The primary language for services is Go.",
                DEFAULT
            )
            .class,
            Class::Update
        );
    }
}
