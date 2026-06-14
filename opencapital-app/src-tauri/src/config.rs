use std::env;
use std::path::PathBuf;

/// AppConfig holds the per-URL parameters the shell talks to. Explicit
/// URLs (no local|prod enum): dev points everything at localhost; a
/// different backend is just different env values.
#[derive(Clone, Debug)]
pub struct AppConfig {
    /// Control plane base, e.g. http://localhost:18080 in dev.
    pub control_plane_url: String,
    /// Kinde tenant, e.g. https://tickviewer.kinde.com.
    pub kinde_domain: String,
    /// Native (public, PKCE) app client id.
    pub kinde_client_id: String,
    /// API audience the access token must carry, e.g. https://tickviewer.io/api.
    pub kinde_audience: String,
    /// Loopback redirect registered with Kinde, e.g. http://localhost:3000.
    pub kinde_redirect_uri: String,
    /// OAuth scope. Defaults to "openid email" so the exchange returns an
    /// id_token with the user's email; audience governs API authorization.
    pub kinde_scope: String,

    // --- Grafana runtime (the "launch Grafana" half) -------------------
    /// Gateway base plugins POST data to (host-reachable), e.g. http://localhost:8090.
    pub gateway_url: String,
    /// Read-gateway base the core-datasource datasource posts metric queries to
    /// (host-reachable), e.g. http://localhost:8095.
    pub read_gateway_url: String,
    /// OTLP collector endpoint plugins ship spans to, e.g. http://localhost:4317.
    pub otlp_endpoint: String,
    /// RisingWave DSN the RisingWave (ops) datasource connects with.
    pub risingwave_dsn: String,
    /// Operator bootstrap token the control plane's instance endpoint
    /// accepts. Read from BOOTSTRAP_TOKEN, else the file at
    /// <repo>/secrets/admin_bootstrap_token. Empty if neither found
    /// (the reconciler will then fail loudly).
    pub bootstrap_token: String,
    /// Where grafana-server + numbat tarballs are downloaded/extracted.
    pub runtime_dir: PathBuf,
    /// Repo root — used to find the instance-bootstrap source (go run),
    /// lib-metrics, and the dev secret. Resolved from REPO_DIR or CWD/../..
    pub repo_dir: PathBuf,
    /// Per-platform grafana-server tarball URL (dl.grafana.com).
    pub grafana_download_url: String,
    /// Per-platform numbat CLI tarball URL (sharkdp/numbat releases).
    pub numbat_download_url: String,

    // --- Local data plane (LOCAL_DATA_PLANE) ---------------------------
    /// When true, the shell spawns the headless data plane natively
    /// (postgres + RisingWave today) instead of relying on a separate
    /// compose stack. Off by default (thin-client / cloud mode).
    pub local_data_plane: bool,
    /// Self-contained RisingWave artifact tarball (built by
    /// .github/workflows/risingwave-macos-artifact.yml, hosted on
    /// opencapital). Downloaded on first launch; no fallback.
    pub risingwave_artifact_url: String,
    /// Portable PostgreSQL binaries tarball (theseus-rs/postgresql-binaries).
    pub postgres_download_url: String,
    /// WSL distro rootfs tarball (built by .github/workflows/wsl-rootfs-artifact.yml,
    /// hosted on opencapital). Imported via `wsl --import` on first
    /// launch on Windows; unused on macOS/Linux.
    #[cfg_attr(not(windows), allow(dead_code))]
    pub wsl_rootfs_url: String,
}

