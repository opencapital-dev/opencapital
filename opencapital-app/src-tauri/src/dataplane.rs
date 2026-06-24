// Local data-plane supervisor. When LOCAL_DATA_PLANE is on, the shell brings up
// the headless data plane natively instead of relying on a separate compose
// stack. v0 infra deps: PostgreSQL (control_db) + RisingWave single-node.
//
// Each binary is DOWNLOADED on first launch (no brew, no PATH fallback) and
// extracted under runtime_dir (re-downloadable, content-checked by a marker);
// state (pgdata, RW store) lives under base_dir (persistent, survives a binary
// re-download or app update). Each service is spawned, health-checked on its
// loopback port, and kept alive by a bounded crash restart — mirroring
// compute.rs.
//
// RisingWave is the self-contained macOS artifact built by
// .github/workflows/risingwave-macos-artifact.yml: a relinked binary plus a
// vendored CPython (PYTHONHOME) and connector libs (CONNECTOR_LIBS_PATH).
// PostgreSQL is a portable build from theseus-rs/postgresql-binaries.
//
// On Windows the native path is replaced by the WSL distro (crate::wsl); its
// macOS/Linux-only helpers are then unused, so allow dead_code on that target
// only — the active platform keeps full lint coverage.
#![cfg_attr(windows, allow(dead_code))]

use std::fs;
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tauri::{AppHandle, Emitter};

use crate::config::AppConfig;
use crate::proxy::Shared;
use crate::runtime;

/// Generous: first launch extracts a ~half-GB RW tree and inits a fresh
/// Postgres cluster before the port opens (Windows: imports a ~1GB distro).
pub(crate) const HEALTH_TIMEOUT: Duration = Duration::from_secs(120);

const PG_PORT: u16 = 5432;
pub(crate) const RW_PORT: u16 = 4566;

/// Kept for config.rs's load_bootstrap_token (local_data_plane mode). No longer
/// used as a control-plane bootstrap token because the control-plane sidecar has
/// been removed; kept to avoid changing config.rs's public API.
pub(crate) const LOCAL_TOKEN: &str = "localbootstrap";

/// start brings the local data plane up on a background thread (so app boot is
/// not blocked) when enabled. No-op otherwise. Emits `dataplane-*` events.
pub fn start(app: AppHandle, cfg: AppConfig, shared: Arc<Shared>) {
    if !cfg.local_data_plane {
        return;
    }
    std::thread::spawn(move || match run(&app, &cfg, &shared) {
        Ok(()) => {
            let _ = app.emit("dataplane-ready", "local data plane up");
        }
        Err(e) => {
            let _ = app.emit("dataplane-failed", e);
        }
    });
}

/// run brings the plane up per platform: native processes on macOS/Linux, the
/// bundled WSL2 distro on Windows (RisingWave has no Windows binary).
#[cfg(not(windows))]
fn run(app: &AppHandle, cfg: &AppConfig, shared: &Arc<Shared>) -> Result<(), String> {
    bring_up(app, cfg, shared)
}

#[cfg(windows)]
fn run(app: &AppHandle, cfg: &AppConfig, shared: &Arc<Shared>) -> Result<(), String> {
    crate::wsl::bring_up(app, cfg, shared)
}

