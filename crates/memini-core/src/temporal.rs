use crate::search::{RerankWeights, Scored, rerank_with};
use chrono::{DateTime, Datelike, Duration, NaiveDate, TimeZone, Utc};
use regex::Regex;
use std::sync::LazyLock;

pub const DEFAULT_TEMPORAL_BOOST: f64 = 0.40;
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TimeAnchor {
    pub days: i64,
    pub tolerance: i64,
}
static NUMBERED: LazyLock<Vec<(Regex, i64)>> = LazyLock::new(|| {
    vec![
        (Regex::new(r"(\d+)\s+days?\s+ago").unwrap(), 1),
        (Regex::new(r"(\d+)\s+weeks?\s+ago").unwrap(), 7),
        (Regex::new(r"(\d+)\s+months?\s+ago").unwrap(), 30),
    ]
});
static MONTH_DAY_YEAR: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+((?:19|20)\d{2})\b").unwrap()
});
static MONTH_YEAR: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+((?:19|20)\d{2})\b").unwrap()
});
static MONTH_DAY: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+(\d{1,2})(?:st|nd|rd|th)?\b").unwrap()
});
static MONTH_ONLY: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\b(?:in|last|back in|since|during)\s+(january|february|march|april|may|june|july|august|september|october|november|december)\b").unwrap()
});
static SEASON: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\b(?:last|in the|this past)\s+(spring|summer|fall|autumn|winter)\b").unwrap()
});
static YEAR: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\bin\s+((?:19|20)\d{2})\b").unwrap());

pub fn anchor(query: &str, now: DateTime<Utc>) -> Option<TimeAnchor> {
    let query = query.to_lowercase();
    for (pattern, multiplier) in NUMBERED.iter() {
        if let Some(captures) = pattern.captures(&query) {
            return Some(TimeAnchor {
                days: captures[1].parse::<i64>().ok()? * multiplier,
                tolerance: if *multiplier == 1 {
                    2
                } else if *multiplier == 7 {
                    5
                } else {
                    10
                },
            });
        }
    }
    for (phrase, days, tolerance) in [
        ("a couple days ago", 2, 2),
        ("a couple of days ago", 2, 2),
        ("yesterday", 1, 1),
        ("a week", 7, 3),
        ("last week", 7, 3),
        ("a month", 30, 7),
        ("last month", 30, 7),
        ("a year", 365, 30),
        ("last year", 365, 30),
        ("recently", 14, 14),
    ] {
        if query.contains(phrase) {
            return Some(TimeAnchor { days, tolerance });
        }
    }
    if let Some(c) = MONTH_DAY_YEAR.captures(&query) {
        return date_anchor(
            c[3].parse().ok()?,
            month(&c[1])?,
            c[2].parse().ok()?,
            now,
            2,
        );
    }
    if let Some(c) = MONTH_YEAR.captures(&query) {
        return date_anchor(c[2].parse().ok()?, month(&c[1])?, 15, now, 15);
    }
    if let Some(c) = MONTH_DAY.captures(&query) {
        return recent_anchor(month(&c[1])?, c[2].parse().ok()?, now, 2);
    }
    if let Some(c) = MONTH_ONLY.captures(&query) {
        return recent_anchor(month(&c[1])?, 15, now, 15);
    }
    if let Some(c) = SEASON.captures(&query) {
        let month = match &c[1] {
            "spring" => 4,
            "summer" => 7,
            "fall" | "autumn" => 10,
            "winter" => 1,
            _ => return None,
        };
        return recent_anchor(month, 15, now, 45);
    }
    if let Some(c) = YEAR.captures(&query) {
        return date_anchor(c[1].parse().ok()?, 7, 1, now, 120);
    }
    None
}
fn month(value: &str) -> Option<u32> {
    [
        "january",
        "february",
        "march",
        "april",
        "may",
        "june",
        "july",
        "august",
        "september",
        "october",
        "november",
        "december",
    ]
    .iter()
    .position(|v| *v == value)
    .map(|v| v as u32 + 1)
}
fn date_anchor(
    year: i32,
    month: u32,
    day: u32,
    now: DateTime<Utc>,
    tolerance: i64,
) -> Option<TimeAnchor> {
    let target =
        Utc.from_utc_datetime(&NaiveDate::from_ymd_opt(year, month, day)?.and_hms_opt(0, 0, 0)?);
    Some(TimeAnchor {
        days: (now - target).num_hours() / 24,
        tolerance,
    })
}
fn recent_anchor(month: u32, day: u32, now: DateTime<Utc>, tolerance: i64) -> Option<TimeAnchor> {
    let mut year = now.year();
    let mut target =
        Utc.from_utc_datetime(&NaiveDate::from_ymd_opt(year, month, day)?.and_hms_opt(0, 0, 0)?);
    if target > now {
        year -= 1;
        target =
            Utc.from_utc_datetime(&NaiveDate::from_ymd_opt(year, month, day)?.and_hms_opt(0, 0, 0)?)
    }
    Some(TimeAnchor {
        days: (now - target).num_hours() / 24,
        tolerance,
    })
}
pub fn rerank_temporal(
    results: &[Scored],
    query: &str,
    now: DateTime<Utc>,
    weights: RerankWeights,
    stability_k: f64,
    boost: f64,
) -> Vec<Scored> {
    let mut base = rerank_with(results, now, weights, stability_k);
    let Some(anchor) = anchor(query, now).filter(|_| boost > 0.0) else {
        return base;
    };
    let target = now - Duration::days(anchor.days);
    for item in &mut base {
        let date = item.memory.valid_from.unwrap_or(item.memory.created_at);
        item.score *= 1.0 + boost * proximity(date, target, anchor.tolerance);
    }
    base.sort_by(|a, b| b.score.total_cmp(&a.score));
    base
}
fn proximity(date: DateTime<Utc>, target: DateTime<Utc>, tolerance: i64) -> f64 {
    if tolerance <= 0 {
        return 0.0;
    }
    let delta = ((target - date).num_seconds() as f64 / 86400.0).abs();
    if delta <= tolerance as f64 {
        1.0
    } else if delta <= (3 * tolerance) as f64 {
        1.0 - (delta - tolerance as f64) / (2 * tolerance) as f64
    } else {
        0.0
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn extracts_relative_and_absolute_anchors() {
        let now = Utc.with_ymd_and_hms(2026, 6, 1, 0, 0, 0).unwrap();
        assert_eq!(
            anchor("what did I do 3 weeks ago", now),
            Some(TimeAnchor {
                days: 21,
                tolerance: 5
            })
        );
        assert_eq!(
            anchor("the milestone yesterday", now),
            Some(TimeAnchor {
                days: 1,
                tolerance: 1
            })
        );
        assert_eq!(
            anchor("what shipped back in march", now),
            date_anchor(2026, 3, 15, now, 15)
        );
        assert_eq!(
            anchor("on october 13, 2023", now),
            date_anchor(2023, 10, 13, now, 2)
        );
        assert!(anchor("may i ask", now).is_none());
    }
}