impl AppConfig {
    pub fn load(resource_dir: Option<&std::path::Path>) -> Self {
        let repo_dir = resolve_repo_dir();
        let bootstrap_token = load_bootstrap_token(&repo_dir);
        // Data-plane endpoints resolve env var > bundled config.json > localhost
        // default, so a packaged build (no shell env) reads config.json while
        // `tauri dev` still overrides via the shell.
        let file = load_config_file(resource_dir);
        AppConfig {
            control_plane_url: pick("CONTROL_PLANE_URL", file.get("control_plane_url"), "http://localhost:18080"),
            kinde_domain: pick("KINDE_DOMAIN", file.get("kinde_domain"), "https://tickviewer.kinde.com"),
            kinde_client_id: pick("KINDE_CLIENT_ID", file.get("kinde_client_id"), "729cbedc064a482b83f32c6971c3872e"),
            kinde_audience: pick("KINDE_AUDIENCE", file.get("kinde_audience"), "https://tickviewer.io/api"),
            kinde_redirect_uri: pick("KINDE_REDIRECT_URI", file.get("kinde_redirect_uri"), "http://localhost:3000"),
            // openid+email so the token exchange returns an id_token carrying
            // the user's email (shown in the shell). Audience still governs the
            // access token's API authorization.
            kinde_scope: pick("KINDE_SCOPE", file.get("kinde_scope"), "openid email"),

            gateway_url: pick("PLUGIN_GATEWAY_URL", file.get("gateway_url"), "http://localhost:8090"),
            read_gateway_url: pick("PLUGIN_READ_GATEWAY_URL", file.get("read_gateway_url"), "http://localhost:8095"),
            otlp_endpoint: pick("PLUGIN_OTLP_ENDPOINT", file.get("otlp_endpoint"), "http://localhost:4317"),
            risingwave_dsn: pick(
                "RISINGWAVE_DSN",
                file.get("risingwave_dsn"),
                "postgres://root:root@localhost:4566/dev?sslmode=disable",
            ),
            bootstrap_token,
            runtime_dir: PathBuf::from(env_or(
                "RUNTIME_DIR",
                &default_runtime_dir().to_string_lossy(),
            )),
            repo_dir,
            grafana_download_url: env_or("GRAFANA_DOWNLOAD_URL", default_grafana_url()),
            numbat_download_url: env_or("NUMBAT_DOWNLOAD_URL", default_numbat_url()),
            local_data_plane: matches!(
                env::var("LOCAL_DATA_PLANE").ok().as_deref(),
                Some("1") | Some("true")
            ),
            risingwave_artifact_url: env_or("RISINGWAVE_ARTIFACT_URL", default_risingwave_artifact_url()),
            postgres_download_url: env_or("POSTGRES_DOWNLOAD_URL", default_postgres_url()),
            wsl_rootfs_url: env_or("WSL_ROOTFS_URL", default_wsl_rootfs_url()),
        }
    }

    /// Shell base dir: ~/.opencapital (or $HOME/.opencapital).
    pub fn base_dir(&self) -> PathBuf {
        home_dir().join(".opencapital")
    }

    /// Per-org instance dir holding provisioning/, plugins/, plugin-cache/,
    /// data/, logs/, grafana.ini.
    pub fn instance_dir(&self, org_id: &str) -> PathBuf {
        self.base_dir().join("instances").join(org_id)
    }

    /// Port the loopback callback listener binds, parsed from the redirect URI.
    pub fn redirect_port(&self) -> u16 {
        url::Url::parse(&self.kinde_redirect_uri)
            .ok()
            .and_then(|u| u.port())
            .unwrap_or(3000)
    }
}

fn env_or(key: &str, default: &str) -> String {
    env::var(key).unwrap_or_else(|_| default.to_string())
}

type FileMap = std::collections::BTreeMap<String, String>;

/// load_config_file reads `<resource_dir>/config.json` into a flat string map.
/// Missing/unreadable/malformed -> empty map (callers fall back to defaults).
fn load_config_file(dir: Option<&std::path::Path>) -> FileMap {
    let Some(dir) = dir else { return FileMap::new() };
    match std::fs::read_to_string(dir.join("config.json")) {
        Ok(s) => serde_json::from_str::<FileMap>(&s).unwrap_or_default(),
        Err(_) => FileMap::new(),
    }
}

/// pick resolves one setting: runtime env var > bundled config.json > default.
/// Empty/whitespace values are treated as unset at each level.
fn pick(env_key: &str, file_val: Option<&String>, default: &str) -> String {
    if let Ok(v) = env::var(env_key) {
        if !v.trim().is_empty() {
            return v;
        }
    }
    if let Some(v) = file_val {
        if !v.trim().is_empty() {
            return v.clone();
        }
    }
    default.to_string()
}

fn home_dir() -> PathBuf {
    env::var("HOME").map(PathBuf::from).unwrap_or_else(|_| PathBuf::from("."))
}

fn default_runtime_dir() -> PathBuf {
    home_dir().join(".opencapital").join("runtime")
}

/// resolve_repo_dir finds the monorepo root. REPO_DIR wins; otherwise walk
/// up from the current dir looking for the lib/instance-bootstrap marker
/// (tauri dev runs with CWD = src-tauri, two levels under the repo).
fn resolve_repo_dir() -> PathBuf {
    if let Ok(d) = env::var("REPO_DIR") {
        return PathBuf::from(d);
    }
    let cwd = env::current_dir().unwrap_or_else(|_| PathBuf::from("."));
    let mut dir = cwd.as_path();
    loop {
        if dir.join("lib/instance-bootstrap/go.mod").exists() {
            return dir.to_path_buf();
        }
        match dir.parent() {
            Some(p) => dir = p,
            None => break,
        }
    }
    // Fall back to CWD/../.. (src-tauri -> opencapital-app -> repo).
    cwd.join("..").join("..")
}

