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
fn kill_stray_dataplane() {
    #[cfg(not(windows))]
    for pat in [
        "/.opencapital/runtime/risingwave/bin/risingwave",
        "/.opencapital/runtime/postgres/bin/postgres",
        "/.opencapital/runtime/services/control-plane",
        "/.opencapital/runtime/services/gateway",
        "/.opencapital/runtime/services/read-gateway",
    ] {
        let _ = std::process::Command::new("pkill").args(["-f", pat]).status();
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
            kinde::install_plugin,
            kinde::plugin_versions,
            kinde::uninstall_plugin,
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
