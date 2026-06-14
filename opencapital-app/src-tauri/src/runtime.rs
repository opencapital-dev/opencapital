// Grafana runtime: ensure the host binaries the desktop shell drives are
// present. Under v8 there are no baked plugins and no baked Grafana — the
// shell downloads vanilla grafana-server + the numbat CLI on first launch
// (the reconciler installs plugins separately). Everything lives under
// RUNTIME_DIR, content-checked by a marker file so re-launches are instant.
//
// Extraction shells out to the system `tar` (macOS/Linux ship it); the v0
// desktop target is macOS/Linux. This is the shell managing its own
// runtime, not build glue.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use crate::config::AppConfig;

/// Vanilla grafana version the shell downloads. Stored in the `.ready` marker;
/// a mismatch forces a re-download (e.g. after bumping this). Keep in sync with
/// the version in `config::default_grafana_url`.
const GRAFANA_VERSION: &str = "13.0.2";

/// Resolved paths to the runtime binaries grafana needs.
#[derive(Clone, Debug)]
pub struct RuntimePaths {
    /// Grafana home (contains conf/, public/, bin/) passed as --homepath.
    pub grafana_homepath: PathBuf,
    /// grafana-server binary (or `grafana` with the `server` subcommand).
    pub grafana_bin: PathBuf,
    /// True when grafana_bin is the unified `grafana` binary needing a
    /// leading `server` subcommand argument.
    pub grafana_needs_server_subcmd: bool,
    /// numbat CLI the core-datasource datasource shells out to.
    pub numbat_bin: PathBuf,
    /// numbat modules dir (NUMBAT_PATH) the CLI loads its stdlib from.
    pub numbat_modules: PathBuf,
}

/// ensure downloads + extracts grafana-server and numbat if missing, then
/// returns the resolved paths. Blocking; call via spawn_blocking.
pub fn ensure<F: Fn(&str)>(cfg: &AppConfig, progress: F) -> Result<RuntimePaths, String> {
    fs::create_dir_all(&cfg.runtime_dir).map_err(|e| format!("mkdir runtime dir: {e}"))?;

    let grafana_home = ensure_grafana(cfg, &progress)?;
    let (grafana_bin, needs_subcmd) = resolve_grafana_bin(&grafana_home)?;
    let (numbat_bin, numbat_modules) = ensure_numbat(cfg, &progress)?;

    Ok(RuntimePaths {
        grafana_homepath: grafana_home,
        grafana_bin,
        grafana_needs_server_subcmd: needs_subcmd,
        numbat_bin,
        numbat_modules,
    })
}

fn ensure_grafana<F: Fn(&str)>(cfg: &AppConfig, progress: &F) -> Result<PathBuf, String> {
    let dest = cfg.runtime_dir.join("grafana");
    let marker = dest.join(".ready");
    if fs::read_to_string(&marker).ok().as_deref().map(str::trim) == Some(GRAFANA_VERSION) {
        return Ok(dest);
    }
    let art = resolve_artifact(
        cfg.artifacts_dir.as_deref(),
        "grafana.tar.gz",
        &cfg.grafana_download_url,
        &cfg.runtime_dir,
    )?;
    progress(if art.owned {
        "Downloading grafana-server (first launch only)…"
    } else {
        "Unpacking bundled grafana-server…"
    });
    let staging = cfg.runtime_dir.join(".grafana-staging");
    let _ = fs::remove_dir_all(&staging);
    fs::create_dir_all(&staging).map_err(|e| format!("mkdir staging: {e}"))?;
    untar(&art.path, &staging)?;
    if art.owned {
        let _ = fs::remove_file(&art.path);
    }

    // The tarball extracts to a single grafana-<ver> dir; move it to dest.
    let inner = single_subdir(&staging)?;
    let _ = fs::remove_dir_all(&dest);
    fs::rename(&inner, &dest).map_err(|e| format!("place grafana: {e}"))?;
    let _ = fs::remove_dir_all(&staging);
    fs::write(&marker, GRAFANA_VERSION).map_err(|e| format!("write marker: {e}"))?;
    Ok(dest)
}

/// overlay_grafana replaces the grafana home's `public/build` + `public/views`
/// with our customized frontend (the bundled `grafana-public` app resource).
/// The nav customization is frontend-only, so this is all that differs from
/// vanilla. Idempotent + cheap: gated on a `.overlay` marker holding the
/// overlay version (`grafana-public/.overlay-version`), so it re-applies only
/// when the app ships a new UI, not on every launch.
pub fn overlay_grafana(grafana_home: &Path, overlay_src: &Path) -> Result<(), String> {
    let version = fs::read_to_string(overlay_src.join(".overlay-version"))
        .map_err(|e| format!("read overlay version: {e}"))?;
    let version = version.trim();

    let marker = grafana_home.join(".overlay");
    if fs::read_to_string(&marker).ok().as_deref().map(str::trim) == Some(version) {
        return Ok(());
    }

    let public = grafana_home.join("public");
    for sub in ["build", "views"] {
        let src = overlay_src.join(sub);
        let dst = public.join(sub);
        let _ = fs::remove_dir_all(&dst);
        copy_dir(&src, &dst)?;
    }
    fs::write(&marker, version).map_err(|e| format!("write overlay marker: {e}"))?;
    Ok(())
}

