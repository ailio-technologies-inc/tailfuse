//! tailfuse desktop app: menu-bar companion to the CLI, backed by
//! tailfuse-core. Shows per-tailnet state, drives login, manages the router
//! and system PAC, and keeps everything inside one user-process — no root
//! helpers, no kernel extensions.
//!
//! Menu-bar behavior: on macOS the app is accessory-only (no Dock icon), the
//! window hides at launch and on close, the tray menu shows live profile
//! state, and Start/Stop works from the tray.

use anyhow::Result;
use serde::Serialize;
use std::collections::BTreeMap;
use std::path::PathBuf;
mod updates;

use std::sync::Arc;
use tailfuse_core::config::{self, Config, Profile, Tunnel};
use tailfuse_core::{daemon, license, router, sync, sysproxy};
use tauri::menu::{CheckMenuItem, Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, State, WindowEvent};
use tauri_plugin_autostart::ManagerExt as AutostartExt;
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_opener::OpenerExt;
use tokio::sync::{watch, Mutex};

const TRAY_ID: &str = "tailfuse-tray";
const TRAY_REFRESH_MS: u64 = 3000;

struct Running {
    daemons: daemon::DaemonSet,
    shutdown: watch::Sender<bool>,
    /// The live routing table, including what the tailnets serve.
    routes: Arc<router::RouteTable>,
}

struct AppState {
    cfg_path: PathBuf,
    cfg: Mutex<Config>,
    running: Mutex<Option<Running>>,
    /// Why the system proxy could not be set at the last start; empty when it was.
    proxy_problem: Mutex<String>,
}

type SharedState = Arc<AppState>;

#[derive(Serialize, Clone)]
struct ProfileView {
    name: String,
    suffixes: Vec<String>,
    online: bool,
    hostname: String,
    ips: Vec<String>,
    error: String,
    /// The login this tailnet is signed in with, when known.
    account: String,
    /// Signed in but disconnected on purpose.
    paused: bool,
}

#[derive(Serialize)]
struct StateView {
    up: bool,
    pac_url: String,
    license: String,
    /// Who the active license key belongs to; None without a valid key.
    licensed_to: Option<String>,
    profiles: Vec<ProfileView>,
    tunnels: Vec<Tunnel>,
    /// Why browsers are not being pointed at Tailfuse, when they are not.
    proxy_problem: String,
    /// The official Tailscale app, while it is connected.
    tailscale_app: Option<TailscaleAppView>,
    /// Where the `tailfuse` command is installed; empty when it is not.
    cli: String,
    /// This build's version (the About pane).
    version: String,
}

#[derive(Serialize)]
struct TailscaleAppView {
    tailnet: String,
    account: String,
    /// The Tailfuse tailnet that is the same network, if there is one.
    same_as: String,
}

#[derive(Serialize)]
struct PhoneSync {
    svg: String,
    link: String,
    profiles: usize,
    tunnels: usize,
    tunnels_dropped: bool,
}

/// The code the phone app scans to set up the same tailnets: setup only, never keys.
#[tauri::command]
async fn phone_sync(state: State<'_, SharedState>) -> Result<PhoneSync, String> {
    let cfg = state.cfg.lock().await.clone();
    let out = sync::link(&cfg, &sync::computer_name()).map_err(|e| format!("{e:#}"))?;
    let svg = sync::qr_svg(&out.link).map_err(|e| format!("{e:#}"))?;
    Ok(PhoneSync {
        svg,
        link: out.link,
        profiles: out.profiles,
        tunnels: out.tunnels,
        tunnels_dropped: out.tunnels_dropped,
    })
}

#[tauri::command]
async fn get_state(app: AppHandle, state: State<'_, SharedState>) -> Result<StateView, String> {
    let cfg = state.cfg.lock().await.clone();
    let running = state.running.lock().await;
    let (up, profiles) = match running.as_ref() {
        Some(r) => {
            let stats = r.daemons.status().await;
            let mut views = Vec::new();
            for (name, prof) in &cfg.profiles {
                let st = stats.iter().find(|s| s.name == *name);
                views.push(ProfileView {
                    name: name.clone(),
                    suffixes: prof.suffixes.clone(),
                    online: st.map(|s| s.online).unwrap_or(false),
                    hostname: st.map(|s| s.hostname.clone()).unwrap_or_default(),
                    ips: st.map(|s| s.tailscale_ips.clone()).unwrap_or(vec![]),
                    error: st.map(|s| s.error.clone()).unwrap_or_default(),
                    account: st
                        .map(|s| s.account.clone())
                        .filter(|a| !a.is_empty())
                        .unwrap_or_else(|| prof.account.clone()),
                    paused: st.map(|s| s.paused).unwrap_or(false),
                });
            }
            (true, views)
        }
        None => (
            false,
            cfg.profiles
                .iter()
                .map(|(name, prof)| ProfileView {
                    name: name.clone(),
                    suffixes: prof.suffixes.clone(),
                    online: false,
                    hostname: String::new(),
                    ips: vec![],
                    error: String::new(),
                    account: prof.account.clone(),
                    paused: false,
                })
                .collect(),
        ),
    };
    Ok(StateView {
        up,
        pac_url: format!("http://{}/tailfuse.pac", cfg.router.listen),
        license: license::status(cfg.profiles.len(), &cfg.license),
        licensed_to: if cfg.license.trim().is_empty() {
            None
        } else {
            license::verify(&cfg.license).ok().map(|l| l.email)
        },
        profiles,
        tunnels: cfg.tunnels.clone(),
        proxy_problem: if up {
            state.proxy_problem.lock().await.clone()
        } else {
            String::new()
        },
        tailscale_app: daemon::official_tailscale()
            .await
            .map(|t| TailscaleAppView {
                same_as: cfg
                    .owner(&format!("host.{}", t.suffix))
                    .map(|(name, _)| name.to_string())
                    .unwrap_or_default(),
                tailnet: t.tailnet,
                account: t.account,
            }),
        cli: cli_path()
            .map(|p| p.display().to_string())
            .unwrap_or_default(),
        version: app.package_info().version.to_string(),
    })
}

/// Start profiles + router + PAC. Shared by the Start command and tray toggle.
async fn start_inner(state: &AppState) -> Result<(), String> {
    // The menu-bar icon animates while Tailfuse starts (spawn_tray_animation).
    STARTING.store(true, std::sync::atomic::Ordering::Relaxed);
    watch_connecting();
    let res = start_inner_locked(state).await;
    STARTING.store(false, std::sync::atomic::Ordering::Relaxed);
    res
}

/// True while Tailfuse is starting its tailnets.
static STARTING: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);
/// Until when (ms since the epoch) the menu-bar animation watches tailnets
/// come up; 0 when it is not watching.
static WATCH_UNTIL: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

