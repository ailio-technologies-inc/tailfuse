//! Profile daemon management: one `tailscaled` subprocess per tailnet, run in
//! userspace networking mode so nothing touches the system routing table,
//! DNS, or firewall. Each instance exposes a dedicated local SOCKS5 proxy
//! which the router fans out to by hostname suffix.

use anyhow::{bail, Context, Result};
use serde::Deserialize;
use std::collections::BTreeMap;
use std::ffi::OsStr;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use tokio::process::{Child, Command};
use tokio::sync::Mutex;

/// Search PATH for a binary, splitting on the platform separator.
fn which_in_path(name: &str) -> Option<PathBuf> {
    std::env::var_os("PATH").and_then(|path| {
        std::env::split_paths(&path)
            .map(|dir| dir.join(name))
            .find(|cand| cand.exists())
    })
}

use crate::config;

#[derive(Debug, Clone, Default)]
pub struct ProfileStatus {
    pub name: String,
    pub running: bool,
    pub online: bool,
    pub hostname: String,
    pub tailscale_ips: Vec<String>,
    pub error: String,
    /// The login this node is signed in with, when known.
    pub account: String,
}

/// What Tailscale reports when a tailnet's policy needs its logging left on.
const LOGGING_REQUIRED: &str = "requires logging";

/// The login name of the signed-in user in a `tailscale status --json` text.
fn account_from_status(text: &str) -> String {
    let Ok(v) = serde_json::from_str::<serde_json::Value>(text) else {
        return String::new();
    };
    let id = v["Self"]["UserID"].to_string();
    v["User"][id.as_str()]["LoginName"]
        .as_str()
        .unwrap_or_default()
        .to_string()
}

#[derive(Debug, Deserialize)]
struct StatusJson {
    #[serde(rename = "BackendState")]
    backend_state: Option<String>,
    #[serde(rename = "Self")]
    self_: Option<SelfJson>,
    #[serde(rename = "TailscaleIPs")]
    tailscale_ips: Option<Vec<String>>,
    #[serde(rename = "Health")]
    health: Option<Vec<String>>,
}

#[derive(Debug, Deserialize)]
struct SelfJson {
    #[serde(rename = "HostName")]
    hostname: Option<String>,
}

/// Find the tailscaled binary: explicit env override, PATH, then common
/// install locations (Homebrew, Linux packages, macOS app bundle).
pub fn find_tailscaled() -> Option<PathBuf> {
    if let Ok(p) = std::env::var("TAILSCALED_PATH") {
        return Some(PathBuf::from(p));
    }
    if let Some(cand) = which_in_path("tailscaled") {
        return Some(cand);
    }
    for cand in [
        "/opt/homebrew/bin/tailscaled",
        "/usr/local/bin/tailscaled",
        "/usr/bin/tailscaled",
        "/usr/sbin/tailscaled",
        "/Applications/Tailscale.app/Contents/MacOS/tailscaled", // standalone macOS variant
    ] {
        let p = Path::new(cand);
        if p.exists() {
            return Some(p.to_path_buf());
        }
    }
    None
}

pub fn find_tailscale() -> Option<PathBuf> {
    if let Ok(p) = std::env::var("TAILSCALE_PATH") {
        return Some(PathBuf::from(p));
    }
    if let Some(cand) = which_in_path("tailscale") {
        return Some(cand);
    }
    for cand in [
        "/opt/homebrew/bin/tailscale",
        "/usr/local/bin/tailscale",
        "/usr/bin/tailscale",
        "/usr/sbin/tailscale",
    ] {
        let p = Path::new(cand);
        if p.exists() {
            return Some(p.to_path_buf());
        }
    }
    None
}

struct RunningProfile {
    child: Child,
    socks_port: u16,
    socket: PathBuf,
}

/// Owns the per-profile tailscaled processes of a running session.
pub struct DaemonSet {
    inner: Arc<Mutex<BTreeMap<String, RunningProfile>>>,
}

impl Default for DaemonSet {
    fn default() -> Self {
        Self {
            inner: Arc::new(Mutex::new(BTreeMap::new())),
        }
    }
}

