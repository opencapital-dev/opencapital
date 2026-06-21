// reconcile/library_panels.rs — Grafana library-panel upsert via the HTTP API.
// Rust port of library_panels.go.
//
// The post-start library-panels phase is non-fatal (Grafana is already up and
// usable; missing panels is a degraded but recoverable state). The caller
// (grafana.rs A6b) will handle the non-fatal semantics; this module returns
// a Result so errors are explicit.
//
// NOTE: metric_deps (injectMetricVarDeps / metricRefVarDeps) is INTENTIONALLY
// NOT PORTED. That surface reads query_entities / .py metric files for the
// read-gateway DSL, which is DEAD in the sql() world. See A6b for deletion.

use std::fs;
use std::path::Path;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::{plugin_display_name, ReconcileDirs, ResolvedPlugin};

// ---------------------------------------------------------------------------
// Auth modes (mirrors Go's grafanaAuth)
// ---------------------------------------------------------------------------

/// GrafanaAuth holds the credentials for one auth mode.
/// WebAuthUser takes priority over Basic when both are set.
#[derive(Debug, Clone, Default)]
pub struct GrafanaAuth {
    pub web_auth_user: Option<String>,
    pub basic_user: Option<String>,
    pub basic_pass: Option<String>,
}

// ---------------------------------------------------------------------------
// Grafana HTTP client (blocking via reqwest::blocking)
// ---------------------------------------------------------------------------

/// GrafanaClient sends authenticated requests to a local Grafana instance.
pub struct GrafanaClient {
    base_url: String,
    auth: GrafanaAuth,
    client: reqwest::blocking::Client,
}

impl GrafanaClient {
    pub fn new(base_url: &str, auth: GrafanaAuth) -> Self {
        GrafanaClient {
            base_url: base_url.trim_end_matches('/').to_string(),
            auth,
            client: reqwest::blocking::Client::builder()
                .timeout(Duration::from_secs(15))
                .build()
                .expect("build blocking reqwest client"),
        }
    }

    fn apply_auth(&self, rb: reqwest::blocking::RequestBuilder) -> reqwest::blocking::RequestBuilder {
        if let Some(user) = &self.auth.web_auth_user {
            return rb.header("X-WEBAUTH-USER", user);
        }
        if let Some(user) = &self.auth.basic_user {
            return rb.basic_auth(user, self.auth.basic_pass.as_deref());
        }
        rb
    }

    // --- /api/health ---------------------------------------------------------

    /// wait_healthy polls /api/health until 200 or timeout.
    /// Mirrors Go's waitHealthy.
    pub fn wait_healthy(&self, timeout: Duration) -> Result<(), String> {
        let url = format!("{}/api/health", self.base_url);
        let deadline = std::time::Instant::now() + timeout;
        loop {
            let rb = self.client.get(&url);
            let rb = self.apply_auth(rb);
            if let Ok(r) = rb.send() {
                if r.status().is_success() {
                    return Ok(());
                }
            }
            if std::time::Instant::now() >= deadline {
                return Err(format!("grafana not healthy within {:?}", timeout));
            }
            std::thread::sleep(Duration::from_secs(2));
        }
    }

    // --- /api/folders --------------------------------------------------------

    fn list_folders(&self) -> Result<Vec<FolderHit>, String> {
        let url = format!("{}/api/folders?limit=1000", self.base_url);
        let resp = self
            .apply_auth(self.client.get(&url))
            .send()
            .map_err(|e| format!("GET folders: {}", e))?;
        if !resp.status().is_success() {
            return Err(format!("GET folders: {}", resp.status()));
        }
        resp.json::<Vec<FolderHit>>()
            .map_err(|e| format!("decode folders: {}", e))
    }

    fn create_folder(&self, title: &str) -> Result<FolderHit, String> {
        let body = serde_json::json!({ "title": title });
        let resp = self
            .apply_auth(self.client.post(format!("{}/api/folders", self.base_url)))
            .json(&body)
            .send()
            .map_err(|e| format!("POST folder {:?}: {}", title, e))?;
        if !resp.status().is_success() {
            return Err(format!("POST folder {:?}: {}", title, resp.status()));
        }
        resp.json::<FolderHit>()
            .map_err(|e| format!("decode created folder {:?}: {}", title, e))
    }

