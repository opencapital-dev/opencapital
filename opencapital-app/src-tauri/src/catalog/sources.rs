// catalog/sources.rs — official ∪ user-added plugin source union with verified badge.
//
// Ports Go's sources package:
//   - Official list URLs → Verified=true
//   - User-added URLs (from local JSON store, not already official) → Verified=false
//   - Dedupe by URL (official wins)
//   - Skip unreachable manifests without failing the whole catalog
//   - list-fetch failure degrades to "no official set" (user-added still served)
//
// User-added sources persist to ~/.opencapital/sources.json, mirroring the
// selection/pin file I/O pattern in config.rs.

use std::path::{Path, PathBuf};

use crate::catalog::manifest::{PluginClient, PluginManifest};
use crate::catalog::registry::{sort_semver_desc, PluginRef, RegistryCoords};

// ---------------------------------------------------------------------------
// Persisted user-added sources (JSON file, mirrors selection/pin pattern)
// ---------------------------------------------------------------------------

/// SourceRecord is one persisted user-added manifest URL.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct SourceRecord {
    pub manifest_url: String,
    pub publisher: String,
    pub enabled: bool,
}

fn sources_path(base: &Path) -> PathBuf {
    base.join("sources.json")
}

/// read_sources_in reads all user-added sources from the JSON store.
/// Missing file → empty list. Mirrors the read_pins_in pattern.
pub fn read_sources_in(base: &Path) -> Result<Vec<SourceRecord>, String> {
    match std::fs::read_to_string(sources_path(base)) {
        Ok(s) => serde_json::from_str(&s).map_err(|e| format!("parse sources: {e}")),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Vec::new()),
        Err(e) => Err(format!("read sources: {e}")),
    }
}

/// write_sources_in writes the full sources list atomically.
fn write_sources_in(base: &Path, records: &[SourceRecord]) -> Result<(), String> {
    let p = sources_path(base);
    std::fs::create_dir_all(p.parent().unwrap()).map_err(|e| format!("mkdir: {e}"))?;
    std::fs::write(&p, serde_json::to_vec_pretty(records).unwrap())
        .map_err(|e| format!("write sources: {e}"))
}

/// add_source_in validates, fetches, and persists a new user-added source URL.
/// Returns the new SourceRecord. Errors if already present (by URL) or
/// unreachable/invalid. Mirrors Go's handleAddSource logic.
pub async fn add_source_in(
    base: &Path,
    http_client: &reqwest::Client,
    manifest_url: &str,
) -> Result<SourceRecord, String> {
    // Validate http(s)
    match url::Url::parse(manifest_url) {
        Ok(u) if u.scheme() == "http" || u.scheme() == "https" => {}
        _ => return Err("manifest_url must be an http(s) URL".into()),
    }

    // Fetch and validate the manifest.
    let pc = PluginClient::new(manifest_url.to_string(), None);
    let m = pc
        .fetch(http_client)
        .await
        .map_err(|e| format!("manifest unreachable or invalid: {e}"))?;

    // Check for duplicates.
    let mut records = read_sources_in(base)?;
    if records.iter().any(|r| r.manifest_url == manifest_url) {
        return Err("source already added".into());
    }

    let record = SourceRecord {
        manifest_url: manifest_url.to_string(),
        publisher: m.publisher.clone(),
        enabled: true,
    };
    records.push(record.clone());
    write_sources_in(base, &records)?;
    Ok(record)
}

/// remove_source_in removes a user-added source by URL.
/// Returns false (no error) if not found, true if deleted.
pub fn remove_source_in(base: &Path, manifest_url: &str) -> Result<bool, String> {
    let mut records = read_sources_in(base)?;
    let before = records.len();
    records.retain(|r| r.manifest_url != manifest_url);
    if records.len() == before {
        return Ok(false);
    }
    write_sources_in(base, &records)?;
    Ok(true)
}

// ---------------------------------------------------------------------------
// Plugin provider: official ∪ user-added → PluginRef list
// ---------------------------------------------------------------------------

