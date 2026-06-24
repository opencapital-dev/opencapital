// reconcile/download.rs — artifact download, sha256 verify, tar extract,
// symlink, and prune. Rust port of reconcile.go.

use std::fs;
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

use super::{ReconcileDirs, ResolvedPlugin};

/// Marker file written inside a cache entry after successful extraction.
/// Idempotency: re-runs with the same sha skip download + extract.
const MARKER_FILE: &str = ".artifact-sha256";

// ---------------------------------------------------------------------------
// install_all
// ---------------------------------------------------------------------------

/// install_all ensures every plugin's binary is present in the cache and
/// symlinked into the plugins dir. Returns the provisionable subset (those
/// with a binary on disk). Mirrors Go's installAll.
pub async fn install_all(
    plugins: &[ResolvedPlugin],
    dirs: &ReconcileDirs,
    platform: &str,
    client: &reqwest::Client,
) -> Result<Vec<ResolvedPlugin>, String> {
    let mut ok: Vec<ResolvedPlugin> = Vec::new();
    for p in plugins {
        if p.grafana_slug.is_empty() {
            continue;
        }
        match &p.artifact {
            None => {
                if p.required {
                    return Err(format!(
                        "required plugin {:?} has no artifact for platform {}",
                        p.plugin_id, platform
                    ));
                }
                // Optional + no artifact → skip silently.
                continue;
            }
            Some(art) => {
                match install(p, art, dirs, platform, client).await {
                    Ok(()) => ok.push(p.clone()),
                    Err(e) => {
                        if p.required {
                            return Err(format!(
                                "install required plugin {:?}: {}",
                                p.plugin_id, e
                            ));
                        }
                        // Optional failure → warn and skip.
                        eprintln!(
                            "[reconcile] optional plugin {} skipped: {}",
                            p.plugin_id, e
                        );
                    }
                }
            }
        }
    }
    Ok(ok)
}

// ---------------------------------------------------------------------------
// install (one plugin)
// ---------------------------------------------------------------------------

/// install ensures a single plugin is in the cache and symlinked.
/// Mirrors Go's install().
async fn install(
    p: &ResolvedPlugin,
    art: &crate::catalog::Artifact,
    dirs: &ReconcileDirs,
    platform: &str,
    client: &reqwest::Client,
) -> Result<(), String> {
    let cache_dir = dirs
        .cache_dir
        .join(&p.plugin_id)
        .join(&p.version)
        .join(platform);
    let link_path = dirs.plugins_dir.join(&p.grafana_slug);

    if cache_valid(&cache_dir, &art.sha256) {
        ensure_backend_executable(&cache_dir)?;
        return link_plugin(&link_path, &cache_dir);
    }

    // Download to a temp file.
    let tmp = download_artifact(client, &art.download_url).await?;
    // Verify sha256.
    let got = file_sha256(&tmp)?;
    if got != art.sha256 {
        let _ = fs::remove_file(&tmp);
        return Err(format!(
            "sha256 mismatch for {}: got {}, want {}",
            p.plugin_id, got, art.sha256
        ));
    }
    // Extract atomically.
    if let Err(e) = extract_into(&tmp, &cache_dir) {
        let _ = fs::remove_file(&tmp);
        return Err(format!("extract {}: {}", p.plugin_id, e));
    }
    let _ = fs::remove_file(&tmp);

    ensure_backend_executable(&cache_dir)?;

    // Write the marker.
    fs::write(cache_dir.join(MARKER_FILE), &art.sha256)
        .map_err(|e| format!("write marker for {}: {}", p.plugin_id, e))?;

    link_plugin(&link_path, &cache_dir)?;
    Ok(())
}

// ---------------------------------------------------------------------------
// Idempotency marker
// ---------------------------------------------------------------------------

/// cache_valid reports whether the cache dir holds an extraction whose marker
/// matches sha. Mirrors Go's cacheValid.
pub(crate) fn cache_valid(cache_dir: &Path, sha: &str) -> bool {
    match fs::read_to_string(cache_dir.join(MARKER_FILE)) {
        Ok(s) => s.trim() == sha,
        Err(_) => false,
    }
}

// ---------------------------------------------------------------------------
// SHA-256
// ---------------------------------------------------------------------------