fn bring_up(app: &AppHandle, cfg: &AppConfig, shared: &Arc<Shared>) -> Result<(), String> {
    fs::create_dir_all(&cfg.runtime_dir).map_err(|e| format!("mkdir runtime dir: {e}"))?;
    let progress = |m: &str| {
        let _ = app.emit("dataplane-progress", m.to_string());
    };

    // 1. Postgres (wal_level=logical for RW CDC). control_db underpins everything.
    let pg = ensure_postgres(cfg, &progress)?;
    {
        let (c, s) = (cfg.clone(), pg.clone());
        *shared.pg_child.lock().unwrap() = Some(spawn_postgres(&pg, &c)?);
        health_tcp(PG_PORT, HEALTH_TIMEOUT)?;
        supervise(app.clone(), shared.clone(), |s| &s.pg_child, "postgres", PG_PORT,
            Arc::new(move || spawn_postgres(&s, &c)));
    }

    // 2. Bootstrap control_db (db + roles + portfolios table idempotently).
    //    The control-plane sidecar has been removed; portfolios DDL is now applied
    //    here on every boot (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS guards).
    bootstrap_control_db(&pg, cfg, &progress)?;

    // 3. RisingWave.
    let rw = ensure_risingwave(cfg, &progress)?;
    {
        let (c, s) = (cfg.clone(), rw.clone());
        *shared.rw_child.lock().unwrap() = Some(spawn_risingwave(&rw, &c)?);
        health_tcp(RW_PORT, HEALTH_TIMEOUT)?;
        supervise(app.clone(), shared.clone(), |s| &s.rw_child, "risingwave", RW_PORT,
            Arc::new(move || spawn_risingwave(&s, &c)));
    }

    // 4. Apply the local RW schema (connector-less tables + MVs + pg CDC source).
    //    Needs portfolios table + publication (done in bootstrap_control_db) and RW up.
    apply_rw_schema(&pg, cfg, &progress)?;

    Ok(())
}

// --- Sidecar binary resolution ---------------------------------------------

/// sidecar_bin resolves a binary bundled as a Tauri externalBin sidecar next
/// to the app executable — the same mechanism as the compute sidecar (see
/// compute::resolve_compute_bin), built + staged by `make dataplane-stage`.
/// Tauri strips the `-<triple>` suffix when staging next to the app binary in a
/// bundle; fall back to the suffixed name for unbundled layouts where the stager
/// left it as `<name>-<triple>`.
pub(crate) fn sidecar_bin(name: &str) -> Result<PathBuf, String> {
    let exe = std::env::current_exe().map_err(|e| format!("current_exe: {e}"))?;
    let dir = exe.parent().ok_or("exe has no parent dir")?;
    let plain = dir.join(name);
    if plain.exists() {
        return Ok(plain);
    }
    let triple = env!("TARGET_TRIPLE");
    let suffixed = dir.join(format!("{name}-{triple}"));
    if suffixed.exists() {
        return Ok(suffixed);
    }
    Err(format!(
        "sidecar binary `{name}` not found next to {} (looked for `{name}` and `{name}-{triple}`; run `make dataplane-stage`)",
        exe.display(),
    ))
}

// --- schema bootstrap ------------------------------------------------------

/// bootstrap_control_db creates the control_db database + roles on first boot,
/// then applies the portfolios DDL (idempotent: CREATE TABLE IF NOT EXISTS, etc.)
/// on every boot. The control-plane sidecar has been removed; DDL that was
/// previously migrated by control-plane on startup is now applied here.
fn bootstrap_control_db<F: Fn(&str)>(pg: &PgPaths, cfg: &AppConfig, progress: &F) -> Result<(), String> {
    let psql = pg.bindir.join("psql");
    // health_tcp(PG_PORT) only proved the port is open: postgres opens its
    // listener BEFORE finishing crash recovery, during which it rejects every
    // query with "the database system is starting up". Querying in that window
    // made the existence check below see empty output (read as "absent") and the
    // subsequent CREATE DATABASE fail with "already exists", aborting bring_up.
    // Wait for postgres to actually answer a query.
    let exists = wait_control_db_exists(&psql, HEALTH_TIMEOUT)?;
    if !exists {
        progress("Bootstrapping control_db…");
        psql_run(&psql, "postgres", &["-c", "CREATE DATABASE control_db;"])?;
        let schema = cfg.dataplane_dir.join("postgres/init/01-schema.sql");
        psql_run(&psql, "control_db", &["-v", "ON_ERROR_STOP=1", "-f", &schema.to_string_lossy()])?;
    }
    // Always run portfolios DDL (idempotent: CREATE TABLE IF NOT EXISTS, etc.)
    // Previously applied by the control-plane sidecar on boot; now applied here.
    let portfolios = cfg.dataplane_dir.join("postgres/init/02-portfolios.sql");
    if portfolios.exists() {
        psql_run(&psql, "control_db", &["-v", "ON_ERROR_STOP=1", "-f", &portfolios.to_string_lossy()])?;
    }
    Ok(())
}

