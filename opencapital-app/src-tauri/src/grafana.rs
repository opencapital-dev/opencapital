// Grafana lifecycle for the desktop shell: provision datasources +
// dashboards, reconcile plugins (instance-bootstrap), render grafana.ini,
// spawn grafana-server, health-poll, and keep it alive (bounded crash
// restart). The webview points at the loopback proxy (/grafana/*), never
// at grafana directly, so auth.proxy can inject the user identity.

use std::fs;
use std::io::{BufRead, BufReader};
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use tauri::{AppHandle, Emitter, Manager, State, WebviewUrl, WebviewWindowBuilder};

use crate::compute;
use crate::config::AppConfig;
use crate::kinde::{self, Session};
use crate::proxy::{self, Shared};
use crate::runtime;

/// emit a launch-progress event the frontend renders as a status line.
fn emit(app: &AppHandle, status: &str, detail: &str) {
    let _ = app.emit(
        "launch-progress",
        serde_json::json!({ "status": status, "detail": detail }),
    );
}

/// LaunchArgs is everything the blocking launch flow needs.
pub struct LaunchArgs {
    pub org_id: String,
    pub webauth_user: String,
    pub webauth_email: String,
}

/// launch_grafana is the single command the shell calls after the user picks
/// an org: mint the instance token, start the loopback, reconcile + spawn
/// grafana, then open the embedded Grafana webview. Progress streams to the
/// frontend via `launch-progress` + `reconcile-progress` events.
#[tauri::command]
pub async fn launch_grafana(
    app: AppHandle,
    org_id: String,
    user_email: String,
    user_name: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
    shared: State<'_, std::sync::Arc<Shared>>,
) -> Result<(), String> {
    // 1. Mint the org-scoped instance token (Option A) and stash it on the
    //    loopback for plugins to fetch.
    let tok_val = kinde::mint_instance_token(cfg.inner(), session.inner(), &org_id).await?;
    let token = tok_val
        .get("token")
        .and_then(|v| v.as_str())
        .ok_or("no token in mint response")?
        .to_string();

    let shared_arc = shared.inner().clone();
    proxy::start(shared_arc.clone())?;
    *shared_arc.instance_token.lock().unwrap() = Some(token);

    // 1b. Reconcile the org's installed plugins to (required ∪ local selection):
    //     the single install/uninstall/self-heal path. Required plugins are
    //     ensured every launch (self-heals an org whose onboarding missed them);
    //     the plugins view only edits the selection, never installs. Runs here in
    //     the async context so it finishes before the blocking grafana reconcile
    //     provisions whatever is now installed.
    emit(&app, "reconcile", "Reconciling plugin selection…");
    kinde::reconcile_plugin_selection(&app, cfg.inner(), session.inner(), &org_id).await?;

    // 2. Heavy lifting (downloads, reconcile, spawn) off the async runtime.
    let cfg_owned = cfg.inner().clone();
    let app_task = app.clone();
    let webauth_user = if user_email.is_empty() { user_name } else { user_email.clone() };
    let args = LaunchArgs { org_id, webauth_user, webauth_email: user_email };
    let shared_task = shared_arc.clone();
    tokio::task::spawn_blocking(move || launch(&app_task, &cfg_owned, &shared_task, &args))
        .await
        .map_err(|e| format!("launch task panicked: {e}"))??;

    // 3. Open the embedded Grafana webview at the loopback proxy.
    let loopback = shared_arc
        .loopback_port
        .lock()
        .unwrap()
        .ok_or("no loopback port")?;
    open_grafana_window(&app, &format!("http://127.0.0.1:{}/grafana/", loopback))
}