/// build_plugin_refs resolves the full set of PluginRefs from:
///   1. Official list (from the list_url marketplace list JSON)
///   2. User-added sources (from the local JSON store)
///
/// Mirrors Go's sources.Provider.Plugins.
///
/// - Official URLs → Verified=true
/// - User-added URLs not in official list → Verified=false
/// - Dedupe by URL (official wins if same URL appears in both)
/// - Unreachable manifests are skipped (no error)
/// - list-fetch failure degrades to "no official set"
pub async fn build_plugin_refs(
    http_client: &reqwest::Client,
    list_url: &str,
    user_sources: &[SourceRecord],
) -> Vec<PluginRef> {
    // Fetch the official list (failure → empty, not fatal).
    let official_urls: Vec<String> = match fetch_list(http_client, list_url).await {
        Ok(urls) => urls,
        Err(e) => {
            eprintln!("marketplace list fetch failed, degrading to empty official set: {e}");
            vec![]
        }
    };

    let official_set: std::collections::HashSet<&str> =
        official_urls.iter().map(String::as_str).collect();

    // Build the ordered URL list: official first, then user-added non-duplicates.
    let mut order: Vec<(String, bool)> = official_urls
        .iter()
        .map(|u| (u.clone(), true))
        .collect();

    for record in user_sources {
        if record.enabled && !official_set.contains(record.manifest_url.as_str()) {
            order.push((record.manifest_url.clone(), false));
        }
    }

    // Fetch each manifest, skip unreachable.
    let mut out: Vec<PluginRef> = Vec::new();
    for (url, verified) in order {
        let pc = PluginClient::new(url.clone(), None);
        let m = match pc.fetch(http_client).await {
            Ok(m) => m,
            Err(e) => {
                eprintln!("plugin manifest unreachable, skipping ({url}): {e}");
                continue;
            }
        };
        out.push(manifest_to_ref(url, verified, m));
    }
    out
}

/// manifest_to_ref converts a fetched PluginManifest into a PluginRef.
fn manifest_to_ref(manifest_url: String, verified: bool, m: PluginManifest) -> PluginRef {
    PluginRef {
        manifest_url,
        plugin_id: m.plugin_id,
        publisher: m.publisher,
        verified,
        reg: RegistryCoords {
            host: m.registry.host,
            namespace: m.registry.namespace,
            staging_namespace: m.registry.staging_namespace,
            public_url: m.registry.public_url,
        },
        validated: sort_semver_desc(&m.versions),
        preview: sort_semver_desc(&m.preview),
    }
}

/// fetch_list fetches the marketplace list URL and returns the plugin manifest URLs.
async fn fetch_list(http_client: &reqwest::Client, url: &str) -> Result<Vec<String>, String> {
    use crate::catalog::manifest::ListClient;
    let lc = ListClient::new(url.to_string(), None);
    lc.fetch(http_client).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn read_write_sources_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        assert!(read_sources_in(dir.path()).unwrap().is_empty());

        let records = vec![
            SourceRecord {
                manifest_url: "https://example.com/plugin.json".into(),
                publisher: "Acme".into(),
                enabled: true,
            },
        ];
        write_sources_in(dir.path(), &records).unwrap();
        let got = read_sources_in(dir.path()).unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].manifest_url, "https://example.com/plugin.json");
        assert_eq!(got[0].publisher, "Acme");
        assert!(got[0].enabled);
    }

    #[test]
    fn remove_source_in_removes_by_url() {
        let dir = tempfile::tempdir().unwrap();
        let records = vec![
            SourceRecord {
                manifest_url: "https://a.com/m.json".into(),
                publisher: "A".into(),
                enabled: true,
            },
            SourceRecord {
                manifest_url: "https://b.com/m.json".into(),
                publisher: "B".into(),
                enabled: true,
            },
        ];
        write_sources_in(dir.path(), &records).unwrap();

        let deleted = remove_source_in(dir.path(), "https://a.com/m.json").unwrap();
        assert!(deleted);
        let remaining = read_sources_in(dir.path()).unwrap();
        assert_eq!(remaining.len(), 1);
        assert_eq!(remaining[0].manifest_url, "https://b.com/m.json");
    }

    #[test]
    fn remove_source_in_not_found_returns_false() {
        let dir = tempfile::tempdir().unwrap();
        let deleted = remove_source_in(dir.path(), "https://nonexistent.com/m.json").unwrap();
        assert!(!deleted);
    }
}