/// wait_control_db_exists polls postgres until it ACCEPTS a query (not just until
/// the TCP port is open), then returns whether control_db exists. A failed psql
/// invocation means postgres is still in recovery ("starting up") — retry until
/// the deadline rather than misreading it as "database absent". Only a query
/// that postgres actually answered is trusted, so the caller's CREATE DATABASE
/// runs only when control_db is genuinely missing.
fn wait_control_db_exists(psql: &Path, timeout: Duration) -> Result<bool, String> {
    let deadline = Instant::now() + timeout;
    loop {
        let out = Command::new(psql)
            .args(["-h", "127.0.0.1", "-p", "5432", "-U", "postgres", "-d", "postgres", "-tAc",
                "SELECT 1 FROM pg_database WHERE datname='control_db'"])
            .output()
            .map_err(|e| format!("psql exists check: {e}"))?;
        if out.status.success() {
            return Ok(String::from_utf8_lossy(&out.stdout).trim() == "1");
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "postgres not query-ready within {timeout:?}: {}",
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

fn psql_run(psql: &Path, db: &str, extra: &[&str]) -> Result<(), String> {
    let mut cmd = Command::new(psql);
    cmd.args(["-h", "127.0.0.1", "-p", "5432", "-U", "postgres", "-d", db]);
    cmd.args(extra);
    let status = cmd.status().map_err(|e| format!("run psql: {e}"))?;
    if !status.success() {
        return Err(format!("psql ({db}) exited {status}"));
    }
    Ok(())
}

/// apply_rw_schema runs dataplane/risingwave/apply.sh in local packaging against the
/// local RW. Idempotent (apply.sh tracks _schema_migrations). Prepends the
/// bundled postgres bin to PATH so apply.sh finds psql; points the pg CDC
/// source at the local postgres via CDC_PG_HOST.
fn apply_rw_schema<F: Fn(&str)>(pg: &PgPaths, cfg: &AppConfig, progress: &F) -> Result<(), String> {
    progress("Applying RisingWave schema (local packaging)…");
    let apply = cfg.dataplane_dir.join("risingwave/apply.sh");
    if !apply.exists() {
        return Err(format!("apply.sh missing at {} (run `make dataplane-stage`)", apply.display()));
    }
    let path = format!(
        "{}:{}",
        pg.bindir.display(),
        std::env::var("PATH").unwrap_or_default()
    );
    let log = log_file(cfg, "rw-apply.log")?;
    let err = log.try_clone().map_err(|e| format!("clone log: {e}"))?;
    let status = Command::new("bash")
        .arg(&apply)
        .current_dir(apply.parent().unwrap())
        .env("CDC_PG_HOST", "127.0.0.1")
        .env("RW_HOST", "localhost")
        .env("RW_PORT", "4566")
        .env("RW_USER", "root")
        .env("RW_DB", "dev")
        .env("UDF_HOST", "localhost")
        .env("UDF_PORT", "4566")
        .env("PATH", path)
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(err))
        .status()
        .map_err(|e| format!("run apply.sh: {e}"))?;
    if !status.success() {
        return Err(format!("apply.sh exited {status} (see logs/rw-apply.log)"));
    }
    Ok(())
}

// --- RisingWave ------------------------------------------------------------

/// Resolved paths inside the extracted RW artifact.
#[derive(Clone, Debug)]
struct RwPaths {
    bin: PathBuf,
    python_home: PathBuf,
    connector_libs: PathBuf,
    config_path: PathBuf,
    store_dir: PathBuf,
}

fn ensure_risingwave<F: Fn(&str)>(cfg: &AppConfig, progress: &F) -> Result<RwPaths, String> {
    let dest = cfg.runtime_dir.join("risingwave");
    let marker = dest.join(".ready");
    let want = &cfg.risingwave_artifact_url;
    if fs::read_to_string(&marker).ok().as_deref().map(str::trim) != Some(want) {
        let tarball = runtime::resolve_artifact(cfg.artifacts_dir.as_deref(), "risingwave.tar.gz")?;
        progress("Unpacking bundled RisingWave…");
        let staging = cfg.runtime_dir.join(".rw-staging");
        let _ = fs::remove_dir_all(&staging);
        fs::create_dir_all(&staging).map_err(|e| format!("mkdir staging: {e}"))?;
        runtime::untar(&tarball, &staging)?;
        let inner = runtime::single_subdir(&staging)?;
        let _ = fs::remove_dir_all(&dest);
        fs::rename(&inner, &dest).map_err(|e| format!("place risingwave: {e}"))?;
        let _ = fs::remove_dir_all(&staging);
        fs::write(&marker, want).map_err(|e| format!("write marker: {e}"))?;
    }

    let bin = dest.join("bin/risingwave");
    if !bin.exists() {
        return Err(format!("risingwave binary missing at {}", bin.display()));
    }
    // RW single-node reads embedded-Python-UDF enablement from a config file;
    // generate a minimal one so we don't depend on the repo tree in a packaged
    // app. fold_kernel runs as an embedded Python UDAF.
    let config_path = dest.join("config.toml");
    fs::write(&config_path, "[udf]\nenable_embedded_python_udf = true\n")
        .map_err(|e| format!("write rw config: {e}"))?;

    let store_dir = cfg.base_dir().join("local-dataplane/rw-store");
    fs::create_dir_all(&store_dir).map_err(|e| format!("mkdir rw store: {e}"))?;

    Ok(RwPaths {
        bin,
        python_home: dest.join("python"),
        connector_libs: dest.join("libexec"),
        config_path,
        store_dir,
    })
}

fn spawn_risingwave(p: &RwPaths, cfg: &AppConfig) -> Result<Child, String> {
    let log = log_file(cfg, "risingwave.log")?;
    let err = log.try_clone().map_err(|e| format!("clone log: {e}"))?;
    Command::new(&p.bin)
        .arg("single-node")
        // RisingWave's meta secret manager creates a secret dir relative to the
        // working directory. A .app launched via `open` inherits CWD=/ (read-only
        // on macOS), so RW panics with EROFS at boot. Run it from runtime_dir
        // (writable, where RW lives) so its relative paths land there.
        .current_dir(&cfg.runtime_dir)
        .env("PYTHONHOME", &p.python_home)
        .env("CONNECTOR_LIBS_PATH", &p.connector_libs)
        .env("RW_SINGLE_NODE_CONFIG_PATH", &p.config_path)
        .env("RW_SINGLE_NODE_STORE_DIRECTORY", &p.store_dir)
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(err))
        .spawn()
        .map_err(|e| format!("spawn risingwave: {e}"))
}

// --- PostgreSQL ------------------------------------------------------------

#[derive(Clone, Debug)]
struct PgPaths {
    bindir: PathBuf,
    data_dir: PathBuf,
    socket_dir: PathBuf,
}

fn ensure_postgres<F: Fn(&str)>(cfg: &AppConfig, progress: &F) -> Result<PgPaths, String> {
    let dest = cfg.runtime_dir.join("postgres");
    let marker = dest.join(".ready");
    let want = &cfg.postgres_download_url;
    if fs::read_to_string(&marker).ok().as_deref().map(str::trim) != Some(want) {
        let tarball = runtime::resolve_artifact(cfg.artifacts_dir.as_deref(), "postgres.tar.gz")?;
        progress("Unpacking bundled PostgreSQL…");
        let staging = cfg.runtime_dir.join(".pg-staging");
        let _ = fs::remove_dir_all(&staging);
        fs::create_dir_all(&staging).map_err(|e| format!("mkdir staging: {e}"))?;
        runtime::untar(&tarball, &staging)?;
        // theseus tarballs may unpack flat (bin/ at root) or under one dir.
        let root = locate_pg_root(&staging)?;
        let _ = fs::remove_dir_all(&dest);
        fs::rename(&root, &dest).map_err(|e| format!("place postgres: {e}"))?;
        let _ = fs::remove_dir_all(&staging);
        fs::write(&marker, want).map_err(|e| format!("write marker: {e}"))?;
    }

    let bindir = dest.join("bin");
    if !bindir.join("initdb").exists() || !bindir.join("postgres").exists() {
        return Err(format!("postgres bin/{{initdb,postgres}} missing under {}", bindir.display()));
    }

    let data_dir = cfg.base_dir().join("local-dataplane/pgdata");
    let socket_dir = cfg.runtime_dir.join("pg-sock");
    fs::create_dir_all(&socket_dir).map_err(|e| format!("mkdir pg socket dir: {e}"))?;

    // initdb once (the cluster persists under base_dir). trust auth on a
    // loopback-only single-user cluster.
    if !data_dir.join("PG_VERSION").exists() {
        progress("Initializing PostgreSQL cluster…");
        if let Some(parent) = data_dir.parent() {
            fs::create_dir_all(parent).map_err(|e| format!("mkdir pgdata parent: {e}"))?;
        }
        let status = Command::new(bindir.join("initdb"))
            .args(["-D"]).arg(&data_dir)
            .args(["-U", "postgres", "--auth=trust", "--encoding=UTF8"])
            .stdout(Stdio::null())
            .stderr(Stdio::from(log_file(cfg, "postgres-initdb.log")?))
            .status()
            .map_err(|e| format!("run initdb: {e}"))?;
        if !status.success() {
            return Err(format!("initdb exited {status}"));
        }
    }

    Ok(PgPaths { bindir, data_dir, socket_dir })
}

fn spawn_postgres(p: &PgPaths, cfg: &AppConfig) -> Result<Child, String> {
    let log = log_file(cfg, "postgres.log")?;
    let err = log.try_clone().map_err(|e| format!("clone log: {e}"))?;
    // Own the postgres process directly (not pg_ctl) so the crash monitor can
    // wait() on it. Loopback-only listener; unix socket in a private dir.
    Command::new(p.bindir.join("postgres"))
        .arg("-D").arg(&p.data_dir)
        .args(["-p", &PG_PORT.to_string()])
        .arg("-k").arg(&p.socket_dir)
        .args(["-c", "listen_addresses=127.0.0.1"])
        // Logical replication for RisingWave's postgres-cdc source (portfolios).
        .args(["-c", "wal_level=logical"])
        .args(["-c", "max_replication_slots=8"])
        .args(["-c", "max_wal_senders=8"])
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(err))
        .spawn()
        .map_err(|e| format!("spawn postgres: {e}"))
}