fn open_grafana_window(app: &AppHandle, url: &str) -> Result<(), String> {
    let parsed = url::Url::parse(url).map_err(|e| format!("bad grafana url: {e}"))?;
    if let Some(w) = app.get_webview_window("grafana") {
        // Relaunch with the window already open: reload it so a backend
        // re-provision (new dashboards / library panels) is actually shown.
        // Without this the webview keeps the stale page it first loaded and is
        // only re-focused, so provisioned changes never appear.
        w.navigate(parsed).map_err(|e| format!("reload grafana window: {e}"))?;
        let _ = w.set_focus();
        return Ok(());
    }
    WebviewWindowBuilder::new(app, "grafana", WebviewUrl::External(parsed))
        .title("OpenCapital — Grafana")
        .inner_size(1400.0, 900.0)
        .build()
        .map_err(|e| format!("open grafana window: {e}"))?;
    Ok(())
}

/// grafana_running reports whether a grafana instance is currently up for this
/// shell. The LaunchView remounts (losing its local phase) when the user
/// navigates to Plugins and back, so it queries this on mount to show
/// Relaunch vs Launch correctly.
#[tauri::command]
pub fn grafana_running(shared: State<'_, std::sync::Arc<Shared>>) -> bool {
    shared.grafana_port.lock().unwrap().is_some()
}

/// launch runs the full blocking flow: ensure runtime, provision, reconcile,
/// spawn grafana, health-poll, start the crash monitor. Returns the grafana
/// upstream port (also stored in shared). Call via spawn_blocking.
pub fn launch(
    app: &AppHandle,
    cfg: &AppConfig,
    shared: &Arc<Shared>,
    args: &LaunchArgs,
) -> Result<u16, String> {
    *shared.webauth_user.lock().unwrap() = Some(args.webauth_user.clone());
    *shared.webauth_email.lock().unwrap() = Some(args.webauth_email.clone());

    let loopback = shared
        .loopback_port
        .lock()
        .unwrap()
        .ok_or("loopback server not started")?;
    let instance_token_url = format!("http://127.0.0.1:{}/instance-token", loopback);

    // Start the compute sidecar first: its loopback URL must be available
    // before reconcile so instance-bootstrap can stamp it into plugin jsonData
    // (Grafana sanitizes the plugin env, so this travels as data).
    emit(app, "compute", "Starting compute sidecar…");
    let compute_port = compute::start(app, cfg, shared)?;
    let compute_url = format!("http://127.0.0.1:{}", compute_port);

    emit(app, "runtime", "Checking grafana runtime…");
    let rt = runtime::ensure(cfg, |m| emit(app, "runtime", m))?;

    // Overlay our customized Grafana frontend (bundled `grafana-public` resource)
    // onto the vanilla grafana just ensured. Frontend-only; idempotent. Absent
    // in dev builds without the staged resource — then grafana runs vanilla.
    if let Ok(overlay) = app.path().resolve("grafana-public", tauri::path::BaseDirectory::Resource) {
        if overlay.exists() {
            emit(app, "runtime", "Applying UI overlay…");
            runtime::overlay_grafana(&rt.grafana_homepath, &overlay)
                .map_err(|e| format!("overlay grafana ui: {e}"))?;
        }
    }

    let inst = cfg.instance_dir(&args.org_id);
    let provisioning = inst.join("provisioning");
    let plugins_dir = inst.join("plugins");
    let cache_dir = cfg.runtime_dir.join("plugin-cache");
    // Writable root for plugin-private SQLite. pluginclient defaults PluginsRoot
    // to /var/lib/plugins (unwritable on the laptop); without PLUGINS_ROOT set on
    // grafana-server, every plugin's OpenDB mkdir fails. Shared across orgs —
    // pluginclient namespaces by <plugin>/<org>/ underneath.
    let plugin_state = cfg.runtime_dir.join("plugin-state");
    for d in [&provisioning, &plugins_dir, &cache_dir, &plugin_state, &inst.join("data"), &inst.join("logs")] {
        fs::create_dir_all(d).map_err(|e| format!("mkdir {}: {e}", d.display()))?;
    }

    // RW host/port the plugins connect to (from the configured DSN). Used
    // both for the reconciler-rendered plugin jsonData and the core-datasource
    // datasource. Grafana sanitizes plugin env, so this travels as data.
    let rw_hostport = dsn_field(&cfg.risingwave_dsn, "hostport").unwrap_or_else(|| "localhost:4566".into());
    let (rw_host, rw_port) = rw_hostport.split_once(':').unwrap_or(("localhost", "4566"));
    let (rw_host, rw_port) = (rw_host.to_string(), rw_port.to_string());

    emit(app, "provision", "Writing datasource provisioning…");
    write_provisioning(cfg, &provisioning)?;

    emit(app, "reconcile", "Reconciling plugins…");
    reconcile(app, cfg, args, &provisioning, &plugins_dir, &cache_dir, &instance_token_url, &rw_host, &rw_port, &compute_url)?;

    emit(app, "config", "Rendering grafana.ini…");
    let port = free_port()?;
    let ini = inst.join("grafana.ini");
    let unsigned = read_unsigned_plugins(&provisioning);
    fs::write(&ini, grafana_ini(&inst, loopback, port, &unsigned))
        .map_err(|e| format!("write ini: {e}"))?;

    emit(app, "spawn", "Starting grafana-server…");
    let spec = SpawnSpec {
        bin: rt.grafana_bin.clone(),
        needs_subcmd: rt.grafana_needs_server_subcmd,
        homepath: rt.grafana_homepath.clone(),
        config: ini.clone(),
        log_path: inst.join("logs/grafana.log"),
        rw_host: rw_host.clone(),
        rw_port: rw_port.clone(),
    };
    // Supersede any grafana from a previous launch before starting this one:
    // bump the generation so a stale crash monitor exits instead of respawning,
    // then kill leftover grafana procs (a dropped Child handle never kills them,
    // so without this each relaunch leaks an instance and the webview can hit a
    // stale upstream).
    let my_gen = shared.generation.fetch_add(1, Ordering::SeqCst) + 1;
    crate::kill_stray_grafana();
    let child = spawn_grafana(&spec)?;
    *shared.child.lock().unwrap() = Some(child);
    *shared.grafana_port.lock().unwrap() = Some(port);

    emit(app, "health", "Waiting for grafana to become healthy…");
    health_poll(port, Duration::from_secs(45))?;

    // Post-start, non-critical: push each plugin's library panels via the
    // Grafana API now that it's healthy. A failure here is logged, not fatal —
    // Grafana is up and usable; the panels can be re-pushed on next launch.
    emit(app, "library-panels", "Publishing library panels…");
    provision_library_panels(
        app, cfg, args, &provisioning, &plugins_dir, &cache_dir, &instance_token_url, &rw_host, &rw_port, &compute_url, port,
    );

    start_crash_monitor(app.clone(), shared.clone(), spec, port, my_gen);

    emit(app, "ready", "Grafana is up.");
    Ok(port)
}