/// load_bootstrap_token reads BOOTSTRAP_TOKEN, else the dev secret file.
fn load_bootstrap_token(repo_dir: &std::path::Path) -> String {
    if let Ok(t) = env::var("BOOTSTRAP_TOKEN") {
        if !t.trim().is_empty() {
            return t.trim().to_string();
        }
    }
    let path = repo_dir.join("secrets/admin_bootstrap_token");
    std::fs::read_to_string(path)
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

/// Grafana ships native macOS arm64 server tarballs from 11.x onward; Linux
/// has amd64/arm64. Windows is a follow-up (v0 desktop targets macOS/Linux).
fn default_grafana_url() -> &'static str {
    if cfg!(target_os = "macos") {
        if cfg!(target_arch = "aarch64") {
            "https://dl.grafana.com/oss/release/grafana-13.0.2.darwin-arm64.tar.gz"
        } else {
            "https://dl.grafana.com/oss/release/grafana-13.0.2.darwin-amd64.tar.gz"
        }
    } else if cfg!(target_arch = "aarch64") {
        "https://dl.grafana.com/oss/release/grafana-13.0.2.linux-arm64.tar.gz"
    } else {
        "https://dl.grafana.com/oss/release/grafana-13.0.2.linux-amd64.tar.gz"
    }
}

/// Self-contained RisingWave artifact. macOS arm64 only in v0 (the only target
/// the artifact workflow builds); other platforms have no artifact yet and the
/// download fails loudly (no fallback, by design).
fn default_risingwave_artifact_url() -> &'static str {
    "https://github.com/opencapital-dev/opencapital/releases/download/risingwave-2.8.0-macos-arm64/risingwave-2.8.0-macos-arm64.tar.gz"
}

/// Portable PostgreSQL (theseus-rs/postgresql-binaries). macOS arm64 in v0.
/// Pinned to 17.x: RisingWave 2.8's postgres-cdc connector rejects Postgres 18
/// ("major version should be <= 17"), and the cloud stack runs postgres:17.
fn default_postgres_url() -> &'static str {
    "https://github.com/theseus-rs/postgresql-binaries/releases/download/17.10.0/postgresql-17.10.0-aarch64-apple-darwin.tar.gz"
}

/// WSL distro rootfs (the whole headless data plane). amd64 in v0 (the only
/// target the rootfs workflow builds); only consumed on Windows.
fn default_wsl_rootfs_url() -> &'static str {
    "https://github.com/opencapital-dev/opencapital/releases/download/wsl-rootfs-v2.8.0-amd64/opencapital-wsl-rootfs-v2.8.0-amd64.tar.gz"
}

fn default_numbat_url() -> &'static str {
    if cfg!(target_os = "macos") {
        if cfg!(target_arch = "aarch64") {
            "https://github.com/sharkdp/numbat/releases/download/v1.23.0/numbat-v1.23.0-aarch64-apple-darwin.tar.gz"
        } else {
            "https://github.com/sharkdp/numbat/releases/download/v1.23.0/numbat-v1.23.0-x86_64-apple-darwin.tar.gz"
        }
    } else if cfg!(target_arch = "aarch64") {
        "https://github.com/sharkdp/numbat/releases/download/v1.23.0/numbat-v1.23.0-aarch64-unknown-linux-gnu.tar.gz"
    } else {
        "https://github.com/sharkdp/numbat/releases/download/v1.23.0/numbat-v1.23.0-x86_64-unknown-linux-gnu.tar.gz"
    }
}

// ---------------------------------------------------------------------------
// Pure helpers — parameterised by base dir so they can be unit-tested without
// Tauri State.
// ---------------------------------------------------------------------------

use std::collections::BTreeMap;

fn pins_path(base: &std::path::Path, org: &str) -> PathBuf {
    base.join("instances").join(org).join("pins.json")
}

pub fn read_pins_in(base: &std::path::Path, org: &str) -> Result<BTreeMap<String, String>, String> {
    match std::fs::read_to_string(pins_path(base, org)) {
        Ok(s) => serde_json::from_str(&s).map_err(|e| format!("parse pins: {e}")),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(BTreeMap::new()),
        Err(e) => Err(format!("read pins: {e}")),
    }
}

pub fn set_pin_in(
    base: &std::path::Path,
    org: &str,
    plugin: &str,
    version: Option<&str>,
) -> Result<(), String> {
    let mut pins = read_pins_in(base, org)?;
    match version {
        Some(v) => { pins.insert(plugin.into(), v.into()); }
        None => { pins.remove(plugin); }
    }
    let p = pins_path(base, org);
    std::fs::create_dir_all(p.parent().unwrap()).map_err(|e| format!("mkdir: {e}"))?;
    std::fs::write(&p, serde_json::to_vec_pretty(&pins).unwrap())
        .map_err(|e| format!("write pins: {e}"))
}

