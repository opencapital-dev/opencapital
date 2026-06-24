// Windows data-plane lifecycle. RisingWave has no Windows binary, so on Windows
// the whole headless plane runs inside ONE bundled WSL2 distro (the host runs
// Grafana + plugins natively, as on macOS). The shell drives the WSL lifecycle
// the same way dataplane.rs drives native processes on macOS: import the bundled
// rootfs on first run, start the in-distro supervisor, health-check, and reach
// the services over `localhost` (WSL2 NAT forwards host -> distro for
// 0.0.0.0-bound listeners). The in-distro half lives in dataplane/wsl/supervisor.sh;
// the rootfs is built by .github/workflows/wsl-rootfs-artifact.yml.
//
// State (pgdata, RW store) lives under /data inside the distro's ext4, so a
// binary update that re-pushes /opt/opencapital never wipes it. A dedicated
// persistent volume + the re-import update path are Phase 6.
//
// The pure parsers (wsl.exe emits UTF-16LE) are compiled + unit-tested on every
// platform; the functions that shell out to wsl.exe are Windows-only.

/// decode_wsl_output turns raw `wsl.exe` stdout into a String. Piped wsl output
/// is UTF-16LE (with interleaved NULs); detect that by the NUL bytes and decode
/// accordingly, else fall back to UTF-8 (newer wsl / non-wsl callers).
#[cfg_attr(not(windows), allow(dead_code))] // used by windows_impl + tests
pub(crate) fn decode_wsl_output(bytes: &[u8]) -> String {
    if bytes.iter().any(|&b| b == 0) {
        let u16s: Vec<u16> = bytes
            .chunks_exact(2)
            .map(|c| u16::from_le_bytes([c[0], c[1]]))
            .collect();
        String::from_utf16_lossy(&u16s)
    } else {
        String::from_utf8_lossy(bytes).into_owned()
    }
}

/// distro_listed reports whether `name` appears in decoded `wsl -l -q` output.
/// Lines may carry a BOM, a trailing CR, or stray NULs from the UTF-16 decode.
#[cfg_attr(not(windows), allow(dead_code))] // used by windows_impl + tests
pub(crate) fn distro_listed(decoded: &str, name: &str) -> bool {
    decoded.lines().any(|l| {
        let t = l.trim().trim_start_matches('\u{feff}').trim_matches('\0').trim();
        t.eq_ignore_ascii_case(name)
    })
}

#[cfg(windows)]
pub(crate) use windows_impl::bring_up;
#[cfg(windows)]
pub(crate) use windows_impl::terminate_stray;

#[cfg(windows)]
mod windows_impl {
    use std::fs;
    use std::os::windows::process::CommandExt;
    use std::process::{Child, Command, Stdio};
    use std::sync::Arc;

    use tauri::{AppHandle, Emitter};

    use crate::config::AppConfig;
    use crate::dataplane::{health_tcp, supervise, HEALTH_TIMEOUT, RW_PORT};
    use crate::proxy::Shared;
    use crate::runtime;

    /// The app's private, dedicated distro name (quiet-listed by `wsl -l -q`).
    const DISTRO: &str = "opencapital";
    /// CREATE_NO_WINDOW — don't flash a console window for each wsl.exe call.
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;

    fn wsl() -> Command {
        let mut c = Command::new("wsl.exe");
        c.creation_flags(CREATE_NO_WINDOW);
        c
    }

    pub(crate) fn bring_up(app: &AppHandle, cfg: &AppConfig, shared: &Arc<Shared>) -> Result<(), String> {
        fs::create_dir_all(&cfg.runtime_dir).map_err(|e| format!("mkdir runtime dir: {e}"))?;
        let progress = |m: &str| {
            let _ = app.emit("dataplane-progress", m.to_string());
        };

        ensure_wsl()?;
        ensure_distro(cfg, &progress)?;

        progress("Starting in-distro data plane…");
        *shared.wsl_child.lock().unwrap() = Some(spawn_supervisor(cfg)?);

        // The supervisor brings services up in order; health-check RisingWave
        // on its host-reachable loopback port (WSL2 NAT forwards localhost).
        // control-plane has been removed; portfolios DDL is now applied by
        // bootstrap_control_db in dataplane.rs.
        health_tcp(RW_PORT, HEALTH_TIMEOUT).map_err(|e| format!("risingwave: {e}"))?;

        let c = cfg.clone();
        supervise(app.clone(), shared.clone(), |s| &s.wsl_child, "wsl-dataplane", RW_PORT,
            Arc::new(move || spawn_supervisor(&c)));
        Ok(())
    }

    /// ensure_wsl verifies WSL is installed + usable. MSI bootstrap (enabling
    /// VirtualMachinePlatform, installing the WSL MSI, the one first-run reboot)
    /// is Phase 6 (the signed installer); here we detect and surface the
    /// unbypassable failures with an actionable error.
    fn ensure_wsl() -> Result<(), String> {
        let out = wsl().arg("--status").output()
            .map_err(|e| format!("wsl.exe not found ({e}); install WSL2 (wsl --install)"))?;
        if !out.status.success() {
            let msg = decode_output(&out.stdout, &out.stderr);
            return Err(format!(
                "WSL not ready: {msg}. Enable virtualization in firmware + run `wsl --install`, then relaunch."
            ));
        }
        Ok(())
    }