// --- provisioning ------------------------------------------------------------

fn write_provisioning(cfg: &AppConfig, provisioning: &Path) -> Result<(), String> {
    let ds_dir = provisioning.join("datasources");
    let plugins_dir = provisioning.join("plugins"); // reconciler writes here
    // Dashboard provisioning (provider YAML + plugin-bundled dashboards under
    // dashboards/plugins/) is now written by the reconciler (instance-bootstrap),
    // not here — see Phase 2.
    for d in [&ds_dir, &plugins_dir] {
        fs::create_dir_all(d).map_err(|e| format!("mkdir {}: {e}", d.display()))?;
    }

    // Only the RisingWave (ops/diagnostics) datasource is rendered here. The
    // core-datasource datasource is rendered by the reconciler (instance-bootstrap)
    // because it needs the per-org platform token the reconciler fetches.
    let ds = format!(
        r#"apiVersion: 1
datasources:
  - name: RisingWave
    uid: risingwave
    type: postgres
    access: proxy
    url: {rw_host}
    user: {rw_user}
    secureJsonData:
      password: {rw_pass}
    jsonData:
      database: {rw_db}
      sslmode: disable
      postgresVersion: 1500
    editable: true
"#,
        rw_host = dsn_field(&cfg.risingwave_dsn, "hostport").unwrap_or_else(|| "localhost:4566".into()),
        rw_user = dsn_field(&cfg.risingwave_dsn, "user").unwrap_or_else(|| "root".into()),
        rw_pass = dsn_field(&cfg.risingwave_dsn, "pass").unwrap_or_else(|| "root".into()),
        rw_db = dsn_field(&cfg.risingwave_dsn, "db").unwrap_or_else(|| "dev".into()),
    );
    fs::write(ds_dir.join("datasources.yml"), ds).map_err(|e| format!("write datasources: {e}"))?;
    Ok(())
}

