// catalog/mod.rs — public API for the in-process plugin catalog.
//
// This module ports the Go control-plane's catalog/federation/version/artifact
// logic into the Tauri shell (Rust), so the marketplace works without a
// control-plane HTTP roundtrip.
//
// Public surface:
//   - catalog::list(client, refs) -> Vec<Plugin>
//   - catalog::versions_with_status(ref) -> Vec<VersionStatus>
//   - catalog::resolve_artifact(client, ref, version, platform) -> Option<Artifact>
//   - catalog::sources::{read_sources_in, add_source_in, remove_source_in, build_plugin_refs}
//   - catalog::registry::{PluginRef, Plugin, Artifact, VersionStatus, SourceInfo}

pub mod manifest;
pub mod registry;
pub mod sources;

pub use registry::{
    Artifact, Plugin, PluginRef, RegistryCoords, SourceInfo, VersionStatus,
    resolve_artifact, sort_semver_desc, versions_with_status,
};
pub use sources::{SourceRecord, add_source_in, read_sources_in, remove_source_in};

use registry::{Footprint, blob_url, fetch_oci_manifest, ghcr_authed_get, tag_forms};

// ---------------------------------------------------------------------------
// Required plugin IDs (control-plane policy, not self-declared by plugins)
// Mirrors Go's catalog.DefaultRequired.
// ---------------------------------------------------------------------------
pub const DEFAULT_REQUIRED: &[&str] = &["core-app", "core-datasource"];

/// list returns the marketplace catalog: every plugin the provider yields that
/// resolves to a footprint at its highest validated (else highest preview) version.
/// Required plugins sort first, then by plugin_id.
/// Mirrors Go's Client.List: fetches the OCI config blob for each plugin to
/// populate display_name, description, type, grafana_slug.
pub async fn list(client: &reqwest::Client, refs: &[PluginRef]) -> Vec<Plugin> {
    let required_set: std::collections::HashSet<&str> =
        DEFAULT_REQUIRED.iter().copied().collect();

    // Resolve every plugin concurrently. Each ref_to_plugin does several
    // sequential GHCR roundtrips (manifest + config blob); serial resolution
    // made the marketplace take many seconds to load with N plugins.
    let resolved = futures::future::join_all(
        refs.iter().map(|r| ref_to_plugin(client, r, &required_set)),
    )
    .await;
    let mut out: Vec<Plugin> = resolved.into_iter().flatten().collect();

    // Required first, then alpha by plugin_id.
    out.sort_by(|a, b| {
        if a.required != b.required {
            return b.required.cmp(&a.required); // true > false
        }
        a.footprint.plugin_id.cmp(&b.footprint.plugin_id)
    });
    out
}

/// ref_to_plugin converts a PluginRef into a Plugin catalog entry.
/// Uses the highest validated version (or highest preview with version="").
/// Fetches the OCI config blob to populate display_name, description, type,
/// grafana_slug. Mirrors Go's Client.latest + Client.read + fetchFootprint.
/// Returns None when neither validated nor preview versions exist, or the OCI
/// manifest is absent.
async fn ref_to_plugin(
    client: &reqwest::Client,
    r: &PluginRef,
    required_set: &std::collections::HashSet<&str>,
) -> Option<Plugin> {
    let (version, namespace, preview_only) = pick_version(r)?;

    // Try each tag form (bare, then v-prefixed) until we get an OCI manifest.
    // Mirrors Go's Client.read loop.
    let mut footprint = Footprint {
        plugin_id: r.plugin_id.clone(),
        grafana_slug: String::new(),
        plugin_type: String::new(),
        display_name: String::new(),
        description: String::new(),
        platform_plugin: false,
        logical_views: vec![],
        query_entities: vec![],
    };
    let mut platforms: Vec<String> = vec![];
    let mut manifest_found = false;

    'outer: for tag in tag_forms(&version) {
        let man = match fetch_oci_manifest(client, &r.reg.host, &namespace, &r.plugin_id, &tag).await {
            Ok(Some(m)) => m,
            Ok(None) => continue,
            Err(e) => {
                eprintln!("catalog: fetch manifest {}/{} @{}: {}", namespace, r.plugin_id, tag, e);
                continue;
            }
        };

        // Fetch the config blob to get the footprint.
        // Mirrors Go's fetchFootprint(ctx, repo, man.Config).
        if !man.config.digest.is_empty() {
            let config_url = blob_url(
                &r.reg.public_base(),
                &namespace,
                &r.plugin_id,
                &man.config.digest,
            );
            match fetch_config_blob(client, &config_url).await {
                Ok(fp) => footprint = fp,
                Err(e) => {
                    eprintln!("catalog: fetch config blob {}: {}", config_url, e);
                    // Proceed with default footprint (plugin_id is set above).
                }
            }
        }

        // Collect platform annotations from layers.
        for layer in &man.layers {
            if let Some(pl) = layer.annotations.get("io.opencapital.platform") {
                if !pl.is_empty() {
                    platforms.push(pl.clone());
                }
            }
        }

        manifest_found = true;
        break 'outer;
    }

    if !manifest_found {
        return None;
    }

    // Ensure plugin_id is always populated from the ref (config blob may omit it).
    if footprint.plugin_id.is_empty() {
        footprint.plugin_id = r.plugin_id.clone();
    }

    // Preview-only: blank the version so latest_validated_version is "".
    // Mirrors Go's catalog.go:155-157: if preview { p.Version = "" }
    let emitted_version = if preview_only { String::new() } else { version };

    Some(Plugin {
        footprint,
        required: required_set.contains(r.plugin_id.as_str()),
        version: emitted_version,
        platforms,
        source: SourceInfo {
            url: r.manifest_url.clone(),
            publisher: r.publisher.clone(),
            verified: r.verified,
        },
    })
}

