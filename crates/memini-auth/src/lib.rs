use std::{
    collections::HashMap,
    fs,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use getrandom::fill;
use memini_store::{ApiKey, ApiKeyStore, StoreError};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;
use thiserror::Error;

const CACHE_TTL: Duration = Duration::from_secs(10);

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Principal {
    pub name: String,
    pub home_namespace: String,
    pub default_namespace: String,
}
#[derive(Debug, Error)]
pub enum AuthError {
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error("{0}")]
    File(String),
}

#[derive(Default)]
struct Cache {
    at: Option<Instant>,
    non_empty: bool,
}
#[derive(Clone, Default)]
pub struct FileKeySet {
    by_hash: HashMap<String, ApiKey>,
    by_name: HashMap<String, ApiKey>,
}
#[derive(Clone)]
pub struct Config {
    admin_key: String,
    store: Option<Arc<dyn ApiKeyStore>>,
    file_keys: Option<Arc<FileKeySet>>,
    cache: Arc<Mutex<Cache>>,
}

impl Config {
    pub fn new(admin_key: impl Into<String>, store: Option<Arc<dyn ApiKeyStore>>) -> Self {
        Self {
            admin_key: admin_key.into(),
            store,
            file_keys: None,
            cache: Arc::new(Mutex::new(Cache::default())),
        }
    }
    pub fn with_file_keys(mut self, keys: Option<Arc<FileKeySet>>) -> Self {
        self.file_keys = keys;
        self
    }
    pub fn file_keys(&self) -> Vec<ApiKey> {
        self.file_keys
            .as_deref()
            .map_or_else(Vec::new, FileKeySet::keys)
    }
    pub fn is_file_key(&self, name: &str) -> bool {
        self.file_keys
            .as_deref()
            .is_some_and(|v| v.by_name.contains_key(name))
    }
    pub fn invalidate(&self) {
        self.cache.lock().expect("auth cache lock").at = None;
    }
    pub async fn authenticate(&self, token: &str) -> Result<(Option<Principal>, bool), AuthError> {
        if !self.admin_key.is_empty()
            && bool::from(token.as_bytes().ct_eq(self.admin_key.as_bytes()))
        {
            return Ok((None, true));
        }
        if !token.is_empty()
            && let Some(files) = &self.file_keys
            && let Some(key) = files.by_hash.get(&hash_token(token))
        {
            return Ok(resolve(key));
        }
        if !token.is_empty()
            && let Some(store) = &self.store
        {
            return Ok(store
                .get_api_key_by_hash(&hash_token(token))
                .await?
                .as_ref()
                .map_or((None, false), resolve));
        }
        if !self.admin_key.is_empty()
            || self
                .file_keys
                .as_deref()
                .is_some_and(|v| !v.by_hash.is_empty())
        {
            return Ok((None, false));
        }
        if let Some(store) = &self.store
            && self.table_non_empty(store).await
        {
            return Ok((None, false));
        }
        Ok((None, true))
    }
    async fn table_non_empty(&self, store: &Arc<dyn ApiKeyStore>) -> bool {
        {
            let cache = self.cache.lock().expect("auth cache lock");
            if cache.at.is_some_and(|v| v.elapsed() < CACHE_TTL) {
                return cache.non_empty;
            }
        }
        let Ok(keys) = store.list_api_keys().await else {
            self.cache.lock().expect("auth cache lock").non_empty = true;
            return true;
        };
        let mut cache = self.cache.lock().expect("auth cache lock");
        cache.at = Some(Instant::now());
        cache.non_empty = !keys.is_empty();
        cache.non_empty
    }
}
fn resolve(key: &ApiKey) -> (Option<Principal>, bool) {
    if key.disabled {
        (None, false)
    } else {
        (
            Some(Principal {
                name: key.name.clone(),
                home_namespace: key.home_namespace.clone(),
                default_namespace: key.default_namespace.clone(),
            }),
            true,
        )
    }
}
pub fn hash_token(token: &str) -> String {
    format!("{:x}", Sha256::digest(token.as_bytes()))
}
pub fn generate_secret() -> Result<String, getrandom::Error> {
    let mut data = [0_u8; 32];
    fill(&mut data)?;
    Ok(data.iter().map(|v| format!("{v:02x}")).collect())
}