/// dsn_field pulls a field out of a postgres:// DSN. Tiny parser; good enough
/// for postgres://user:pass@host:port/db?...
fn dsn_field(dsn: &str, field: &str) -> Option<String> {
    let rest = dsn.strip_prefix("postgres://")?;
    let (creds_host, dbq) = rest.split_once('/')?;
    let (creds, hostport) = creds_host.split_once('@')?;
    let (user, pass) = creds.split_once(':').unwrap_or((creds, ""));
    let db = dbq.split('?').next().unwrap_or(dbq);
    match field {
        "user" => Some(user.to_string()),
        "pass" => Some(pass.to_string()),
        "hostport" => Some(hostport.to_string()),
        "db" => Some(db.to_string()),
        _ => None,
    }
}

// --- reconcile ---------------------------------------------------------------

/// instance_bootstrap_command builds an invocation of the bundled
/// `instance-bootstrap` sidecar binary (resolved next to the app exe, the same
/// way as the data-plane services — built + staged by `make dataplane-stage`),
/// pre-loaded with the shared Config env that both subcommands (reconcile +
/// library-panels) read. The caller appends the subcommand arg and any
/// step-specific env (e.g. GRAFANA_URL for library-panels). stdout + stderr are
/// piped so progress can be streamed line by line. Returns an error if the
/// sidecar is missing (a packaging bug — the app never shells out to `go`).
#[allow(clippy::too_many_arguments)]
fn instance_bootstrap_command(
    cfg: &AppConfig,
    args: &LaunchArgs,
    provisioning: &Path,
    plugins_dir: &Path,
    cache_dir: &Path,
    instance_token_url: &str,
    rw_host: &str,
    rw_port: &str,
    compute_url: &str,
) -> Result<Command, String> {
    let bin = crate::dataplane::sidecar_bin("instance-bootstrap")?;
    // Writable SQLite root stamped into every plugin's jsonData as pluginsRoot.
    // Grafana sanitizes the plugin subprocess env, so this must travel as data,
    // not as a PLUGINS_ROOT env on grafana-server.
    let plugin_state = cfg.runtime_dir.join("plugin-state");
    let mut cmd = Command::new(&bin);
    cmd.env("ORG_ID", &args.org_id)
        .env("PLUGIN_STATE_DIR", &plugin_state)
        .env("CONTROL_PLANE_URL", &cfg.control_plane_url)
        .env("BOOTSTRAP_TOKEN", &cfg.bootstrap_token)
        .env("GRAFANA_PROVISIONING_DIR", provisioning)
        .env("GRAFANA_PLUGINS_DIR", plugins_dir)
        .env("PLUGIN_CACHE_DIR", cache_dir)
        .env("PLUGIN_CONTROL_PLANE_URL", &cfg.control_plane_url)
        .env("PLUGIN_GATEWAY_URL", &cfg.gateway_url)
        .env("PLUGIN_READ_GATEWAY_URL", &cfg.read_gateway_url)
        .env("PLUGIN_OTLP_ENDPOINT", &cfg.otlp_endpoint)
        .env("INSTANCE_TOKEN_URL", instance_token_url)
        .env("PLUGIN_RISINGWAVE_HOST", rw_host)
        .env("PLUGIN_RISINGWAVE_PORT", rw_port)
        .env("PLUGIN_COMPUTE_URL", compute_url)
        .env("PLUGIN_PINS", crate::config::pins_env_value(&cfg.base_dir(), &args.org_id))
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    Ok(cmd)
}

