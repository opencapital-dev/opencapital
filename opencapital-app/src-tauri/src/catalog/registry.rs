// catalog/registry.rs — semver helpers, core data types, blob-URL construction,
// and artifact resolution via the GHCR anonymous token dance.
//
// This is a Rust port of the Go registry/semver.go + registry/catalog.go logic.
// The GHCR OCI-manifest fetch uses `reqwest` directly (no oras-go equivalent in
// Rust); the anonymous token dance is reproduced from auth.Client behaviour.

use serde::{Deserialize, Serialize};

// ---------------------------------------------------------------------------
// Data types (mirror Go's Plugin / Artifact / VersionStatus / SourceInfo)
// ---------------------------------------------------------------------------

/// Footprint is the plugin's install metadata read from the OCI config blob.
/// Mirrors Go's install.Footprint.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Footprint {
    pub plugin_id: String,
    pub grafana_slug: String,
    #[serde(rename = "type")]
    pub plugin_type: String,
    pub display_name: String,
    pub description: String,
    #[serde(default)]
    pub platform_plugin: bool,
    #[serde(default)]
    pub logical_views: Vec<serde_json::Value>,
    #[serde(default)]
    pub query_entities: Vec<serde_json::Value>,
}

/// SourceInfo is the display + trust metadata surfaced on every catalog entry.
/// Mirrors Go's registry.SourceInfo.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct SourceInfo {
    pub url: String,
    pub publisher: String,
    pub verified: bool,
}

/// Plugin is a catalog entry: the plugin's self-described footprint plus
/// control-plane-applied policy (required), the resolved latest version, the
/// platforms published for it, and the source it was discovered through.
/// Mirrors Go's registry.Plugin.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Plugin {
    #[serde(flatten)]
    pub footprint: Footprint,
    pub required: bool,
    pub version: String,
    pub platforms: Vec<String>,
    pub source: SourceInfo,
}

/// Artifact is the resolved per-platform download: a blob-by-digest URL the
/// reconciler fetches, plus the digest and size.
/// Mirrors Go's registry.Artifact.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Artifact {
    pub download_url: String,
    pub sha256: String,
    pub size_bytes: i64,
}

/// VersionStatus pairs a version tag with whether it has been promoted to the
/// trusted namespace (validated=true) or exists only in staging (validated=false).
/// Mirrors Go's registry.VersionStatus.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VersionStatus {
    pub version: String,
    pub validated: bool,
}

/// PluginRef holds the resolution inputs for one plugin: its manifest URL
/// (identity), display + trust metadata, the registry hosting it, and its
/// version sets (semver-desc). Mirrors Go's registry.PluginRef.
#[derive(Debug, Clone)]
pub struct PluginRef {
    pub manifest_url: String,
    pub plugin_id: String,
    pub publisher: String,
    pub verified: bool,
    pub reg: RegistryCoords,
    /// Validated versions, semver-desc.
    pub validated: Vec<String>,
    /// Preview versions, semver-desc.
    pub preview: Vec<String>,
}

/// RegistryCoords holds the OCI registry coordinates for one plugin.
/// Mirrors the relevant fields of Go's registry.Registry.
#[derive(Debug, Clone, Default)]
pub struct RegistryCoords {
    pub host: String,
    pub namespace: String,
    pub staging_namespace: String,
    pub public_url: String,
}

impl RegistryCoords {
    /// public_base returns the base URL for blob downloads.
    /// If public_url is set, use it; otherwise fall back to https://<host>.
    pub fn public_base(&self) -> String {
        if !self.public_url.is_empty() {
            self.public_url.trim_end_matches('/').to_string()
        } else {
            format!("https://{}", self.host)
        }
    }
}

// ---------------------------------------------------------------------------
// Semver helpers (port of Go semver.go)
// ---------------------------------------------------------------------------

/// normalize_semver returns a tag's v-prefixed canonical form for comparison,
/// or None if it isn't semver. Tolerates bare tags like "0.1.0" (no leading v).
/// Mirrors Go's normSemver.
pub fn normalize_semver(tag: &str) -> Option<String> {
    if is_valid_semver(tag) {
        return Some(tag.to_string());
    }
    let with_v = format!("v{}", tag);
    if is_valid_semver(&with_v) {
        return Some(with_v);
    }
    None
}

