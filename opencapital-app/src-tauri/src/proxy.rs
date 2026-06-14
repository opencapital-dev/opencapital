// Loopback server: one tiny_http server on 127.0.0.1:S serving two roles.
//
//   GET /instance-token  -> the current short-lived instance token (Option
//                           A). Plugins fetch it here (instanceTokenUrl) and
//                           present it as the /jwt/mint bearer. The shell
//                           keeps it fresh; the plugin never sees the Kinde
//                           token.
//   /grafana/*           -> reverse-proxies to the local grafana-server,
//                           injecting X-Webauth-{User,Email,Name} so
//                           grafana's auth.proxy auto-signs-in the laptop
//                           owner. Only /grafana/* is proxied — the
//                           plugin->control-plane and plugin->RW hops are
//                           direct.
//
// Single-user desktop: everything is loopback-only, so no external auth on
// the token endpoint is needed (nothing off-host can reach 127.0.0.1:S).

use std::sync::{Arc, Mutex};
use std::thread;

use tiny_http::{Header, Response, Server};

/// Shared runtime state, read by the loopback server and written by the
/// launch flow. Cloned (Arc) into the server thread.
#[derive(Default)]
pub struct Shared {
    pub loopback_port: Mutex<Option<u16>>,
    pub instance_token: Mutex<Option<String>>,
    pub grafana_port: Mutex<Option<u16>>,
    pub webauth_user: Mutex<Option<String>>,
    pub webauth_email: Mutex<Option<String>>,
    /// Running grafana-server child; the crash monitor takes + replaces it.
    pub child: Mutex<Option<std::process::Child>>,
    /// Loopback port the compute sidecar listens on. Published to plugins as
    /// the compute URL via instance-bootstrap jsonData (Grafana sanitizes the
    /// plugin env, so this travels as data, not env).
    pub compute_port: Mutex<Option<u16>>,
    /// Running compute sidecar child; the crash monitor takes + replaces it.
    pub compute_child: Mutex<Option<std::process::Child>>,
    /// Local data-plane children (LOCAL_DATA_PLANE). Each supervisor takes +
    /// replaces its own handle on crash-restart.
    pub pg_child: Mutex<Option<std::process::Child>>,
    pub rw_child: Mutex<Option<std::process::Child>>,
    pub cp_child: Mutex<Option<std::process::Child>>,
    pub gw_child: Mutex<Option<std::process::Child>>,
    pub rg_child: Mutex<Option<std::process::Child>>,
    /// On Windows the whole plane runs in one WSL distro; this is the
    /// long-lived `wsl … supervisor.sh` child the crash monitor wait()s on.
    #[cfg_attr(not(windows), allow(dead_code))]
    pub wsl_child: Mutex<Option<std::process::Child>>,
    /// Launch generation, bumped by each launch(). A crash monitor from a
    /// superseded launch sees the mismatch and exits instead of respawning a
    /// grafana the relaunch already replaced.
    pub generation: std::sync::atomic::AtomicU64,
}

impl Shared {
    pub fn new() -> Arc<Shared> {
        Arc::new(Shared::default())
    }
}

/// start binds the loopback server (port 0 = OS-assigned), records the port
/// in shared, and spawns the serving thread. Idempotent: returns the
/// existing port if already started.
pub fn start(shared: Arc<Shared>) -> Result<u16, String> {
    if let Some(p) = *shared.loopback_port.lock().unwrap() {
        return Ok(p);
    }
    let server = Arc::new(Server::http("127.0.0.1:0").map_err(|e| format!("bind loopback: {e}"))?);
    let port = match server.server_addr() {
        tiny_http::ListenAddr::IP(a) => a.port(),
        _ => return Err("loopback not an IP addr".into()),
    };
    *shared.loopback_port.lock().unwrap() = Some(port);

    // Thread pool: the Grafana SPA fires many concurrent asset + plugin-
    // module requests. A single serving thread serializes them and the UI
    // crawls (panels stuck "Loading plugin panel…"). Several threads share
    // the Server (tiny_http recv() is concurrent). Each builds its OWN
    // blocking client INSIDE the thread — a reqwest::blocking client owns an
    // internal tokio runtime and must never be created/dropped on an async
    // worker (start() is called from the async launch command), or it panics
    // ("Cannot drop a runtime ... from within an asynchronous context").
    for _ in 0..24 {
        let s = server.clone();
        let st = shared.clone();
        thread::spawn(move || {
            let cl = reqwest::blocking::Client::builder()
                .redirect(reqwest::redirect::Policy::none())
                .build()
                .expect("blocking client");
            loop {
                match s.recv() {
                    Ok(req) => handle(&st, &cl, req),
                    Err(_) => break,
                }
            }
        });
    }
    Ok(port)
}