/// pins_env_value renders the org's local pins as a JSON object string for the
/// instance-bootstrap PLUGIN_PINS env var. Unreadable/missing -> "{}".
pub fn pins_env_value(base: &std::path::Path, org: &str) -> String {
    match read_pins_in(base, org) {
        Ok(pins) => serde_json::to_string(&pins).unwrap_or_else(|_| "{}".into()),
        Err(_) => "{}".into(),
    }
}

fn settings_path(base: &std::path::Path) -> PathBuf {
    base.join("settings.json")
}

pub fn read_show_preview_in(base: &std::path::Path) -> Result<bool, String> {
    match std::fs::read_to_string(settings_path(base)) {
        Ok(s) => {
            let v: serde_json::Value =
                serde_json::from_str(&s).map_err(|e| format!("parse settings: {e}"))?;
            Ok(v.get("show_preview").and_then(|b| b.as_bool()).unwrap_or(false))
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(format!("read settings: {e}")),
    }
}

pub fn set_show_preview_in(base: &std::path::Path, on: bool) -> Result<(), String> {
    let p = settings_path(base);
    let mut v: serde_json::Value = match std::fs::read_to_string(&p) {
        Ok(s) => serde_json::from_str(&s).unwrap_or_else(|_| serde_json::json!({})),
        Err(_) => serde_json::json!({}),
    };
    v["show_preview"] = serde_json::Value::Bool(on);
    std::fs::create_dir_all(p.parent().unwrap()).map_err(|e| format!("mkdir: {e}"))?;
    std::fs::write(&p, serde_json::to_vec_pretty(&v).unwrap())
        .map_err(|e| format!("write settings: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pick_prefers_env_then_file_then_default() {
        let mut f = FileMap::new();
        f.insert("k".into(), "fromfile".into());
        std::env::set_var("OC_TEST_PICK_KEY", "fromenv");
        assert_eq!(pick("OC_TEST_PICK_KEY", f.get("k"), "def"), "fromenv");
        std::env::remove_var("OC_TEST_PICK_KEY");
        assert_eq!(pick("OC_TEST_PICK_KEY", f.get("k"), "def"), "fromfile");
        assert_eq!(pick("OC_TEST_PICK_KEY", None, "def"), "def");
    }

    #[test]
    fn load_config_file_reads_json_or_empty() {
        let dir = tempfile::tempdir().unwrap();
        assert!(load_config_file(Some(dir.path())).is_empty());
        std::fs::write(
            dir.path().join("config.json"),
            r#"{"gateway_url":"https://gw.example"}"#,
        )
        .unwrap();
        let m = load_config_file(Some(dir.path()));
        assert_eq!(m.get("gateway_url").map(String::as_str), Some("https://gw.example"));
    }

    #[test]
    fn pins_roundtrip_per_org() {
        let dir = tempfile::tempdir().unwrap();
        set_pin_in(dir.path(), "org1", "yfinance", Some("v1.0.3")).unwrap();
        let pins = read_pins_in(dir.path(), "org1").unwrap();
        assert_eq!(pins.get("yfinance").map(String::as_str), Some("v1.0.3"));
        set_pin_in(dir.path(), "org1", "yfinance", None).unwrap();
        assert!(read_pins_in(dir.path(), "org1").unwrap().get("yfinance").is_none());
    }

    #[test]
    fn pins_are_isolated_per_org() {
        let dir = tempfile::tempdir().unwrap();
        set_pin_in(dir.path(), "orgA", "yfinance", Some("v1")).unwrap();
        assert!(read_pins_in(dir.path(), "orgB").unwrap().is_empty());
    }

    #[test]
    fn pins_env_value_renders_json_or_empty() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(pins_env_value(dir.path(), "org1"), "{}");
        set_pin_in(dir.path(), "org1", "yfinance", Some("v1.0.3")).unwrap();
        let v: serde_json::Value =
            serde_json::from_str(&pins_env_value(dir.path(), "org1")).unwrap();
        assert_eq!(v.get("yfinance").and_then(|s| s.as_str()), Some("v1.0.3"));
    }

    #[test]
    fn show_preview_defaults_false_and_roundtrips() {
        let dir = tempfile::tempdir().unwrap();
        assert!(!read_show_preview_in(dir.path()).unwrap());
        set_show_preview_in(dir.path(), true).unwrap();
        assert!(read_show_preview_in(dir.path()).unwrap());
    }

    #[test]
    fn set_show_preview_preserves_other_settings_keys() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("settings.json"), br#"{"theme":"dark"}"#).unwrap();
        set_show_preview_in(dir.path(), true).unwrap();
        let v: serde_json::Value = serde_json::from_slice(
            &std::fs::read(dir.path().join("settings.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(v.get("theme").and_then(|t| t.as_str()), Some("dark"));
        assert_eq!(v.get("show_preview").and_then(|b| b.as_bool()), Some(true));
    }
}