/// pick_version returns (version, namespace, preview_only):
///   - validated: (highest validated version, trusted namespace, false)
///   - preview-only: (highest preview version, staging namespace, true)
/// Returns None when no versions exist.
/// Mirrors Go's pick() in catalog.go.
fn pick_version(r: &PluginRef) -> Option<(String, String, bool)> {
    if let Some(v) = r.validated.first() {
        return Some((v.clone(), r.reg.namespace.clone(), false));
    }
    if let Some(v) = r.preview.first() {
        return Some((v.clone(), r.reg.staging_namespace.clone(), true));
    }
    None
}

/// fetch_config_blob fetches a raw blob URL and deserializes it as a Footprint.
/// Mirrors Go's fetchFootprint reading + json.Unmarshal(b, &fp).
/// The blob URL is pre-constructed via blob_url() — no OCI token dance needed
/// for public blobs (GHCR serves anonymous blob GETs after a manifest fetch).
async fn fetch_config_blob(client: &reqwest::Client, url: &str) -> Result<Footprint, String> {
    // GHCR blobs require the anonymous token dance — a plain GET returns 401,
    // which left every footprint empty; install_all then skips plugins whose
    // grafana_slug is empty, so nothing got installed.
    let resp = ghcr_authed_get(client, url).await?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("config blob {status}: {body}"));
    }
    resp.json::<Footprint>()
        .await
        .map_err(|e| format!("decode footprint: {e}"))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalog::registry::RegistryCoords;

    #[test]
    fn pick_version_validated_wins() {
        let r = PluginRef {
            manifest_url: "http://example.com/m.json".into(),
            plugin_id: "test".into(),
            publisher: "pub".into(),
            verified: true,
            reg: RegistryCoords {
                host: "ghcr.io".into(),
                namespace: "ns/trusted".into(),
                staging_namespace: "ns/staging".into(),
                public_url: String::new(),
            },
            validated: vec!["v1.0.0".into()],
            preview: vec!["v1.1.0".into()],
        };
        let (ver, ns, preview) = pick_version(&r).unwrap();
        assert_eq!(ver, "v1.0.0");
        assert_eq!(ns, "ns/trusted");
        assert!(!preview);
    }

    #[test]
    fn pick_version_preview_only() {
        let r = PluginRef {
            manifest_url: "http://example.com/m.json".into(),
            plugin_id: "test".into(),
            publisher: "pub".into(),
            verified: true,
            reg: RegistryCoords {
                host: "ghcr.io".into(),
                namespace: "ns/trusted".into(),
                staging_namespace: "ns/staging".into(),
                public_url: String::new(),
            },
            validated: vec![],
            preview: vec!["v0.9.0".into()],
        };
        let (ver, ns, preview) = pick_version(&r).unwrap();
        assert_eq!(ver, "v0.9.0");
        assert_eq!(ns, "ns/staging");
        assert!(preview);
    }

    #[test]
    fn pick_version_none_when_empty() {
        let r = PluginRef {
            manifest_url: "http://example.com/m.json".into(),
            plugin_id: "test".into(),
            publisher: "pub".into(),
            verified: true,
            reg: RegistryCoords::default(),
            validated: vec![],
            preview: vec![],
        };
        assert!(pick_version(&r).is_none());
    }
}
