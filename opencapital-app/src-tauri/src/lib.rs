mod catalog;
mod compute;
mod config;
mod dataplane;
mod grafana;
mod kinde;
mod proxy;
pub mod reconcile;
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
/// SIGKILLs the app but orphans its compute child).
///
/// Two names to match: under `tauri dev` the externalBin runs as
/// `compute-<triple>`, but in the BUNDLED app Tauri strips the triple and the
/// sidecar runs as `…/Contents/MacOS/compute`. Matching only the dev name left
/// the bundled sidecar (the one users run) orphaned on every exit.
fn kill_stray_compute() {
    let triple = format!("compute-{}", env!("TARGET_TRIPLE"));
    for pat in [triple.as_str(), "Contents/MacOS/compute"] {
        let _ = std::process::Command::new("pkill").args(["-f", pat]).status();
    }
}

/// kill_stray_dataplane best-effort terminates a postgres/risingwave left over
/// from a prior run (a `tauri dev` rebuild SIGKILLs the app but orphans its
/// data-plane children, which then hold the fixed ports 5432/4566).
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
    }
    // Windows: the whole plane is one WSL distro — terminate it wholesale.
    #[cfg(windows)]
    wsl::terminate_stray();
}

/// kill_stray_plugins best-effort terminates Grafana plugin backends (gpx_*)
/// the app launched. Grafana reaps these itself on a clean stop, but if it was
/// orphaned they linger; this is the teardown/startup backstop. On Windows the
/// backends run inside the WSL distro, reaped by kill_stray_dataplane.
#[cfg(not(windows))]
fn kill_stray_plugins() {
    for pat in [
        "/.opencapital/runtime/plugin-cache/",
        "/.opencapital/instance/plugins/",
    ] {
        let _ = std::process::Command::new("pkill").args(["-f", pat]).status();
    }
}
#[cfg(windows)]
fn kill_stray_plugins() {}

/// shutdown_children tears down every process the app spawned, on app exit.
/// Sets `shutting_down` FIRST so the crash monitors don't revive anything,
/// then kills the data-plane + Grafana + sidecars + plugin backends. Grafana
/// is signalled before the plugin backstop so it can stop its own plugins
/// gracefully (pkill sends SIGTERM).
fn shutdown_children(shared: &Shared) {
    shared
        .shutting_down
        .store(true, std::sync::atomic::Ordering::SeqCst);
    kill_stray_grafana();
    kill_stray_plugins();
    kill_stray_compute();
    kill_stray_dataplane();
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // Backstop for an UNCLEAN prior exit (SIGKILL, crash, force-quit, a
    // `tauri dev` rebuild) where RunEvent::Exit never ran — and to free the
    // fixed ports 5432/4566 before re-spawning. The graceful path is the
    // RunEvent::Exit teardown below.
    kill_stray_grafana();
    kill_stray_compute();
    kill_stray_dataplane();
    let app = tauri::Builder::default()
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
            kinde::me_profile,
            kinde::logout,
            kinde::instance_token,
            kinde::marketplace_catalog,
            kinde::list_sources,
            kinde::add_source,
            kinde::remove_source,
            kinde::plugin_versions,
            kinde::get_plugin_selection,
            kinde::set_plugin_selection,
            kinde::seed_plugin_selection,
            kinde::get_plugin_pin,
            kinde::set_plugin_pin,
            grafana::launch_grafana,
            grafana::grafana_running,
        ])
        .build(tauri::generate_context!())
        .expect("error while building tauri application");

    // Reap every spawned child on quit. RunEvent::Exit fires once, on the way
    // out, for a graceful quit (Cmd+Q / app quit). Ungraceful kills can't run
    // this — the startup kill_stray_* above is their backstop.
    app.run(|app_handle, event| {
        if let tauri::RunEvent::Exit = event {
            if let Some(shared) = app_handle.try_state::<Arc<Shared>>() {
                shutdown_children(shared.inner());
            }
        }
    });
}
