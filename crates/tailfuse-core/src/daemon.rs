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
    /// Signed in but switched off (`tailscale down`): a deliberate disconnect.
    pub paused: bool,
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
    // PATH last: a program earlier on PATH must not stand in for an installed one.
    which_in_path("tailscaled")
}

pub fn find_tailscale() -> Option<PathBuf> {
    if let Ok(p) = std::env::var("TAILSCALE_PATH") {
        return Some(PathBuf::from(p));
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
    which_in_path("tailscale")
}

struct RunningProfile {
    child: Child,
    /// Stops `child` if Tailfuse dies without cleaning up (see `watchdog`);
    /// dropped, and so ended, with the profile on a normal stop.
    _watchdog: Option<Child>,
    socks_port: u16,
    socket: PathBuf,
}

/// Owns the per-profile tailscaled processes of a running session. Clones
/// share the same processes.
#[derive(Clone)]
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
            config::ensure_private_dir(&state_dir)?;
            // A tailscaled left over from a Tailfuse that was killed (not
            // quit) still holds this tailnet's state; a second one would then
            // fail, and the first listens on a socket file that is about to be
            // replaced, so nothing could reach either. Stop it first.
            stop_orphans(&socket).await;
            if socket.exists() {
                let _ = std::fs::remove_file(&socket);
            }

            let log_path = config::state_dir().join(format!("{name}.log"));
            let log_file = config::private_log(&log_path)?;
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

            kill_with_tailfuse(&child);
            let _watchdog = child.id().and_then(watchdog);
            set.inner.lock().await.insert(
                name.clone(),
                RunningProfile {
                    child,
                    _watchdog,
                    socks_port,
                    socket: socket.clone(),
                },
            );

            // Apply prefs now if the profile is already authenticated.
            let _ = apply_profile_prefs(name, prof, &socket).await;
            // A tailnet the switch has turned off stays off: a signed-in node
            // would otherwise reconnect on every spawn, dragging every
            // tailnet up whenever one of them connects. Wait until the daemon
            // answers, then hold it down in the background.
            if !prof.connected {
                let socket = socket.clone();
                let name = name.clone();
                tokio::spawn(async move {
                    for _ in 0..20 {
                        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
                        if tailscale_cli(&socket, &["status", "--json"]).await.is_ok() {
                            let _ = tokio::time::timeout(
                                std::time::Duration::from_secs(10),
                                tailscale_cli(&socket, &["down"]),
                            )
                            .await;
                            tracing::info!(profile = name, "held disconnected (switch off)");
                            break;
                        }
                    }
                });
            }
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

    /// What each running tailnet serves, for routing beyond configured suffixes:
    /// the Tailscale addresses of its online devices (not this node's own) and
    /// the domains its Tailscale DNS answers (split-DNS routes and search
    /// domains). A tailnet that doesn't answer is left out of this round.
    pub async fn learn(&self) -> Vec<(String, Vec<std::net::IpAddr>, Vec<String>)> {
        let sockets: Vec<(String, PathBuf)> = self
            .inner
            .lock()
            .await
            .iter()
            .map(|(n, r)| (n.clone(), r.socket.clone()))
            .collect();
        let mut out = Vec::new();
        for (name, socket) in sockets {
            let Ok(text) = tailscale_cli(&socket, &["status", "--json"]).await else {
                continue;
            };
            let ips = online_peer_ips(&text);
            let domains = tailscale_cli(&socket, &["dns", "status"])
                .await
                .map(|t| served_domains(&t))
                .unwrap_or_default();
            out.push((name, ips, domains));
        }
        out
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
                        }
                        ps.paused = st.backend_state.as_deref() == Some("Stopped");
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

/// A small `sh` that outlives Tailfuse if Tailfuse is killed: it polls once a
/// second, and when Tailfuse's process is gone it stops `child` (a SIGKILL'd
/// parent cannot clean up, and macOS has no parent-death signal). It checks the
/// pid still runs tailscaled before stopping it, and exits on its own once the
/// daemon is gone. On a normal stop Tailfuse ends it (kill_on_drop). Unix only;
/// on Windows, `kill_with_tailfuse` does this job.
fn watchdog(child: u32) -> Option<Child> {
    #[cfg(unix)]
    {
        const SCRIPT: &str = r#"p=$1; c=$2
while kill -0 "$p" 2>/dev/null && kill -0 "$c" 2>/dev/null; do sleep 1; done
if ! kill -0 "$p" 2>/dev/null; then
  ps -p "$c" -o command= 2>/dev/null | grep -q tailscaled && kill "$c" 2>/dev/null
fi"#;
        Command::new("/bin/sh")
            .args(["-c", SCRIPT, "tailfuse-watchdog"])
            .arg(std::process::id().to_string())
            .arg(child.to_string())
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .ok()
    }
    #[cfg(not(unix))]
    {
        let _ = child;
        None
    }
}

/// Windows: put `child` in a job object that ends every process in it when
/// Tailfuse's last handle to the job closes, which the OS does when Tailfuse
/// exits for any reason, a kill included. Elsewhere this does nothing (Unix
/// uses `watchdog`).
fn kill_with_tailfuse(child: &Child) {
    #[cfg(windows)]
    {
        use std::sync::OnceLock;
        use windows_sys::Win32::System::JobObjects::{
            AssignProcessToJobObject, CreateJobObjectW, JobObjectExtendedLimitInformation,
            SetInformationJobObject, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
            JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
        };
        // One job for the whole session, created on first use and never
        // closed by us: the OS closes it, and so ends the daemons, when
        // Tailfuse exits.
        static JOB: OnceLock<usize> = OnceLock::new();
        let job = *JOB.get_or_init(|| unsafe {
            let job = CreateJobObjectW(std::ptr::null(), std::ptr::null());
            if job.is_null() {
                return 0;
            }
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            SetInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                &info as *const _ as *const core::ffi::c_void,
                std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            );
            job as usize
        });
        if job == 0 {
            return;
        }
        if let Some(handle) = child.raw_handle() {
            unsafe {
                AssignProcessToJobObject(job as _, handle as _);
            }
        }
    }
    #[cfg(not(windows))]
    let _ = child;
}