impl DaemonSet {
    /// Start one tailscaled per profile. Already-authenticated profiles come
    /// up on their own; others sit logged-out until `login`.
    pub async fn start(cfg: &config::Config) -> Result<Self> {
        cfg.validate()?;
        let tailscaled = find_tailscaled().context(
            "tailscaled not found — install Tailscale (https://tailscale.com/download) or set TAILSCALED_PATH",
        )?;
        let set = Self::default();
        for (index, (name, prof)) in cfg.profiles.iter().enumerate() {
            let socks_port = config::profile_socks_port(index);
            let state_dir = config::profile_state_dir(name);
            let socket = config::profile_socket(name);
            std::fs::create_dir_all(&state_dir)?;
            if socket.exists() {
                let _ = std::fs::remove_file(&socket);
            }

            let log_path = config::state_dir().join(format!("{name}.log"));
            let log_file = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&log_path)?;
            let err_file = log_file.try_clone()?;

            let mut cmd = Command::new(&tailscaled);
            cmd.args([
                "--tun=userspace-networking".as_ref(),
                OsStr::new("--statedir"),
                state_dir.as_os_str(),
                OsStr::new("--socket"),
                socket.as_os_str(),
                OsStr::new("--socks5-server"),
                OsStr::new(&format!("127.0.0.1:{socks_port}")),
            ]);
            // Tailscale's own log upload stays off unless the tailnet demands it.
            if !prof.tailscale_logs {
                cmd.arg("--no-logs-no-support");
            }
            let child = cmd
                .stdin(Stdio::null())
                .stdout(Stdio::from(log_file))
                .stderr(Stdio::from(err_file))
                .kill_on_drop(true)
                .spawn()
                .with_context(|| format!("starting tailscaled for profile {name:?}"))?;

            set.inner.lock().await.insert(
                name.clone(),
                RunningProfile {
                    child,
                    socks_port,
                    socket: socket.clone(),
                },
            );

            // Apply prefs now if the profile is already authenticated.
            let _ = apply_profile_prefs(name, prof, &socket).await;
            tracing::info!(profile = name, %socks_port, "tailscaled started (userspace mode)");
        }
        Ok(set)
    }

    pub async fn socks_port(&self, profile: &str) -> Option<u16> {
        self.inner.lock().await.get(profile).map(|r| r.socks_port)
    }

    pub async fn socket(&self, profile: &str) -> Option<PathBuf> {
        self.inner
            .lock()
            .await
            .get(profile)
            .map(|r| r.socket.clone())
    }

    /// Poll `tailscale status --json` per profile.
    pub async fn status(&self) -> Vec<ProfileStatus> {
        let map = self.inner.lock().await;
        let mut out = Vec::new();
        for (name, r) in map.iter() {
            let mut ps = ProfileStatus {
                name: name.clone(),
                running: true,
                ..Default::default()
            };
            match tailscale_cli(&r.socket, &["status", "--json"]).await {
                Ok(text) => match serde_json::from_str::<StatusJson>(&text) {
                    Ok(st) => {
                        ps.account = account_from_status(&text);
                        if st
                            .health
                            .iter()
                            .flatten()
                            .any(|h| h.contains(LOGGING_REQUIRED))
                        {
                            ps.error = "This tailnet requires Tailscale's logging. Remove it here and add it again with Sign in with Tailscale.".into();
                        } else if st.backend_state.as_deref() == Some("Stopped") {
                            // Signed in but not connected: connect it.
                            let _ = tokio::time::timeout(
                                std::time::Duration::from_secs(5),
                                tailscale_cli(&r.socket, &["up", "--accept-dns=false"]),
                            )
                            .await;
                        }
                        ps.online = st.backend_state.as_deref() == Some("Running");
                        if let Some(s) = st.self_ {
                            ps.hostname = s.hostname.unwrap_or_default();
                        }
                        ps.tailscale_ips = st.tailscale_ips.unwrap_or_default();
                    }
                    Err(e) => ps.error = format!("parsing status: {e}"),
                },
                Err(e) => ps.error = e.to_string(),
            }
            out.push(ps);
        }
        out
    }

    pub async fn stop(&self) {
        let mut map = self.inner.lock().await;
        for (name, r) in map.iter_mut() {
            let _ = r.child.start_kill();
            tracing::info!(profile = name, "tailscaled stopped");
        }
        map.clear();
    }
}

