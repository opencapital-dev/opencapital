// reconcile/provision.rs — render Grafana provisioning YAML.
// Rust port of bootstrap.go (Render / renderApp / renderDatasource /
// renderMinimalDatasource / writeUnsignedPluginsList / pruneProvisioning).

use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::{ProvisioningConfig, ReconcileDirs, ResolvedPlugin};

// ---------------------------------------------------------------------------
// YAML document shapes (mirroring Go's struct tags)
// ---------------------------------------------------------------------------

/// Grafana plugins provisioning schema.
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProvisioningDoc {
    api_version: u32,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    apps: Vec<ProvisioningApp>,
}

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProvisioningApp {
    #[serde(rename = "type")]
    plugin_type: String,
    org_id: u32,
    disabled: bool,
    json_data: HashMap<String, serde_json::Value>,
    secure_json_data: HashMap<String, String>,
}

/// Grafana datasource provisioning schema.
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DatasourceDoc {
    api_version: u32,
    datasources: Vec<DatasourceEntry>,
}

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DatasourceEntry {
    name: String,
    uid: String,
    #[serde(rename = "type")]
    ds_type: String,
    access: String,
    is_default: bool,
    editable: bool,
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    json_data: HashMap<String, serde_json::Value>,
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    secure_json_data: HashMap<String, String>,
}

// ---------------------------------------------------------------------------
// render — main entry
// ---------------------------------------------------------------------------

/// render writes one provisioning YAML per plugin into ProvisioningDir.
/// Mirrors Go's Render.
pub fn render(
    plugins: &[ResolvedPlugin],
    dirs: &ReconcileDirs,
    cfg: &ProvisioningConfig,
) -> Result<(), String> {
    let out_dir = dirs.provisioning.join("plugins");
    fs::create_dir_all(&out_dir)
        .map_err(|e| format!("mkdir {:?}: {}", out_dir, e))?;

    prune_provisioning(&out_dir, plugins)?;

    for p in plugins {
        if p.grafana_slug.is_empty() {
            continue;
        }
        match p.plugin_type.as_str() {
            "datasource" => {
                if p.platform_plugin {
                    render_datasource(p, plugins, dirs, cfg)?;
                } else {
                    render_minimal_datasource(p, dirs, cfg)?;
                }
            }
            "app" => render_app(p, &out_dir, cfg)?,
            "panel" => {
                // Panels only need to load; no provisioning entry.
            }
            other => {
                eprintln!(
                    "[reconcile] plugin {} has unknown grafana type {:?}; skipping provisioning",
                    p.plugin_id, other
                );
            }
        }
    }

    write_unsigned_plugins_list(plugins, &dirs.provisioning)?;
    Ok(())
}

// ---------------------------------------------------------------------------
// App plugin YAML
// ---------------------------------------------------------------------------

fn render_app(
    p: &ResolvedPlugin,
    out_dir: &Path,
    cfg: &ProvisioningConfig,
) -> Result<(), String> {
    let mut json_data: HashMap<String, serde_json::Value> = HashMap::new();
    json_data.insert("pluginId".into(), p.plugin_id.clone().into());
    json_data.insert("orgId".into(), cfg.org_id.clone().into());
    json_data.insert("controlPlaneUrl".into(), cfg.control_plane_url.clone().into());
    json_data.insert(
        "otelExporterOtlpEndpoint".into(),
        cfg.otlp_endpoint.clone().into(),
    );
    json_data.insert("instanceTokenUrl".into(), cfg.instance_token_url.clone().into());
    json_data.insert("risingwaveHost".into(), cfg.risingwave_host.clone().into());
    json_data.insert("risingwavePort".into(), cfg.risingwave_port.clone().into());
    json_data.insert("postgresHost".into(), cfg.postgres_host.clone().into());
    json_data.insert("postgresPort".into(), cfg.postgres_port.clone().into());
    json_data.insert("controlDb".into(), cfg.control_db.clone().into());
    json_data.insert("pluginsRoot".into(), dirs_plugin_state(cfg).into());

    let mut secure: HashMap<String, String> = HashMap::new();
    secure.insert("platformToken".into(), p.platform_token.clone());

    let doc = ProvisioningDoc {
        api_version: 1,
        apps: vec![ProvisioningApp {
            plugin_type: p.grafana_slug.clone(),
            org_id: 1,
            disabled: false,
            json_data,
            secure_json_data: secure,
        }],
    };

    let data = serde_yaml::to_string(&doc)
        .map_err(|e| format!("marshal yaml for {}: {}", p.plugin_id, e))?;
    let path = out_dir.join(format!("{}.yaml", p.plugin_id));
    fs::write(&path, data.as_bytes())
        .map_err(|e| format!("write {:?}: {}", path, e))?;
    Ok(())
}