/// Keep `routes` up to date with what each tailnet serves: now, then every 30
/// seconds, until `shutdown` turns true.
pub fn spawn_learning(
    daemons: DaemonSet,
    routes: std::sync::Arc<crate::router::RouteTable>,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) {
    tokio::spawn(async move {
        let mut wait = std::time::Duration::from_secs(3);
        loop {
            tokio::select! {
                _ = tokio::time::sleep(wait) => {}
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() { return; }
                }
            }
            for (name, ips, domains) in daemons.learn().await {
                routes.set_learned(&name, &ips, &domains);
            }
            routes.recheck_hosts().await;
            wait = std::time::Duration::from_secs(30);
        }
    });
}

/// Tailscale addresses of the online peers in a `tailscale status --json`.
fn online_peer_ips(text: &str) -> Vec<std::net::IpAddr> {
    let Ok(v) = serde_json::from_str::<serde_json::Value>(text) else {
        return Vec::new();
    };
    let Some(peers) = v["Peer"].as_object() else {
        return Vec::new();
    };
    peers
        .values()
        .filter(|p| p["Online"].as_bool().unwrap_or(false))
        .flat_map(|p| p["TailscaleIPs"].as_array().cloned().unwrap_or_default())
        .filter_map(|ip| ip.as_str()?.parse().ok())
        .collect()
}

/// The domains a tailnet's DNS answers, from `tailscale dns status`: its
/// split-DNS routes and search domains. Tailscale's own `ts.net` route (the
/// same on every tailnet) and single-label names are left out, so no tailnet
/// claims another's MagicDNS names or a whole top-level domain.
fn served_domains(text: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut section = "";
    for line in text.lines() {
        let t = line.trim();
        if t.ends_with(':') && !t.starts_with('-') {
            section = if t.starts_with("Split DNS Routes") {
                "split"
            } else if t == "Search Domains:" {
                "search"
            } else {
                ""
            };
            continue;
        }
        if t.starts_with("===") {
            section = "";
            continue;
        }
        let Some(item) = t.strip_prefix("- ") else {
            continue;
        };
        if section.is_empty() {
            continue;
        }
        let domain = item
            .split("->")
            .next()
            .unwrap_or("")
            .trim()
            .trim_end_matches('.')
            .to_lowercase();
        if domain.is_empty() || !domain.contains('.') || domain == "ts.net" {
            continue;
        }
        if !out.contains(&domain) {
            out.push(domain);
        }
    }
    out
}