/// file_sha256 computes the hex-encoded sha256 of a file.
/// Mirrors Go's fileSHA256.
pub(crate) fn file_sha256(path: &Path) -> Result<String, String> {
    let mut f = fs::File::open(path).map_err(|e| format!("open {:?}: {}", path, e))?;
    let mut hasher = Sha256::new();
    let mut buf = [0u8; 65536];
    loop {
        let n = f
            .read(&mut buf)
            .map_err(|e| format!("read {:?}: {}", path, e))?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok(hex::encode(hasher.finalize()))
}

/// bytes_sha256 computes the hex-encoded sha256 of a byte slice.
/// Used in tests.
pub(crate) fn bytes_sha256(data: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(data);
    hex::encode(hasher.finalize())
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

/// download_artifact streams a URL to a temp file, returning the path.
/// Mirrors Go's download(). GHCR requires the anonymous token dance for blobs,
/// so this goes through registry::ghcr_authed_get (a plain GET 401s).
pub(crate) async fn download_artifact(
    client: &reqwest::Client,
    url: &str,
) -> Result<PathBuf, String> {
    // GHCR requires the anonymous token dance for blobs too — a plain GET
    // returns 401, then redirects (307) to a pre-signed CDN URL.
    let resp = crate::catalog::registry::ghcr_authed_get(client, url)
        .await
        .map_err(|e| format!("download {}: {}", url, e))?;

    if !resp.status().is_success() {
        return Err(format!("download {}: HTTP {}", url, resp.status()));
    }

    // Stream to a named temp file.
    let tmp_path = std::env::temp_dir().join(format!(
        "plugin-{}.tar.gz",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    ));

    let bytes = resp
        .bytes()
        .await
        .map_err(|e| format!("read body {}: {}", url, e))?;

    let mut f = fs::File::create(&tmp_path)
        .map_err(|e| format!("create tmp {:?}: {}", tmp_path, e))?;
    f.write_all(&bytes)
        .map_err(|e| format!("write tmp {:?}: {}", tmp_path, e))?;
    f.flush()
        .map_err(|e| format!("flush tmp {:?}: {}", tmp_path, e))?;

    Ok(tmp_path)
}

// ---------------------------------------------------------------------------
// Extract
// ---------------------------------------------------------------------------

/// extract_into unpacks a .tar.gz into dest atomically (extract to staging,
/// remove old dest, rename). Guards against zip-slip.
/// Mirrors Go's extractInto.
pub(crate) fn extract_into(tarball: &Path, dest: &Path) -> Result<(), String> {
    let parent = dest
        .parent()
        .ok_or_else(|| format!("no parent for {:?}", dest))?;
    fs::create_dir_all(parent).map_err(|e| format!("mkdir {:?}: {}", parent, e))?;

    // Staging dir next to dest.
    let staging = {
        let name = format!(
            ".extract-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos()
        );
        parent.join(name)
    };

    if let Err(e) = untar(tarball, &staging) {
        let _ = fs::remove_dir_all(&staging);
        return Err(format!("untar {:?}: {}", tarball, e));
    }

    // Atomically replace dest.
    if dest.exists() {
        fs::remove_dir_all(dest)
            .map_err(|e| format!("remove old dest {:?}: {}", dest, e))?;
    }
    fs::rename(&staging, dest)
        .map_err(|e| format!("rename {:?} -> {:?}: {}", staging, dest, e))?;

    Ok(())
}

/// untar extracts a .tar.gz file into dest, guarding against zip-slip.
/// Mirrors Go's untar.
pub(crate) fn untar(tarball: &Path, dest: &Path) -> Result<(), String> {
    let f = fs::File::open(tarball).map_err(|e| format!("open tarball {:?}: {}", tarball, e))?;
    let gz = flate2::read::GzDecoder::new(f);
    let mut archive = tar::Archive::new(gz);

    for entry in archive
        .entries()
        .map_err(|e| format!("tar entries: {}", e))?
    {
        let mut entry = entry.map_err(|e| format!("tar entry: {}", e))?;
        let header = entry.header();
        let entry_path = entry
            .path()
            .map_err(|e| format!("tar entry path: {}", e))?
            .to_path_buf();

        let target = safe_join(dest, &entry_path)?;

        match header.entry_type() {
            tar::EntryType::Directory => {
                fs::create_dir_all(&target)
                    .map_err(|e| format!("mkdir {:?}: {}", target, e))?;
            }
            tar::EntryType::Regular => {
                if let Some(parent) = target.parent() {
                    fs::create_dir_all(parent)
                        .map_err(|e| format!("mkdir {:?}: {}", parent, e))?;
                }
                let mode = header.mode().unwrap_or(0o644);
                let mut out = fs::OpenOptions::new()
                    .create(true)
                    .write(true)
                    .truncate(true)
                    .open(&target)
                    .map_err(|e| format!("create {:?}: {}", target, e))?;
                io::copy(&mut entry, &mut out)
                    .map_err(|e| format!("write {:?}: {}", target, e))?;
                // Apply permissions on Unix.
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    let perm = fs::Permissions::from_mode(mode & 0o777);
                    let _ = fs::set_permissions(&target, perm);
                }
                let _ = mode; // suppress unused on non-Unix
            }
            _ => {
                // Skip symlinks, hard links, devices — plugin tarballs are plain files.
            }
        }
    }
    Ok(())
}

/// safe_join joins name onto dir, rejecting entries that escape dir (zip-slip).
/// Mirrors Go's safeJoin.
pub(crate) fn safe_join(dir: &Path, name: &Path) -> Result<PathBuf, String> {
    if name.is_absolute() {
        return Err(format!("unsafe absolute tar entry {:?}", name));
    }
    // Walk components, tracking depth. Any ".." that would take us above the
    // root (depth < 0) is an escape attempt → reject.
    let mut depth: i64 = 0;
    let mut clean = PathBuf::new();
    for c in name.components() {
        match c {
            std::path::Component::ParentDir => {
                depth -= 1;
                if depth < 0 {
                    return Err(format!(
                        "unsafe tar entry {:?} escapes target",
                        name
                    ));
                }
                clean.pop();
            }
            std::path::Component::Normal(n) => {
                depth += 1;
                clean.push(n);
            }
            std::path::Component::CurDir => {}
            _ => {
                return Err(format!("unsafe tar entry {:?}", name));
            }
        }
    }
    let target = dir.join(&clean);
    // Belt-and-suspenders: verify the joined path still starts with dir.
    if target != dir && !target.starts_with(dir) {
        return Err(format!("unsafe tar entry {:?} escapes target", name));
    }
    Ok(target)
}

// ---------------------------------------------------------------------------
// Backend executable bit
// ---------------------------------------------------------------------------

/// ensure_backend_executable reads plugin.json and chmod +x's the backend
/// binary if the plugin declares one. Mirrors Go's ensureBackendExecutable.
/// No-op on non-Unix targets.
pub(crate) fn ensure_backend_executable(dir: &Path) -> Result<(), String> {
    let pj_path = dir.join("plugin.json");
    let bytes = match fs::read(&pj_path) {
        Ok(b) => b,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(format!("read plugin.json {:?}: {}", pj_path, e)),
    };
    let v: serde_json::Value =
        serde_json::from_slice(&bytes).map_err(|e| format!("parse plugin.json: {}", e))?;
    let backend = v.get("backend").and_then(|b| b.as_bool()).unwrap_or(false);
    let executable = v
        .get("executable")
        .and_then(|e| e.as_str())
        .unwrap_or("");
    if !backend || executable.is_empty() {
        return Ok(());
    }
    let prefix = Path::new(executable)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or(executable)
        .to_string();

    let entries = fs::read_dir(dir).map_err(|e| format!("readdir {:?}: {}", dir, e))?;
    for entry in entries.flatten() {
        let fname = entry.file_name();
        let fname_str = fname.to_string_lossy();
        if !fname_str.starts_with(&prefix) {
            continue;
        }
        if entry.metadata().map(|m| m.is_dir()).unwrap_or(false) {
            continue;
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let path = entry.path();
            let perm = fs::Permissions::from_mode(0o755);
            fs::set_permissions(&path, perm)
                .map_err(|e| format!("chmod {:?}: {}", path, e))?;
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Symlink
// ---------------------------------------------------------------------------

/// link_plugin points link_path at cache_dir via a symlink, replacing any
/// existing entry. Mirrors Go's linkPlugin.
pub(crate) fn link_plugin(link_path: &Path, cache_dir: &Path) -> Result<(), String> {
    if let Some(parent) = link_path.parent() {
        fs::create_dir_all(parent)
            .map_err(|e| format!("mkdir {:?}: {}", parent, e))?;
    }

    // If it already points at the right target, leave it.
    #[cfg(unix)]
    if let Ok(dst) = std::fs::read_link(link_path) {
        if dst == cache_dir {
            return Ok(());
        }
    }

    // Remove whatever is there (symlink, file, or dir).
    if link_path.exists() || link_path.symlink_metadata().is_ok() {
        let _ = fs::remove_file(link_path);
        let _ = fs::remove_dir_all(link_path);
    }

    #[cfg(unix)]
    std::os::unix::fs::symlink(cache_dir, link_path)
        .map_err(|e| format!("symlink {:?} -> {:?}: {}", link_path, cache_dir, e))?;

    // On non-Unix targets, fall back to a deep copy (not implemented for v0 —
    // desktop targets macOS/Linux only).
    #[cfg(not(unix))]
    return Err("symlink not supported on this platform".to_string());

    Ok(())
}

// ---------------------------------------------------------------------------
// Prune
// ---------------------------------------------------------------------------

/// prune removes plugins-dir symlinks that point into our cache but whose
/// slug is no longer in the desired set. Mirrors Go's prune.
pub fn prune(plugins: &[ResolvedPlugin], dirs: &ReconcileDirs) -> Result<(), String> {
    let want: std::collections::HashSet<&str> = plugins
        .iter()
        .filter(|p| !p.grafana_slug.is_empty())
        .map(|p| p.grafana_slug.as_str())
        .collect();

    let entries = match fs::read_dir(&dirs.plugins_dir) {
        Ok(e) => e,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => {
            return Err(format!(
                "read plugins dir {:?}: {}",
                dirs.plugins_dir, e
            ))
        }
    };

    let cache_abs = dirs
        .cache_dir
        .canonicalize()
        .unwrap_or_else(|_| dirs.cache_dir.clone());

    for entry in entries.flatten() {
        let name = entry.file_name();
        let name_str = name.to_string_lossy();
        if want.contains(name_str.as_ref()) {
            continue;
        }
        let link_path = dirs.plugins_dir.join(&name);
        // Only remove symlinks that point into our cache.
        #[cfg(unix)]
        {
            let meta = match fs::symlink_metadata(&link_path) {
                Ok(m) => m,
                Err(_) => continue,
            };
            if !meta.file_type().is_symlink() {
                continue;
            }
            let dst = match fs::read_link(&link_path) {
                Ok(d) => d,
                Err(_) => continue,
            };
            let dst_abs = dst.canonicalize().unwrap_or(dst);
            if dst_abs.starts_with(&cache_abs) {
                fs::remove_file(&link_path)
                    .map_err(|e| format!("remove stale link {:?}: {}", link_path, e))?;
                eprintln!("[reconcile] pruned {}", name_str);
            }
        }
        // Non-Unix: no symlinks managed, nothing to prune.
        #[cfg(not(unix))]
        let _ = cache_abs;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// hex helper (tiny — avoids adding a hex crate just for this; sha2 already
// pulls in its own encoding via the Digest trait but not hex::encode).
// We use the `hex` feature available from sha2's deps or just roll our own.
// ---------------------------------------------------------------------------

mod hex {
    pub fn encode(bytes: impl AsRef<[u8]>) -> String {
        bytes
            .as_ref()
            .iter()
            .map(|b| format!("{:02x}", b))
            .collect()
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write as _;
    use tempfile::TempDir;

    // --- sha256 verify -------------------------------------------------------

    #[test]
    fn sha256_correct_digest_passes() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("data.bin");
        let content = b"hello reconciler";
        std::fs::write(&path, content).unwrap();

        let got = file_sha256(&path).unwrap();
        let want = bytes_sha256(content);
        assert_eq!(got, want);
    }

    #[test]
    fn sha256_mismatch_detected() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("data.bin");
        std::fs::write(&path, b"real content").unwrap();

        let got = file_sha256(&path).unwrap();
        // A deliberately wrong hash.
        assert_ne!(got, "0000000000000000000000000000000000000000000000000000000000000000");
    }

    // --- cache_valid idempotency ---------------------------------------------

    #[test]
    fn cache_valid_matches_marker() {
        let dir = TempDir::new().unwrap();
        let cache_dir = dir.path().join("cache");
        std::fs::create_dir_all(&cache_dir).unwrap();

        let sha = "abc123deadbeef";
        std::fs::write(cache_dir.join(MARKER_FILE), sha).unwrap();

        assert!(cache_valid(&cache_dir, sha));
    }

    #[test]
    fn cache_valid_wrong_sha_is_false() {
        let dir = TempDir::new().unwrap();
        let cache_dir = dir.path().join("cache");
        std::fs::create_dir_all(&cache_dir).unwrap();
        std::fs::write(cache_dir.join(MARKER_FILE), "abc123").unwrap();

        assert!(!cache_valid(&cache_dir, "different-sha"));
    }

    #[test]
    fn cache_valid_missing_marker_is_false() {
        let dir = TempDir::new().unwrap();
        let cache_dir = dir.path().join("no-cache");
        // Dir does not exist at all.
        assert!(!cache_valid(&cache_dir, "any-sha"));
    }

    // --- safe_join zip-slip guard --------------------------------------------

    #[test]
    fn safe_join_normal_path_ok() {
        let dir = Path::new("/tmp/root");
        let name = Path::new("plugin/module/file.js");
        let result = safe_join(dir, name).unwrap();
        assert_eq!(result, PathBuf::from("/tmp/root/plugin/module/file.js"));
    }

    #[test]
    fn safe_join_dotdot_rejected() {
        let dir = Path::new("/tmp/root");
        let name = Path::new("../etc/passwd");
        assert!(safe_join(dir, name).is_err());
    }

    #[test]
    fn safe_join_absolute_rejected() {
        let dir = Path::new("/tmp/root");
        let name = Path::new("/etc/passwd");
        assert!(safe_join(dir, name).is_err());
    }

    // --- extract_into (tar.gz round-trip) ------------------------------------

    #[test]
    fn extract_into_round_trip() {
        use flate2::write::GzEncoder;
        use flate2::Compression;

        let dir = TempDir::new().unwrap();

        // Build a minimal .tar.gz in memory.
        let tar_gz_bytes = {
            let mut buf = Vec::new();
            {
                let gz = GzEncoder::new(&mut buf, Compression::default());
                let mut tb = tar::Builder::new(gz);

                // Add one directory.
                let mut header_dir = tar::Header::new_gnu();
                header_dir.set_entry_type(tar::EntryType::Directory);
                header_dir.set_path("plugin-dir/").unwrap();
                header_dir.set_size(0);
                header_dir.set_mode(0o755);
                header_dir.set_cksum();
                tb.append(&header_dir, &mut io::Cursor::new(b"")).unwrap();

                // Add one file inside it.
                let content = b"console.log('hello');";
                let mut header_file = tar::Header::new_gnu();
                header_file.set_entry_type(tar::EntryType::Regular);
                header_file.set_path("plugin-dir/module.js").unwrap();
                header_file.set_size(content.len() as u64);
                header_file.set_mode(0o644);
                header_file.set_cksum();
                tb.append(&header_file, &mut io::Cursor::new(content))
                    .unwrap();

                tb.into_inner()
                    .unwrap()
                    .finish()
                    .unwrap();
            }
            buf
        };

        let tarball_path = dir.path().join("test.tar.gz");
        std::fs::write(&tarball_path, &tar_gz_bytes).unwrap();

        let dest = dir.path().join("extracted");
        extract_into(&tarball_path, &dest).unwrap();

        // The extracted file should be present.
        let extracted_file = dest.join("plugin-dir").join("module.js");
        assert!(extracted_file.exists(), "extracted file missing");
        let got = std::fs::read_to_string(&extracted_file).unwrap();
        assert_eq!(got, "console.log('hello');");
    }

    // --- bytes_sha256 --------------------------------------------------------

    #[test]
    fn bytes_sha256_known_value() {
        // SHA-256 of empty string is well-known.
        let got = bytes_sha256(b"");
        assert_eq!(
            got,
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
    }
}
