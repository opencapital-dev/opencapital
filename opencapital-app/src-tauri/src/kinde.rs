// Kinde PKCE login + the v8 Option A instance-token exchange.
//
// Flow: the shell holds a Kinde access token (from the browser PKCE login
// below). It exchanges that at the control plane for a short-lived
// instance token (POST /v1/instance/token), which it later hands to
// plugins. The shell never gives the Kinde token to Grafana or plugins.

use std::sync::{Mutex, OnceLock};
use std::time::Instant;

use base64::Engine;
use rand::RngCore;
use sha2::{Digest, Sha256};
use tauri::{Emitter, State};

// ---------------------------------------------------------------------------
// Shared HTTP client — one reqwest::Client per process (connection pool reuse).
// Eliminates per-command-call reqwest::Client::new() allocations.
// ---------------------------------------------------------------------------

fn shared_http_client() -> &'static reqwest::Client {
    static CLIENT: OnceLock<reqwest::Client> = OnceLock::new();
    CLIENT.get_or_init(reqwest::Client::new)
}

// ---------------------------------------------------------------------------
// Manifest refs cache — avoids re-fetching all per-plugin manifests on every
// catalog/version-dropdown call.  Keyed by (list_url, user_sources snapshot)
// with a 60-second TTL that matches the PluginClient manifest TTL.
// On stale the previous refs are served immediately while the background refresh
// is triggered on the NEXT call (serve-stale, same pattern as PluginClient).
// ---------------------------------------------------------------------------

#[derive(Clone)]
struct CachedRefs {
    refs: Vec<crate::catalog::PluginRef>,
    /// Serialised representation of the inputs used to build this cache entry,
    /// used for invalidation when list_url or user_sources changes.
    cache_key: String,
    fetched_at: Instant,
}

fn refs_cache() -> &'static Mutex<Option<CachedRefs>> {
    static CACHE: OnceLock<Mutex<Option<CachedRefs>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(None))
}

/// build_refs_cached builds (or serves from cache) the full PluginRef list.
/// Shared by catalog_req and plugin_versions to avoid redundant manifest fetches.
async fn build_refs_cached(
    list_url: &str,
    user_sources: &[crate::catalog::SourceRecord],
) -> Vec<crate::catalog::PluginRef> {
    const REFS_TTL: std::time::Duration = std::time::Duration::from_secs(60);

    // Build a cheap cache key from the inputs.
    let cache_key = format!(
        "{}|{}",
        list_url,
        user_sources
            .iter()
            .map(|s| format!("{}:{}", s.manifest_url, s.enabled))
            .collect::<Vec<_>>()
            .join(",")
    );

    // Fast path: serve from cache if key matches and TTL hasn't expired.
    {
        let guard = refs_cache().lock().unwrap();
        if let Some(cached) = &*guard {
            if cached.cache_key == cache_key && cached.fetched_at.elapsed() < REFS_TTL {
                return cached.refs.clone();
            }
        }
    }

    // Slow path: re-fetch all manifests.
    let client = shared_http_client();
    let refs = crate::catalog::sources::build_plugin_refs(client, list_url, user_sources).await;

    // Store in cache.
    {
        let mut guard = refs_cache().lock().unwrap();
        *guard = Some(CachedRefs {
            refs: refs.clone(),
            cache_key,
            fetched_at: Instant::now(),
        });
    }

    refs
}

use crate::config::AppConfig;

/// Session holds the live Kinde access token in memory. No refresh token
/// is stored (offline scope is off for now); the user re-logs in when the
/// access token expires. `email` comes from Kinde's userinfo endpoint at login
/// (falling back to the id_token email claim) so the UI shows a human identity.
#[derive(Default)]
pub struct Session {
    pub access_token: Mutex<Option<String>>,
    pub email: Mutex<Option<String>>,
}

fn b64url(bytes: &[u8]) -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
}

fn random_b64url(n: usize) -> String {
    let mut buf = vec![0u8; n];
    rand::thread_rng().fill_bytes(&mut buf);
    b64url(&buf)
}

/// PKCE S256: challenge = base64url(sha256(verifier)).
fn code_challenge(verifier: &str) -> String {
    let digest = Sha256::digest(verifier.as_bytes());
    b64url(&digest)
}