/// drain_stderr reads whatever the child wrote to stderr (best-effort).
fn drain_stderr(child: &mut Child) -> String {
    child
        .stderr
        .take()
        .map(|mut s| {
            let mut buf = String::new();
            use std::io::Read;
            let _ = s.read_to_string(&mut buf);
            buf
        })
        .unwrap_or_default()
}

#[allow(clippy::too_many_arguments)]
fn reconcile(
    app: &AppHandle,
    cfg: &AppConfig,
    args: &LaunchArgs,
    provisioning: &Path,
    plugins_dir: &Path,
    cache_dir: &Path,
    instance_token_url: &str,
    rw_host: &str,
    rw_port: &str,
    compute_url: &str,
) -> Result<(), String> {
    if cfg.bootstrap_token.is_empty() {
        return Err("bootstrap token not set (local data plane provides it automatically; for a remote control plane set BOOTSTRAP_TOKEN)".into());
    }
    let mut child = instance_bootstrap_command(
        cfg, args, provisioning, plugins_dir, cache_dir, instance_token_url, rw_host, rw_port, compute_url,
    )?
    .spawn()
    .map_err(|e| format!("spawn reconciler: {e}"))?;

    if let Some(out) = child.stdout.take() {
        let reader = BufReader::new(out);
        for line in reader.lines().map_while(Result::ok) {
            let _ = app.emit("reconcile-progress", &line);
        }
    }
    let status = child.wait().map_err(|e| format!("reconciler wait: {e}"))?;
    if !status.success() {
        return Err(format!("reconciler failed ({status}): {}", drain_stderr(&mut child)));
    }
    Ok(())
}

/// provision_library_panels runs the post-start `library-panels` subcommand
/// once Grafana is healthy: it upserts each installed plugin's library panels
/// via the Grafana HTTP API. Auth is the webauth admin user (no password) —
/// the loopback auth.proxy injects identity, and library-panels passes the
/// user via GRAFANA_WEBAUTH_USER.
///
/// Non-fatal: unlike reconcile, a failure here must NOT abort the launch.
/// Grafana is already up and usable; missing library panels is a degraded but
/// recoverable state. On failure we emit a labelled warning and return Ok.
#[allow(clippy::too_many_arguments)]
fn provision_library_panels(
    app: &AppHandle,
    cfg: &AppConfig,
    args: &LaunchArgs,
    provisioning: &Path,
    plugins_dir: &Path,
    cache_dir: &Path,
    instance_token_url: &str,
    rw_host: &str,
    rw_port: &str,
    compute_url: &str,
    port: u16,
) {
    let mut cmd = match instance_bootstrap_command(
        cfg, args, provisioning, plugins_dir, cache_dir, instance_token_url, rw_host, rw_port, compute_url,
    ) {
        Ok(c) => c,
        Err(e) => {
            let _ = app.emit("library-panels-progress", format!("WARN: {e}"));
            return;
        }
    };
    cmd.arg("library-panels")
        .env("GRAFANA_URL", format!("http://127.0.0.1:{}", port))
        .env("GRAFANA_WEBAUTH_USER", &args.webauth_user);

    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            let _ = app.emit(
                "library-panels-progress",
                format!("WARN: failed to spawn library-panels: {e}"),
            );
            return;
        }
    };

    if let Some(out) = child.stdout.take() {
        let reader = BufReader::new(out);
        for line in reader.lines().map_while(Result::ok) {
            let _ = app.emit("library-panels-progress", &line);
        }
    }
    match child.wait() {
        Ok(status) if status.success() => {}
        Ok(status) => {
            let _ = app.emit(
                "library-panels-progress",
                format!("WARN: library-panels failed ({status}); panels may be missing: {}", drain_stderr(&mut child)),
            );
        }
        Err(e) => {
            let _ = app.emit("library-panels-progress", format!("WARN: library-panels wait: {e}"));
        }
    }
}

// --- grafana.ini -------------------------------------------------------------