/// copy_dir recursively copies src -> dst (dst must not exist). Shells out to
/// the system `cp -R` (macOS/Linux), matching how this module untars.
fn copy_dir(src: &Path, dst: &Path) -> Result<(), String> {
    if let Some(parent) = dst.parent() {
        fs::create_dir_all(parent).map_err(|e| format!("mkdir {}: {e}", parent.display()))?;
    }
    let status = Command::new("cp")
        .arg("-R")
        .arg(src)
        .arg(dst)
        .status()
        .map_err(|e| format!("run cp: {e}"))?;
    if !status.success() {
        return Err(format!("cp -R {} {}: exited {status}", src.display(), dst.display()));
    }
    Ok(())
}

fn resolve_grafana_bin(home: &Path) -> Result<(PathBuf, bool), String> {
    let server = home.join("bin/grafana-server");
    if server.exists() {
        return Ok((server, false));
    }
    let unified = home.join("bin/grafana");
    if unified.exists() {
        return Ok((unified, true));
    }
    Err(format!("no grafana binary under {}/bin", home.display()))
}

fn ensure_numbat<F: Fn(&str)>(cfg: &AppConfig, progress: &F) -> Result<(PathBuf, PathBuf), String> {
    let dest = cfg.runtime_dir.join("numbat");
    let bin = dest.join("numbat");
    let modules = dest.join("modules");
    if bin.exists() && modules.exists() {
        return Ok((bin, modules));
    }
    progress("Downloading numbat CLI (first launch only)…");
    let tarball = download(&cfg.numbat_download_url, &cfg.runtime_dir, "numbat.tar.gz")?;
    progress("Extracting numbat…");
    let staging = cfg.runtime_dir.join(".numbat-staging");
    let _ = fs::remove_dir_all(&staging);
    fs::create_dir_all(&staging).map_err(|e| format!("mkdir staging: {e}"))?;
    untar(&tarball, &staging)?;
    let _ = fs::remove_file(&tarball);

    let inner = single_subdir(&staging)?;
    let _ = fs::remove_dir_all(&dest);
    fs::rename(&inner, &dest).map_err(|e| format!("place numbat: {e}"))?;
    let _ = fs::remove_dir_all(&staging);
    if !bin.exists() {
        return Err(format!("numbat binary missing at {}", bin.display()));
    }
    Ok((bin, modules))
}

/// An artifact tarball resolved either from the app bundle (offline) or a
/// download. `owned` is true when we downloaded it into runtime_dir (caller
/// should delete after extract); false for a read-only bundled resource.
pub(crate) struct Artifact {
    pub path: PathBuf,
    pub owned: bool,
}

/// resolve_artifact prefers the bundled tarball `<artifacts_dir>/<name>` (staged
/// into a release build so the app is fully offline) and falls back to
/// downloading `url` into `runtime_dir`. Same artifact either way — the caller's
/// extract + version-marker logic is unchanged.
pub(crate) fn resolve_artifact(
    artifacts_dir: Option<&Path>,
    name: &str,
    url: &str,
    runtime_dir: &Path,
) -> Result<Artifact, String> {
    if let Some(dir) = artifacts_dir {
        let bundled = dir.join(name);
        if bundled.exists() {
            return Ok(Artifact { path: bundled, owned: false });
        }
    }
    let path = download(url, runtime_dir, name)?;
    Ok(Artifact { path, owned: true })
}

/// download fetches url to dir/name (blocking) and returns the path.
pub(crate) fn download(url: &str, dir: &Path, name: &str) -> Result<PathBuf, String> {
    let resp = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(600))
        .build()
        .map_err(|e| format!("http client: {e}"))?
        .get(url)
        .send()
        .map_err(|e| format!("download {url}: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("download {url}: HTTP {}", resp.status()));
    }
    let bytes = resp.bytes().map_err(|e| format!("read {url}: {e}"))?;
    let path = dir.join(name);
    fs::write(&path, &bytes).map_err(|e| format!("write {name}: {e}"))?;
    Ok(path)
}

pub(crate) fn untar(tarball: &Path, dest: &Path) -> Result<(), String> {
    let status = Command::new("tar")
        .arg("-xzf")
        .arg(tarball)
        .arg("-C")
        .arg(dest)
        .status()
        .map_err(|e| format!("run tar: {e}"))?;
    if !status.success() {
        return Err(format!("tar exited {status}"));
    }
    Ok(())
}

/// single_subdir returns the one directory inside parent (tarballs that
/// unpack to a single top-level dir).
pub(crate) fn single_subdir(parent: &Path) -> Result<PathBuf, String> {
    let mut dirs = vec![];
    for e in fs::read_dir(parent).map_err(|e| format!("read staging: {e}"))? {
        let e = e.map_err(|e| format!("read entry: {e}"))?;
        if e.path().is_dir() {
            dirs.push(e.path());
        }
    }
    match dirs.len() {
        1 => Ok(dirs.pop().unwrap()),
        n => Err(format!("expected 1 dir in archive, found {n}")),
    }
}