/// kinde_login runs the full browser PKCE flow and stores the access token.
#[tauri::command]
pub async fn kinde_login(cfg: State<'_, AppConfig>, session: State<'_, Session>) -> Result<(), String> {
    let cfg = cfg.inner().clone();
    let verifier = random_b64url(48);
    let challenge = code_challenge(&verifier);
    let state = random_b64url(16);

    let mut auth = url::Url::parse(&format!("{}/oauth2/auth", cfg.kinde_domain))
        .map_err(|e| format!("bad kinde_domain: {e}"))?;
    {
        let mut q = auth.query_pairs_mut();
        q.append_pair("response_type", "code");
        q.append_pair("client_id", &cfg.kinde_client_id);
        q.append_pair("redirect_uri", &cfg.kinde_redirect_uri);
        q.append_pair("audience", &cfg.kinde_audience);
        q.append_pair("state", &state);
        q.append_pair("code_challenge", &challenge);
        q.append_pair("code_challenge_method", "S256");
        if !cfg.kinde_scope.is_empty() {
            q.append_pair("scope", &cfg.kinde_scope);
        }
    }

    let port = cfg.redirect_port();
    let expected_state = state.clone();
    // tiny_http is blocking; run the one-shot callback listener off the
    // async runtime and await the result.
    let listener = tokio::task::spawn_blocking(move || wait_for_code(port, &expected_state));

    open::that(auth.as_str()).map_err(|e| format!("open browser: {e}"))?;

    let code = listener
        .await
        .map_err(|e| format!("callback task: {e}"))??;

    let tokens = exchange_code(&cfg, &code, &verifier).await?;
    // Prefer Kinde's userinfo preferred_email (reliable even when the id_token
    // omits the email claim); fall back to the id_token email claim.
    let email = fetch_kinde_email(&cfg, &tokens.access_token)
        .await
        .or_else(|| tokens.id_token.as_deref().and_then(email_from_id_token));
    *session.email.lock().unwrap() = email;
    *session.access_token.lock().unwrap() = Some(tokens.access_token);
    Ok(())
}

/// fetch_kinde_email calls Kinde's userinfo endpoint with the access token and
/// returns `preferred_email`. More reliable than the id_token's `email` claim,
/// which Kinde omits unless it's explicitly added to the token. Returns None on
/// any failure so the caller can fall back to the id_token.
async fn fetch_kinde_email(cfg: &AppConfig, access_token: &str) -> Option<String> {
    let url = format!("{}/oauth2/user_profile", cfg.kinde_domain);
    let v = shared_http_client()
        .get(&url)
        .bearer_auth(access_token)
        .send()
        .await
        .ok()?
        .error_for_status()
        .ok()?
        .json::<serde_json::Value>()
        .await
        .ok()?;
    v.get("preferred_email")
        .or_else(|| v.get("email"))
        .and_then(|x| x.as_str())
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

/// email_from_id_token reads the `email` claim out of a JWT id_token without
/// verifying the signature — the token came straight from Kinde over TLS in
/// the PKCE exchange, so it's trusted; we only need the claim for display.
fn email_from_id_token(id_token: &str) -> Option<String> {
    let payload = id_token.split('.').nth(1)?;
    let bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(payload)
        .ok()?;
    let claims: serde_json::Value = serde_json::from_slice(&bytes).ok()?;
    claims
        .get("email")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
}

/// wait_for_code blocks on a one-request loopback server, returning the
/// authorization code once Kinde redirects back. Rejects on state mismatch.
fn wait_for_code(port: u16, expected_state: &str) -> Result<String, String> {
    let server = tiny_http::Server::http(("127.0.0.1", port))
        .map_err(|e| format!("bind callback :{port}: {e}"))?;
    for request in server.incoming_requests() {
        let url = format!("http://localhost{}", request.url());
        let parsed = url::Url::parse(&url).map_err(|e| format!("parse callback: {e}"))?;
        let mut code = None;
        let mut got_state = None;
        for (k, v) in parsed.query_pairs() {
            match k.as_ref() {
                "code" => code = Some(v.into_owned()),
                "state" => got_state = Some(v.into_owned()),
                _ => {}
            }
        }
        let body = "<html><body>Login complete. You can close this tab.</body></html>";
        let header = tiny_http::Header::from_bytes(&b"Content-Type"[..], &b"text/html"[..]).unwrap();
        let _ = request.respond(tiny_http::Response::from_string(body).with_header(header));

        if got_state.as_deref() != Some(expected_state) {
            return Err("oauth state mismatch".into());
        }
        return code.ok_or_else(|| "no code in callback".into());
    }
    Err("callback server closed without a request".into())
}

#[derive(serde::Deserialize)]
struct TokenResponse {
    access_token: String,
    /// Present when the `openid` scope is requested; carries identity claims.
    id_token: Option<String>,
}

/// exchange_code swaps the authorization code for tokens (PKCE, public client
/// — no secret). Returns the access token plus the id_token (for email).
async fn exchange_code(
    cfg: &AppConfig,
    code: &str,
    verifier: &str,
) -> Result<TokenResponse, String> {
    let resp = shared_http_client()
        .post(format!("{}/oauth2/token", cfg.kinde_domain))
        .form(&[
            ("grant_type", "authorization_code"),
            ("client_id", cfg.kinde_client_id.as_str()),
            ("code", code),
            ("redirect_uri", cfg.kinde_redirect_uri.as_str()),
            ("code_verifier", verifier),
        ])
        .send()
        .await
        .map_err(|e| format!("token exchange: {e}"))?;
    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("token exchange {status}: {body}"));
    }
    resp.json().await.map_err(|e| format!("decode token: {e}"))
}