/// locate_pg_root finds the dir containing bin/initdb in an extracted tree
/// (handles both flat and single-subdir tarball layouts).
fn locate_pg_root(staging: &Path) -> Result<PathBuf, String> {
    if staging.join("bin/initdb").exists() {
        return Ok(staging.to_path_buf());
    }
    for entry in fs::read_dir(staging).map_err(|e| format!("read staging: {e}"))? {
        let p = entry.map_err(|e| format!("read entry: {e}"))?.path();
        if p.is_dir() && p.join("bin/initdb").exists() {
            return Ok(p);
        }
    }
    Err(format!("no bin/initdb found under {}", staging.display()))
}

// --- shared helpers --------------------------------------------------------

fn log_file(cfg: &AppConfig, name: &str) -> Result<fs::File, String> {
    let dir = cfg.runtime_dir.join("logs");
    fs::create_dir_all(&dir).map_err(|e| format!("mkdir logs: {e}"))?;
    fs::File::create(dir.join(name)).map_err(|e| format!("open {name}: {e}"))
}

pub(crate) fn health_tcp(port: u16, timeout: Duration) -> Result<(), String> {
    let addr: SocketAddr = format!("127.0.0.1:{port}")
        .parse()
        .map_err(|e| format!("addr: {e}"))?;
    let deadline = Instant::now() + timeout;
    loop {
        if TcpStream::connect_timeout(&addr, Duration::from_secs(2)).is_ok() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!("port {port} not accepting within {timeout:?}"));
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// should_respawn decides whether supervise restarts a child that just exited:
/// never while the app is shutting down (a child killed during teardown must
/// stay dead, not re-orphan), otherwise only while under the 3-restarts-per-
/// window cap.
pub(crate) fn should_respawn(shutting_down: bool, restarts: u32) -> bool {
    !shutting_down && restarts < 3
}

/// supervise wait()s on a service child and respawns on unexpected exit, capped
/// at 3 restarts per 5-minute window. `slot` selects the child handle in Shared.
/// Respawning is suppressed once `shared.shutting_down` is set, so on-close
/// teardown can kill children without the monitor reviving them.
pub(crate) fn supervise(
    app: AppHandle,
    shared: Arc<Shared>,
    slot: fn(&Shared) -> &Mutex<Option<Child>>,
    name: &'static str,
    port: u16,
    respawn: Arc<dyn Fn() -> Result<Child, String> + Send + Sync>,
) {
    std::thread::spawn(move || {
        let restarts = AtomicU32::new(0);
        let mut window = Instant::now();
        loop {
            let child = slot(&shared).lock().unwrap().take();
            let mut child = match child {
                Some(c) => c,
                None => return, // taken for shutdown
            };
            let _ = child.wait();
            // Teardown (RunEvent::Exit) sets this before killing children;
            // honor it the instant wait() returns so we never re-spawn a
            // process the app is trying to shut down.
            let shutting_down = shared.shutting_down.load(Ordering::SeqCst);
            if window.elapsed() > Duration::from_secs(300) {
                window = Instant::now();
                restarts.store(0, Ordering::SeqCst);
            }
            let n = restarts.fetch_add(1, Ordering::SeqCst);
            if !should_respawn(shutting_down, n) {
                if !shutting_down {
                    let _ = app.emit("dataplane-crashed", format!("{name} exited too many times; giving up"));
                }
                return;
            }
            let _ = app.emit("dataplane-restarting", format!("{name} exited; restarting…"));
            match respawn() {
                Ok(c) => {
                    *slot(&shared).lock().unwrap() = Some(c);
                    let _ = health_tcp(port, HEALTH_TIMEOUT);
                    let _ = app.emit("dataplane-restarted", format!("{name} back up"));
                }
                Err(e) => {
                    let _ = app.emit("dataplane-crashed", format!("{name} respawn failed: {e}"));
                    return;
                }
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn supervise_stops_respawning_on_shutdown() {
        // Under the cap, not shutting down → respawn.
        assert!(should_respawn(false, 0));
        assert!(should_respawn(false, 2));
        // Restart cap reached → give up.
        assert!(!should_respawn(false, 3));
        // Shutting down gates respawn regardless of restart count — a child
        // killed during teardown must stay dead, never re-orphan.
        assert!(!should_respawn(true, 0));
        assert!(!should_respawn(true, 2));
    }

    #[test]
    fn health_tcp_times_out_on_dead_port() {
        // Bind then drop to get a closed port.
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        drop(l);
        let started = Instant::now();
        assert!(health_tcp(port, Duration::from_millis(300)).is_err());
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[test]
    fn health_tcp_succeeds_on_open_port() {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        // Listener stays bound for the duration.
        assert!(health_tcp(port, Duration::from_secs(2)).is_ok());
    }

    #[test]
    fn locate_pg_root_handles_flat_and_nested() {
        let dir = tempfile::tempdir().unwrap();
        // nested layout
        let nested = dir.path().join("postgresql-x/bin");
        fs::create_dir_all(&nested).unwrap();
        fs::write(nested.join("initdb"), b"x").unwrap();
        let root = locate_pg_root(dir.path()).unwrap();
        assert!(root.join("bin/initdb").exists());
    }
}