    /// ensure_folder resolves the folder titled `title`, creating if absent.
    /// Mirrors Go's ensureFolder.
    pub fn ensure_folder(&self, title: &str) -> Result<FolderHit, String> {
        let folders = self.list_folders()?;
        if let Some(f) = folders.into_iter().find(|f| f.title == title) {
            return Ok(f);
        }
        self.create_folder(title)
    }

    // --- /api/library-elements -----------------------------------------------

    /// get_library_element returns the element for uid, or None when absent (404).
    /// Mirrors Go's getLibraryElement.
    pub fn get_library_element(&self, uid: &str) -> Result<Option<LibraryElementResult>, String> {
        let url = format!("{}/api/library-elements/{}", self.base_url, uid);
        let resp = self
            .apply_auth(self.client.get(&url))
            .send()
            .map_err(|e| format!("GET library-element {}: {}", uid, e))?;
        if resp.status().as_u16() == 404 {
            return Ok(None);
        }
        if !resp.status().is_success() {
            return Err(format!("GET library-element {}: {}", uid, resp.status()));
        }
        let r: LibraryElementResult = resp
            .json()
            .map_err(|e| format!("decode library-element {}: {}", uid, e))?;
        Ok(Some(r))
    }

    /// create_library_element POSTs a new library element (kind 1 = panel).
    /// Mirrors Go's createLibraryElement.
    pub fn create_library_element(
        &self,
        uid: &str,
        name: &str,
        folder_uid: &str,
        model: &Value,
    ) -> Result<(), String> {
        let body = serde_json::json!({
            "uid": uid,
            "name": name,
            "kind": 1,
            "model": model,
            "folderUid": folder_uid,
        });
        let resp = self
            .apply_auth(
                self.client
                    .post(format!("{}/api/library-elements", self.base_url)),
            )
            .json(&body)
            .send()
            .map_err(|e| format!("POST library-element {}: {}", uid, e))?;
        if !resp.status().is_success() {
            return Err(format!("POST library-element {}: {}", uid, resp.status()));
        }
        Ok(())
    }