/// Stop any orphaned `tailscaled` still serving `socket`: one whose Tailfuse is
/// gone, so the system has re-parented it to launchd/init (parent pid 1). A
/// daemon whose parent is alive belongs to a running Tailfuse (the app, or a
/// `tailfuse up` in a terminal) and is never touched. Unix only: on Windows the
/// job object ends a Tailfuse's daemons with it.
async fn stop_orphans(socket: &Path) {
    #[cfg(unix)]
    {
        let Ok(out) = Command::new("ps").args(["-axo", "pid=,ppid=,command="]).output().await else {
            return;
        };
        let needle = format!("--socket {}", socket.display());
        let pids: Vec<i32> = String::from_utf8_lossy(&out.stdout)
            .lines()
            .filter(|l| l.contains("tailscaled") && orphan_matches(l, &needle))
            .filter_map(|l| {
                let mut f = l.split_whitespace();
                let pid: i32 = f.next()?.parse().ok()?;
                let ppid: i32 = f.next()?.parse().ok()?;
                (ppid == 1).then_some(pid)
            })
            .collect();
        if pids.is_empty() {
            return;
        }
        tracing::warn!("stopping leftover tailscaled for {}: {pids:?}", socket.display());
        for pid in &pids {
            let _ = Command::new("kill").arg(pid.to_string()).status().await;
        }
        // Give them a moment to release the state directory.
        for _ in 0..20 {
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
            let alive = Command::new("kill")
                .args(["-0", &pids[0].to_string()])
                .status()
                .await
                .map(|s| s.success())
                .unwrap_or(false);
            if !alive {
                break;
            }
        }
    }
    #[cfg(not(unix))]
    let _ = socket;
}

/// True when a `ps` command line runs with exactly this `--socket` value (the
/// path is followed by a space or ends the line, so `a.sock` never matches
/// `a.sock2`).
fn orphan_matches(line: &str, needle: &str) -> bool {
    line.match_indices(needle).any(|(i, _)| {
        matches!(line[i + needle.len()..].chars().next(), None | Some(' '))
    })
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

/// Connect (`up`) or disconnect (`down`) one running profile's node. The
/// choice is kept by tailscaled, so it survives a restart of Tailfuse.
pub async fn set_connected(name: &str, on: bool) -> Result<()> {
    let socket = config::profile_socket(name);
    if !socket.exists() {
        bail!("{name} is not running; turn Tailfuse on first");
    }
    let args: &[&str] = if on {
        &["up", "--accept-dns=false"]
    } else {
        &["down"]
    };
    tokio::time::timeout(
        std::time::Duration::from_secs(15),
        tailscale_cli(&socket, args),
    )
    .await
    .context("Tailscale took too long to answer")??;
    Ok(())
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
    config::ensure_private_dir(&state_dir)?;
    if socket.exists() {
        let _ = std::fs::remove_file(&socket);
    }
    let log_path = config::state_dir().join(format!("{SIGNIN_SLOT}.log"));
    let log_file = config::private_log(&log_path)?;
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

#[cfg(test)]
mod orphan_tests {
    use super::orphan_matches;

    #[test]
    fn matches_only_the_exact_socket() {
        let line = "66801 /opt/homebrew/bin/tailscaled --tun=userspace-networking --statedir /x/profiles/ailio --socket /x/ailio.sock --socks5-server 127.0.0.1:43101";
        assert!(orphan_matches(line, "--socket /x/ailio.sock"));
        assert!(!orphan_matches(line, "--socket /x/ail"));
        assert!(!orphan_matches(line, "--socket /x/change.sock"));
        assert!(orphan_matches("1 tailscaled --socket /x/a.sock", "--socket /x/a.sock"));
    }
}

#[cfg(test)]
mod learn_tests {
    use super::{online_peer_ips, served_domains};

    #[test]
    fn domains_skip_ts_net_and_keep_split_and_search() {
        let text = "=== MagicDNS configuration ===\n\nSplit DNS Routes:\n  - ts.net.                        -> 199.247.155.53\n  - ailio.dev.                     -> 100.100.100.100\n\nSearch Domains:\n  - tail8e2dc7.ts.net\n  - corp.example\n\n=== System DNS configuration ===\n\nSearch domains:\n  - localdomain\n";
        assert_eq!(served_domains(text), vec!["ailio.dev", "tail8e2dc7.ts.net", "corp.example"]);
    }

    #[test]
    fn only_online_peers_count() {
        let text = r#"{"Peer":{"a":{"Online":true,"TailscaleIPs":["100.74.194.18","fd7a:115c:a1e0::1"]},"b":{"Online":false,"TailscaleIPs":["100.70.0.9"]}}}"#;
        let ips: Vec<String> = online_peer_ips(text).iter().map(|i| i.to_string()).collect();
        assert_eq!(ips, vec!["100.74.194.18", "fd7a:115c:a1e0::1"]);
    }
}