    /// ensure_distro imports the bundled rootfs as the `opencapital` distro on
    /// first run (distro presence is the marker — no re-download once imported).
    fn ensure_distro<F: Fn(&str)>(cfg: &AppConfig, progress: &F) -> Result<(), String> {
        let listed = wsl().args(["-l", "-q"]).output()
            .map_err(|e| format!("wsl -l -q: {e}"))?;
        if super::distro_listed(&super::decode_wsl_output(&listed.stdout), DISTRO) {
            return Ok(());
        }

        progress("Downloading data-plane distro (first launch only)…");
        let tarball = runtime::download(&cfg.wsl_rootfs_url, &cfg.runtime_dir, "wsl-rootfs.tar.gz")?;
        let install_dir = cfg.runtime_dir.join("wsl-distro");
        fs::create_dir_all(&install_dir).map_err(|e| format!("mkdir distro dir: {e}"))?;

        progress("Importing data-plane distro…");
        let out = wsl()
            .arg("--import").arg(DISTRO).arg(&install_dir).arg(&tarball)
            .args(["--version", "2"])
            .output()
            .map_err(|e| format!("wsl --import: {e}"))?;
        if !out.status.success() {
            return Err(format!("wsl --import failed: {}", decode_output(&out.stdout, &out.stderr)));
        }
        let _ = fs::remove_file(&tarball);
        Ok(())
    }

    /// spawn_supervisor runs the in-distro supervisor as the `dataplane` user.
    /// GHCR config is forwarded via WSLENV for future plugin-registry use.
    /// The child is long-lived — it blocks on `wait` inside the distro keeping
    /// services alive — so the crash monitor wait()s on it like any other
    /// data-plane child.
    fn spawn_supervisor(cfg: &AppConfig) -> Result<Child, String> {
        let log = log_file(cfg, "wsl-supervisor.log")?;
        let err = log.try_clone().map_err(|e| format!("clone log: {e}"))?;

        let mut cmd = wsl();
        cmd.args(["-d", DISTRO, "-u", "dataplane", "--", "/opt/opencapital/supervisor.sh"])
            .env("WSLENV", "REGISTRY_OWNER/u:REGISTRY_PASSWORD/u")
            .env("REGISTRY_OWNER", "opencapital-dev");
        if let Ok(tok) = std::env::var("REGISTRY_PASSWORD") {
            cmd.env("REGISTRY_PASSWORD", tok);
        }
        cmd.stdout(Stdio::from(log))
            .stderr(Stdio::from(err))
            .spawn()
            .map_err(|e| format!("spawn wsl supervisor: {e}"))
    }

    /// terminate_stray stops a distro left running by a prior app run (the WSL
    /// analog of kill_stray_dataplane on macOS). Best-effort.
    pub(crate) fn terminate_stray() {
        let _ = wsl().args(["--terminate", DISTRO]).creation_flags(CREATE_NO_WINDOW).status();
    }

    fn decode_output(stdout: &[u8], stderr: &[u8]) -> String {
        let s = super::decode_wsl_output(stdout);
        let e = super::decode_wsl_output(stderr);
        let joined = format!("{} {}", s.trim(), e.trim());
        joined.trim().to_string()
    }

    fn log_file(cfg: &AppConfig, name: &str) -> Result<fs::File, String> {
        let dir = cfg.runtime_dir.join("logs");
        fs::create_dir_all(&dir).map_err(|e| format!("mkdir logs: {e}"))?;
        fs::File::create(dir.join(name)).map_err(|e| format!("open {name}: {e}"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decode_utf16le_wsl_output() {
        // "opencapital\n" as UTF-16LE (what piped wsl.exe emits).
        let s = "opencapital\n";
        let mut bytes = Vec::new();
        for u in s.encode_utf16() {
            bytes.extend_from_slice(&u.to_le_bytes());
        }
        assert_eq!(decode_wsl_output(&bytes).trim(), "opencapital");
    }

    #[test]
    fn decode_utf8_passthrough() {
        assert_eq!(decode_wsl_output(b"Ubuntu\n").trim(), "Ubuntu");
    }

    #[test]
    fn distro_listed_finds_among_lines() {
        let decoded = "Ubuntu\r\nopencapital\r\ndocker-desktop\r\n";
        assert!(distro_listed(decoded, "opencapital"));
        assert!(distro_listed(decoded, "OpenCapital")); // case-insensitive
    }

    #[test]
    fn distro_listed_false_when_absent() {
        assert!(!distro_listed("Ubuntu\r\nkali-linux\r\n", "opencapital"));
    }

    #[test]
    fn distro_listed_tolerates_bom_and_nuls() {
        // BOM + stray NUL that survive a UTF-16 decode of a single-line list.
        let decoded = "\u{feff}opencapital\u{0}";
        assert!(distro_listed(decoded, "opencapital"));
    }
}