/// is_valid_semver checks whether a string is a valid v-prefixed semver.
/// We implement a subset: must start with 'v', then major.minor.patch digits.
fn is_valid_semver(s: &str) -> bool {
    let s = s.strip_prefix('v').unwrap_or("");
    let parts: Vec<&str> = s.split('.').collect();
    if parts.len() < 3 {
        return false;
    }
    // Allow pre-release / build metadata after patch (e.g. v0.1.0-alpha)
    let patch_and_rest = parts[2];
    let patch_base = patch_and_rest.split('-').next().unwrap_or("").split('+').next().unwrap_or("");
    parts[0].parse::<u64>().is_ok()
        && parts[1].parse::<u64>().is_ok()
        && patch_base.parse::<u64>().is_ok()
}

/// sort_semver_desc returns the semver tags (original tag strings), greatest
/// first. Bare and v-prefixed tags are ordered together.
/// Mirrors Go's sortSemverDesc.
pub fn sort_semver_desc(tags: &[String]) -> Vec<String> {
    let mut valid: Vec<(String, String)> = tags
        .iter()
        .filter_map(|t| normalize_semver(t).map(|n| (t.clone(), n)))
        .collect();
    valid.sort_by(|a, b| compare_semver_desc(&a.1, &b.1));
    valid.into_iter().map(|(orig, _)| orig).collect()
}

/// compare_semver_desc compares two v-prefixed semver strings in descending order.
fn compare_semver_desc(a: &str, b: &str) -> std::cmp::Ordering {
    parse_semver_parts(b).cmp(&parse_semver_parts(a))
}

/// parse_semver_parts parses "vMAJOR.MINOR.PATCH[-prerelease][+build]" into
/// (major, minor, patch) tuple for comparison. Pre-release is intentionally
/// ignored for now (simple ordering).
fn parse_semver_parts(s: &str) -> (u64, u64, u64) {
    let s = s.strip_prefix('v').unwrap_or(s);
    let parts: Vec<&str> = s.split('.').collect();
    let major = parts.first().and_then(|p| p.parse().ok()).unwrap_or(0);
    let minor = parts.get(1).and_then(|p| p.parse().ok()).unwrap_or(0);
    let patch = parts
        .get(2)
        .and_then(|p| p.split('-').next())
        .and_then(|p| p.split('+').next())
        .and_then(|p| p.parse().ok())
        .unwrap_or(0);
    (major, minor, patch)
}

// ---------------------------------------------------------------------------
// Blob URL (port of Go's publicBase + the format string in ResolveArtifact)
// ---------------------------------------------------------------------------

/// blob_url constructs the OCI blob download URL.
/// Shape: {publicURL}/v2/{namespace}/{id}/blobs/{digest}
/// Mirrors Go: fmt.Sprintf("%s/v2/%s/%s/blobs/%s", ref.Reg.publicBase(), ns, id, l.Digest.String())
pub fn blob_url(public_base: &str, namespace: &str, id: &str, digest: &str) -> String {
    format!(
        "{}/v2/{}/{}/blobs/{}",
        public_base.trim_end_matches('/'),
        namespace,
        id,
        digest
    )
}

// ---------------------------------------------------------------------------
// OCI manifest types (minimal subset of OCI image spec)
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
pub struct OciDescriptor {
    pub digest: String,
    pub size: i64,
    #[serde(default)]
    pub annotations: std::collections::HashMap<String, String>,
}

#[derive(Debug, Deserialize)]
pub struct OciManifest {
    #[serde(default)]
    pub config: OciDescriptor,
    #[serde(default)]
    pub layers: Vec<OciDescriptor>,
}

impl Default for OciDescriptor {
    fn default() -> Self {
        OciDescriptor {
            digest: String::new(),
            size: 0,
            annotations: std::collections::HashMap::new(),
        }
    }
}

// The annotation key for the platform label stamped by plugindist on each layer.
const PLATFORM_ANNOTATION: &str = "io.opencapital.platform";

// ---------------------------------------------------------------------------
// GHCR anonymous token dance + artifact resolution
// ---------------------------------------------------------------------------