/// Something just asked tailnets to come up (Tailfuse turned on, a tailnet's
/// switch): watch them for up to 90 s and animate the menu-bar icon meanwhile.
fn watch_connecting() {
    WATCH_UNTIL.store(now_ms() + 90_000, std::sync::atomic::Ordering::Relaxed);
}

/// Animate the menu-bar icon while tailnets connect: a spark runs round the
/// dots (icons/tray-connecting, drawn by scripts/gen-icons.sh).
///
/// It costs nothing at rest: tailnets are only asked for their state (one
/// `tailscale status` each) once a second for the short while after something
/// asked them to come up (watch_connecting), and the watch ends as soon as
/// none is on its way up, or after 90 s. A tailnet waiting for a sign-in does
/// not count as on its way up.
fn spawn_tray_animation(app: AppHandle) {
    const FRAMES: [&[u8]; 10] = [
        include_bytes!("../icons/tray-connecting/0.png"),
        include_bytes!("../icons/tray-connecting/1.png"),
        include_bytes!("../icons/tray-connecting/2.png"),
        include_bytes!("../icons/tray-connecting/3.png"),
        include_bytes!("../icons/tray-connecting/4.png"),
        include_bytes!("../icons/tray-connecting/5.png"),
        include_bytes!("../icons/tray-connecting/6.png"),
        include_bytes!("../icons/tray-connecting/7.png"),
        include_bytes!("../icons/tray-connecting/8.png"),
        include_bytes!("../icons/tray-connecting/9.png"),
    ];
    let frames: Vec<tauri::image::Image<'static>> = FRAMES
        .iter()
        .filter_map(|b| tauri::image::Image::from_bytes(b).ok())
        .collect();
    let Ok(rest) = tauri::image::Image::from_bytes(include_bytes!("../icons/tray-36.png")) else {
        return;
    };
    if frames.is_empty() {
        return;
    }
    tauri::async_runtime::spawn(async move {
        use std::sync::atomic::Ordering::Relaxed;
        use std::time::{Duration, Instant};
        let mut connecting = false;
        let mut polled = Instant::now() - Duration::from_secs(10);
        // The watch this loop last saw, and how many polls of it found nothing coming up.
        let mut seen_watch = 0u64;
        let mut quiet_polls = 0u32;
        let mut animating = false;
        let mut frame = 0usize;
        loop {
            let starting = STARTING.load(Relaxed);
            let until = WATCH_UNTIL.load(Relaxed);
            if until != seen_watch {
                seen_watch = until;
                quiet_polls = 0;
            }
            let watching = starting || now_ms() < until;
            if !watching {
                connecting = false;
            } else if starting {
                connecting = true;
            } else if polled.elapsed() >= Duration::from_secs(1) {
                polled = Instant::now();
                let daemons =
                    app.try_state::<SharedState>()
                        .map(|state| match state.running.try_lock() {
                            Ok(g) => Ok(g.as_ref().map(|r| r.daemons.clone())),
                            Err(_) => Err(()),
                        });
                connecting = match daemons {
                    Some(Ok(Some(d))) => d.status().await.iter().any(|s| s.connecting),
                    Some(Ok(None)) | None => false,
                    // Busy (a start or stop holds it): keep the last answer.
                    Some(Err(())) => connecting,
                };
                // A switched-on node can still read "Stopped" for a moment, so
                // the watch ends only after a few quiet polls in a row.
                if connecting {
                    quiet_polls = 0;
                } else {
                    quiet_polls += 1;
                    if quiet_polls >= 3 {
                        let _ = WATCH_UNTIL.compare_exchange(until, 0, Relaxed, Relaxed);
                    }
                }
            }
            if let Some(tray) = app.tray_by_id(TRAY_ID) {
                if connecting {
                    let _ = tray.set_icon(Some(frames[frame % frames.len()].clone()));
                    frame += 1;
                    animating = true;
                } else if animating {
                    let _ = tray.set_icon(Some(rest.clone()));
                    animating = false;
                    frame = 0;
                }
            }
            // At rest this only reads two atomics twice a second.
            tokio::time::sleep(Duration::from_millis(if connecting { 110 } else { 500 })).await;
        }
    });
}

async fn start_inner_locked(state: &AppState) -> Result<(), String> {
    let mut running = state.running.lock().await;
    if running.is_some() {
        return Ok(());
    }
    let cfg = state.cfg.lock().await.clone();
    license::check(cfg.profiles.len(), &cfg.license).map_err(|e| e.to_string())?;
    cfg.validate().map_err(|e| e.to_string())?;
    let daemons = daemon::DaemonSet::start(&cfg)
        .await
        .map_err(|e| e.to_string())?;

    let mut socks_ports = BTreeMap::new();
    for (i, name) in cfg.profiles.keys().enumerate() {
        socks_ports.insert(name.clone(), config::profile_socks_port(i));
    }
    let routes = router::RouteTable::build(&cfg, &socks_ports);
    let r = Arc::new(router::Router::new(&cfg, &cfg.router.listen, routes));
    let (shutdown, rx) = watch::channel(false);
    if let Err(e) = r.clone().serve(&cfg.router.listen, rx.clone()).await {
        return Err(format!("router: {e:#}"));
    }
    // Learn what every connected tailnet serves before browsers fetch the PAC.
    daemons.learn_initial(&cfg, &r.routes()).await;
    daemon::spawn_learning(daemons.clone(), r.routes(), rx);
    // Best-effort system PAC; the window shows the manual URL regardless.
    let pac_url = format!("http://{}/tailfuse.pac", cfg.router.listen);
    *state.proxy_problem.lock().await = match sysproxy::apply(&pac_url) {
        Ok(()) => String::new(),
        Err(e) if sysproxy::needs_permission(&e) => "permission".into(),
        Err(e) => {
            tracing::warn!("system PAC: {e:#}");
            format!("{e:#}")
        }
    };
    *running = Some(Running {
        daemons,
        shutdown,
        routes: r.routes(),
    });
    Ok(())
}

/// Stop everything we started. Shared by the Stop command and tray toggle.
async fn stop_inner(state: &AppState) {
    let mut running = state.running.lock().await;
    if let Some(r) = running.take() {
        let _ = r.shutdown.send(true);
        r.daemons.stop().await;
        let _ = sysproxy::clear();
    }
}

#[tauri::command]
async fn start(state: State<'_, SharedState>) -> Result<(), String> {
    start_inner(&state).await
}

#[tauri::command]
async fn stop(state: State<'_, SharedState>) -> Result<(), String> {
    stop_inner(&state).await;
    Ok(())
}