/// Read the comma-joined Grafana slugs the reconciler wrote to
/// `<provisioning>/unsigned-plugins`. Returns the trimmed contents, or an
/// empty string if the file is missing (expected before first reconcile) or
/// empty. Any other I/O error is logged and treated as empty so Grafana can
/// still start, but the operator can see what went wrong.
fn read_unsigned_plugins(provisioning: &Path) -> String {
    fs::read_to_string(provisioning.join("unsigned-plugins"))
        .map(|s| s.trim().to_string())
        .unwrap_or_else(|e| {
            if e.kind() != std::io::ErrorKind::NotFound {
                eprintln!("[grafana] read unsigned-plugins: {e}");
            }
            String::new()
        })
}

fn grafana_ini(inst: &Path, loopback: u16, port: u16, unsigned: &str) -> String {
    format!(
        r#"[paths]
data = {data}
logs = {logs}
plugins = {plugins}
provisioning = {provisioning}

[server]
http_addr = 127.0.0.1
http_port = {port}
root_url = http://127.0.0.1:{loopback}/grafana/
serve_from_sub_path = true

[analytics]
reporting_enabled = false
check_for_updates = false

[security]
allow_embedding = true

[users]
auto_assign_org = true
auto_assign_org_role = Admin
home_page = /a/core-app

[explore]
enabled = false

[unified_alerting]
enabled = false

[auth]
disable_login_form = true

[auth.proxy]
enabled = true
header_name = X-WEBAUTH-USER
header_property = username
auto_sign_up = true
headers = Email:X-WEBAUTH-EMAIL Name:X-WEBAUTH-NAME
enable_login_token = false

[plugins]
allow_loading_unsigned_plugins = {unsigned}

[log]
mode = console file
level = {log_level}
"#,
        data = inst.join("data").to_string_lossy(),
        logs = inst.join("logs").to_string_lossy(),
        plugins = inst.join("plugins").to_string_lossy(),
        provisioning = inst.join("provisioning").to_string_lossy(),
        port = port,
        loopback = loopback,
        unsigned = unsigned,
        log_level = std::env::var("GF_LOG_LEVEL").unwrap_or_else(|_| "info".into()),
    )
}

// --- spawn / health / crash monitor -----------------------------------------

#[derive(Clone)]
struct SpawnSpec {
    bin: PathBuf,
    needs_subcmd: bool,
    homepath: PathBuf,
    config: PathBuf,
    log_path: PathBuf,
    // App plugins reach RisingWave via pluginclient, which reads the RW host
    // from RISINGWAVE_HOST/PORT env (default "risingwave" — the compose
    // service name, unresolvable on the laptop). Pass the host-reachable
    // values so plugins connect to the local backend.
    rw_host: String,
    rw_port: String,
}

fn spawn_grafana(spec: &SpawnSpec) -> Result<Child, String> {
    let log = fs::File::create(&spec.log_path).map_err(|e| format!("open grafana log: {e}"))?;
    let log_err = log.try_clone().map_err(|e| format!("clone log handle: {e}"))?;
    let mut cmd = Command::new(&spec.bin);
    if spec.needs_subcmd {
        cmd.arg("server");
    }
    cmd.arg(format!("--homepath={}", spec.homepath.to_string_lossy()))
        .arg(format!("--config={}", spec.config.to_string_lossy()))
        // NOTE: only GF_* env survives into plugin subprocesses (Grafana
        // sanitizes the rest), so plugin config like the SQLite root and the RW
        // host travels via jsonData (instance-bootstrap), NOT env. These two are
        // set for any non-sanitizing tooling but are not relied upon.
        .env("RISINGWAVE_HOST", &spec.rw_host)
        .env("RISINGWAVE_PORT", &spec.rw_port)
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(log_err));
    cmd.spawn().map_err(|e| format!("spawn grafana-server: {e}"))
}

