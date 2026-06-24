// catalog/manifest.rs — plugin manifest + marketplace-list fetch with TTL cache.
//
// Ports Go's manifest package:
//   - PluginManifest / RegistrySpec structs
//   - PluginClient: per-plugin manifest fetch with 60-second TTL and serve-stale
//   - ListClient: marketplace list fetch with 60-second TTL and serve-stale
//
// URL validation: only http(s) are accepted (mirrors Go's url.Parse check).

use serde::{Deserialize, Serialize};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// DEFAULT_TTL is how long a successfully-fetched manifest is served before
/// the next request triggers a refresh. Mirrors Go's DefaultTTL = 60s.
pub const DEFAULT_TTL: Duration = Duration::from_secs(60);

/// RegistrySpec holds the OCI registry coordinates for one plugin.
/// Mirrors Go's manifest.RegistrySpec.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct RegistrySpec {
    pub host: String,
    pub namespace: String,
    #[serde(rename = "publicURL", default)]
    pub public_url: String,
}

/// PluginManifest is the per-plugin self-describing manifest.
/// Mirrors Go's manifest.PluginManifest.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PluginManifest {
    #[serde(rename = "schemaVersion", default)]
    pub schema_version: u32,
    #[serde(rename = "pluginId")]
    pub plugin_id: String,
    #[serde(default)]
    pub publisher: String,
    pub registry: RegistrySpec,
    #[serde(default)]
    pub versions: Vec<String>,
}

/// validate_plugin validates a PluginManifest, mirroring Go's validatePlugin.
pub fn validate_plugin(m: &PluginManifest) -> Result<(), String> {
    if m.plugin_id.is_empty() {
        return Err("pluginId required".into());
    }
    if m.registry.host.is_empty() {
        return Err("registry.host required".into());
    }
    if m.registry.namespace.is_empty() {
        return Err("registry.namespace required".into());
    }
    Ok(())
}

/// validate_http_url returns an error if the URL is not http or https.
fn validate_http_url(url: &str) -> Result<(), String> {
    match url::Url::parse(url) {
        Ok(u) if u.scheme() == "http" || u.scheme() == "https" => Ok(()),
        Ok(u) => Err(format!("URL scheme {:?} is not http(s): {}", u.scheme(), url)),
        Err(e) => Err(format!("invalid URL {url}: {e}")),
    }
}

// ---------------------------------------------------------------------------
// PluginClient — per-plugin manifest URL with TTL cache and serve-stale
// ---------------------------------------------------------------------------

struct PluginCache {
    cached: Option<PluginManifest>,
    fetched_at: Option<Instant>,
}

/// PluginClient fetches + caches one per-plugin manifest URL.
/// Mirrors Go's manifest.PluginClient.
pub struct PluginClient {
    url: String,
    ttl: Duration,
    inner: Arc<Mutex<PluginCache>>,
}

impl PluginClient {
    pub fn new(url: String, ttl: Option<Duration>) -> Self {
        PluginClient {
            url,
            ttl: ttl.unwrap_or(DEFAULT_TTL),
            inner: Arc::new(Mutex::new(PluginCache {
                cached: None,
                fetched_at: None,
            })),
        }
    }

