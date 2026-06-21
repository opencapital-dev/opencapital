// reconcile/dashboards.rs — copy plugin-bundled dashboard JSON + write the
// Grafana dashboard provider YAML.
// Rust port of provision_dashboards.go.

use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::{plugin_display_name, ReconcileDirs, ResolvedPlugin};

// ---------------------------------------------------------------------------
// YAML document shapes
// ---------------------------------------------------------------------------

/// Grafana dashboard provisioning provider schema.
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DashboardProviderDoc {
    api_version: u32,
    providers: Vec<DashboardProvider>,
}

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DashboardProvider {
    name: String,
    org_id: u32,
    #[serde(rename = "type")]
    provider_type: String,
    disable_deletion: bool,
    allow_ui_updates: bool,
    update_interval_seconds: u32,
    options: DashboardProviderOptions,
}

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DashboardProviderOptions {
    path: String,
    folders_from_files_structure: bool,
}

// ---------------------------------------------------------------------------
// provision_dashboards — public entry
// ---------------------------------------------------------------------------

/// provision_dashboards copies each app plugin's bundled dashboard JSON into
/// <ProvisioningDir>/dashboards/plugins/<display_name>/ and writes a single
/// provider YAML at <ProvisioningDir>/dashboards/plugin-dashboards.yaml.
/// Mirrors Go's provisionDashboards.
pub fn provision_dashboards(
    plugins: &[ResolvedPlugin],
    dirs: &ReconcileDirs,
) -> Result<(), String> {
    let dash_root = dirs.provisioning.join("dashboards");
    let content_root = dash_root.join("plugins");
    fs::create_dir_all(&content_root)
        .map_err(|e| format!("mkdir {:?}: {}", content_root, e))?;

    // Collect app plugins that have a dashboards/ dir in their installed bundle.
    struct PluginDash {
        plugin_id: String,
        title: String,
        src_dir: PathBuf,
    }
    let mut to_provision: Vec<PluginDash> = Vec::new();
    let mut desired_titles: std::collections::HashSet<String> = std::collections::HashSet::new();

    for p in plugins {
        if p.plugin_type != "app" || p.grafana_slug.is_empty() {
            continue;
        }
        let src_dir = dirs.plugins_dir.join(&p.grafana_slug).join("dashboards");
        if !src_dir.exists() {
            continue; // plugin ships no dashboards/ — not an error
        }
        let title = plugin_display_name(&dirs.plugins_dir, &p.grafana_slug, &p.plugin_id);
        desired_titles.insert(title.clone());
        to_provision.push(PluginDash {
            plugin_id: p.plugin_id.clone(),
            title,
            src_dir,
        });
    }

    // Prune dirs for plugins no longer present.
    prune_dashboard_dirs(&content_root, &desired_titles)?;

    // Copy dashboard JSON for each plugin (clear dest first for exact reconcile).
    for pd in &to_provision {
        let dest_dir = content_root.join(&pd.title);
        if dest_dir.exists() {
            fs::remove_dir_all(&dest_dir)
                .map_err(|e| format!("clear dashboard dir for {}: {}", pd.plugin_id, e))?;
        }
        fs::create_dir_all(&dest_dir)
            .map_err(|e| format!("mkdir {:?}: {}", dest_dir, e))?;
        copy_dashboard_json(&pd.src_dir, &dest_dir)
            .map_err(|e| format!("copy dashboards for {}: {}", pd.plugin_id, e))?;
    }

    // Write the provider YAML.
    write_dashboard_provider_yaml(&dash_root, &content_root)
}

// ---------------------------------------------------------------------------
// prune_dashboard_dirs
// ---------------------------------------------------------------------------

