// reconcile/mod.rs — Rust port of lib/instance-bootstrap (A6a).
//
// This module re-implements the Go instance-bootstrap reconciler in Rust so the
// Tauri shell can eventually drop the Go sidecar (A6b). In A6a it is purely
// additive: it compiles and passes unit tests; grafana.rs still shells out to
// the Go sidecar.
//
// Public entry point:
//   reconcile::reconcile(plugins, dirs, client) -> Result<(), String>
//
// Intentionally skipped:
//   - metric_deps.go: reads query_entities / .py metric files for the
//     read-gateway DSL. That surface is DEAD in the sql() world (replaced by
//     direct RisingWave SQL). Do not port; mark for deletion in A6b.

pub mod dashboards;
pub mod download;
pub mod library_panels;
pub mod provision;

use std::path::{Path, PathBuf};

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// ReconcileDirs holds every directory path the reconciler needs.
/// Mirrors grafana.rs's local variables: provisioning, plugins_dir, cache_dir.
#[derive(Debug, Clone)]
pub struct ReconcileDirs {
    /// Grafana's provisioning directory (datasources/, plugins/, dashboards/ live here).
    pub provisioning: PathBuf,
    /// Grafana's plugins directory where symlinks are written (<slug> -> cache entry).
    pub plugins_dir: PathBuf,
    /// Content-addressed cache: <plugin_id>/<version>/<platform>/ per extracted artifact.
    pub cache_dir: PathBuf,
    /// Writable root for per-plugin SQLite (stamped into jsonData as pluginsRoot).
    pub plugin_state_dir: PathBuf,
}

/// ProvisioningConfig carries the instance-level wiring values that get stamped
/// into every plugin's jsonData / secureJsonData. Mirrors Go's Config provisioning
/// fields (without the control-plane fetch fields — those are resolved upstream
/// by the catalog module before calling reconcile).
#[derive(Debug, Clone)]
pub struct ProvisioningConfig {
    /// Org UUID (rendered into jsonData.orgId).
    pub org_id: String,
    /// URL the plugin calls to reach the control plane.
    pub control_plane_url: String,
    /// OTel collector endpoint.
    pub otlp_endpoint: String,
    /// Loopback URL where the shell serves the short-lived instance token.
    pub instance_token_url: String,
    /// RisingWave pg-wire host (plugins connect via pluginclient).
    pub risingwave_host: String,
    /// RisingWave pg-wire port.
    pub risingwave_port: String,
    /// OLTP Postgres host for direct plugin writes.
    pub postgres_host: String,
    /// OLTP Postgres port.
    pub postgres_port: String,
    /// Database name in the OLTP store.
    pub control_db: String,
    /// Loopback URL of the Python compute sidecar.
    pub compute_url: String,
    /// Writable SQLite root stamped into jsonData.pluginsRoot.
    /// Mirrors the plugin_state_dir in ReconcileDirs.
    pub plugin_state_dir: String,
}

/// ResolvedPlugin is a catalog-resolved plugin entry ready for the reconciler.
/// It carries enough to install the binary AND render provisioning YAML.
/// The catalog module populates this; the reconciler does not call the
/// control-plane or the catalog.
#[derive(Debug, Clone)]
pub struct ResolvedPlugin {
    /// Canonical plugin identifier (e.g. "core-app").
    pub plugin_id: String,
    /// Grafana slug (directory name under the plugins dir, e.g. "core-app").
    pub grafana_slug: String,
    /// Grafana plugin type: "app", "datasource", or "panel".
    pub plugin_type: String,
    /// Whether this is a platform/infra plugin (affects datasource provisioning shape).
    pub platform_plugin: bool,
    /// Whether the plugin is required (install failure is fatal when true).
    pub required: bool,
    /// Resolved version string (e.g. "v0.1.3").
    pub version: String,
    /// Resolved artifact for the running platform, or None when no artifact is
    /// published for this platform (e.g. a panel plugin with no binary).
    pub artifact: Option<crate::catalog::Artifact>,
}

// ---------------------------------------------------------------------------
// Main entry point
// ---------------------------------------------------------------------------

/// reconcile installs every plugin's binary (download → verify → extract →
/// symlink) and writes Grafana provisioning YAML.
///
/// Mirrors Go's bootstrap.Run, but receives the already-resolved plugin set +
/// artifacts from the catalog module (no control-plane round-trips here).
///
/// Steps:
///   1. install_all — ensure every plugin's binary is in the cache + symlinked
///   2. prune       — remove symlinks no longer in the desired set
///   3. render      — write provisioning YAML (app/datasource/panel)
///   4. dashboards  — copy bundled dashboard JSON + write provider YAML
pub async fn reconcile(
    plugins: &[ResolvedPlugin],
    dirs: &ReconcileDirs,
    prov_cfg: &ProvisioningConfig,
    client: &reqwest::Client,
) -> Result<(), String> {
    let platform = host_platform();

    // 1. Install all (download / verify / extract / symlink).
    let provisionable = download::install_all(plugins, dirs, &platform, client).await?;

    // 2. Prune stale symlinks.
    download::prune(plugins, dirs)?;

    // 3. Render provisioning YAML (app / datasource / panel).
    provision::render(&provisionable, dirs, prov_cfg)?;

    // 4. Copy bundled dashboards + write the provider YAML.
    dashboards::provision_dashboards(&provisionable, dirs)?;

    Ok(())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// host_platform returns the running host's platform tag (e.g. "darwin-arm64").
/// Mirrors Go's hostPlatform when cfg.Platform is empty.
pub fn host_platform() -> String {
    let os = std::env::consts::OS;
    let arch = std::env::consts::ARCH;
    // Map Rust's OS/arch names to the Go / plugindist convention. plugindist
    // stamps layers with GOOS-GOARCH (e.g. "darwin-arm64"); Rust reports the OS
    // as "macos" and arch as "aarch64", which would never match.
    let os_tag = match os {
        "macos" => "darwin",
        other => other,
    };
    let arch_tag = match arch {
        "aarch64" => "arm64",
        "x86_64" => "amd64",
        other => other,
    };
    format!("{}-{}", os_tag, arch_tag)
}

/// plugin_display_name reads the "name" field from the installed bundle's
/// plugin.json. Falls back to plugin_id on any read/parse error.
/// Mirrors Go's pluginDisplayName.
pub(crate) fn plugin_display_name(plugins_dir: &Path, slug: &str, plugin_id: &str) -> String {
    let path = plugins_dir.join(slug).join("plugin.json");
    let bytes = match std::fs::read(&path) {
        Ok(b) => b,
        Err(_) => return plugin_id.to_string(),
    };
    let v: serde_json::Value = match serde_json::from_slice(&bytes) {
        Ok(v) => v,
        Err(_) => return plugin_id.to_string(),
    };
    v.get("name")
        .and_then(|n| n.as_str())
        .filter(|s| !s.is_empty())
        .unwrap_or(plugin_id)
        .to_string()
}
