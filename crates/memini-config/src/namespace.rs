use std::{
    collections::HashMap,
    env, fs,
    path::{Path, PathBuf},
    process::{Command, Stdio},
};

use serde::Deserialize;

pub const DEFAULT_NAMESPACE: &str = "default";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NamespaceSource {
    Override,
    Environment,
    GitRemote,
    Git,
    CurrentDirectory,
    Fallback,
}

pub fn resolve_default_namespace() -> (String, NamespaceSource) {
    if let Some(value) = first_non_empty_env(&["MEMINI_DEFAULT_NAMESPACE", "MEMINI_NAMESPACE"]) {
        return (
            sanitize_namespace_path(&value),
            NamespaceSource::Environment,
        );
    }
    let Ok(cwd) = env::current_dir() else {
        return (DEFAULT_NAMESPACE.into(), NamespaceSource::Fallback);
    };
    if let Some(top) = git(&cwd, &["rev-parse", "--show-toplevel"]) {
        return (
            sanitize_namespace(top.file_name().and_then(|v| v.to_str()).unwrap_or_default()),
            NamespaceSource::Git,
        );
    }
    (
        sanitize_namespace(cwd.file_name().and_then(|v| v.to_str()).unwrap_or_default()),
        NamespaceSource::CurrentDirectory,
    )
}

pub fn resolve_plugin_namespace(dir: Option<&Path>) -> (String, NamespaceSource) {
    let override_value = namespace_override(dir);
    let environment = first_non_empty_env(&["MEMINI_NAMESPACE", "MEMINI_DEFAULT_NAMESPACE"]);
    resolve_plugin_namespace_with(
        dir,
        override_value,
        environment,
        env::var("MEMINI_AGENT").ok(),
    )
}

fn resolve_plugin_namespace_with(
    dir: Option<&Path>,
    override_value: Option<String>,
    environment: Option<String>,
    agent: Option<String>,
) -> (String, NamespaceSource) {
    let (namespace, source) = if let Some(value) = override_value {
        (sanitize_namespace_path(&value), NamespaceSource::Override)
    } else if let Some(value) = environment {
        (
            sanitize_namespace_path(&value),
            NamespaceSource::Environment,
        )
    } else {
        let directory = dir
            .map(Path::to_path_buf)
            .or_else(|| env::current_dir().ok());
        directory.map_or_else(
            || (DEFAULT_NAMESPACE.into(), NamespaceSource::Fallback),
            |value| resolve_dir_namespace(&value),
        )
    };
    (with_agent_segment(&namespace, agent.as_deref()), source)
}

#[derive(Deserialize)]
struct OverridesFile {
    #[serde(default)]
    overrides: HashMap<String, NamespaceOverride>,
}

#[derive(Deserialize)]
struct NamespaceOverride {
    namespace: String,
}

pub fn overrides_path() -> Option<PathBuf> {
    let base = env::var("XDG_CONFIG_HOME")
        .ok()
        .filter(|value| !value.trim().is_empty())
        .map(PathBuf::from)
        .or_else(|| {
            env::var("HOME")
                .ok()
                .filter(|value| !value.trim().is_empty())
                .map(|home| PathBuf::from(home).join(".config"))
        })?;
    Some(base.join("memini").join("overrides.json"))
}

pub fn namespace_override(dir: Option<&Path>) -> Option<String> {
    namespace_override_from_path(dir, &overrides_path()?)
}

fn namespace_override_from_path(dir: Option<&Path>, path: &Path) -> Option<String> {
    let raw = fs::read(path).ok()?;
    let file: OverridesFile = serde_json::from_slice(&raw).ok()?;
    if file.overrides.is_empty() {
        return None;
    }
    let key = override_key_for(dir)?;
    file.overrides
        .get(&key.to_string_lossy().into_owned())
        .and_then(|entry| {
            let namespace = entry.namespace.trim();
            (!namespace.is_empty()).then(|| sanitize_namespace_path(namespace))
        })
}

fn override_key_for(dir: Option<&Path>) -> Option<PathBuf> {
    let dir = dir
        .map(Path::to_path_buf)
        .or_else(|| env::current_dir().ok())?;
    let key = git(&dir, &["rev-parse", "--show-toplevel"]).unwrap_or(dir);
    if key.is_absolute() {
        Some(key)
    } else {
        env::current_dir().ok().map(|cwd| cwd.join(key))
    }
}

pub fn resolve_dir_namespace(dir: &Path) -> (String, NamespaceSource) {
    if let Some(remote) =
        git(dir, &["remote", "get-url", "origin"]).and_then(|v| v.to_str().map(str::to_owned))
        && let Some(name) = repo_name_from_remote(&remote)
    {
        return (sanitize_namespace(&name), NamespaceSource::GitRemote);
    }
    if let Some(top) = git(dir, &["rev-parse", "--show-toplevel"]) {
        return (
            sanitize_namespace(top.file_name().and_then(|v| v.to_str()).unwrap_or_default()),
            NamespaceSource::Git,
        );
    }
    (
        sanitize_namespace(dir.file_name().and_then(|v| v.to_str()).unwrap_or_default()),
        NamespaceSource::CurrentDirectory,
    )
}

fn git(dir: &Path, args: &[&str]) -> Option<PathBuf> {
    let output = Command::new("git")
        .args(args)
        .current_dir(dir)
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .output()
        .ok()?;
    output
        .status
        .success()
        .then(|| PathBuf::from(String::from_utf8_lossy(&output.stdout).trim().to_owned()))
}