fn prune_dashboard_dirs(
    content_root: &Path,
    desired: &std::collections::HashSet<String>,
) -> Result<(), String> {
    let entries = match fs::read_dir(content_root) {
        Ok(e) => e,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(format!("read dir {:?}: {}", content_root, e)),
    };
    for entry in entries.flatten() {
        if !entry.metadata().map(|m| m.is_dir()).unwrap_or(false) {
            continue;
        }
        let name = entry.file_name();
        if desired.contains(name.to_string_lossy().as_ref()) {
            continue;
        }
        let path = content_root.join(&name);
        fs::remove_dir_all(&path)
            .map_err(|e| format!("remove stale dashboard dir {:?}: {}", path, e))?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// copy_dashboard_json
// ---------------------------------------------------------------------------

/// copy_dashboard_json recursively copies every *.json file from src_dir into
/// dest_dir, preserving subdirectory structure.
/// Mirrors Go's copyDashboardJSON.
fn copy_dashboard_json(src_dir: &Path, dest_dir: &Path) -> Result<(), io::Error> {
    for entry in fs::read_dir(src_dir)? {
        let entry = entry?;
        let path = entry.path();
        if path.is_dir() {
            let sub_dest = dest_dir.join(entry.file_name());
            fs::create_dir_all(&sub_dest)?;
            copy_dashboard_json(&path, &sub_dest)?;
        } else if path.extension().and_then(|e| e.to_str()) == Some("json") {
            let dest = dest_dir.join(entry.file_name());
            fs::copy(&path, &dest)?;
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Provider YAML
// ---------------------------------------------------------------------------

fn write_dashboard_provider_yaml(
    dash_root: &Path,
    content_root: &Path,
) -> Result<(), String> {
    let doc = DashboardProviderDoc {
        api_version: 1,
        providers: vec![DashboardProvider {
            name: "plugin-dashboards".into(),
            org_id: 1,
            provider_type: "file".into(),
            disable_deletion: true,
            allow_ui_updates: false,
            update_interval_seconds: 30,
            options: DashboardProviderOptions {
                path: content_root.to_string_lossy().into_owned(),
                folders_from_files_structure: true,
            },
        }],
    };
    let data = serde_yaml::to_string(&doc)
        .map_err(|e| format!("marshal dashboard provider yaml: {}", e))?;
    let path = dash_root.join("plugin-dashboards.yaml");
    fs::write(&path, data.as_bytes())
        .map_err(|e| format!("write dashboard provider yaml {:?}: {}", path, e))?;
    Ok(())
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    fn make_dirs(base: &Path) -> ReconcileDirs {
        let provisioning = base.join("provisioning");
        let plugins_dir = base.join("plugins");
        let cache_dir = base.join("cache");
        let plugin_state_dir = base.join("plugin-state");
        for d in [&provisioning, &plugins_dir, &cache_dir, &plugin_state_dir] {
            fs::create_dir_all(d).unwrap();
        }
        ReconcileDirs {
            provisioning,
            plugins_dir,
            cache_dir,
            plugin_state_dir,
        }
    }

    #[test]
    fn provision_dashboards_copies_json_and_writes_provider() {
        let tmp = TempDir::new().unwrap();
        let dirs = make_dirs(tmp.path());

        // Create a fake installed plugin with a dashboards/ dir.
        let plugin_slug = "core-app";
        let plugin_dir = dirs.plugins_dir.join(plugin_slug);
        let dash_src = plugin_dir.join("dashboards");
        fs::create_dir_all(&dash_src).unwrap();
        fs::write(dash_src.join("overview.json"), r#"{"title":"Overview"}"#).unwrap();
        // Write plugin.json so plugin_display_name can read it.
        fs::write(
            plugin_dir.join("plugin.json"),
            r#"{"name":"Core App","id":"core-app"}"#,
        )
        .unwrap();

        let plugin = crate::reconcile::ResolvedPlugin {
            plugin_id: "core-app".into(),
            grafana_slug: plugin_slug.into(),
            plugin_type: "app".into(),
            platform_plugin: false,
            required: false,
            version: "v0.1.0".into(),
            artifact: None,
        };

        provision_dashboards(&[plugin], &dirs).unwrap();

        // Provider YAML should be written.
        let provider_yaml = dirs.provisioning.join("dashboards").join("plugin-dashboards.yaml");
        assert!(provider_yaml.exists(), "provider YAML missing");
        let content = fs::read_to_string(&provider_yaml).unwrap();
        assert!(content.contains("plugin-dashboards"), "wrong name");
        assert!(content.contains("foldersFromFilesStructure"), "missing foldersFromFilesStructure");

        // Dashboard JSON should be copied under dashboards/plugins/<display_name>/.
        let content_root = dirs.provisioning.join("dashboards").join("plugins");
        // Display name is "Core App" from plugin.json.
        let expected_json = content_root.join("Core App").join("overview.json");
        assert!(expected_json.exists(), "dashboard JSON not copied to {:?}", expected_json);
    }

    #[test]
    fn provision_dashboards_skips_panel_plugins() {
        let tmp = TempDir::new().unwrap();
        let dirs = make_dirs(tmp.path());

        let panel = crate::reconcile::ResolvedPlugin {
            plugin_id: "my-panel".into(),
            grafana_slug: "my-panel".into(),
            plugin_type: "panel".into(),
            platform_plugin: false,
            required: false,
            version: "v0.1.0".into(),
            artifact: None,
        };

        // Should not error even though there's no dashboards/ dir.
        provision_dashboards(&[panel], &dirs).unwrap();
        let content_root = dirs.provisioning.join("dashboards").join("plugins");
        // No plugin dirs should have been created under plugins/.
        if content_root.exists() {
            let entries: Vec<_> = fs::read_dir(&content_root)
                .unwrap()
                .flatten()
                .collect();
            assert!(entries.is_empty(), "unexpected entries under plugins/");
        }
    }

    #[test]
    fn prune_dashboard_dirs_removes_stale() {
        let tmp = TempDir::new().unwrap();
        let content_root = tmp.path().join("plugins");
        fs::create_dir_all(&content_root).unwrap();

        // Create stale dir and current dir.
        fs::create_dir_all(content_root.join("Stale Plugin")).unwrap();
        fs::create_dir_all(content_root.join("Current Plugin")).unwrap();

        let desired: std::collections::HashSet<String> =
            ["Current Plugin".to_string()].into_iter().collect();
        prune_dashboard_dirs(&content_root, &desired).unwrap();

        assert!(!content_root.join("Stale Plugin").exists());
        assert!(content_root.join("Current Plugin").exists());
    }
}
