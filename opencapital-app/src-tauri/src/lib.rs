mod compute;
mod config;
mod dataplane;
mod grafana;
mod kinde;
mod proxy;
mod runtime;
mod wsl;

use config::AppConfig;
use kinde::Session;
use proxy::Shared;
use std::sync::Arc;
use tauri::Manager;

/// kill_stray_grafana best-effort terminates any grafana-server left over
/// from a prior run of this shell (e.g. a `tauri dev` rebuild SIGKILLs the
/// app but orphans its grafana child). Without this, orphaned instances pile
/// up and the loopback proxy can hit a stale/dead upstream.
pub(crate) fn kill_stray_grafana() {
    let _ = std::process::Command::new("pkill")
        .args(["-f", "/.opencapital/runtime/grafana/bin/grafana server"])
        .status();
}

/// kill_stray_compute best-effort terminates any compute sidecar orphaned by a
/// prior run of this shell (same hazard as grafana: a `tauri dev` rebuild
/// SIGKILLs the app but orphans its compute child). The sidecar is the
/// externalBin staged next to the app executable as `compute-<triple>`.
fn kill_stray_compute() {
    let pat = format!("compute-{}", env!("TARGET_TRIPLE"));
    let _ = std::process::Command::new("pkill").args(["-f", &pat]).status();
}

/// kill_stray_dataplane best-effort terminates a postgres/risingwave left over
/// from a prior run (a `tauri dev` rebuild SIGKILLs the app but orphans its
/// data-plane children, which then hold the fixed ports 5432/4566).
///
/// Go service sidecars (control-plane, gateway, read-gateway) live next to the
/// app exe, so their path differs between a local dev build, a downloaded .app,
/// and any previous release. Path-based pkill only kills processes from the
/// SAME exe directory. We complement it with port-based kills so that a stale
/// sidecar from ANY prior app version is cleared before we try to bind the same
/// fixed ports — avoiding the scenario where the old control-plane (on port
/// 18080) has a dead postgres and serves 500s to the new session.
fn kill_stray_dataplane() {
    #[cfg(not(windows))]
    {
        // Postgres + RisingWave still run from runtime_dir (downloaded artifacts).
        for pat in [
            "/.opencapital/runtime/risingwave/bin/risingwave",
            "/.opencapital/runtime/postgres/bin/postgres",
        ] {
            let _ = std::process::Command::new("pkill").args(["-f", pat]).status();
        }
        // Go service sidecars: kill by fixed port so we catch orphans from any
        // app instance (local dev, downloaded release, previous version).
        for port in [
            dataplane::CP_PORT,
            dataplane::GW_PORT,
            dataplane::RG_PORT,
        ] {
            let _ = std::process::Command::new("sh")
                .args([
                    "-c",
                    &format!("lsof -ti tcp:{port} 2>/dev/null | xargs kill 2>/dev/null"),
                ])
                .status();
        }
        // Also kill from this exe's dir to catch the triple-suffixed dev layout
        // (`control-plane-aarch64-apple-darwin`) that lsof might miss if the
        // process hasn't bound its port yet.
        if let Ok(exe) = std::env::current_exe() {
            if let Some(dir) = exe.parent() {
                for name in ["control-plane", "gateway", "read-gateway"] {
                    let pat = format!("{}/{}", dir.display(), name);
                    let _ = std::process::Command::new("pkill").args(["-f", &pat]).status();
                }
            }
        }
    }
    // Windows: the whole plane is one WSL distro — terminate it wholesale.
    #[cfg(windows)]
    wsl::terminate_stray();
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    kill_stray_grafana();
    kill_stray_compute();
    kill_stray_dataplane();
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_process::init())
        .manage(Session::default())
        .manage(Shared::new())
        .setup(|app| {
            let resource_dir = app.path().resource_dir().ok();
            let cfg = AppConfig::load(resource_dir.as_deref());
            // Bring up the local data plane in the background when enabled, so
            // it is warming while the user logs in (no-op in thin-client mode).
            let shared = app.state::<Arc<Shared>>().inner().clone();
            dataplane::start(app.handle().clone(), cfg.clone(), shared);
            app.manage(cfg);
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            kinde::kinde_login,
            kinde::me_orgs,
            kinde::me_profile,
            kinde::logout,
            kinde::create_org,
            kinde::instance_token,
            kinde::marketplace_catalog,
            kinde::list_sources,
            kinde::add_source,
            kinde::remove_source,
            kinde::plugin_versions,
            kinde::get_plugin_selection,
            kinde::set_plugin_selection,
            kinde::seed_plugin_selection,
            kinde::get_show_preview,
            kinde::set_show_preview,
            kinde::get_plugin_pin,
            kinde::set_plugin_pin,
            grafana::launch_grafana,
            grafana::grafana_running,
        ])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