// ---------------------------------------------------------------------------
// Platform datasource YAML
// ---------------------------------------------------------------------------

fn render_datasource(
    p: &ResolvedPlugin,
    all_plugins: &[ResolvedPlugin],
    dirs: &ReconcileDirs,
    cfg: &ProvisioningConfig,
) -> Result<(), String> {
    // Build the pluginTokens map: plugin_id -> platformToken for every installed plugin.
    let token_map: HashMap<String, String> = all_plugins
        .iter()
        .filter(|pl| !pl.platform_token.is_empty())
        .map(|pl| (pl.plugin_id.clone(), pl.platform_token.clone()))
        .collect();
    let tokens_json = serde_json::to_string(&token_map)
        .map_err(|e| format!("marshal pluginTokens for {}: {}", p.plugin_id, e))?;

    let mut json_data: HashMap<String, serde_json::Value> = HashMap::new();
    json_data.insert("computeUrl".into(), cfg.compute_url.clone().into());
    json_data.insert("pluginId".into(), p.plugin_id.clone().into());
    json_data.insert("orgId".into(), cfg.org_id.clone().into());
    json_data.insert("controlPlaneUrl".into(), cfg.control_plane_url.clone().into());
    json_data.insert("instanceTokenUrl".into(), cfg.instance_token_url.clone().into());
    json_data.insert("pluginsRoot".into(), dirs_plugin_state(cfg).into());
    json_data.insert(
        "pluginsInstallDir".into(),
        dirs.plugins_dir.to_string_lossy().into_owned().into(),
    );

    let mut secure: HashMap<String, String> = HashMap::new();
    secure.insert("platformToken".into(), p.platform_token.clone());
    secure.insert("pluginTokens".into(), tokens_json);

    let doc = DatasourceDoc {
        api_version: 1,
        datasources: vec![DatasourceEntry {
            name: p.plugin_id.clone(),
            uid: p.plugin_id.clone(),
            ds_type: p.grafana_slug.clone(),
            access: "proxy".into(),
            is_default: false,
            editable: true,
            json_data,
            secure_json_data: secure,
        }],
    };

    write_datasource_doc(p, &dirs.provisioning, doc)
}

// ---------------------------------------------------------------------------
// Minimal (community) datasource YAML
// ---------------------------------------------------------------------------

fn render_minimal_datasource(
    p: &ResolvedPlugin,
    dirs: &ReconcileDirs,
    _cfg: &ProvisioningConfig,
) -> Result<(), String> {
    let doc = DatasourceDoc {
        api_version: 1,
        datasources: vec![DatasourceEntry {
            name: p.plugin_id.clone(),
            uid: p.plugin_id.clone(),
            ds_type: p.grafana_slug.clone(),
            access: "proxy".into(),
            is_default: false,
            editable: true,
            json_data: HashMap::new(),
            secure_json_data: HashMap::new(),
        }],
    };
    write_datasource_doc(p, &dirs.provisioning, doc)
}

fn write_datasource_doc(
    p: &ResolvedPlugin,
    provisioning: &Path,
    doc: DatasourceDoc,
) -> Result<(), String> {
    let data = serde_yaml::to_string(&doc)
        .map_err(|e| format!("marshal datasource yaml for {}: {}", p.plugin_id, e))?;
    let ds_dir = provisioning.join("datasources");
    fs::create_dir_all(&ds_dir)
        .map_err(|e| format!("mkdir {:?}: {}", ds_dir, e))?;
    let path = ds_dir.join(format!("{}.yaml", p.plugin_id));
    fs::write(&path, data.as_bytes())
        .map_err(|e| format!("write {:?}: {}", path, e))?;
    Ok(())
}

// ---------------------------------------------------------------------------
// unsigned-plugins sidecar
// ---------------------------------------------------------------------------