    /// fetch returns the cached manifest, refreshing it if stale.
    /// On a refresh failure with a warm cache it returns the stale value;
    /// on a cold miss it returns the error.
    /// Mirrors Go's PluginClient.Fetch.
    pub async fn fetch(&self, client: &reqwest::Client) -> Result<PluginManifest, String> {
        // Fast path: serve from cache if fresh.
        {
            let guard = self.inner.lock().unwrap();
            if let (Some(cached), Some(fetched_at)) = (&guard.cached, guard.fetched_at) {
                if fetched_at.elapsed() < self.ttl {
                    return Ok(cached.clone());
                }
            }
        }

        // Slow path: fetch, validate, cache.
        let result = fetch_json::<PluginManifest>(client, &self.url).await;
        match result {
            Ok(m) => {
                validate_plugin(&m)?;
                let mut guard = self.inner.lock().unwrap();
                guard.cached = Some(m.clone());
                guard.fetched_at = Some(Instant::now());
                Ok(m)
            }
            Err(e) => {
                let guard = self.inner.lock().unwrap();
                if let Some(stale) = &guard.cached {
                    // Serve stale on refresh failure.
                    eprintln!("plugin manifest refresh failed, serving stale ({}): {}", self.url, e);
                    Ok(stale.clone())
                } else {
                    Err(format!("fetch plugin manifest {}: {}", self.url, e))
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// ListClient — marketplace list URL with TTL cache and serve-stale
// ---------------------------------------------------------------------------

/// listDoc is the JSON shape for the curated marketplace list.
#[derive(Deserialize)]
struct ListDoc {
    #[allow(dead_code)]
    #[serde(rename = "schemaVersion", default)]
    schema_version: u32,
    plugins: Vec<String>,
}

struct ListCache {
    cached: Option<Vec<String>>,
    fetched_at: Option<Instant>,
}

/// ListClient fetches + caches the curated marketplace list (array of per-plugin
/// manifest URLs). Mirrors Go's manifest.ListClient.
pub struct ListClient {
    url: String,
    ttl: Duration,
    inner: Arc<Mutex<ListCache>>,
}

impl ListClient {
    pub fn new(url: String, ttl: Option<Duration>) -> Self {
        ListClient {
            url,
            ttl: ttl.unwrap_or(DEFAULT_TTL),
            inner: Arc::new(Mutex::new(ListCache {
                cached: None,
                fetched_at: None,
            })),
        }
    }

    /// fetch returns the cached URL list, refreshing it if stale.
    /// On a refresh failure with a warm cache it returns the stale value;
    /// on a cold miss it returns the error.
    /// Mirrors Go's ListClient.Fetch.
    pub async fn fetch(&self, client: &reqwest::Client) -> Result<Vec<String>, String> {
        // Fast path.
        {
            let guard = self.inner.lock().unwrap();
            if let (Some(cached), Some(fetched_at)) = (&guard.cached, guard.fetched_at) {
                if fetched_at.elapsed() < self.ttl {
                    return Ok(cached.clone());
                }
            }
        }

        // Slow path.
        let result = fetch_json::<ListDoc>(client, &self.url).await;
        match result {
            Ok(doc) => {
                // Validate each URL is http(s).
                for u in &doc.plugins {
                    validate_http_url(u)
                        .map_err(|e| format!("marketplace list entry {u:?}: {e}"))?;
                }
                let mut guard = self.inner.lock().unwrap();
                guard.cached = Some(doc.plugins.clone());
                guard.fetched_at = Some(Instant::now());
                Ok(doc.plugins)
            }
            Err(e) => {
                let guard = self.inner.lock().unwrap();
                if let Some(stale) = &guard.cached {
                    eprintln!("marketplace list refresh failed, serving stale ({}): {}", self.url, e);
                    Ok(stale.clone())
                } else {
                    Err(format!("fetch marketplace list {}: {}", self.url, e))
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Shared HTTP helper
// ---------------------------------------------------------------------------

/// fetch_json GETs a URL and JSON-decodes the body. Returns an error on
/// non-200 or decode failure.
pub async fn fetch_json<T: serde::de::DeserializeOwned>(
    client: &reqwest::Client,
    url: &str,
) -> Result<T, String> {
    validate_http_url(url)?;
    let resp = client
        .get(url)
        .send()
        .await
        .map_err(|e| format!("GET {url}: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("GET {url} status {status}: {body}"));
    }
    resp.json::<T>()
        .await
        .map_err(|e| format!("decode JSON from {url}: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_http_url_accepts_http_and_https() {
        assert!(validate_http_url("http://example.com/m.json").is_ok());
        assert!(validate_http_url("https://example.com/m.json").is_ok());
    }

    #[test]
    fn validate_http_url_rejects_other_schemes() {
        assert!(validate_http_url("ftp://example.com/m.json").is_err());
        assert!(validate_http_url("file:///tmp/m.json").is_err());
        assert!(validate_http_url("not-a-url").is_err());
    }

    #[test]
    fn validate_plugin_rejects_missing_fields() {
        let mut m = PluginManifest {
            schema_version: 1,
            plugin_id: "".into(),
            publisher: "".into(),
            registry: RegistrySpec::default(),
            versions: vec![],
        };
        assert!(validate_plugin(&m).is_err()); // missing pluginId
        m.plugin_id = "test".into();
        assert!(validate_plugin(&m).is_err()); // missing registry.host
        m.registry.host = "ghcr.io".into();
        assert!(validate_plugin(&m).is_err()); // missing registry.namespace
        m.registry.namespace = "ns/p".into();
        assert!(validate_plugin(&m).is_ok()); // OK
    }
}
