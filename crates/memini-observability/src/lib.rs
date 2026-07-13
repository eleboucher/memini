use chrono::{DateTime, SecondsFormat, Utc};
use std::{
    collections::BTreeMap,
    fmt::Write,
    sync::{Arc, Mutex},
};

#[derive(Clone, Default)]
pub struct Registry(Arc<Mutex<RegistryData>>);

#[derive(Default)]
struct RegistryData {
    counters: BTreeMap<Series, f64>,
    gauges: BTreeMap<Series, f64>,
    histograms: BTreeMap<Series, Histogram>,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
struct Series {
    name: String,
    labels: Vec<(String, String)>,
}

#[derive(Default)]
struct Histogram {
    count: u64,
    sum: f64,
}

impl Registry {
    pub fn inc(&self, name: &str, labels: &[(&str, &str)]) {
        self.add(name, labels, 1.0);
    }

    pub fn add(&self, name: &str, labels: &[(&str, &str)], value: f64) {
        if let Ok(mut data) = self.0.lock() {
            *data.counters.entry(series(name, labels)).or_default() += value;
        }
    }

    pub fn set(&self, name: &str, labels: &[(&str, &str)], value: f64) {
        if let Ok(mut data) = self.0.lock() {
            data.gauges.insert(series(name, labels), value);
        }
    }

    pub fn observe(&self, name: &str, labels: &[(&str, &str)], value: f64) {
        if let Ok(mut data) = self.0.lock() {
            let histogram = data.histograms.entry(series(name, labels)).or_default();
            histogram.count += 1;
            histogram.sum += value;
        }
    }

    pub fn render(&self) -> String {
        let Ok(data) = self.0.lock() else {
            return String::new();
        };
        let mut output = String::new();
        for (series, value) in &data.counters {
            render_sample(&mut output, &series.name, &series.labels, *value);
        }
        for (series, value) in &data.gauges {
            render_sample(&mut output, &series.name, &series.labels, *value);
        }
        for (series, histogram) in &data.histograms {
            render_sample(
                &mut output,
                &format!("{}_sum", series.name),
                &series.labels,
                histogram.sum,
            );
            render_sample(
                &mut output,
                &format!("{}_count", series.name),
                &series.labels,
                histogram.count as f64,
            );
        }
        output
    }
}

fn series(name: &str, labels: &[(&str, &str)]) -> Series {
    let mut labels = labels
        .iter()
        .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
        .collect::<Vec<_>>();
    labels.sort();
    Series {
        name: name.to_owned(),
        labels,
    }
}

fn render_sample(output: &mut String, name: &str, labels: &[(String, String)], value: f64) {
    output.push_str(name);
    if !labels.is_empty() {
        output.push('{');
        for (index, (label, value)) in labels.iter().enumerate() {
            if index > 0 {
                output.push(',');
            }
            let escaped = value.replace('\\', "\\\\").replace('"', "\\\"");
            let _ = write!(output, "{label}=\"{escaped}\"");
        }
        output.push('}');
    }
    let _ = writeln!(output, " {value}");
}

#[derive(Clone, Default)]
pub struct DependencyTracker(Arc<Mutex<BTreeMap<String, DependencyStatus>>>);

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct DependencyStatus {
    pub ok: bool,
    pub last_error: String,
    pub last_success: Option<DateTime<Utc>>,
}

impl DependencyTracker {
    pub fn record(&self, dependency: &str, error: Option<&str>) {
        if let Ok(mut statuses) = self.0.lock() {
            let status = statuses.entry(dependency.to_owned()).or_default();
            match error {
                Some(error) => {
                    status.ok = false;
                    status.last_error = error.to_owned();
                }
                None => {
                    status.ok = true;
                    status.last_error.clear();
                    status.last_success = Some(Utc::now());
                }
            }
        }
    }

    pub fn get(&self, dependency: &str) -> DependencyStatus {
        self.0
            .lock()
            .ok()
            .and_then(|statuses| statuses.get(dependency).cloned())
            .unwrap_or(DependencyStatus {
                ok: true,
                ..Default::default()
            })
    }
}

pub fn timestamp(value: Option<DateTime<Utc>>) -> Option<String> {
    value.map(|value| value.to_rfc3339_opts(SecondsFormat::Secs, true))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn registry_and_dependencies() {
        let registry = Registry::default();
        registry.inc("calls_total", &[("result", "ok")]);
        registry.observe("latency_seconds", &[("op", "read")], 0.5);
        let rendered = registry.render();
        assert!(rendered.contains("calls_total{result=\"ok\"} 1"));
        assert!(rendered.contains("latency_seconds_sum{op=\"read\"} 0.5"));

        let dependencies = DependencyTracker::default();
        dependencies.record("llm", Some("down"));
        assert!(!dependencies.get("llm").ok);
        dependencies.record("llm", None);
        assert!(dependencies.get("llm").ok);
    }
}