/// write_unsigned_plugins_list writes a comma-joined, sorted list of Grafana
/// slugs to ProvisioningDir/unsigned-plugins. Mirrors Go's writeUnsignedPluginsList.
pub(crate) fn write_unsigned_plugins_list(
    plugins: &[ResolvedPlugin],
    provisioning: &Path,
) -> Result<(), String> {
    let mut seen: HashSet<&str> = HashSet::new();
    for p in plugins {
        if !p.grafana_slug.is_empty() {
            seen.insert(&p.grafana_slug);
        }
    }
    let mut slugs: Vec<&str> = seen.into_iter().collect();
    slugs.sort_unstable();
    let content = slugs.join(",");
    let path = provisioning.join("unsigned-plugins");
    fs::write(&path, content.as_bytes())
        .map_err(|e| format!("write unsigned-plugins: {}", e))?;
    Ok(())
}

// ---------------------------------------------------------------------------
// prune stale .yaml files
// ---------------------------------------------------------------------------

/// prune_provisioning removes <plugin_id>.yaml files in out_dir that are no
/// longer in the desired (provisionable) set. Mirrors Go's pruneProvisioning.
fn prune_provisioning(out_dir: &Path, desired: &[ResolvedPlugin]) -> Result<(), String> {
    let want: HashSet<String> = desired
        .iter()
        .filter(|p| !p.grafana_slug.is_empty() && p.plugin_type == "app")
        .map(|p| format!("{}.yaml", p.plugin_id))
        .collect();

    let entries = match fs::read_dir(out_dir) {
        Ok(e) => e,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(format!("read dir {:?}: {}", out_dir, e)),
    };

    for entry in entries.flatten() {
        let name = entry.file_name();
        let name_str = name.to_string_lossy();
        if entry.metadata().map(|m| m.is_dir()).unwrap_or(true) {
            continue;
        }
        if !name_str.ends_with(".yaml") || want.contains(name_str.as_ref()) {
            continue;
        }
        let path = out_dir.join(&name);
        fs::remove_file(&path)
            .map_err(|e| format!("remove stale provisioning {:?}: {}", path, e))?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Helper: plugin state dir path (from ProvisioningConfig)
// ---------------------------------------------------------------------------

/// dirs_plugin_state returns the plugin state dir string to stamp into
/// jsonData.pluginsRoot. We store it in ProvisioningConfig for simplicity.
fn dirs_plugin_state(cfg: &ProvisioningConfig) -> String {
    // The state dir is carried in ReconcileDirs in the reconciler; the
    // ProvisioningConfig has a matching field for rendering.
    // We expose it via ProvisioningConfig::plugin_state_dir.
    // (See mod.rs where we add plugin_state_dir to ProvisioningConfig.)
    cfg.plugin_state_dir.clone()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    fn test_cfg() -> ProvisioningConfig {
        ProvisioningConfig {
            org_id: "test-org-uuid".into(),
            control_plane_url: "https://cp.example.com".into(),
            otlp_endpoint: "http://otel:4317".into(),
            instance_token_url: "http://127.0.0.1:9090/instance-token".into(),
            risingwave_host: "127.0.0.1".into(),
            risingwave_port: "4566".into(),
            postgres_host: "127.0.0.1".into(),
            postgres_port: "5432".into(),
            control_db: "control_db".into(),
            compute_url: "http://127.0.0.1:8080".into(),
            plugin_state_dir: "/tmp/plugin-state".into(),
        }
    }

    fn test_dirs(provisioning: &Path) -> ReconcileDirs {
        ReconcileDirs {
            provisioning: provisioning.to_path_buf(),
            plugins_dir: provisioning.parent().unwrap().join("plugins"),
            cache_dir: provisioning.parent().unwrap().join("cache"),
            plugin_state_dir: "/tmp/plugin-state".into(),
        }
    }

    fn app_plugin(id: &str, slug: &str) -> ResolvedPlugin {
        ResolvedPlugin {
            plugin_id: id.into(),
            grafana_slug: slug.into(),
            plugin_type: "app".into(),
            platform_plugin: false,
            required: false,
            version: "v0.1.0".into(),
            platform_token: "tok-abc".into(),
            artifact: None,
        }
    }

    // --- App provisioning YAML shape -----------------------------------------

    #[test]
    fn render_app_yaml_contains_expected_keys() {
        let tmp = TempDir::new().unwrap();
        let provisioning = tmp.path().join("provisioning");
        std::fs::create_dir_all(provisioning.join("plugins")).unwrap();

        let p = app_plugin("core-app", "core-app");
        let dirs = test_dirs(&provisioning);
        let cfg = test_cfg();

        render_app(&p, &provisioning.join("plugins"), &cfg).unwrap();

        let yaml_path = provisioning.join("plugins").join("core-app.yaml");
        assert!(yaml_path.exists());

        let yaml_str = std::fs::read_to_string(&yaml_path).unwrap();

        // Must contain the Postgres + RisingWave coordinate keys.
        assert!(
            yaml_str.contains("risingwaveHost"),
            "missing risingwaveHost in:\n{yaml_str}"
        );
        assert!(
            yaml_str.contains("risingwavePort"),
            "missing risingwavePort in:\n{yaml_str}"
        );
        assert!(
            yaml_str.contains("postgresHost"),
            "missing postgresHost in:\n{yaml_str}"
        );
        assert!(
            yaml_str.contains("postgresPort"),
            "missing postgresPort in:\n{yaml_str}"
        );
        assert!(
            yaml_str.contains("controlDb"),
            "missing controlDb in:\n{yaml_str}"
        );
        assert!(
            yaml_str.contains("instanceTokenUrl"),
            "missing instanceTokenUrl in:\n{yaml_str}"
        );

        // Must NOT contain the removed gateway fields.
        assert!(
            !yaml_str.contains("gatewayUrl"),
            "unexpected gatewayUrl in:\n{yaml_str}"
        );
        assert!(
            !yaml_str.contains("readGatewayUrl"),
            "unexpected readGatewayUrl in:\n{yaml_str}"
        );
    }

    // --- Datasource provisioning YAML shape ----------------------------------

    #[test]
    fn render_datasource_yaml_has_compute_url_and_no_gateway() {
        let tmp = TempDir::new().unwrap();
        let provisioning = tmp.path().join("provisioning");
        std::fs::create_dir_all(&provisioning).unwrap();
        std::fs::create_dir_all(provisioning.join("datasources")).unwrap();

        let p = ResolvedPlugin {
            plugin_id: "core-datasource".into(),
            grafana_slug: "core-datasource".into(),
            plugin_type: "datasource".into(),
            platform_plugin: true,
            required: true,
            version: "v0.1.0".into(),
            platform_token: "tok-ds".into(),
            artifact: None,
        };
        let dirs = test_dirs(&provisioning);
        let cfg = test_cfg();
        let all_plugins = vec![p.clone()];

        render_datasource(&p, &all_plugins, &dirs, &cfg).unwrap();

        let yaml_path = provisioning.join("datasources").join("core-datasource.yaml");
        assert!(yaml_path.exists());
        let yaml_str = std::fs::read_to_string(&yaml_path).unwrap();

        assert!(
            yaml_str.contains("computeUrl"),
            "missing computeUrl:\n{yaml_str}"
        );
        assert!(
            !yaml_str.contains("gatewayUrl"),
            "unexpected gatewayUrl:\n{yaml_str}"
        );
        assert!(
            !yaml_str.contains("readGatewayUrl"),
            "unexpected readGatewayUrl:\n{yaml_str}"
        );
    }

    // --- unsigned-plugins sidecar --------------------------------------------

    #[test]
    fn write_unsigned_plugins_list_sorted_and_deduped() {
        let tmp = TempDir::new().unwrap();
        let provisioning = tmp.path().join("provisioning");
        std::fs::create_dir_all(&provisioning).unwrap();

        let plugins = vec![
            app_plugin("z-plugin", "z-slug"),
            app_plugin("a-plugin", "a-slug"),
            app_plugin("m-plugin", "a-slug"), // duplicate slug
        ];
        write_unsigned_plugins_list(&plugins, &provisioning).unwrap();

        let content =
            std::fs::read_to_string(provisioning.join("unsigned-plugins")).unwrap();
        // Slugs sorted, deduplicated.
        assert_eq!(content, "a-slug,z-slug");
    }

    // --- prune_provisioning --------------------------------------------------

    #[test]
    fn prune_removes_stale_yaml() {
        let tmp = TempDir::new().unwrap();
        let out_dir = tmp.path().join("plugins");
        std::fs::create_dir_all(&out_dir).unwrap();

        // Write two YAML files.
        std::fs::write(out_dir.join("old-plugin.yaml"), "old").unwrap();
        std::fs::write(out_dir.join("core-app.yaml"), "current").unwrap();

        // Only core-app is in the desired set.
        let desired = vec![app_plugin("core-app", "core-app")];
        prune_provisioning(&out_dir, &desired).unwrap();

        assert!(!out_dir.join("old-plugin.yaml").exists());
        assert!(out_dir.join("core-app.yaml").exists());
    }
}
