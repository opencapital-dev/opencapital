// Compute sidecar lifecycle for the desktop shell: resolve the bundled
// `compute` binary (Tauri externalBin, placed next to the app executable),
// spawn it on a free loopback port, health-poll until ready, and keep it
// alive (bounded crash restart). The sidecar URL is published to Shared so
// instance-bootstrap can stamp it into every plugin's jsonData — Grafana
// sanitizes the plugin subprocess env, so this must travel as data.

use std::fs;
use std::net::TcpListener;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use tauri::{AppHandle, Emitter};

use crate::config::AppConfig;
use crate::proxy::Shared;

/// Generous cold-start budget: the one-file PyInstaller bundle self-extracts
/// (~tens of MB) on first run before it binds the port — several seconds. Mirror
/// grafana's generous health timeout rather than a few hundred ms.
const HEALTH_TIMEOUT: Duration = Duration::from_secs(60);

/// SpawnSpec is everything a (re)spawn of the compute sidecar needs. Cloned
/// into the crash monitor so it can respawn with identical config.
#[derive(Clone)]
struct SpawnSpec {
    bin: PathBuf,
    host: String,
    port: u16,
    risingwave_dsn: String,
    log_path: PathBuf,
}

/// start resolves the bundled compute binary, picks a free loopback port,
/// spawns + health-polls the sidecar, publishes its URL to Shared (for the
/// plugin data channel), and starts the crash monitor. Blocking; call from
/// the same spawn_blocking launch flow grafana uses, before reconcile so the
/// URL is available when instance-bootstrap renders plugin jsonData.
pub fn start(app: &AppHandle, cfg: &AppConfig, shared: &Arc<Shared>) -> Result<u16, String> {
    let bin = resolve_compute_bin()?;
    let port = free_port()?;
    let log_path = cfg.runtime_dir.join("compute.log");
    fs::create_dir_all(&cfg.runtime_dir).map_err(|e| format!("mkdir runtime dir: {e}"))?;

    let spec = SpawnSpec {
        bin,
        host: "127.0.0.1".into(),
        port,
        risingwave_dsn: cfg.risingwave_dsn.clone(),
        log_path,
    };

    let child = spawn_compute(&spec)?;
    *shared.compute_child.lock().unwrap() = Some(child);
    *shared.compute_port.lock().unwrap() = Some(port);

    health_poll(port, HEALTH_TIMEOUT)?;

    start_crash_monitor(app.clone(), shared.clone(), spec, port);
    Ok(port)
}

/// resolve_compute_bin locates the `compute` sidecar. Tauri places an
/// externalBin next to the main app executable (both in `tauri dev` and in a
/// bundle), so we resolve it relative to current_exe()'s dir — mirroring how
/// grafana.rs resolves a path and spawns via std::process::Command rather than
/// pulling in the shell plugin. The on-disk name is `compute` (the bundler
/// strips the `-<triple>` suffix when staging next to the binary); fall back to
/// the suffixed name for unbundled dev layouts where the stager left it as
/// `compute-<triple>`.
fn resolve_compute_bin() -> Result<PathBuf, String> {
    let exe = std::env::current_exe().map_err(|e| format!("current_exe: {e}"))?;
    let dir = exe
        .parent()
        .ok_or("exe has no parent dir")?
        .to_path_buf();

    let plain = dir.join("compute");
    if plain.exists() {
        return Ok(plain);
    }
    let triple = host_target_triple();
    let suffixed = dir.join(format!("compute-{triple}"));
    if suffixed.exists() {
        return Ok(suffixed);
    }
    Err(format!(
        "compute sidecar not found next to {} (looked for `compute` and `compute-{triple}`; run `make compute-sidecar-stage`)",
        exe.display(),
    ))
}

/// host_target_triple returns the compile-time target triple, matching the
/// `compute-<triple>` name Tauri's externalBin staging produces.
fn host_target_triple() -> &'static str {
    env!("TARGET_TRIPLE")
}

fn spawn_compute(spec: &SpawnSpec) -> Result<Child, String> {
    let log = fs::File::create(&spec.log_path).map_err(|e| format!("open compute log: {e}"))?;
    let log_err = log.try_clone().map_err(|e| format!("clone log handle: {e}"))?;
    let mut cmd = Command::new(&spec.bin);
    cmd.env("COMPUTE_HOST", &spec.host)
        .env("COMPUTE_PORT", spec.port.to_string())
        .env("RISINGWAVE_DSN", &spec.risingwave_dsn)
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(log_err));
    cmd.spawn().map_err(|e| format!("spawn compute sidecar: {e}"))
}

fn health_poll(port: u16, timeout: Duration) -> Result<(), String> {
    let url = format!("http://127.0.0.1:{}/health", port);
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
            return Err(format!("compute sidecar not healthy within {:?}", timeout));
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// start_crash_monitor waits on the compute child and restarts it on
/// unexpected exit, capped at 3 restarts per 5-minute window.
fn start_crash_monitor(app: AppHandle, shared: Arc<Shared>, spec: SpawnSpec, port: u16) {
    std::thread::spawn(move || {
        let restarts = AtomicU32::new(0);
        let mut window_start = Instant::now();
        loop {
            let child = shared.compute_child.lock().unwrap().take();
            let mut child = match child {
                Some(c) => c,
                None => return, // shut down elsewhere
            };
            let _ = child.wait();
            if window_start.elapsed() > Duration::from_secs(300) {
                window_start = Instant::now();
                restarts.store(0, Ordering::SeqCst);
            }
            if restarts.fetch_add(1, Ordering::SeqCst) >= 3 {
                let _ = app.emit("compute-crashed", "compute exited too many times; giving up");
                *shared.compute_port.lock().unwrap() = None;
                return;
            }
            let _ = app.emit("compute-restarting", "compute exited; restarting…");
            match spawn_compute(&spec) {
                Ok(c) => {
                    *shared.compute_child.lock().unwrap() = Some(c);
                    let _ = health_poll(port, HEALTH_TIMEOUT);
                    let _ = app.emit("compute-restarted", "compute back up");
                }
                Err(e) => {
                    let _ = app.emit("compute-crashed", format!("respawn failed: {e}"));
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
    use super::*;

    #[test]
    fn free_port_returns_distinct_bindable_ports() {
        let a = free_port().unwrap();
        let b = free_port().unwrap();
        assert_ne!(a, 0);
        assert_ne!(b, 0);
        // Both must be bindable right after handing them out.
        TcpListener::bind(("127.0.0.1", a)).unwrap();
        TcpListener::bind(("127.0.0.1", b)).unwrap();
    }

    #[test]
    fn host_target_triple_is_non_empty() {
        assert!(!host_target_triple().is_empty());
    }

    #[test]
    fn health_poll_times_out_on_dead_port() {
        // Bind then drop to get a port nothing is listening on.
        let port = free_port().unwrap();
        let started = Instant::now();
        let r = health_poll(port, Duration::from_millis(300));
        assert!(r.is_err());
        // Returns at the deadline, not hangs forever.
        assert!(started.elapsed() < Duration::from_secs(5));
    }
}
