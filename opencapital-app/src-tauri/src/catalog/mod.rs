// catalog/mod.rs — public API for the in-process plugin catalog.
//
// This module ports the Go control-plane's catalog/federation/version/artifact
// logic into the Tauri shell (Rust), so the marketplace works without a
// control-plane HTTP roundtrip.
//
// Public surface:
//   - catalog::list(refs) -> Vec<Plugin>
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

use registry::Footprint;

// ---------------------------------------------------------------------------
// Required plugin IDs (control-plane policy, not self-declared by plugins)
// Mirrors Go's catalog.DefaultRequired.
// ---------------------------------------------------------------------------
pub const DEFAULT_REQUIRED: &[&str] = &["core-app", "core-datasource"];

/// list returns the marketplace catalog: every plugin the provider yields that
/// resolves to a footprint at its highest validated (else highest preview) version.
/// Required plugins sort first, then by plugin_id.
/// Mirrors Go's Client.List but operates on already-resolved PluginRefs (no OCI
/// fetch — the Tauri catalog reads the footprint from the manifest, not a config blob).
pub fn list(refs: &[PluginRef]) -> Vec<Plugin> {
    let required_set: std::collections::HashSet<&str> =
        DEFAULT_REQUIRED.iter().copied().collect();

    let mut out: Vec<Plugin> = refs
        .iter()
        .filter_map(|r| ref_to_plugin(r, &required_set))
        .collect();

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
/// Returns None when neither validated nor preview versions exist.
fn ref_to_plugin(r: &PluginRef, required_set: &std::collections::HashSet<&str>) -> Option<Plugin> {
    let (version, _preview_only) = pick_version(r)?;
    Some(Plugin {
        footprint: Footprint {
            plugin_id: r.plugin_id.clone(),
            // These fields come from the OCI config blob in the Go version.
            // In the Tauri port we don't fetch the OCI blob for the catalog list —
            // the manifest provides enough identity for display. The footprint fields
            // that can't come from the manifest are left empty (grafana_slug, etc.)
            // and will be populated when instance-bootstrap resolves the artifact.
            grafana_slug: String::new(),
            plugin_type: String::new(),
            display_name: String::new(),
            description: String::new(),
            platform_plugin: false,
            logical_views: vec![],
            query_entities: vec![],
        },
        required: required_set.contains(r.plugin_id.as_str()),
        version,
        platforms: vec![],
        source: SourceInfo {
            url: r.manifest_url.clone(),
            publisher: r.publisher.clone(),
            verified: r.verified,
        },
    })
}

/// pick_version returns (version, preview_only): the highest validated version
/// if any, else the highest preview version (with preview_only=true).
/// Returns None when no versions exist.
fn pick_version(r: &PluginRef) -> Option<(String, bool)> {
    if let Some(v) = r.validated.first() {
        return Some((v.clone(), false));
    }
    if let Some(v) = r.preview.first() {
        return Some((v.clone(), true));
    }
    None
}