/// ghcr_authed_get performs a GET that handles GHCR's anonymous token dance:
/// on a 401 carrying a Bearer challenge it fetches an anonymous token and
/// retries with it. GHCR requires a token for BOTH manifests and blobs (config
/// blobs and layer tarballs) — a plain GET returns 401. Blob requests then
/// redirect (307) to a pre-signed CDN URL; reqwest follows the redirect and
/// drops the Authorization header on the cross-host hop, so the pre-signed URL
/// serves the bytes. Returns the final response for the caller to inspect.
pub async fn ghcr_authed_get(
    client: &reqwest::Client,
    url: &str,
) -> Result<reqwest::Response, String> {
    let resp = client
        .get(url)
        .send()
        .await
        .map_err(|e| format!("GET {url}: {e}"))?;
    if resp.status().as_u16() != 401 {
        return Ok(resp);
    }
    let www_auth = resp
        .headers()
        .get("www-authenticate")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    if www_auth.is_empty() {
        // 401 with no challenge — nothing to retry; let the caller see it.
        return Ok(resp);
    }
    let token = fetch_ghcr_token(client, &www_auth).await?;
    client
        .get(url)
        .bearer_auth(&token)
        .send()
        .await
        .map_err(|e| format!("GET {url} (authed retry): {e}"))
}

/// fetch_oci_manifest fetches the OCI image manifest for (host, namespace, id,
/// tag). On a 401 from GHCR it reads the WWW-Authenticate header, fetches an
/// anonymous token from the GHCR token endpoint, and retries.
/// Returns None when the repository or tag is absent (404/403/401 with no
/// WWW-Authenticate to retry).
pub async fn fetch_oci_manifest(
    client: &reqwest::Client,
    host: &str,
    namespace: &str,
    id: &str,
    tag: &str,
) -> Result<Option<OciManifest>, String> {
    let url = format!("https://{}/v2/{}/{}/manifests/{}", host, namespace, id, tag);

    // First attempt — anonymous (no auth).
    let resp = client
        .get(&url)
        .header(
            "Accept",
            "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json",
        )
        .send()
        .await
        .map_err(|e| format!("fetch OCI manifest: {e}"))?;

    match resp.status().as_u16() {
        200 => {
            let m: OciManifest = resp
                .json()
                .await
                .map_err(|e| format!("decode OCI manifest: {e}"))?;
            return Ok(Some(m));
        }
        401 => {
            // Try the GHCR token dance.
            let www_auth = resp
                .headers()
                .get("www-authenticate")
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_string();

            if www_auth.is_empty() {
                // 401 with no challenge → private repo, treat as absent.
                return Ok(None);
            }

            let token = fetch_ghcr_token(client, &www_auth).await?;
            // Retry with the anonymous token.
            let resp2 = client
                .get(&url)
                .header(
                    "Accept",
                    "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json",
                )
                .bearer_auth(&token)
                .send()
                .await
                .map_err(|e| format!("fetch OCI manifest (retry): {e}"))?;

            match resp2.status().as_u16() {
                200 => {
                    let m: OciManifest = resp2
                        .json()
                        .await
                        .map_err(|e| format!("decode OCI manifest (retry): {e}"))?;
                    Ok(Some(m))
                }
                404 | 403 | 401 => Ok(None),
                s => Err(format!("OCI manifest retry status {s}")),
            }
        }
        404 | 403 => Ok(None),
        s => {
            let body = resp.text().await.unwrap_or_default();
            Err(format!("OCI manifest status {s}: {body}"))
        }
    }
}