/// Rebuild the tray menu from the given state snapshot.
fn rebuild_tray_menu(app: &AppHandle, state: &AppState, stats: &[daemon::ProfileStatus]) {
    let Ok(cfg) = config::load(&state.cfg_path) else {
        return;
    };
    let is_up = state
        .running
        .try_lock()
        .map(|g| g.is_some())
        .unwrap_or(true);
    let any_online = stats.iter().any(|s| s.online);
    let mut items: Vec<Box<dyn tauri::menu::IsMenuItem<tauri::Wry>>> = Vec::new();

    // Status line first, the macOS menu-bar convention: what the app is doing
    // right now, then the things, then the actions.
    let header = if !is_up {
        "Tailfuse is off"
    } else if any_online {
        "Tailfuse is connected"
    } else {
        "Tailfuse is waiting for sign-in"
    };
    if let Ok(item) = MenuItem::with_id(app, "status", header, false, None::<&str>) {
        items.push(Box::new(item));
    }

    // One live item per profile: the state is in the label, the click acts —
    // an online profile opens the window, a logged-out one starts its login.
    if cfg.profiles.is_empty() {
        if let Ok(item) =
            MenuItem::with_id(app, "no-profiles", "No tailnets yet", false, None::<&str>)
        {
            items.push(Box::new(item));
        }
    }
    for name in cfg.profiles.keys() {
        let st = stats.iter().find(|s| s.name == *name);
        let online = st.map(|s| s.online).unwrap_or(false);
        let paused = st.map(|s| s.paused).unwrap_or(false);
        let label = if !is_up || online || paused {
            name.clone()
        } else {
            format!("Sign in to {name}…")
        };
        // A checkmark means connected; choosing it connects or disconnects.
        if let Ok(item) = CheckMenuItem::with_id(
            app,
            format!("profile-{name}"),
            label,
            true,
            online,
            None::<&str>,
        ) {
            items.push(Box::new(item));
        }
    }
    if let Ok(sep) = PredefinedMenuItem::separator(app) {
        items.push(Box::new(sep));
    }
    if let Ok(show) = MenuItem::with_id(app, "show", "Open Tailfuse", true, None::<&str>) {
        items.push(Box::new(show));
    }
    let toggle_label = if is_up {
        "Turn Tailfuse off"
    } else {
        "Turn Tailfuse on"
    };
    if let Ok(toggle) = MenuItem::with_id(app, "toggle", toggle_label, true, None::<&str>) {
        items.push(Box::new(toggle));
    }
    if let Ok(sep) = PredefinedMenuItem::separator(app) {
        items.push(Box::new(sep));
    }
    // The system's About panel: name, icon, version and copyright.
    let info = app.package_info();
    let about = tauri::menu::AboutMetadata {
        name: Some("Tailfuse".into()),
        version: Some(info.version.to_string()),
        short_version: Some(info.version.to_string()),
        copyright: Some("© Ailio Technologies Inc.".into()),
        website: Some("https://tailfuse.com".into()),
        website_label: Some("tailfuse.com".into()),
        icon: app.default_window_icon().cloned(),
        ..Default::default()
    };
    if let Ok(item) = PredefinedMenuItem::about(app, Some("About Tailfuse"), Some(about)) {
        items.push(Box::new(item));
    }
    if let Ok(quit) = MenuItem::with_id(app, "quit", "Quit Tailfuse", true, None::<&str>) {
        items.push(Box::new(quit));
    }
    let refs: Vec<&dyn tauri::menu::IsMenuItem<tauri::Wry>> =
        items.iter().map(|b| b.as_ref()).collect();
    let Ok(menu) = Menu::with_items(app, &refs) else {
        return;
    };
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        if let Err(e) = tray.set_menu(Some(menu)) {
            tracing::warn!("tray menu update: {e}");
        }
    }
}

/// Snapshot state and refresh the tray menu. Never fatal — the tray is
/// decorative.
fn refresh_tray(app: &AppHandle) {
    let Some(state) = app.try_state::<SharedState>() else {
        return;
    };
    let state = Arc::clone(&state);
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let stats = match state.running.lock().await.as_ref() {
            Some(r) => r.daemons.status().await,
            None => vec![],
        };
        rebuild_tray_menu(&app, &state, &stats);
    });
}

#[tauri::command]
async fn login(
    state: State<'_, SharedState>,
    name: String,
    app: AppHandle,
) -> Result<Option<String>, String> {
    let cfg = state.cfg.lock().await.clone();
    if state.running.lock().await.is_none() {
        return Err("turn Tailfuse on first".into());
    }
    let url = daemon::login_start(&cfg, &name)
        .await
        .map_err(|e| e.to_string())?;
    if let Some(u) = &url {
        let _ = app.opener().open_url(u, None::<&str>);
        let name = name.clone();
        tokio::spawn(async move {
            if let Err(e) = daemon::login_wait(&name).await {
                tracing::warn!(profile = %name, "login: {e:#}");
            }
        });
    }
    Ok(url)
}

/// Mail domains that say nothing about whose tailnet this is.
const PERSONAL_MAIL: &[&str] = &[
    "gmail",
    "googlemail",
    "outlook",
    "hotmail",
    "live",
    "msn",
    "icloud",
    "me",
    "mac",
    "yahoo",
    "proton",
    "protonmail",
    "pm",
    "fastmail",
    "hey",
    "aol",
    "gmx",
    "github",
];

/// A short profile name from the account that signed in: `someone@example.com`
/// gives `example`, `someone@gmail.com` gives `someone`. Falls back to the
/// tailnet's own name, then its MagicDNS label. Made unique.
fn name_for_tailnet(cfg: &Config, found: &daemon::SignedIn) -> String {
    let clean = |text: &str| -> String {
        text.to_lowercase()
            .chars()
            .filter(|c| c.is_ascii_alphanumeric() || *c == '-')
            .take(24)
            .collect()
    };
    let first_label = |text: &str| clean(text.split('.').next().unwrap_or(""));
    let mut base = match found.account.split_once('@') {
        Some((user, domain)) => {
            let org = first_label(domain);
            if org.is_empty() || PERSONAL_MAIL.contains(&org.as_str()) {
                clean(user)
            } else {
                org
            }
        }
        None => String::new(),
    };
    if base.is_empty() {
        base = first_label(found.tailnet.split('@').next_back().unwrap_or(""));
    }
    if base.is_empty() {
        base = first_label(&found.suffix);
    }
    if base.is_empty() {
        base = "tailnet".into();
    }
    let mut name = base.clone();
    let mut n = 2;
    while cfg.profiles.contains_key(&name) {
        name = format!("{base}-{n}");
        n += 1;
    }
    name
}