fn health_poll(port: u16, timeout: Duration) -> Result<(), String> {
    let url = format!("http://127.0.0.1:{}/api/health", port);
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(2))
        .build()
        .map_err(|e| format!("health client: {e}"))?;
    let deadline = Instant::now() + timeout;
    loop {
        if let Ok(r) = client.get(&url).send() {
            if r.status().is_success() {
                return Ok(());
            }
        }
        if Instant::now() >= deadline {
            return Err(format!("grafana not healthy within {:?}", timeout));
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// start_crash_monitor waits on the grafana child and restarts it on
/// unexpected exit, capped at 3 restarts per 5-minute window.
fn start_crash_monitor(app: AppHandle, shared: Arc<Shared>, spec: SpawnSpec, port: u16, my_gen: u64) {
    std::thread::spawn(move || {
        let restarts = AtomicU32::new(0);
        let mut window_start = Instant::now();
        loop {
            // Take the child out, wait on it, then decide.
            let child = shared.child.lock().unwrap().take();
            let mut child = match child {
                Some(c) => c,
                None => return, // shut down elsewhere
            };
            let _ = child.wait();
            // A newer launch superseded this grafana (relaunch bumped the
            // generation + killed it) — exit instead of respawning a stale one.
            if shared.generation.load(Ordering::SeqCst) != my_gen {
                return;
            }
            // Reset the window every 5 minutes.
            if window_start.elapsed() > Duration::from_secs(300) {
                window_start = Instant::now();
                restarts.store(0, Ordering::SeqCst);
            }
            if restarts.fetch_add(1, Ordering::SeqCst) >= 3 {
                let _ = app.emit("grafana-crashed", "grafana exited too many times; giving up");
                *shared.grafana_port.lock().unwrap() = None;
                return;
            }
            let _ = app.emit("grafana-restarting", "grafana exited; restarting…");
            match spawn_grafana(&spec) {
                Ok(c) => {
                    *shared.child.lock().unwrap() = Some(c);
                    let _ = health_poll(port, Duration::from_secs(45));
                    let _ = app.emit("grafana-restarted", "grafana back up");
                }
                Err(e) => {
                    let _ = app.emit("grafana-crashed", format!("respawn failed: {e}"));
                    return;
                }
            }
        }
    });
}

/// free_port asks the OS for an unused loopback TCP port.
fn free_port() -> Result<u16, String> {
    let l = TcpListener::bind("127.0.0.1:0").map_err(|e| format!("pick port: {e}"))?;
    let p = l.local_addr().map_err(|e| format!("addr: {e}"))?.port();
    Ok(p)
}

#[cfg(test)]
mod tests {
    use super::read_unsigned_plugins;
    use std::fs;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU32, Ordering};

    /// Unique temp dir under the system temp dir, cleaned up on drop.
    struct TempDir(PathBuf);

    impl TempDir {
        fn new() -> Self {
            static COUNTER: AtomicU32 = AtomicU32::new(0);
            let n = COUNTER.fetch_add(1, Ordering::Relaxed);
            let p = std::env::temp_dir().join(format!(
                "opencapital-grafana-test-{}-{}-{}",
                std::process::id(),
                n,
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_nanos(),
            ));
            fs::create_dir_all(&p).unwrap();
            TempDir(p)
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn read_unsigned_plugins_returns_trimmed_contents() {
        let tmp = TempDir::new();
        let provisioning = tmp.0.join("provisioning");
        fs::create_dir_all(&provisioning).unwrap();
        fs::write(provisioning.join("unsigned-plugins"), "a-slug,b-slug\n").unwrap();
        assert_eq!(read_unsigned_plugins(&provisioning), "a-slug,b-slug");
    }

    #[test]
    fn read_unsigned_plugins_missing_file_is_empty() {
        let tmp = TempDir::new();
        let provisioning = tmp.0.join("provisioning");
        fs::create_dir_all(&provisioning).unwrap();
        assert_eq!(read_unsigned_plugins(&provisioning), "");
    }

    #[test]
    fn read_unsigned_plugins_empty_file_is_empty() {
        let tmp = TempDir::new();
        let provisioning = tmp.0.join("provisioning");
        fs::create_dir_all(&provisioning).unwrap();
        fs::write(provisioning.join("unsigned-plugins"), "").unwrap();
        assert_eq!(read_unsigned_plugins(&provisioning), "");
    }
}