/// fetch_ghcr_token parses a WWW-Authenticate Bearer challenge and fetches
/// an anonymous token from the stated realm.
async fn fetch_ghcr_token(client: &reqwest::Client, www_auth: &str) -> Result<String, String> {
    // Parse: Bearer realm="…",service="…",scope="…"
    let (realm, service, scope) = parse_bearer_challenge(www_auth)?;

    let mut req = client.get(&realm);
    if !service.is_empty() {
        req = req.query(&[("service", &service)]);
    }
    if !scope.is_empty() {
        req = req.query(&[("scope", &scope)]);
    }

    let resp = req
        .send()
        .await
        .map_err(|e| format!("fetch GHCR token: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("GHCR token {status}: {body}"));
    }
    let body: serde_json::Value = resp
        .json()
        .await
        .map_err(|e| format!("decode GHCR token: {e}"))?;
    body.get("token")
        .or_else(|| body.get("access_token"))
        .and_then(|v| v.as_str())
        .map(str::to_string)
        .ok_or_else(|| "GHCR token response missing 'token' field".to_string())
}

/// parse_bearer_challenge extracts realm, service, scope from a
/// `Bearer realm="…",service="…",scope="…"` header value.
fn parse_bearer_challenge(s: &str) -> Result<(String, String, String), String> {
    let s = s.trim().strip_prefix("Bearer ").unwrap_or(s.trim());
    let mut realm = String::new();
    let mut service = String::new();
    let mut scope = String::new();

    // Simple key="value" parser (values may contain commas in scope).
    let mut rest = s;
    while !rest.is_empty() {
        // Find key=
        let eq = rest.find('=').ok_or_else(|| format!("bad Bearer challenge: {s}"))?;
        let key = rest[..eq].trim();
        rest = &rest[eq + 1..];
        // Value is quoted
        let value = if rest.starts_with('"') {
            let end = rest[1..].find('"').ok_or("unterminated quote in Bearer challenge")?;
            let v = rest[1..end + 1].to_string();
            rest = rest[end + 2..].trim_start_matches(',').trim();
            v
        } else {
            // Unquoted (shouldn't happen in practice, but be lenient)
            let end = rest.find(',').unwrap_or(rest.len());
            let v = rest[..end].to_string();
            rest = rest[end..].trim_start_matches(',').trim();
            v
        };
        match key {
            "realm" => realm = value,
            "service" => service = value,
            "scope" => scope = value,
            _ => {}
        }
    }
    if realm.is_empty() {
        return Err(format!("Bearer challenge missing realm: {s}"));
    }
    Ok((realm, service, scope))
}

/// tag_forms returns the tag candidates for a version: a v-prefixed version is
/// taken verbatim; a bare semver is tried v-prefixed first (GHCR's published
/// form) then verbatim — v-first avoids a wasted token+404 roundtrip per plugin.
pub fn tag_forms(v: &str) -> Vec<String> {
    if v.starts_with('v') {
        vec![v.to_string()]
    } else {
        vec![format!("v{}", v), v.to_string()]
    }
}

/// resolve_artifact returns the per-platform tarball blob for (id, version),
/// trying the trusted namespace first then staging. Returns None when neither
/// namespace has a layer for that platform.
/// Mirrors Go's Client.ResolveArtifact.
pub async fn resolve_artifact(
    client: &reqwest::Client,
    plugin_ref: &PluginRef,
    version: &str,
    platform: &str,
) -> Result<Option<Artifact>, String> {
    let reg = &plugin_ref.reg;
    let mut namespaces = vec![reg.namespace.clone()];
    if !reg.staging_namespace.is_empty() {
        namespaces.push(reg.staging_namespace.clone());
    }

    for ns in &namespaces {
        for tag in tag_forms(version) {
            let manifest = fetch_oci_manifest(client, &reg.host, ns, &plugin_ref.plugin_id, &tag).await?;
            let Some(man) = manifest else { continue };
            for layer in &man.layers {
                if layer.annotations.get(PLATFORM_ANNOTATION).map(String::as_str) == Some(platform) {
                    let digest = &layer.digest;
                    // Strip "sha256:" prefix to get just the hex for Sha256 field.
                    let sha256 = digest
                        .strip_prefix("sha256:")
                        .unwrap_or(digest)
                        .to_string();
                    return Ok(Some(Artifact {
                        download_url: blob_url(&reg.public_base(), ns, &plugin_ref.plugin_id, digest),
                        sha256,
                        size_bytes: layer.size,
                    }));
                }
            }
        }
    }
    Ok(None)
}

/// versions_with_status returns every known version of a plugin, newest first.
/// A version is Validated when it is in the ref's validated set; otherwise it
/// is preview. The list is the union of validated and preview sets, compared on
/// normalized semver so a version in both collapses to one entry (in its
/// validated form).
/// Mirrors Go's Client.VersionsWithStatus.
pub fn versions_with_status(plugin_ref: &PluginRef) -> Vec<VersionStatus> {
    use std::collections::HashMap;

    let mut validated_set: std::collections::HashSet<String> = std::collections::HashSet::new();
    for v in &plugin_ref.validated {
        if let Some(n) = normalize_semver(v) {
            validated_set.insert(n);
        }
    }

    // Build repr map: normalized -> original, letting validated overwrite preview
    // for the same version (so validated form is used).
    let mut repr: HashMap<String, String> = HashMap::new();
    // Preview first, then validated overwrites.
    for v in plugin_ref.preview.iter().chain(plugin_ref.validated.iter()) {
        if let Some(n) = normalize_semver(v) {
            repr.insert(n, v.clone());
        }
    }

    let mut all: Vec<String> = repr.into_values().collect();
    all = sort_semver_desc(&all);

    all.iter()
        .map(|v| VersionStatus {
            version: v.clone(),
            validated: normalize_semver(v).map(|n| validated_set.contains(&n)).unwrap_or(false),
        })
        .collect()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // --- From the brief (Step 1 / Step 3) ---

    #[test]
    fn sorts_semver_desc_mixed_prefix() {
        let input: Vec<String> = vec!["0.1.2".into(), "v0.1.10".into(), "0.1.3".into()];
        let got = sort_semver_desc(&input);
        assert_eq!(got, vec!["v0.1.10", "0.1.3", "0.1.2"]);
    }

    #[test]
    fn blob_url_shape() {
        let u = blob_url(
            "https://ghcr.io",
            "acme/oc-plugins",
            "core-app",
            "sha256:abcd",
        );
        assert_eq!(
            u,
            "https://ghcr.io/v2/acme/oc-plugins/core-app/blobs/sha256:abcd"
        );
    }

    // --- Additional semver coverage (mirrors Go's semver_test.go) ---

    #[test]
    fn normalize_semver_bare() {
        assert_eq!(normalize_semver("0.1.0"), Some("v0.1.0".into()));
        assert_eq!(normalize_semver("v0.1.0"), Some("v0.1.0".into()));
        assert_eq!(normalize_semver("garbage"), None);
        assert_eq!(normalize_semver("latest"), None);
    }

    #[test]
    fn latest_semver_picks_greatest() {
        let tags: Vec<String> = vec!["v0.2.0".into(), "v0.10.0".into(), "v0.1.0".into()];
        let sorted = sort_semver_desc(&tags);
        assert_eq!(sorted.first().map(String::as_str), Some("v0.10.0"));
    }

    #[test]
    fn sort_semver_desc_bare_and_v_mixed() {
        let tags: Vec<String> = vec!["0.1.0".into(), "v1.0.4".into(), "0.1.1".into(), "v1.0.1".into()];
        let got = sort_semver_desc(&tags);
        assert_eq!(got, vec!["v1.0.4", "v1.0.1", "0.1.1", "0.1.0"]);
    }

    #[test]
    fn sort_semver_desc_ignores_non_semver() {
        let tags: Vec<String> = vec!["latest".into(), "v1.0.0".into(), "garbage".into()];
        let got = sort_semver_desc(&tags);
        assert_eq!(got, vec!["v1.0.0"]);
    }

    #[test]
    fn blob_url_no_trailing_slash() {
        let u = blob_url(
            "https://ghcr.io/",
            "ns/plugins",
            "my-plugin",
            "sha256:deadbeef",
        );
        assert_eq!(
            u,
            "https://ghcr.io/v2/ns/plugins/my-plugin/blobs/sha256:deadbeef"
        );
    }

    #[test]
    fn versions_with_status_union_and_dedup() {
        let ref_ = PluginRef {
            manifest_url: "http://example.com/m.json".into(),
            plugin_id: "test".into(),
            publisher: "test".into(),
            verified: true,
            reg: RegistryCoords::default(),
            validated: vec!["v0.1.2".into()],
            preview: vec!["v0.1.3".into(), "v0.1.2".into()],
        };
        let vs = versions_with_status(&ref_);
        // v0.1.3 preview, v0.1.2 validated. Newest first.
        assert_eq!(vs.len(), 2);
        assert_eq!(vs[0].version, "v0.1.3");
        assert!(!vs[0].validated);
        assert_eq!(vs[1].version, "v0.1.2");
        assert!(vs[1].validated);
    }

    #[test]
    fn parse_bearer_challenge_standard() {
        let s = r#"Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:foo/bar:pull""#;
        let (realm, service, scope) = parse_bearer_challenge(s).unwrap();
        assert_eq!(realm, "https://ghcr.io/token");
        assert_eq!(service, "ghcr.io");
        assert_eq!(scope, "repository:foo/bar:pull");
    }

    #[test]
    fn tag_forms_bare_gives_both() {
        // v-prefixed first (GHCR's published form), then bare fallback.
        assert_eq!(tag_forms("0.1.2"), vec!["v0.1.2", "0.1.2"]);
    }

    #[test]
    fn tag_forms_v_prefixed_gives_one() {
        assert_eq!(tag_forms("v0.1.2"), vec!["v0.1.2"]);
    }
}