/// Add a tailnet the way the Tailscale app does: sign in in the browser, and
/// the tailnet's own name and address suffix fill themselves in.
#[tauri::command]
async fn sign_in_new_tailnet(
    state: State<'_, SharedState>,
    app: AppHandle,
) -> Result<String, String> {
    {
        let cfg = state.cfg.lock().await;
        license::check(cfg.profiles.len() + 1, &cfg.license).map_err(|e| e.to_string())?;
    }
    let pending = daemon::new_tailnet_start()
        .await
        .map_err(|e| format!("{e:#}"))?;
    if let Some(u) = &pending.url {
        let _ = app.opener().open_url(u, None::<&str>);
    }
    let found = pending.finish().await.map_err(|e| format!("{e:#}"))?;

    // A tailnet that is already listed is signed in where it stands; a new one gets a profile.
    let name = {
        let mut cfg = state.cfg.lock().await;
        let existing = cfg
            .owner(&format!("host.{}", found.suffix))
            .map(|(owner, _)| owner.to_string());
        match existing {
            Some(owner) => {
                if let Some(prof) = cfg.profiles.get_mut(&owner) {
                    prof.account = found.account.clone();
                    prof.tailscale_logs = found.needs_logs;
                }
                cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
                owner
            }
            None => {
                let name = name_for_tailnet(&cfg, &found);
                cfg.profiles.insert(
                    name.clone(),
                    Profile {
                        suffixes: vec![format!(".{}", found.suffix)],
                        account: found.account.clone(),
                        tailscale_logs: found.needs_logs,
                        ..Default::default()
                    },
                );
                let checked = cfg
                    .validate()
                    .map_err(|e| e.to_string())
                    .and_then(|_| {
                        license::check(cfg.profiles.len(), &cfg.license).map_err(|e| e.to_string())
                    })
                    .and_then(|_| cfg.save(&state.cfg_path).map_err(|e| e.to_string()));
                if let Err(e) = checked {
                    cfg.profiles.remove(&name);
                    daemon::discard_signin_state();
                    return Err(e);
                }
                name
            }
        }
    };

    // Hand the finished sign-in to that profile and bring everything (back) up.
    stop_inner(&state).await;
    daemon::adopt_signin_state(&name).map_err(|e| format!("{e:#}"))?;
    start_inner(&state).await?;
    refresh_tray(&app);
    notify(
        &app,
        "Tailnet signed in",
        &format!("{name} is signed in and coming online."),
    );
    Ok(name)
}

/// A native system notification; best effort.
fn notify(app: &AppHandle, title: &str, body: &str) {
    let _ = app.notification().builder().title(title).body(body).show();
}

/// A native alert window for something that went wrong.
#[tauri::command]
fn alert_user(app: AppHandle, title: String, body: String) {
    use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
    app.dialog()
        .message(body)
        .title(title)
        .kind(MessageDialogKind::Warning)
        .show(|_| {});
}

/// Shown from the window for a problem the person may not be looking at.
#[tauri::command]
fn notify_user(app: AppHandle, title: String, body: String) {
    notify(&app, &title, &body);
}

/// Point the system's browsers at Tailfuse, asking macOS for an
/// administrator password. Done once; later starts find it in place.
#[tauri::command]
async fn connect_browsers(state: State<'_, SharedState>, app: AppHandle) -> Result<(), String> {
    let cfg = state.cfg.lock().await.clone();
    let pac_url = format!("http://{}/tailfuse.pac", cfg.router.listen);
    let res = tauri::async_runtime::spawn_blocking(move || sysproxy::apply_with_prompt(&pac_url))
        .await
        .map_err(|e| e.to_string())?;
    match res {
        Ok(()) => {
            state.proxy_problem.lock().await.clear();
            notify(
                &app,
                "System proxy set",
                "Your tailnet addresses now open as usual.",
            );
            Ok(())
        }
        Err(e) => Err(format!("{e:#}")),
    }
}

/// Take Tailfuse's automatic proxy address out of the system settings.
#[tauri::command]
async fn disconnect_browsers(state: State<'_, SharedState>) -> Result<(), String> {
    tauri::async_runtime::spawn_blocking(sysproxy::clear_with_prompt)
        .await
        .map_err(|e| e.to_string())?
        .map_err(|e| format!("{e:#}"))?;
    if state.running.lock().await.is_some() {
        *state.proxy_problem.lock().await = "permission".into();
    }
    Ok(())
}

/// Where the `tailfuse` command is installed, if it is.
fn cli_path() -> Option<PathBuf> {
    let name = if cfg!(windows) {
        "tailfuse.exe"
    } else {
        "tailfuse"
    };
    let mut dirs: Vec<PathBuf> = std::env::var_os("PATH")
        .map(|p| std::env::split_paths(&p).collect())
        .unwrap_or_default();
    // A menu-bar app's PATH is short; look where the installer puts it too.
    dirs.push(PathBuf::from("/usr/local/bin"));
    dirs.push(PathBuf::from("/opt/homebrew/bin"));
    if let Some(home) = std::env::var_os("HOME").map(PathBuf::from) {
        dirs.push(home.join(".local/bin"));
        dirs.push(home.join(".cargo/bin"));
    }
    dirs.into_iter().map(|d| d.join(name)).find(|p| p.exists())
}