/// Run `tailscale --socket <sock> <args>`, returning stdout on success.
pub async fn tailscale_cli(socket: &Path, args: &[&str]) -> Result<String> {
    let bin = find_tailscale().context("tailscale CLI not found")?;
    let out = Command::new(bin)
        .arg("--socket")
        .arg(socket)
        .args(args)
        .stdin(Stdio::null())
        .output()
        .await
        .context("running tailscale CLI")?;
    if !out.status.success() {
        bail!(
            "tailscale {} failed: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

/// The MagicDNS suffix of the tailnet the official Tailscale client on this
/// machine is logged into (e.g. `tail8e2dc7.ts.net`), or None when the client
/// is not installed, running or logged in. Used by `profile import`.
pub async fn detect_official_tailnet_suffix() -> Option<String> {
    let bin = find_tailscale()?;
    let out = Command::new(bin)
        .args(["status", "--json"])
        .stdin(Stdio::null())
        .output()
        .await
        .ok()?;
    if !out.status.success() {
        return None;
    }
    #[derive(Deserialize)]
    struct St {
        #[serde(rename = "CurrentTailnet")]
        current: Option<Tn>,
        #[serde(rename = "MagicDNSSuffix")]
        magic: Option<String>,
    }
    #[derive(Deserialize)]
    struct Tn {
        #[serde(rename = "MagicDNSSuffix")]
        magic: Option<String>,
    }
    let st: St = serde_json::from_slice(&out.stdout).ok()?;
    st.current.and_then(|t| t.magic).or(st.magic)
}
/// The official Tailscale client on this machine, when it is connected.
#[derive(Debug, Clone, Default)]
pub struct OfficialTailscale {
    /// The tailnet's display name, e.g. `example.com`.
    pub tailnet: String,
    /// MagicDNS suffix without the leading dot.
    pub suffix: String,
    /// The login it is signed in with.
    pub account: String,
}

/// `Some` while the official Tailscale client is installed and connected.
pub async fn official_tailscale() -> Option<OfficialTailscale> {
    let bin = find_tailscale()?;
    let out = tokio::time::timeout(
        std::time::Duration::from_secs(4),
        Command::new(bin)
            .args(["status", "--json"])
            .stdin(Stdio::null())
            .output(),
    )
    .await
    .ok()?
    .ok()?;
    let text = String::from_utf8_lossy(&out.stdout).to_string();
    let st: LoginStatus = serde_json::from_str(&text).ok()?;
    if st.backend_state.as_deref() != Some("Running") {
        return None;
    }
    Some(OfficialTailscale {
        tailnet: st
            .current
            .as_ref()
            .and_then(|t| t.name.clone())
            .unwrap_or_default(),
        suffix: st
            .current
            .and_then(|t| t.magic)
            .or(st.magic)
            .unwrap_or_default(),
        account: account_from_status(&text),
    })
}

/// Disconnect the official Tailscale client (`tailscale down`). It stays
/// signed in and can be reconnected from its own menu.
pub async fn official_tailscale_disconnect() -> Result<()> {
    let bin = find_tailscale().context("tailscale CLI not found")?;
    let out = Command::new(bin)
        .arg("down")
        .stdin(Stdio::null())
        .output()
        .await
        .context("running tailscale down")?;
    if !out.status.success() {
        bail!(
            "Tailscale would not disconnect: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    Ok(())
}

/// Begin authenticating a profile. Returns `None` when the profile used an
/// auth key (already done), `Some(url)` for interactive login — the caller
/// shows/opens the URL and then calls [`login_wait`].
pub async fn login_start(cfg: &config::Config, name: &str) -> Result<Option<String>> {
    let prof = cfg
        .profiles
        .get(name)
        .with_context(|| format!("no such profile {name:?}"))?;
    let socket = config::profile_socket(name);
    if !socket.exists() {
        bail!("profile {name:?} is not running — start `tailfuse up` first");
    }
    if !prof.auth_key.is_empty() {
        tailscale_cli(
            &socket,
            &["up", "--auth-key", &prof.auth_key, "--accept-dns=false"],
        )
        .await?;
        return Ok(None);
    }
    begin_browser_login(&socket).await
}

/// What `tailscale status --json` says about a login in progress.
#[derive(Debug, Deserialize, Default)]
struct LoginStatus {
    #[serde(rename = "BackendState")]
    backend_state: Option<String>,
    #[serde(rename = "AuthURL")]
    auth_url: Option<String>,
    #[serde(rename = "CurrentTailnet")]
    current: Option<TailnetJson>,
    #[serde(rename = "MagicDNSSuffix")]
    magic: Option<String>,
    #[serde(rename = "Health")]
    health: Option<Vec<String>>,
    #[serde(skip)]
    account: String,
}

#[derive(Debug, Deserialize)]
struct TailnetJson {
    #[serde(rename = "Name")]
    name: Option<String>,
    #[serde(rename = "MagicDNSSuffix")]
    magic: Option<String>,
}

async fn login_status(socket: &Path) -> Option<LoginStatus> {
    let text = tailscale_cli(socket, &["status", "--json"]).await.ok()?;
    let mut st: LoginStatus = serde_json::from_str(&text).ok()?;
    st.account = account_from_status(&text);
    Some(st)
}

/// Ask the daemon on `socket` for an interactive login and return the page to
/// open in a browser (`None` when it is already signed in). `tailscale login`
/// stays in the foreground until the browser step is done, so it is left
/// running on its own and the link is read from the daemon's status instead.
async fn begin_browser_login(socket: &Path) -> Result<Option<String>> {
    let bin = find_tailscale().context("tailscale CLI not found")?;
    Command::new(bin)
        .arg("--socket")
        .arg(socket)
        .args(["login", "--accept-dns=false"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .context("running tailscale login")?;
    for _ in 0..60 {
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        if let Some(st) = login_status(socket).await {
            if st.backend_state.as_deref() == Some("Running") {
                return Ok(None);
            }
            if let Some(url) = st.auth_url.filter(|u| !u.is_empty()) {
                return Ok(Some(url));
            }
        }
    }
    bail!("Tailscale did not offer a sign-in page")
}

/// State slot used while signing in to a tailnet that has no profile yet.
const SIGNIN_SLOT: &str = "signin-pending";

/// A tailnet being signed in to before it has a profile: its own throwaway
/// tailscaled, which lets the browser login say which tailnet this is.
pub struct NewTailnet {
    child: Child,
    socket: PathBuf,
    /// Running with Tailscale's logging allowed (the tailnet asked for it).
    logs: bool,
    /// The page to open in a browser.
    pub url: Option<String>,
}

/// The tailnet a finished sign-in landed in.
#[derive(Debug, Clone)]
pub struct SignedIn {
    /// MagicDNS suffix without the leading dot, e.g. `tail8e2dc7.ts.net`.
    pub suffix: String,
    /// The tailnet's display name, e.g. `example.com`; may be empty.
    pub tailnet: String,
    /// The login used, e.g. `someone@example.com`; may be empty.
    pub account: String,
    /// The tailnet requires Tailscale's logging to stay on.
    pub needs_logs: bool,
}

/// Start the throwaway tailscaled for a sign-in and wait for its socket.
async fn spawn_signin_daemon(allow_logs: bool) -> Result<Child> {
    let tailscaled = find_tailscaled().context(
        "tailscaled not found — install Tailscale (https://tailscale.com/download) or set TAILSCALED_PATH",
    )?;
    let state_dir = config::profile_state_dir(SIGNIN_SLOT);
    let socket = config::profile_socket(SIGNIN_SLOT);
    std::fs::create_dir_all(&state_dir)?;
    if socket.exists() {
        let _ = std::fs::remove_file(&socket);
    }
    let log_path = config::state_dir().join(format!("{SIGNIN_SLOT}.log"));
    let log_file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&log_path)?;
    let err_file = log_file.try_clone()?;
    let mut cmd = Command::new(&tailscaled);
    cmd.args([
        "--tun=userspace-networking".as_ref(),
        OsStr::new("--statedir"),
        state_dir.as_os_str(),
        OsStr::new("--socket"),
        socket.as_os_str(),
    ]);
    if !allow_logs {
        cmd.arg("--no-logs-no-support");
    }
    let child = cmd
        .stdin(Stdio::null())
        .stdout(Stdio::from(log_file))
        .stderr(Stdio::from(err_file))
        .kill_on_drop(true)
        .spawn()
        .context("starting tailscaled for the sign-in")?;
    // The socket appears a moment after the daemon starts.
    for _ in 0..50 {
        if socket.exists() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    }
    Ok(child)
}

/// Start signing in to a tailnet that is not configured yet.
pub async fn new_tailnet_start() -> Result<NewTailnet> {
    let _ = std::fs::remove_dir_all(config::profile_state_dir(SIGNIN_SLOT));
    let child = spawn_signin_daemon(false).await?;
    let socket = config::profile_socket(SIGNIN_SLOT);
    let url = begin_browser_login(&socket).await?;
    Ok(NewTailnet {
        child,
        socket,
        logs: false,
        url,
    })
}

impl NewTailnet {
    /// Wait for the browser step (about five minutes at most), report which
    /// tailnet it was, and stop the throwaway daemon. Its state stays on disk
    /// for [`adopt_signin_state`].
    pub async fn finish(mut self) -> Result<SignedIn> {
        let mut found = None;
        for _ in 0..150 {
            tokio::time::sleep(std::time::Duration::from_secs(2)).await;
            let Some(st) = login_status(&self.socket).await else {
                continue;
            };
            // Some tailnets refuse a node whose Tailscale logging is off: the
            // login is done, so carry on with the same state, logging allowed.
            if !self.logs
                && st
                    .health
                    .iter()
                    .flatten()
                    .any(|h| h.contains(LOGGING_REQUIRED))
            {
                let _ = self.child.start_kill();
                let _ = self.child.wait().await;
                self.child = spawn_signin_daemon(true).await?;
                self.logs = true;
                continue;
            }
            let state = st.backend_state.as_deref().unwrap_or_default();
            let suffix = st
                .current
                .as_ref()
                .and_then(|t| t.magic.clone())
                .or(st.magic.clone())
                .filter(|s| !s.is_empty());
            // Signed in but not connected (it was held back before): connect it.
            if state == "Stopped" && suffix.is_some() {
                let _ = tokio::time::timeout(
                    std::time::Duration::from_secs(5),
                    tailscale_cli(&self.socket, &["up", "--accept-dns=false"]),
                )
                .await;
                continue;
            }
            if state == "Running" {
                if let Some(suffix) = suffix {
                    found = Some(SignedIn {
                        suffix: suffix.trim_matches('.').to_lowercase(),
                        tailnet: st.current.and_then(|t| t.name).unwrap_or_default(),
                        account: st.account,
                        needs_logs: self.logs,
                    });
                    break;
                }
            }
        }
        let _ = self.child.start_kill();
        let _ = self.child.wait().await;
        let _ = std::fs::remove_file(&self.socket);
        found.context("the sign-in was not finished in time")
    }
}

/// Hand the signed-in state over to the profile called `name`, so that
/// profile starts already authenticated.
pub fn adopt_signin_state(name: &str) -> Result<()> {
    let from = config::profile_state_dir(SIGNIN_SLOT);
    let to = config::profile_state_dir(name);
    let _ = std::fs::remove_dir_all(&to);
    std::fs::rename(&from, &to).with_context(|| format!("saving the sign-in for {name:?}"))
}

/// Throw away a sign-in that will not be used.
pub fn discard_signin_state() {
    let _ = std::fs::remove_dir_all(config::profile_state_dir(SIGNIN_SLOT));
}

/// Poll until the profile finishes authenticating (or ~8 minutes pass).
pub async fn login_wait(name: &str) -> Result<()> {
    let socket = config::profile_socket(name);
    for _ in 0..240 {
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
        if let Ok(text) = tailscale_cli(&socket, &["status", "--json"]).await {
            if let Ok(st) = serde_json::from_str::<StatusJson>(&text) {
                if st.backend_state.as_deref() == Some("Running") {
                    return Ok(());
                }
            }
        }
    }
    bail!("timed out waiting for authentication")
}

/// Convenience: start + wait, returning the URL when interactive.
pub async fn login(cfg: &config::Config, name: &str) -> Result<Option<String>> {
    let url = login_start(cfg, name).await?;
    if url.is_some() {
        login_wait(name).await?;
    }
    Ok(url)
}

/// Best-effort application of accept_routes / exit_node on an already-auth'd profile.
async fn apply_profile_prefs(name: &str, prof: &config::Profile, socket: &Path) -> Result<()> {
    if prof.accept_routes {
        let _ = tailscale_cli(socket, &["set", "--accept-routes"]).await;
    }
    if !prof.exit_node.is_empty() {
        // Resolve the peer name to an IP via status, then set exit node.
        if let Ok(text) = tailscale_cli(socket, &["status", "--json"]).await {
            #[derive(Deserialize)]
            struct St {
                #[serde(rename = "Peer")]
                peer: Option<BTreeMap<String, PeerJson>>,
            }
            #[derive(Deserialize)]
            struct PeerJson {
                #[serde(rename = "HostName")]
                hostname: Option<String>,
                #[serde(rename = "TailscaleIPs")]
                ips: Option<Vec<String>>,
            }
            if let Ok(st) = serde_json::from_str::<St>(&text) {
                let want = prof.exit_node.to_lowercase();
                for (_, p) in st.peer.unwrap_or_default() {
                    let hn = p.hostname.unwrap_or_default().to_lowercase();
                    if hn == want || hn.starts_with(&format!("{want}.")) {
                        if let Some(ip) = p.ips.and_then(|v| v.into_iter().next()) {
                            let _ = tailscale_cli(socket, &["set", "--exit-node", &ip]).await;
                            tracing::info!(profile = name, %ip, "exit node set");
                        }
                        break;
                    }
                }
            }
        }
    }
    Ok(())
}