#[derive(Deserialize)]
struct Document {
    #[serde(default)]
    keys: Vec<Entry>,
}
#[derive(Deserialize)]
struct Entry {
    name: String,
    #[serde(default)]
    hash: String,
    #[serde(default)]
    secret: String,
    #[serde(default)]
    home: String,
    #[serde(default)]
    default_namespace: String,
    #[serde(default)]
    disabled: bool,
}
pub fn load_file_keys(path: &str) -> Result<FileKeySet, AuthError> {
    let raw = fs::read_to_string(path)
        .map_err(|e| AuthError::File(format!("api keys file {path}: {e}")))?;
    let doc: Document = serde_yaml::from_str(&raw)
        .map_err(|e| AuthError::File(format!("api keys file {path}: invalid YAML: {e}")))?;
    let mut set = FileKeySet::default();
    for (index, entry) in doc.keys.into_iter().enumerate() {
        let key = validate_entry(index, &entry)
            .map_err(|e| AuthError::File(format!("api keys file {path}: {e}")))?;
        if set.by_name.contains_key(&key.name) {
            return Err(AuthError::File(format!(
                "api keys file {path}: entry #{} (name {:?}): duplicate name",
                index + 1,
                key.name
            )));
        }
        if let Some(previous) = set.by_hash.get(&key.hash) {
            return Err(AuthError::File(format!(
                "api keys file {path}: entry #{} (name {:?}): hash collides with entry for {:?}",
                index + 1,
                key.name,
                previous.name
            )));
        }
        set.by_hash.insert(key.hash.clone(), key.clone());
        set.by_name.insert(key.name.clone(), key);
    }
    Ok(set)
}
fn validate_entry(index: usize, entry: &Entry) -> Result<ApiKey, String> {
    let name = entry.name.trim();
    let label = format!("entry #{}", index + 1);
    if name.is_empty() {
        return Err(format!("{label}: name is required"));
    }
    let label = format!("{label} (name {name:?})");
    let has_hash = !entry.hash.trim().is_empty();
    let has_secret = !entry.secret.is_empty();
    if has_hash == has_secret {
        return Err(format!(
            "{label}: exactly one of hash or secret is required"
        ));
    }
    let hash = if has_hash {
        let h = entry.hash.trim().to_lowercase();
        if h.len() != 64 || !h.bytes().all(|v| v.is_ascii_hexdigit()) {
            return Err(format!(
                "{label}: hash must be a 32-byte hex-encoded SHA-256 (got {} bytes)",
                h.len() / 2
            ));
        }
        h
    } else {
        hash_token(&entry.secret)
    };
    let home = normalize_namespace(&entry.home);
    if !home.is_empty() {
        validate_namespace(&home).map_err(|e| format!("{label}: invalid home namespace: {e}"))?;
    }
    let default_namespace = normalize_namespace(&entry.default_namespace);
    if !default_namespace.is_empty() {
        validate_namespace(&default_namespace)
            .map_err(|e| format!("{label}: invalid default_namespace: {e}"))?;
    }
    Ok(ApiKey {
        name: name.to_owned(),
        hash,
        home_namespace: home,
        default_namespace,
        created_at: None,
        disabled: entry.disabled,
    })
}
impl FileKeySet {
    pub fn keys(&self) -> Vec<ApiKey> {
        let mut v: Vec<_> = self.by_name.values().cloned().collect();
        v.sort_by(|a, b| a.name.cmp(&b.name));
        v
    }
    pub async fn shadowed_names(&self, store: &dyn ApiKeyStore) -> Result<Vec<String>, StoreError> {
        let mut names: Vec<_> = store
            .list_api_keys()
            .await?
            .into_iter()
            .filter(|v| self.by_name.contains_key(&v.name))
            .map(|v| v.name)
            .collect();
        names.sort();
        Ok(names)
    }
}
pub fn normalize_namespace(value: &str) -> String {
    let mut v = value.trim().trim_matches('/').to_owned();
    while v.contains("//") {
        v = v.replace("//", "/");
    }
    v
}
pub fn validate_namespace(value: &str) -> Result<(), String> {
    if value.is_empty() {
        Err("namespace is empty".into())
    } else if value.len() > 256 {
        Err("namespace exceeds 256 bytes".into())
    } else if value.contains('\0') {
        Err("namespace contains NUL byte".into())
    } else {
        Ok(())
    }
}
pub fn home_conflict_warning(key_name: &str, key_home: &str, header_home: &str) -> String {
    if key_home.is_empty() || header_home.is_empty() || key_home == header_home {
        String::new()
    } else {
        format!(
            "home namespace {header_home:?} from X-Memini-Home ignored: API key {key_name:?} is bound to home {key_home:?}, which wins"
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn helpers() {
        assert_eq!(
            hash_token("abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(generate_secret().unwrap().len(), 64);
        assert_eq!(normalize_namespace(" work//memini/ "), "work/memini");
        assert!(validate_namespace("work/memini").is_ok());
        assert!(validate_namespace("").is_err());
    }
    #[tokio::test]
    async fn admin_and_dev() {
        let admin = Config::new("secret", None);
        assert!(admin.authenticate("secret").await.unwrap().1);
        assert!(!admin.authenticate("wrong").await.unwrap().1);
        let dev = Config::new("", None);
        assert!(dev.authenticate("garbage").await.unwrap().1);
    }
}