pub fn repo_name_from_remote(value: &str) -> Option<String> {
    let mut cleaned = value.trim().trim_end_matches('/');
    if cleaned.len() >= 4 && cleaned[cleaned.len() - 4..].eq_ignore_ascii_case(".git") {
        cleaned = &cleaned[..cleaned.len() - 4];
    }
    if cleaned.is_empty() {
        return None;
    }
    if let Some((host, rest)) = cleaned.split_once(':')
        && !host.contains('/')
        && !rest.is_empty()
        && !rest.starts_with('/')
    {
        return last_segment(rest);
    }
    last_segment(cleaned)
}

fn last_segment(value: &str) -> Option<String> {
    value.rsplit('/').find(|v| !v.is_empty()).map(str::to_owned)
}

pub fn sanitize_namespace(value: &str) -> String {
    let value = value.trim().trim_end_matches('/');
    let leaf = value
        .rsplit('/')
        .find(|v| !v.is_empty())
        .unwrap_or_default();
    if leaf.is_empty() || leaf == "." {
        DEFAULT_NAMESPACE.into()
    } else {
        leaf.into()
    }
}

pub fn sanitize_namespace_path(value: &str) -> String {
    let parts: Vec<_> = value.trim().split('/').filter(|v| !v.is_empty()).collect();
    if parts.is_empty() {
        DEFAULT_NAMESPACE.into()
    } else {
        parts.join("/")
    }
}

pub fn with_agent_segment(namespace: &str, agent: Option<&str>) -> String {
    let Some(agent) = agent.map(str::trim).filter(|v| !v.is_empty()) else {
        return namespace.into();
    };
    let mut segment = String::with_capacity(agent.len());
    let mut replacing = false;
    for character in agent.chars() {
        if character.is_ascii_alphanumeric() || matches!(character, '.' | '_' | '-') {
            segment.push(character);
            replacing = false;
        } else if !replacing {
            segment.push('-');
            replacing = true;
        }
    }
    let segment = segment.trim_matches('-');
    if segment.is_empty() {
        namespace.into()
    } else {
        format!("{namespace}/{segment}")
    }
}

fn first_non_empty_env(names: &[&str]) -> Option<String> {
    names
        .iter()
        .filter_map(|name| env::var(name).ok())
        .find(|value| !value.trim().is_empty())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn remote_names_match_go_and_plugins() {
        for (input, expected) in [
            ("git@github.com:eleboucher/memini.git", "memini"),
            ("https://github.com/eleboucher/memini/", "memini"),
            ("https://github.com/eleboucher/Memini.GIT", "Memini"),
            ("git@gitlab.com:group/subgroup/proj.git", "proj"),
            ("not-a-url", "not-a-url"),
        ] {
            assert_eq!(repo_name_from_remote(input).as_deref(), Some(expected));
        }
        assert_eq!(repo_name_from_remote(""), None);
    }

    #[test]
    fn basename_sanitization_matches_go() {
        for (input, expected) in [
            ("", "default"),
            ("   ", "default"),
            ("my-project", "my-project"),
            ("/home/dev/my-project", "my-project"),
            ("nested/path/leaf", "leaf"),
            (".", "default"),
            ("/", "default"),
        ] {
            assert_eq!(sanitize_namespace(input), expected);
        }
    }

    #[test]
    fn path_sanitization_preserves_namespace_hierarchy() {
        for (input, expected) in [
            ("project/agent", "project/agent"),
            ("", "default"),
            ("/x/", "x"),
            ("a//b", "a/b"),
            ("  team/proj  ", "team/proj"),
            ("///", "default"),
        ] {
            assert_eq!(sanitize_namespace_path(input), expected);
        }
    }

    #[test]
    fn agent_segment_matches_plugin_rules() {
        assert_eq!(with_agent_segment("project", None), "project");
        assert_eq!(
            with_agent_segment("project", Some(" reviewer ")),
            "project/reviewer"
        );
        assert_eq!(
            with_agent_segment("project", Some("--code / review--")),
            "project/code-review"
        );
        assert_eq!(with_agent_segment("project", Some("///")), "project");
    }

    #[test]
    fn project_override_beats_environment_namespace() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("overrides.json");
        let project = dir.path().join("project");
        std::fs::create_dir_all(&project).unwrap();
        let key = override_key_for(Some(&project)).unwrap();
        std::fs::write(
            &path,
            serde_json::json!({
                "version": 1,
                "overrides": {
                    key.to_string_lossy().into_owned(): {
                        "namespace": "acme/api",
                        "setAt": "2026-07-12T20:30:00Z"
                    }
                }
            })
            .to_string(),
        )
        .unwrap();
        let override_value = namespace_override_from_path(Some(&project), &path);
        let resolved = resolve_plugin_namespace_with(
            Some(&project),
            override_value,
            Some("pinned-everywhere".into()),
            None,
        );
        assert_eq!(resolved, ("acme/api".into(), NamespaceSource::Override));
        assert_eq!(namespace_override_from_path(Some(dir.path()), &path), None);
    }

    #[test]
    fn malformed_override_file_degrades_to_automatic_resolution() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("overrides.json");
        std::fs::write(&path, "{ not json").unwrap();
        assert_eq!(namespace_override_from_path(Some(dir.path()), &path), None);
    }
}