fn handle(shared: &Arc<Shared>, client: &reqwest::blocking::Client, req: tiny_http::Request) {
    let url = req.url().to_string();
    let path = url.split('?').next().unwrap_or("");

    if path == "/instance-token" {
        let token = shared.instance_token.lock().unwrap().clone();
        match token {
            Some(t) => {
                let body = format!("{{\"token\":{}}}", json_string(&t));
                let hdr = Header::from_bytes(&b"Content-Type"[..], &b"application/json"[..]).unwrap();
                let _ = req.respond(Response::from_string(body).with_header(hdr));
            }
            None => {
                let _ = req.respond(Response::from_string("no token").with_status_code(503u16));
            }
        }
        return;
    }

    if path == "/grafana" || path.starts_with("/grafana/") {
        proxy_grafana(shared, client, req);
        return;
    }

    let _ = req.respond(Response::from_string("not found").with_status_code(404u16));
}

fn proxy_grafana(
    shared: &Arc<Shared>,
    client: &reqwest::blocking::Client,
    mut req: tiny_http::Request,
) {
    let gport = match *shared.grafana_port.lock().unwrap() {
        Some(p) => p,
        None => {
            let _ = req.respond(Response::from_string("grafana starting").with_status_code(503u16));
            return;
        }
    };
    let user = shared.webauth_user.lock().unwrap().clone().unwrap_or_default();
    let email = shared.webauth_email.lock().unwrap().clone().unwrap_or_default();

    let upstream = format!("http://127.0.0.1:{}{}", gport, req.url());
    let method = reqwest::Method::from_bytes(req.method().to_string().as_bytes())
        .unwrap_or(reqwest::Method::GET);

    // Read the request body (bounded by grafana's own limits upstream).
    let mut body = Vec::new();
    let _ = req.as_reader().read_to_end(&mut body);

    let mut rb = client.request(method, &upstream).body(body);
    // Forward original headers except Host + any inbound webauth spoof.
    for h in req.headers() {
        let name = h.field.as_str().as_str().to_ascii_lowercase();
        if name == "host"
            || name.starts_with("x-webauth-")
            || name == "content-length"
        {
            continue;
        }
        rb = rb.header(h.field.as_str().as_str(), h.value.as_str());
    }
    rb = rb
        .header("X-WEBAUTH-USER", &user)
        .header("X-WEBAUTH-EMAIL", &email)
        .header("X-WEBAUTH-NAME", &user);

    let resp = match rb.send() {
        Ok(r) => r,
        Err(e) => {
            let _ = req.respond(
                Response::from_string(format!("upstream error: {e}")).with_status_code(502u16),
            );
            return;
        }
    };

    let status = resp.status().as_u16();
    let mut headers = Vec::new();
    for (k, v) in resp.headers().iter() {
        let kn = k.as_str().to_ascii_lowercase();
        // Drop hop-by-hop + length headers tiny_http recomputes. KEEP
        // content-encoding: reqwest (built without the gzip feature) hands
        // back the still-compressed body, so the browser must see the
        // encoding header to decompress it — otherwise it renders gzip as
        // garbage.
        if kn == "transfer-encoding" || kn == "content-length" || kn == "connection" {
            continue;
        }
        if let Ok(h) = Header::from_bytes(k.as_str().as_bytes(), v.as_bytes()) {
            headers.push(h);
        }
    }
    let bytes = resp.bytes().map(|b| b.to_vec()).unwrap_or_default();
    let len = bytes.len();
    let response = Response::new(
        tiny_http::StatusCode(status),
        headers,
        std::io::Cursor::new(bytes),
        Some(len),
        None,
    );
    let _ = req.respond(response);
}

/// json_string quotes + escapes a string as a JSON string literal.
fn json_string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}