/// me_profile returns the human identity decoded from the id_token at login.
/// `preferred_email` is empty when the id_token carried no email claim (e.g.
/// the openid/email scopes weren't granted).
#[tauri::command]
pub fn me_profile(session: State<'_, Session>) -> serde_json::Value {
    let email = session.email.lock().unwrap().clone().unwrap_or_default();
    serde_json::json!({ "preferred_email": email })
}

/// logout clears the in-memory Kinde session. No refresh token is stored, so
/// this is a full sign-out; the next action needs a fresh browser login.
#[tauri::command]
pub fn logout(session: State<'_, Session>) {
    *session.access_token.lock().unwrap() = None;
    *session.email.lock().unwrap() = None;
}

/// instance_token returns a static local instance token for the loopback proxy
/// and plugin instanceTokenUrl. The control-plane sidecar has been removed;
/// single-user local mode needs no JWT signing.
#[tauri::command]
pub async fn instance_token(
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<serde_json::Value, String> {
    mint_instance_token(cfg.inner(), session.inner()).await
}

/// mint_instance_token returns a static local instance token.
/// The control-plane sidecar has been removed; single-user local mode needs
/// no JWT signing — a fixed opaque token is sufficient for the loopback proxy
/// and plugin instanceTokenUrl.
pub async fn mint_instance_token(
    cfg: &AppConfig,
    session: &Session,
) -> Result<serde_json::Value, String> {
    let _ = (cfg, session); // no HTTP call needed in serviceless mode
    Ok(serde_json::json!({
        "token": "local",
        "exp": 9999999999i64,
    }))
}

/// marketplace_catalog lists the available plugins with their installed state.
/// Served in-process from the federated plugin catalog (no control-plane HTTP
/// roundtrip). `installed` is approximated from the local selection file: a
/// plugin is considered installed if it is required or present in the selection.
#[tauri::command]
pub async fn marketplace_catalog(
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<serde_json::Value, String> {
    let _ = session; // auth check dropped for in-process call
    catalog_req(cfg.inner()).await
}

/// catalog_req builds the marketplace catalog from the in-process federated
/// plugin catalog. `installed` is approximated from the local selection file
/// (required ∪ selection). Reusable by the marketplace_catalog command and the
/// launch-time selection reconcile.
pub async fn catalog_req(
    cfg: &AppConfig,
) -> Result<serde_json::Value, String> {
    let user_sources = crate::catalog::sources::read_sources_in(&cfg.base_dir())?;
    let refs = build_refs_cached(&cfg.plugin_list_url, &user_sources).await;

    let client = shared_http_client();
    let plugins = crate::catalog::list(client, &refs).await;

    // Determine installed state: required ∪ local selection.
    let selection: std::collections::HashSet<String> =
        crate::config::read_selection_in(&cfg.base_dir())?
            .into_iter()
            .collect();

    let entries: Vec<serde_json::Value> = plugins
        .iter()
        .map(|p| {
            let installed = p.required || selection.contains(&p.footprint.plugin_id);
            serde_json::json!({
                "plugin_id": p.footprint.plugin_id,
                "grafana_slug": p.footprint.grafana_slug,
                "display_name": p.footprint.display_name,
                "description": p.footprint.description,
                "type": p.footprint.plugin_type,
                "required": p.required,
                "installed": installed,
                "latest_validated_version": p.version,
                "source": {
                    "url": p.source.url,
                    "publisher": p.source.publisher,
                    "verified": p.source.verified,
                }
            })
        })
        .collect();

    Ok(serde_json::json!({
        "plugins": entries,
    }))
}

/// list_sources returns the user-added plugin manifest URLs from the local
/// JSON store (in-process, no control-plane HTTP roundtrip).
/// Global (not org-scoped); the official set is implicit in the catalog.
#[tauri::command]
pub async fn list_sources(
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<serde_json::Value, String> {
    let _ = session; // auth check dropped for in-process call
    let records = crate::catalog::sources::read_sources_in(&cfg.base_dir())?;
    let entries: Vec<serde_json::Value> = records
        .iter()
        .map(|r| {
            serde_json::json!({
                "manifest_url": r.manifest_url,
                "publisher": r.publisher,
                "enabled": r.enabled,
            })
        })
        .collect();
    Ok(serde_json::Value::Array(entries))
}

/// add_source validates + persists a user-added per-plugin manifest URL to the
/// local JSON store (in-process). Surfaces validation errors verbatim so the UI
/// can show "manifest unreachable or invalid: …".
#[tauri::command]
pub async fn add_source(
    manifest_url: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<serde_json::Value, String> {
    let _ = session; // auth check dropped for in-process call
    let record = crate::catalog::sources::add_source_in(
        &cfg.base_dir(),
        shared_http_client(),
        &manifest_url,
    )
    .await?;
    Ok(serde_json::json!({
        "manifest_url": record.manifest_url,
        "publisher": record.publisher,
        "enabled": record.enabled,
    }))
}

/// remove_source deletes a user-added source from the local JSON store
/// (in-process, no control-plane HTTP roundtrip).
#[tauri::command]
pub async fn remove_source(
    manifest_url: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<(), String> {
    let _ = session; // auth check dropped for in-process call
    let deleted = crate::catalog::sources::remove_source_in(&cfg.base_dir(), &manifest_url)?;
    if !deleted {
        return Err(format!("source not found: {manifest_url}"));
    }
    Ok(())
}

/// reconcile_plugin_selection updates the local selection to include all
/// required plugins (self-heal) and seeds the selection file on first launch.
/// No HTTP calls: the control-plane sidecar has been removed. The actual binary
/// install/uninstall is handled by the Rust reconciler in grafana.rs.
pub async fn reconcile_plugin_selection(
    app: &tauri::AppHandle,
    cfg: &AppConfig,
    session: &Session,
) -> Result<(), String> {
    use std::collections::BTreeSet;
    let _ = session; // auth not needed; catalog is in-process
    let catalog = catalog_req(cfg).await?;
    let entries = catalog
        .get("plugins")
        .and_then(|v| v.as_array())
        .ok_or("catalog: missing plugins array")?;

    let mut required = BTreeSet::new();
    for e in entries {
        let Some(id) = e.get("plugin_id").and_then(|v| v.as_str()).filter(|s| !s.is_empty()) else {
            continue;
        };
        if e.get("required").and_then(|v| v.as_bool()).unwrap_or(false) {
            required.insert(id.to_string());
        }
    }

    // Seed selection on first launch (no-op if already seeded).
    let base = cfg.base_dir();
    if !crate::config::selection_exists_in(&base) {
        crate::config::write_selection_in(&base, &[])?;
    }
    let mut selection: BTreeSet<String> = crate::config::read_selection_in(&base)?
        .into_iter()
        .collect();

    // Ensure required plugins are in selection (self-heal).
    for id in &required {
        selection.insert(id.clone());
    }
    crate::config::write_selection_in(&base, &selection.into_iter().collect::<Vec<_>>())?;

    let _ = app.emit("reconcile-progress", "Plugin selection reconciled.");
    Ok(())
}

#[derive(serde::Serialize)]
pub struct VersionStatus {
    pub version: String,
    pub validated: bool,
}

/// plugin_versions lists the published versions of a plugin with their
/// validation status, newest first. Now served in-process from the federated
/// plugin catalog (no control-plane HTTP roundtrip). Uses the shared refs
/// cache to avoid re-fetching all manifests on every version-dropdown open.
#[tauri::command]
pub async fn plugin_versions(
    plugin_id: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<Vec<VersionStatus>, String> {
    let _ = session; // auth check dropped for in-process call
    let user_sources = crate::catalog::sources::read_sources_in(&cfg.base_dir())?;
    let refs = build_refs_cached(&cfg.plugin_list_url, &user_sources).await;

    // Find the ref for this plugin_id.
    let plugin_ref = refs
        .iter()
        .find(|r| r.plugin_id == plugin_id);

    match plugin_ref {
        None => Ok(vec![]), // unknown id → empty list
        Some(r) => {
            let vs = crate::catalog::versions_with_status(r);
            Ok(vs
                .into_iter()
                .map(|v| VersionStatus {
                    version: v.version,
                    validated: v.validated,
                })
                .collect())
        }
    }
}

/// get_plugin_selection returns the desired OPTIONAL plugins (the local
/// selection the plugins view writes). Required plugins are always installed at
/// launch regardless and are not part of this list.
#[tauri::command]
pub fn get_plugin_selection(
    cfg: State<'_, AppConfig>,
) -> Result<Vec<String>, String> {
    crate::config::read_selection_in(&cfg.base_dir())
}

/// seed_plugin_selection initializes the selection from the plugins already
/// installed (passed by the caller) the FIRST time — i.e. only when no selection
/// file exists yet. This migrates an instance from the old immediate-install
/// UI so the plugins view and launch agree that those plugins stay selected. A
/// no-op once a selection exists (so a deliberately-empty selection is honored).
/// Launch performs the same seed via config helpers; both go through
/// selection_exists_in + write_selection_in, so the rule lives in one place.
#[tauri::command]
pub fn seed_plugin_selection(
    installed: Vec<String>,
    cfg: State<'_, AppConfig>,
) -> Result<(), String> {
    let base = cfg.base_dir();
    if !crate::config::selection_exists_in(&base) {
        crate::config::write_selection_in(&base, &installed)?;
    }
    Ok(())
}

/// set_plugin_selection records whether the user wants `plugin_id` installed.
/// This does NOT install — launch reconciles installed == required ∪ selection
/// (see grafana::reconcile_plugin_selection). Selecting/deselecting takes effect
/// on the next launch.
#[tauri::command]
pub fn set_plugin_selection(
    plugin_id: String,
    selected: bool,
    cfg: State<'_, AppConfig>,
) -> Result<(), String> {
    crate::config::set_selection_in(&cfg.base_dir(), &plugin_id, selected)
}

/// get_show_preview returns the global preview-version toggle from shell settings.
#[tauri::command]
pub fn get_show_preview(cfg: State<'_, AppConfig>) -> Result<bool, String> {
    crate::config::read_show_preview_in(&cfg.base_dir())
}

/// set_show_preview persists the global preview-version toggle.
#[tauri::command]
pub fn set_show_preview(on: bool, cfg: State<'_, AppConfig>) -> Result<(), String> {
    crate::config::set_show_preview_in(&cfg.base_dir(), on)
}

/// get_plugin_pin returns the locally-pinned version for a plugin,
/// or None if no pin is set (meaning "use latest validated").
#[tauri::command]
pub fn get_plugin_pin(
    plugin_id: String,
    cfg: State<'_, AppConfig>,
) -> Result<Option<String>, String> {
    let pins = crate::config::read_pins_in(&cfg.base_dir())?;
    Ok(pins.get(&plugin_id).cloned())
}

/// set_plugin_pin writes or removes a local version pin for a plugin.
#[tauri::command]
pub fn set_plugin_pin(
    plugin_id: String,
    version: Option<String>,
    cfg: State<'_, AppConfig>,
) -> Result<(), String> {
    crate::config::set_pin_in(&cfg.base_dir(), &plugin_id, version.as_deref())
}

fn current_token(session: &Session) -> Result<String, String> {
    session
        .access_token
        .lock()
        .unwrap()
        .clone()
        .ok_or_else(|| "not logged in".to_string())
}

async fn read_json(resp: reqwest::Response, what: &str) -> Result<serde_json::Value, String> {
    let status = resp.status();
    let body = resp.bytes().await.map_err(|e| format!("read {what}: {e}"))?;
    if !status.is_success() {
        return Err(format!("{what} {status}: {}", String::from_utf8_lossy(&body)));
    }
    serde_json::from_slice(&body).map_err(|e| format!("decode {what}: {e}"))
}