    /// patch_library_element PATCHes an existing library element.
    /// Sends folderId (deprecated numeric) to move panels between folders.
    /// Mirrors Go's patchLibraryElement.
    pub fn patch_library_element(
        &self,
        uid: &str,
        name: &str,
        model: &Value,
        version: i64,
        folder_id: i64,
    ) -> Result<(), String> {
        let body = serde_json::json!({
            "name": name,
            "kind": 1,
            "model": model,
            "version": version,
            "folderId": folder_id,
        });
        let resp = self
            .apply_auth(
                self.client
                    .patch(format!("{}/api/library-elements/{}", self.base_url, uid)),
            )
            .json(&body)
            .send()
            .map_err(|e| format!("PATCH library-element {}: {}", uid, e))?;
        if !resp.status().is_success() {
            return Err(format!("PATCH library-element {}: {}", uid, resp.status()));
        }
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
pub struct FolderHit {
    pub id: i64,
    pub uid: String,
    pub title: String,
}

#[derive(Debug, Deserialize)]
pub struct LibraryElementResult {
    pub result: LibraryElementInner,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LibraryElementInner {
    pub uid: String,
    pub name: String,
    pub kind: i64,
    pub model: Value,
    pub version: i64,
    pub folder_uid: String,
}

// ---------------------------------------------------------------------------
// library_panel_uid
// ---------------------------------------------------------------------------

/// library_panel_uid derives and validates the Grafana UID for a plugin's panel.
/// uid = "<plugin_id>-<stem>", max 40 chars, charset a-zA-Z0-9-_.
/// Mirrors Go's libraryPanelUID.
pub fn library_panel_uid(plugin_id: &str, stem: &str) -> Result<String, String> {
    let uid = format!("{}-{}", plugin_id, stem);
    if uid.len() > 40 {
        return Err(format!(
            "derived UID {:?} is {} chars, max 40 — shorten plugin_id or panel stem",
            uid,
            uid.len()
        ));
    }
    if !uid.chars().all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_') {
        return Err(format!(
            "derived UID {:?} contains invalid characters (allowed: a-zA-Z0-9-_)",
            uid
        ));
    }
    Ok(uid)
}

// ---------------------------------------------------------------------------
// models_equal
// ---------------------------------------------------------------------------

/// models_equal compares two JSON values semantically by re-serialising.
/// Mirrors Go's modelsEqual.
pub fn models_equal(a: &Value, b: &Value) -> bool {
    // Normalise through canonical JSON (marshal/unmarshal in the same order).
    match (serde_json::to_string(a), serde_json::to_string(b)) {
        (Ok(sa), Ok(sb)) => sa == sb,
        _ => false,
    }
}

// ---------------------------------------------------------------------------
// apply_grafana_model_defaults
// ---------------------------------------------------------------------------

/// apply_grafana_model_defaults injects "type" and "description" into the
/// model object when absent, mirroring Grafana's syncFieldsWithModel.
/// Used for idempotent comparison: GET returns mutated model, so we must
/// compare the file model after applying the same defaults.
/// Mirrors Go's applyGrafanaModelDefaults.
pub fn apply_grafana_model_defaults(model: Value) -> Value {
    let mut m = match model {
        Value::Object(m) => m,
        other => return other,
    };
    m.entry("type").or_insert(Value::String(String::new()));
    m.entry("description")
        .or_insert(Value::String(String::new()));
    Value::Object(m)
}

// ---------------------------------------------------------------------------
// provision_library_panels — public entry
// ---------------------------------------------------------------------------

/// provision_library_panels upserts every installed plugin's library panels into
/// the local Grafana instance. Mirrors Go's ProvisionLibraryPanels.
///
/// This is a blocking call; call via `spawn_blocking` in async contexts.
pub fn provision_library_panels(
    plugins: &[ResolvedPlugin],
    dirs: &ReconcileDirs,
    grafana_url: &str,
    auth: GrafanaAuth,
    health_timeout: Duration,
) -> Result<(), String> {
    let gc = GrafanaClient::new(grafana_url, auth);
    gc.wait_healthy(health_timeout)?;

    for p in plugins {
        if p.grafana_slug.is_empty() {
            continue;
        }
        let lp_dir = dirs.plugins_dir.join(&p.grafana_slug).join("library-panels");
        let entries = match fs::read_dir(&lp_dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
            Err(e) => {
                return Err(format!(
                    "read library-panels dir for {}: {}",
                    p.plugin_id, e
                ))
            }
        };

        let mut panels: Vec<String> = entries
            .flatten()
            .filter(|e| {
                !e.metadata().map(|m| m.is_dir()).unwrap_or(true)
                    && e.file_name().to_string_lossy().ends_with(".json")
            })
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();

        if panels.is_empty() {
            continue;
        }
        panels.sort_unstable();

        // Ensure the per-plugin folder exists.
        let folder_title =
            plugin_display_name(&dirs.plugins_dir, &p.grafana_slug, &p.plugin_id);
        let panel_folder = gc
            .ensure_folder(&folder_title)
            .map_err(|e| format!("ensure folder for {}: {}", p.plugin_id, e))?;

        for fname in &panels {
            let stem = fname.trim_end_matches(".json");
            let uid = library_panel_uid(&p.plugin_id, stem)
                .map_err(|e| format!("plugin {} panel {}: {}", p.plugin_id, stem, e))?;

            let model_bytes = fs::read(lp_dir.join(fname))
                .map_err(|e| format!("read panel {}/{}: {}", p.plugin_id, fname, e))?;
            let model: Value = serde_json::from_slice(&model_bytes)
                .map_err(|e| format!("parse panel {}/{}: {}", p.plugin_id, fname, e))?;

            // Derive display name from model.title or fallback to stem.
            let name = model
                .get("title")
                .and_then(|t| t.as_str())
                .filter(|s| !s.is_empty())
                .unwrap_or(stem)
                .to_string();

            // NOTE: injectMetricVarDeps is intentionally not called here.
            // That read-gateway DSL surface is DEAD in the sql() world.

            let existing = gc
                .get_library_element(&uid)
                .map_err(|e| format!("get library-element {}: {}", uid, e))?;

            match existing {
                None => {
                    gc.create_library_element(&uid, &name, &panel_folder.uid, &model)
                        .map_err(|e| format!("create library-element {}: {}", uid, e))?;
                    eprintln!(
                        "[reconcile] library-panel created: plugin={} uid={}",
                        p.plugin_id, uid
                    );
                }
                Some(ex) => {
                    let desired_model = apply_grafana_model_defaults(model.clone());
                    if models_equal(&ex.result.model, &desired_model)
                        && ex.result.name == name
                        && ex.result.folder_uid == panel_folder.uid
                    {
                        // Already up-to-date — no-op.
                        continue;
                    }
                    gc.patch_library_element(
                        &uid,
                        &name,
                        &model,
                        ex.result.version,
                        panel_folder.id,
                    )
                    .map_err(|e| format!("patch library-element {}: {}", uid, e))?;
                    eprintln!(
                        "[reconcile] library-panel patched: plugin={} uid={}",
                        p.plugin_id, uid
                    );
                }
            }
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // --- library_panel_uid ---------------------------------------------------

    #[test]
    fn uid_valid() {
        let uid = library_panel_uid("core-app", "portfolio-overview").unwrap();
        assert_eq!(uid, "core-app-portfolio-overview");
    }

    #[test]
    fn uid_too_long_is_error() {
        // Make a uid that exceeds 40 chars.
        let plugin_id = "a-very-long-plugin-identifier";
        let stem = "an-equally-long-panel-stem-here";
        let result = library_panel_uid(plugin_id, stem);
        assert!(result.is_err(), "expected error for long UID");
    }

    #[test]
    fn uid_invalid_charset_is_error() {
        // Space in stem should be rejected.
        let result = library_panel_uid("plugin", "bad stem!");
        assert!(result.is_err(), "expected error for invalid charset");
    }

    // --- models_equal --------------------------------------------------------

    #[test]
    fn models_equal_same_value() {
        let a = serde_json::json!({"type": "graph", "title": "CPU"});
        let b = serde_json::json!({"type": "graph", "title": "CPU"});
        assert!(models_equal(&a, &b));
    }

    #[test]
    fn models_equal_different_values() {
        let a = serde_json::json!({"title": "CPU"});
        let b = serde_json::json!({"title": "MEM"});
        assert!(!models_equal(&a, &b));
    }

    // --- apply_grafana_model_defaults ----------------------------------------

    #[test]
    fn model_defaults_injects_type_and_description() {
        let model = serde_json::json!({"title": "My Panel"});
        let out = apply_grafana_model_defaults(model);
        assert_eq!(out.get("type").and_then(|v| v.as_str()), Some(""));
        assert_eq!(
            out.get("description").and_then(|v| v.as_str()),
            Some("")
        );
        // Existing fields preserved.
        assert_eq!(
            out.get("title").and_then(|v| v.as_str()),
            Some("My Panel")
        );
    }

    #[test]
    fn model_defaults_does_not_overwrite_existing_type() {
        let model = serde_json::json!({"type": "timeseries"});
        let out = apply_grafana_model_defaults(model);
        assert_eq!(
            out.get("type").and_then(|v| v.as_str()),
            Some("timeseries")
        );
    }

    #[test]
    fn model_defaults_non_object_is_passthrough() {
        let model = serde_json::json!("not an object");
        let out = apply_grafana_model_defaults(model.clone());
        assert_eq!(out, model);
    }

    // --- provision_library_panels (network tests are gated #[ignore]) --------

    #[test]
    #[ignore] // requires a live Grafana instance
    fn provision_library_panels_network_smoke() {
        // This test exists as a template for manual/integration runs.
        // Run with: cargo test -- --ignored reconcile::library_panels::tests::provision_library_panels_network_smoke
    }
}
