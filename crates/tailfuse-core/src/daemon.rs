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
}

#[derive(Debug, Deserialize)]
struct StatusJson {
    #[serde(rename = "BackendState")]
    backend_state: Option<String>,
    #[serde(rename = "Self")]
    self_: Option<SelfJson>,
    #[serde(rename = "TailscaleIPs")]
    tailscale_ips: Option<Vec<String>>,
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

            let child = Command::new(&tailscaled)
                .args([
                    "--tun=userspace-networking".as_ref(),
                    OsStr::new("--statedir"),
                    state_dir.as_os_str(),
                    OsStr::new("--socket"),
                    socket.as_os_str(),
                    OsStr::new("--socks5-server"),
                    OsStr::new(&format!("127.0.0.1:{socks_port}")),
                    OsStr::new("--no-logs-no-flush"),
                ])
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
    let out = match tailscale_cli(&socket, &["login", "--accept-dns=false"]).await {
        Ok(o) => o,
        Err(_) => tailscale_cli(&socket, &["up", "--accept-dns=false"]).await?,
    };
    Ok(out
        .lines()
        .find(|l| l.contains("http"))
        .map(|l| l.trim().to_string()))
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
