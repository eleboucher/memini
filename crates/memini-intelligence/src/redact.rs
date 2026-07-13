use regex::{Captures, Regex};
use serde_json::{Map, Value};
use std::sync::LazyLock;

pub const MARKER: &str = "[REDACTED]";
static RULES: LazyLock<Vec<(Regex, &'static str)>> = LazyLock::new(|| {
    [
        (
            r"(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----",
            MARKER,
        ),
        (r"\bAKIA[0-9A-Z]{16}\b", MARKER),
        (r"\bgh[pousr]_[A-Za-z0-9]{20,}\b", MARKER),
        (r"\bxox[baprs]-[A-Za-z0-9-]{10,}\b", MARKER),
        (r"\bsk-[A-Za-z0-9_-]{16,}\b", MARKER),
        (r"\bAIza[0-9A-Za-z_-]{35}\b", MARKER),
        (
            r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b",
            MARKER,
        ),
    ]
    .into_iter()
    .map(|(p, r)| (Regex::new(p).unwrap(), r))
    .collect()
});
static KEY_VALUE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?i)([A-Za-z0-9_.-]*(?:secret|password|passwd|token|api[_-]?key|access[_-]?key|client[_-]?secret|auth[_-]?token)[A-Za-z0-9_.-]*\s*[=:]\s*)(['"]?)[^\s"']{4,}"#).unwrap()
});
static BEARER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}").unwrap());
static URL_INFO: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:)[^/\s:@]+(@)").unwrap());

pub fn secrets(input: &str) -> String {
    let mut out = input.to_owned();
    for (regex, replacement) in RULES.iter() {
        out = regex.replace_all(&out, *replacement).into_owned();
    }
    out = KEY_VALUE
        .replace_all(&out, |c: &Captures<'_>| {
            format!("{}{}{}", &c[1], &c[2], MARKER)
        })
        .into_owned();
    out = BEARER
        .replace_all(&out, |c: &Captures<'_>| format!("{}{}", &c[1], MARKER))
        .into_owned();
    URL_INFO
        .replace_all(&out, |c: &Captures<'_>| {
            format!("{}{}{}", &c[1], MARKER, &c[2])
        })
        .into_owned()
}
pub fn metadata(map: &mut Map<String, Value>) {
    for value in map.values_mut() {
        redact_value(value);
    }
}
fn redact_value(value: &mut Value) {
    match value {
        Value::String(s) => *s = secrets(s),
        Value::Array(v) => v.iter_mut().for_each(redact_value),
        Value::Object(v) => metadata(v),
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn secret_contract() {
        let cases = [
            ("", ""),
            (
                "the access token expires soon",
                "the access token expires soon",
            ),
            (
                r#"curl -H "Authorization: Bearer abc123DEF456ghi" https://api"#,
                r#"curl -H "Authorization: Bearer [REDACTED]" https://api"#,
            ),
            (
                "DEPLOY_TOKEN=s3cr3t-value ./deploy.sh",
                "DEPLOY_TOKEN=[REDACTED] ./deploy.sh",
            ),
            (r#"password: "hunter2pass""#, r#"password: "[REDACTED]""#),
            ("key AKIAIOSFODNN7EXAMPLE here", "key [REDACTED] here"),
            (
                "clone https://alice:s3cret@github.com/x/y",
                "clone https://alice:[REDACTED]@github.com/x/y",
            ),
        ];
        for (input, want) in cases {
            assert_eq!(secrets(input), want);
        }
    }
    #[test]
    fn metadata_contract() {
        let mut v = serde_json::json!({"commands":["go test ./...", "curl -H 'Authorization: Bearer tok12345678' x"], "nested":{"note":"DEPLOY_TOKEN=abcd1234"}, "count":3});
        metadata(v.as_object_mut().unwrap());
        assert_eq!(v["nested"]["note"], "DEPLOY_TOKEN=[REDACTED]");
        assert_eq!(v["count"], 3);
    }
}
