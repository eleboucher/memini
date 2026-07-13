use std::{
    env,
    path::{Path, PathBuf},
    process::{Command, Stdio},
};

pub const DEFAULT_NAMESPACE: &str = "default";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NamespaceSource {
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
    let (namespace, source) = if let Some(value) =
        first_non_empty_env(&["MEMINI_NAMESPACE", "MEMINI_DEFAULT_NAMESPACE"])
    {
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
    (
        with_agent_segment(&namespace, env::var("MEMINI_AGENT").ok().as_deref()),
        source,
    )
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
}