/// Run a program, returning its trimmed stdout or a readable error.
#[cfg(not(windows))]
fn run_tool(program: &str, args: &[&str]) -> Result<String, String> {
    let out = std::process::Command::new(program)
        .args(args)
        .output()
        .map_err(|e| format!("could not run {program}: {e}"))?;
    if !out.status.success() {
        return Err(format!(
            "{program} failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// Download the `tailfuse` command for this computer from the latest release,
/// check it against the release's published SHA-256, and put it on the PATH:
/// /usr/local/bin on macOS (macOS asks for an administrator password),
/// ~/.local/bin on Linux.
#[cfg(not(windows))]
fn install_cli_blocking() -> Result<String, String> {
    let (platform, sums) = match (std::env::consts::OS, std::env::consts::ARCH) {
        ("macos", _) => ("macos", "SHA256SUMS-macos.txt"),
        ("linux", "aarch64") => ("linux-arm64", "SHA256SUMS-linux.txt"),
        ("linux", _) => ("linux-amd64", "SHA256SUMS-linux.txt"),
        _ => return Err("there is no tailfuse command for this system yet".into()),
    };
    let work = config::state_dir().join("cli-install");
    let _ = std::fs::remove_dir_all(&work);
    std::fs::create_dir_all(&work).map_err(|e| e.to_string())?;
    let archive = work.join("tailfuse.tar.gz");
    let archive_s = archive.to_string_lossy().to_string();
    let work_s = work.to_string_lossy().to_string();

    // The site redirects to the release asset; curl reports where it landed.
    let landed = run_tool(
        "curl",
        &[
            "-fsSL",
            "--max-time",
            "120",
            "-o",
            &archive_s,
            "-w",
            "%{url_effective}",
            &format!("https://tailfuse.com/download/cli/{platform}"),
        ],
    )?;
    let asset = landed.rsplit('/').next().unwrap_or_default().to_string();
    let base = landed
        .strip_suffix(asset.as_str())
        .unwrap_or_default()
        .to_string();
    if !asset.ends_with(".tar.gz") || !base.starts_with("https://") {
        return Err("the download did not lead to a release file".into());
    }
    let published = run_tool(
        "curl",
        &["-fsSL", "--max-time", "60", &format!("{base}{sums}")],
    )?;
    let want = published
        .lines()
        .find(|l| l.trim_end().ends_with(asset.as_str()))
        .and_then(|l| l.split_whitespace().next())
        .ok_or("the release does not list a checksum for this file")?
        .to_lowercase();
    let got = run_tool("shasum", &["-a", "256", &archive_s])
        .or_else(|_| run_tool("sha256sum", &[&archive_s]))?;
    let got = got
        .split_whitespace()
        .next()
        .unwrap_or_default()
        .to_lowercase();
    if got != want {
        return Err(
            "the download did not match the release's checksum, so it was not installed".into(),
        );
    }
    run_tool("tar", &["-xzf", &archive_s, "-C", &work_s])?;
    let binary = work.join("tailfuse");
    if !binary.exists() {
        return Err("the download did not contain the tailfuse command".into());
    }
    let binary_s = binary.to_string_lossy().to_string();

    #[cfg(target_os = "macos")]
    let dest = {
        let script = r#"on run argv
do shell script "/bin/mkdir -p /usr/local/bin && /usr/bin/install -m 755 " & quoted form of (item 1 of argv) & " /usr/local/bin/tailfuse" with prompt "Tailfuse wants to install the tailfuse command." with administrator privileges
end run"#;
        let out = std::process::Command::new("osascript")
            .args(["-e", script, &binary_s])
            .output()
            .map_err(|e| e.to_string())?;
        if !out.status.success() {
            let why = String::from_utf8_lossy(&out.stderr);
            return Err(if why.contains("-128") {
                "you cancelled the password prompt".into()
            } else {
                format!("macOS did not install it: {}", why.trim())
            });
        }
        PathBuf::from("/usr/local/bin/tailfuse")
    };
    #[cfg(not(target_os = "macos"))]
    let dest = {
        let dir = std::env::var_os("HOME")
            .map(PathBuf::from)
            .ok_or("no home folder")?
            .join(".local/bin");
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        let dest = dir.join("tailfuse");
        run_tool(
            "install",
            &["-m", "755", &binary_s, &dest.to_string_lossy()],
        )?;
        dest
    };
    let _ = std::fs::remove_dir_all(&work);
    Ok(format!(
        "installed at {}. Open a new terminal and run: tailfuse status",
        dest.display()
    ))
}

#[cfg(windows)]
fn install_cli_blocking() -> Result<String, String> {
    Err("on Windows, download it from tailfuse.com/download/cli/windows".into())
}

/// Install the `tailfuse` command-line tool.
#[tauri::command]
async fn install_cli() -> Result<String, String> {
    tauri::async_runtime::spawn_blocking(install_cli_blocking)
        .await
        .map_err(|e| e.to_string())?
}

/// Quick connect or disconnect one tailnet. Connecting turns Tailfuse on
/// first when it is off; a tailnet that was never signed in opens the sign-in.
#[tauri::command]
async fn set_tailnet(
    state: State<'_, SharedState>,
    app: AppHandle,
    name: String,
    on: bool,
) -> Result<(), String> {
    if on {
        watch_connecting();
    }
    if on && state.running.lock().await.is_none() {
        start_with_only(&state, &name).await?;
        // The node needs a moment before it answers.
        tokio::time::sleep(std::time::Duration::from_millis(1500)).await;
    }
    if !on && state.running.lock().await.is_none() {
        return Ok(());
    }
    match daemon::set_connected(&name, on).await {
        Ok(()) => {}
        Err(e) if on && format!("{e:#}").to_lowercase().contains("login") => {
            let cfg = state.cfg.lock().await.clone();
            if let Some(u) = daemon::login_start(&cfg, &name)
                .await
                .map_err(|e| e.to_string())?
            {
                let _ = app.opener().open_url(&u, None::<&str>);
            }
        }
        Err(e) => return Err(format!("{e:#}")),
    }
    // Remember the choice so starting Tailfuse (or connecting another
    // tailnet) keeps this one the way the switch left it.
    remember_connected(&state, &name, on).await;
    refresh_tray(&app);
    Ok(())
}

/// Turning on one tailnet while Tailfuse is off starts Tailfuse with only
/// that tailnet connected; the others come up switched off.
async fn start_with_only(state: &AppState, name: &str) -> Result<(), String> {
    if state.running.lock().await.is_some() {
        return Ok(());
    }
    {
        let mut cfg = state.cfg.lock().await;
        for (n, prof) in cfg.profiles.iter_mut() {
            prof.connected = n == name;
        }
        cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    }
    start_inner(state).await
}

/// Remember a tailnet's switch so starting Tailfuse keeps it that way.
async fn remember_connected(state: &AppState, name: &str, on: bool) {
    let mut cfg = state.cfg.lock().await;
    if let Some(prof) = cfg.profiles.get_mut(name) {
        prof.connected = on;
        let _ = cfg.save(&state.cfg_path);
    }
}

/// About pane: look for a newer version now. Installs it; relaunches straight
/// away while Tailfuse is off, otherwise on the next stop (as the background
/// checker does).
#[tauri::command]
async fn check_updates(app: AppHandle, state: State<'_, SharedState>) -> Result<String, String> {
    match updates::check_now(&app)
        .await
        .map_err(|e| format!("{e:#}"))?
    {
        None => Ok(format!(
            "You have the latest version ({}).",
            app.package_info().version
        )),
        Some(v) => {
            if state.running.lock().await.is_none() {
                app.restart();
            }
            Ok(format!(
                "Version {v} is installed. It starts the next time Tailfuse is off."
            ))
        }
    }
}

/// Disconnect the official Tailscale app; it stays signed in.
#[tauri::command]
async fn disconnect_tailscale_app() -> Result<(), String> {
    daemon::official_tailscale_disconnect()
        .await
        .map_err(|e| format!("{e:#}"))
}

#[tauri::command]
async fn import_from_tailscale(state: State<'_, SharedState>) -> Result<String, String> {
    let suffix = daemon::detect_official_tailnet_suffix().await.ok_or_else(|| {
        "could not read the Tailscale app's tailnet. Is Tailscale installed, running and signed in?"
            .to_string()
    })?;
    let mut cfg = state.cfg.lock().await;
    // Reuse an existing profile that already owns this suffix.
    if let Some((name, _)) = cfg.owner(&suffix) {
        return Ok(format!("already imported as profile {name:?}"));
    }
    let name = "imported".to_string();
    let name = if cfg.profiles.contains_key(&name) {
        format!("imported-{suffix}")
    } else {
        name
    };
    cfg.profiles.insert(
        name.clone(),
        Profile {
            suffixes: vec![format!(".{suffix}")],
            ..Default::default()
        },
    );
    cfg.validate().map_err(|e| e.to_string())?;
    license::check(cfg.profiles.len(), &cfg.license).map_err(|e| e.to_string())?;
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(format!(
        "imported tailnet {suffix} as {name:?}. Press Sign in to connect it"
    ))
}

#[tauri::command]
async fn add_profile(
    state: State<'_, SharedState>,
    name: String,
    suffixes: Vec<String>,
) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    if cfg.profiles.contains_key(&name) {
        return Err(format!("a tailnet called {name:?} already exists"));
    }
    cfg.profiles.insert(
        name.clone(),
        Profile {
            suffixes,
            ..Default::default()
        },
    );
    cfg.validate().map_err(|e| e.to_string())?;
    license::check(cfg.profiles.len(), &cfg.license).map_err(|e| e.to_string())?;
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(())
}

/// What the options form edits: everything about a tailnet that is not its name or suffixes.
#[derive(Serialize, serde::Deserialize)]
struct ProfileOptions {
    accept_routes: bool,
    exit_node: String,
    hostname: String,
    advertise_routes: Vec<String>,
    advertise_exit_node: bool,
    exit_node_allow_lan_access: bool,
    ssh: bool,
    shields_up: bool,
    serve: Vec<tailfuse_core::config::ServeEntry>,
}

#[tauri::command]
async fn get_profile_options(
    state: State<'_, SharedState>,
    name: String,
) -> Result<ProfileOptions, String> {
    let cfg = state.cfg.lock().await;
    let p = cfg
        .profiles
        .get(&name)
        .ok_or_else(|| format!("there is no tailnet called {name:?}"))?;
    Ok(ProfileOptions {
        accept_routes: p.accept_routes,
        exit_node: p.exit_node.clone(),
        hostname: p.hostname.clone(),
        advertise_routes: p.advertise_routes.clone(),
        advertise_exit_node: p.advertise_exit_node,
        exit_node_allow_lan_access: p.exit_node_allow_lan_access,
        ssh: p.ssh,
        shields_up: p.shields_up,
        serve: p.serve.clone(),
    })
}

#[tauri::command]
async fn set_profile_options(
    state: State<'_, SharedState>,
    name: String,
    options: ProfileOptions,
) -> Result<(), String> {
    let prof = {
        let mut cfg = state.cfg.lock().await;
        let p = cfg
            .profiles
            .get_mut(&name)
            .ok_or_else(|| format!("there is no tailnet called {name:?}"))?;
        p.accept_routes = options.accept_routes;
        p.exit_node = options.exit_node.trim().to_string();
        p.hostname = options.hostname.trim().to_string();
        p.advertise_routes = options.advertise_routes;
        p.advertise_exit_node = options.advertise_exit_node;
        p.exit_node_allow_lan_access = options.exit_node_allow_lan_access;
        p.ssh = options.ssh;
        p.shields_up = options.shields_up;
        p.serve = options.serve;
        let p = p.clone();
        cfg.validate().map_err(|e| e.to_string())?;
        cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
        p
    };
    daemon::apply_prefs(&name, &prof)
        .await
        .map_err(|e| e.to_string())
}

/// Any `tailscale` subcommand for a tailnet (ping, whois, netcheck, file, cert, lock…).
#[tauri::command]
async fn tailscale_run(name: String, args: Vec<String>) -> Result<String, String> {
    daemon::tailscale_run(&name, &args)
        .await
        .map_err(|e| e.to_string())
}

/// Taildrop: pick files with the native dialog and send them to a device on a tailnet.
#[tauri::command]
async fn taildrop_send(app: AppHandle, name: String, device: String) -> Result<String, String> {
    use tauri_plugin_dialog::DialogExt;
    let device = device.trim().trim_end_matches(':').to_string();
    if device.is_empty() {
        return Err("Type the name of the device to send to".into());
    }
    let picker = app.clone();
    let picked = tokio::task::spawn_blocking(move || picker.dialog().file().blocking_pick_files())
        .await
        .map_err(|e| e.to_string())?;
    let Some(files) = picked else {
        return Ok("Nothing sent".into());
    };
    let mut args = vec!["file".to_string(), "cp".to_string()];
    for f in &files {
        let path = f.clone().into_path().map_err(|e| e.to_string())?;
        args.push(path.to_string_lossy().to_string());
    }
    args.push(format!("{device}:"));
    daemon::tailscale_run(&name, &args)
        .await
        .map_err(|e| e.to_string())?;
    Ok(format!(
        "Sent {} to {device}",
        if files.len() == 1 {
            "1 file".to_string()
        } else {
            format!("{} files", files.len())
        }
    ))
}

/// Taildrop: move files waiting for this tailnet's node into Downloads.
#[tauri::command]
async fn taildrop_receive(app: AppHandle, name: String) -> Result<String, String> {
    use tauri::Manager;
    let dir = app.path().download_dir().map_err(|e| e.to_string())?;
    let args = vec![
        "file".to_string(),
        "get".to_string(),
        "--conflict=rename".to_string(),
        dir.to_string_lossy().to_string(),
    ];
    let out = daemon::tailscale_run(&name, &args)
        .await
        .map_err(|e| e.to_string())?;
    let out = out.trim();
    Ok(if out.is_empty() {
        format!(
            "Checked for files; anything waiting is now in {}",
            dir.display()
        )
    } else {
        out.to_string()
    })
}

/// A Let's Encrypt certificate for this device's MagicDNS name, saved to Downloads.
#[tauri::command]
async fn tailscale_cert(app: AppHandle, name: String) -> Result<String, String> {
    use tauri::Manager;
    let status = daemon::tailscale_run(&name, &["status".into(), "--json".into()])
        .await
        .map_err(|e| e.to_string())?;
    let v: serde_json::Value = serde_json::from_str(&status).map_err(|e| e.to_string())?;
    let dns = v["Self"]["DNSName"]
        .as_str()
        .unwrap_or("")
        .trim_end_matches('.')
        .to_string();
    if dns.is_empty() {
        return Err("This device has no MagicDNS name yet. Turn on HTTPS certificates for the tailnet in the Tailscale admin console.".into());
    }
    let dir = app.path().download_dir().map_err(|e| e.to_string())?;
    let (crt, key) = (
        dir.join(format!("{dns}.crt")),
        dir.join(format!("{dns}.key")),
    );
    let args = vec![
        "cert".to_string(),
        "--cert-file".into(),
        crt.to_string_lossy().to_string(),
        "--key-file".into(),
        key.to_string_lossy().to_string(),
        dns.clone(),
    ];
    daemon::tailscale_run(&name, &args)
        .await
        .map_err(|e| e.to_string())?;
    Ok(format!(
        "Saved a certificate for {dns} to {}",
        dir.display()
    ))
}

#[tauri::command]
async fn remove_profile(state: State<'_, SharedState>, name: String) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    if cfg.profiles.remove(&name).is_none() {
        return Err(format!("there is no tailnet called {name:?}"));
    }
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn add_tunnel(
    state: State<'_, SharedState>,
    name: String,
    listen: String,
    target: String,
    profile: String,
) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    cfg.tunnels.push(Tunnel {
        name,
        listen,
        target,
        profile,
    });
    cfg.validate().map_err(|e| e.to_string())?;
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn remove_tunnel(state: State<'_, SharedState>, name: String) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    let before = cfg.tunnels.len();
    cfg.tunnels.retain(|t| t.name != name);
    if cfg.tunnels.len() == before {
        return Err(format!("there is no local port called {name:?}"));
    }
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn activate_license(state: State<'_, SharedState>, key: String) -> Result<String, String> {
    // A short code is exchanged for a signed key at tailfuse.com first.
    let key = if license::is_code(&key) {
        tauri::async_runtime::spawn_blocking(move || license::redeem(&key))
            .await
            .map_err(|e| e.to_string())?
            .map_err(|e| format!("{e:#}"))?
    } else {
        key
    };
    let mut cfg = state.cfg.lock().await;
    let lic = license::verify(&key).map_err(|e| format!("activation failed: {e:#}"))?;
    license::check(cfg.profiles.len(), &key).map_err(|e| e.to_string())?;
    cfg.license = key;
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(format!("activated for {}. Unlimited tailnets", lic.email))
}

#[tauri::command]
async fn test_host(state: State<'_, SharedState>, host: String) -> Result<String, String> {
    // While Tailfuse runs, ask the router itself: it also knows the domains
    // and devices each tailnet serves. Otherwise only the configured suffixes.
    let routes = state
        .running
        .lock()
        .await
        .as_ref()
        .map(|r| r.routes.clone());
    let owner = match routes {
        Some(routes) => routes.resolve_owner(&host).await.map(|(n, _)| n),
        None => state
            .cfg
            .lock()
            .await
            .owner(&host)
            .map(|(n, _)| n.to_string()),
    };
    Ok(match owner {
        Some(name) => format!("{host} → profile {name:?}"),
        None => format!("{host} → not owned (would be refused)"),
    })
}

#[tauri::command]
async fn get_autostart(app: AppHandle) -> Result<bool, String> {
    app.autolaunch().is_enabled().map_err(|e| e.to_string())
}

#[tauri::command]
async fn set_autostart(app: AppHandle, enabled: bool) -> Result<(), String> {
    let auto = app.autolaunch();
    if enabled {
        auto.enable().map_err(|e| e.to_string())
    } else {
        auto.disable().map_err(|e| e.to_string())
    }
}

/// Show a window and reliably focus it. tao's macOS `set_focus` only takes
/// effect once the window reports visible (its `show` is dispatched), and an
/// accessory-policy app needs the activate that rides along with it — so a
/// single `show(); set_focus()` loses the race and the window stays behind.
/// Show first, then focus once the window is actually on screen.
fn show_and_focus(w: &tauri::WebviewWindow) {
    let _ = w.unminimize();
    let _ = w.show();
    let w = w.clone();
    std::thread::spawn(move || {
        for delay in [60, 160] {
            std::thread::sleep(std::time::Duration::from_millis(delay));
            if w.is_visible().unwrap_or(false) {
                let _ = w.set_focus();
            }
        }
    });
}

fn show_main_window(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        show_and_focus(&w);
    }
}

/// Toggle the menu-bar panel (panel.html): position it under the tray icon,
/// show and focus it, or hide it when it is open. A left click opens it; a
/// right click opens the native menu (build_tray).
/// When the panel last hid because it lost focus (ms since the epoch). Clicking
/// the tray icon while the panel is open blurs it first, so without this the
/// same click would hide it and then open it again.
static PANEL_BLURRED_AT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

fn toggle_panel(app: &AppHandle) {
    let Some(panel) = app.get_webview_window("panel") else {
        return;
    };
    if panel.is_visible().unwrap_or(false) {
        let _ = panel.hide();
        return;
    }
    let blurred = PANEL_BLURRED_AT.load(std::sync::atomic::Ordering::Relaxed);
    if now_ms().saturating_sub(blurred) < 300 {
        return;
    }
    // Anchor the panel's top center under the tray icon (macOS; elsewhere the
    // window opens wherever the compositor puts it).
    // The tray reports its rect in physical pixels; the panel is placed in
    // logical points. Treating one as the other doubled the position on a
    // Retina display and put the panel off the right edge of the screen.
    #[cfg(target_os = "macos")]
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        if let Ok(Some(rect)) = tray.rect() {
            let scale = panel
                .current_monitor()
                .ok()
                .flatten()
                .map(|m| m.scale_factor())
                .unwrap_or_else(|| panel.scale_factor().unwrap_or(1.0));
            let pos: tauri::LogicalPosition<f64> = rect.position.to_logical(scale);
            let tray_size: tauri::LogicalSize<f64> = rect.size.to_logical(scale);
            let width = panel
                .outer_size()
                .map(|s| s.to_logical::<f64>(scale).width)
                .unwrap_or(340.0);
            let x = (pos.x + tray_size.width / 2.0 - width / 2.0).max(8.0);
            let y = pos.y + tray_size.height + 2.0;
            let _ = panel.set_position(tauri::Position::Logical(tauri::LogicalPosition { x, y }));
        }
    }
    let _ = panel.show();
    show_and_focus(&panel);
}

/// The panel reports its content height after each render; the window takes
/// that height (plus the panel's 6px margin all round), so no empty, clickable
/// strip hangs below it.
#[tauri::command]
fn fit_panel(app: AppHandle, height: f64) {
    if let Some(panel) = app.get_webview_window("panel") {
        let h = (height + 12.0).clamp(120.0, 640.0);
        let _ = panel.set_size(tauri::Size::Logical(tauri::LogicalSize {
            width: 340.0,
            height: h,
        }));
    }
}

#[tauri::command]
fn open_main_window(app: AppHandle) {
    show_main_window(&app);
}

#[tauri::command]
fn quit_app(app: AppHandle) {
    app.exit(0);
}

fn build_tray(app: &AppHandle) -> Result<()> {
    let show = MenuItem::with_id(app, "show", "Open Tailfuse", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit Tailfuse", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;
    // The menu-bar glyph: full-colour ochre nodes, readable on light and dark bars.
    let icon = tauri::image::Image::from_bytes(include_bytes!("../icons/tray-36.png"))?;
    let builder = TrayIconBuilder::with_id(TRAY_ID)
        .menu(&menu)
        // Left click: the panel. Right click: the native menu, which keeps
        // Quit and a checkmark per tailnet within reach.
        .show_menu_on_left_click(false)
        .tooltip("Tailfuse")
        .icon(icon)
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                toggle_panel(tray.app_handle());
            }
        })
        .on_menu_event(|app, event| match event.id().as_ref() {
            "quit" => {
                if let Some(state) = app.try_state::<SharedState>() {
                    let state = Arc::clone(&state);
                    let app = AppHandle::clone(app);
                    tauri::async_runtime::spawn(async move {
                        stop_inner(&state).await;
                        app.exit(0);
                    });
                } else {
                    app.exit(0);
                }
            }
            "show" => show_main_window(app),
            "toggle" => {
                if let Some(state) = app.try_state::<SharedState>() {
                    let state = Arc::clone(&state);
                    let app = AppHandle::clone(app);
                    tauri::async_runtime::spawn(async move {
                        let is_up = state.running.lock().await.is_some();
                        let res = if is_up {
                            stop_inner(&state).await;
                            Ok(())
                        } else {
                            start_inner(&state).await
                        };
                        if let Err(e) = res {
                            tracing::warn!("tray toggle: {e}");
                            notify(&app, "Tailfuse could not turn on", &e);
                        }
                        refresh_tray(&app);
                    });
                }
            }
            id if id.starts_with("profile-") => {
                let name = id.strip_prefix("profile-").unwrap_or_default().to_string();
                if let Some(state) = app.try_state::<SharedState>() {
                    let state = Arc::clone(&state);
                    let app = AppHandle::clone(app);
                    tauri::async_runtime::spawn(async move {
                        let st = match state.running.lock().await.as_ref() {
                            Some(r) => r
                                .daemons
                                .status()
                                .await
                                .into_iter()
                                .find(|s| s.name == name),
                            None => None,
                        };
                        let online = st.as_ref().map(|s| s.online).unwrap_or(false);
                        let paused = st.as_ref().map(|s| s.paused).unwrap_or(false);
                        let res = async {
                            if online {
                                // Connected: disconnect it.
                                daemon::set_connected(&name, false)
                                    .await
                                    .map_err(|e| format!("{e:#}"))?;
                                remember_connected(&state, &name, false).await;
                                Ok::<(), String>(())
                            } else {
                                watch_connecting();
                                let was_off = state.running.lock().await.is_none();
                                start_with_only(&state, &name).await?;
                                if was_off {
                                    // Started with only this one; give the node a moment.
                                    tokio::time::sleep(std::time::Duration::from_millis(1500))
                                        .await;
                                }
                                // A signed-in node just connects; one that needs a
                                // sign-in (never signed in, or signed out) gets it.
                                let connected = if paused || was_off {
                                    match daemon::set_connected(&name, true).await {
                                        Ok(()) => true,
                                        Err(e)
                                            if format!("{e:#}")
                                                .to_lowercase()
                                                .contains("login") =>
                                        {
                                            false
                                        }
                                        Err(e) => return Err(format!("{e:#}")),
                                    }
                                } else {
                                    false
                                };
                                if connected {
                                    remember_connected(&state, &name, true).await;
                                    Ok(())
                                } else {
                                    // Never signed in (or signed out): run the browser sign-in.
                                    let cfg = state.cfg.lock().await.clone();
                                    if let Some(u) = daemon::login_start(&cfg, &name)
                                        .await
                                        .map_err(|e| e.to_string())?
                                    {
                                        let _ = app.opener().open_url(&u, None::<&str>);
                                        daemon::login_wait(&name)
                                            .await
                                            .map_err(|e| e.to_string())?;
                                    }
                                    Ok(())
                                }
                            }
                        }
                        .await;
                        if let Err(e) = res {
                            tracing::warn!(profile = %name, "tray: {e}");
                            notify(&app, &format!("Tailfuse could not change {name}"), &e);
                        }
                        refresh_tray(&app);
                    });
                }
            }
            _ => {}
        });
    let _tray = builder.build(app)?;
    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let cfg_path = config::default_config_path();
    let cfg = config::load(&cfg_path).unwrap_or_default();
    // Rewrite the config once at launch: it becomes private to this user, and
    // any key still written in it moves into the system keychain.
    if cfg_path.exists() {
        if let Err(e) = cfg.save(&cfg_path) {
            tracing::warn!("securing the config: {e:#}");
        }
    }
    // Nothing set up yet: open the window so a first launch shows where to begin.
    let first_run = cfg.profiles.is_empty();

    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .manage(Arc::new(AppState {
            cfg_path,
            cfg: Mutex::new(cfg),
            running: Mutex::new(None),
            proxy_problem: Mutex::new(String::new()),
        }))
        .setup(move |app| {
            // Menu-bar behavior on macOS: no Dock icon, window hidden at
            // launch — the tray is the app. Other platforms keep a window.
            // A first launch with no tailnets keeps the window up instead.
            #[cfg(target_os = "macos")]
            {
                app.set_activation_policy(tauri::ActivationPolicy::Accessory);
                if !first_run {
                    if let Some(w) = app.get_webview_window("main") {
                        let _ = w.hide();
                    }
                }
            }
            #[cfg(not(target_os = "macos"))]
            let _ = first_run;
            // Close hides to the tray instead of killing the daemons.
            if let Some(w) = app.get_webview_window("main") {
                let win = w.clone();
                w.on_window_event(move |event| {
                    if let WindowEvent::CloseRequested { api, .. } = event {
                        api.prevent_close();
                        let _ = win.hide();
                    }
                });
            }
            // The menu-bar panel hides as soon as focus leaves it.
            if let Some(w) = app.get_webview_window("panel") {
                let handle = app.handle().clone();
                w.on_window_event(move |event| {
                    if matches!(event, WindowEvent::Focused(false)) {
                        PANEL_BLURRED_AT.store(now_ms(), std::sync::atomic::Ordering::Relaxed);
                        if let Some(panel) = handle.get_webview_window("panel") {
                            let _ = panel.hide();
                        }
                    }
                });
            }
            build_tray(app.handle())?;
            spawn_tray_animation(app.handle().clone());
            // Updates from GitHub Releases; relaunches only while Tailfuse is off.
            if let Some(state) = app.try_state::<SharedState>() {
                let state = Arc::clone(&state);
                updates::spawn(app.handle().clone(), move || {
                    let state = Arc::clone(&state);
                    async move { state.running.lock().await.is_some() }
                });
            }
            // Keep the tray menu's profile states fresh.
            let handle = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                loop {
                    tokio::time::sleep(std::time::Duration::from_millis(TRAY_REFRESH_MS)).await;
                    refresh_tray(&handle);
                }
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            get_state,
            phone_sync,
            start,
            stop,
            login,
            add_profile,
            get_profile_options,
            set_profile_options,
            tailscale_run,
            taildrop_send,
            taildrop_receive,
            tailscale_cert,
            import_from_tailscale,
            sign_in_new_tailnet,
            disconnect_tailscale_app,
            connect_browsers,
            disconnect_browsers,
            notify_user,
            alert_user,
            install_cli,
            set_tailnet,
            check_updates,
            remove_profile,
            add_tunnel,
            remove_tunnel,
            activate_license,
            test_host,
            get_autostart,
            set_autostart,
            open_main_window,
            fit_panel,
            quit_app,
        ])
        .build(tauri::generate_context!())
        .expect("error while running tailfuse")
        .run(|_app, _event| {
            // Opening the app again (Finder, Spotlight, `open -a`) while it is
            // already in the menu bar brings its window up.
            #[cfg(target_os = "macos")]
            if let tauri::RunEvent::Reopen { .. } = _event {
                show_main_window(_app);
            }
        });
}
