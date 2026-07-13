use crate::{RecallInput, Result, Service, ServiceError};
use memini_core::memory::Tier;
use std::collections::HashSet;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Origin {
    Primary,
    Ancestor,
    Home,
    Link,
    Call,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReadSetEntry {
    pub namespace: String,
    pub origin: Origin,
    pub tiers: Option<Vec<Tier>>,
}
pub(crate) fn ancestors(namespace: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut value = namespace;
    while let Some(index) = value.rfind('/') {
        value = &value[..index];
        if !value.is_empty() {
            out.push(value.to_owned())
        }
    }
    out
}
fn durable(requested: &[Tier]) -> Vec<Tier> {
    if requested.is_empty() {
        vec![Tier::Semantic, Tier::Procedural]
    } else {
        requested
            .iter()
            .copied()
            .filter(|v| matches!(v, Tier::Semantic | Tier::Procedural))
            .collect()
    }
}
impl Service {
    pub(crate) async fn resolve_read_set(&self, input: &RecallInput) -> Result<Vec<ReadSetEntry>> {
        if input.namespaces.len() > 16 {
            return Err(ServiceError::InvalidInput(
                "recall: at most 16 explicit namespaces".into(),
            ));
        }
        let all_needed = input.subtree
            || input.scope == "everywhere"
            || input.namespaces.iter().any(|v| v.ends_with("/*"));
        let all = if all_needed {
            self.store.list_namespaces().await?
        } else {
            vec![]
        };
        let mut entries = Vec::new();
        if !input.namespaces.is_empty() {
            for raw in &input.namespaces {
                let prefix = raw.strip_suffix("/*");
                let names = if let Some(prefix) = prefix {
                    all.iter()
                        .filter(|v| *v == prefix || v.starts_with(&(prefix.to_owned() + "/")))
                        .cloned()
                        .collect()
                } else {
                    vec![raw.clone()]
                };
                for ns in names {
                    entries.push(ReadSetEntry {
                        origin: if ns == input.namespace {
                            Origin::Primary
                        } else {
                            Origin::Call
                        },
                        namespace: ns,
                        tiers: None,
                    });
                }
            }
        } else {
            if !matches!(input.scope.as_str(), "" | "full" | "project" | "everywhere") {
                return Err(ServiceError::InvalidInput(format!(
                    "recall: invalid scope {:?}",
                    input.scope
                )));
            }
            let primary = if input.subtree || input.scope == "everywhere" {
                all.iter()
                    .filter(|v| {
                        *v == &input.namespace || v.starts_with(&(input.namespace.clone() + "/"))
                    })
                    .cloned()
                    .collect::<Vec<_>>()
            } else {
                vec![input.namespace.clone()]
            };
            for ns in primary {
                entries.push(ReadSetEntry {
                    namespace: ns,
                    origin: Origin::Primary,
                    tiers: None,
                });
            }
            if self.cascade && input.scope != "project" {
                let tiers = durable(&input.tiers);
                if !tiers.is_empty() {
                    for ns in ancestors(&input.namespace) {
                        entries.push(ReadSetEntry {
                            namespace: ns,
                            origin: Origin::Ancestor,
                            tiers: Some(tiers.clone()),
                        });
                    }
                    if !input.home.is_empty() {
                        entries.push(ReadSetEntry {
                            namespace: input.home.clone(),
                            origin: Origin::Home,
                            tiers: Some(tiers.clone()),
                        });
                    }
                    if let Some(links) = &self.links {
                        for link in links.list_links(&input.namespace).await? {
                            let restricted = if link.tiers.is_empty() {
                                tiers.clone()
                            } else {
                                tiers
                                    .iter()
                                    .copied()
                                    .filter(|v| link.tiers.contains(v))
                                    .collect()
                            };
                            if !restricted.is_empty() {
                                entries.push(ReadSetEntry {
                                    namespace: link.destination,
                                    origin: Origin::Link,
                                    tiers: Some(restricted),
                                });
                            }
                        }
                    }
                }
            }
        }
        let mut seen = HashSet::new();
        entries.retain(|v| seen.insert(v.namespace.clone()));
        if let Some(index) = entries.iter().position(|v| v.namespace == input.namespace) {
            entries.swap(0, index)
        }
        entries.truncate(64);
        Ok(entries)
    }
}
