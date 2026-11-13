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
use std::sync::Arc;
use tailfuse_core::config::{self, Config, Profile, Tunnel};
use tailfuse_core::{daemon, license, router, sysproxy};
use tauri::menu::{CheckMenuItem, Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, State, WindowEvent};
use tauri_plugin_autostart::ManagerExt as AutostartExt;
use tauri_plugin_opener::OpenerExt;
use tokio::sync::{watch, Mutex};

const TRAY_ID: &str = "tailfuse-tray";
const TRAY_REFRESH_MS: u64 = 3000;

struct Running {
    daemons: daemon::DaemonSet,
    shutdown: watch::Sender<bool>,
}

struct AppState {
    cfg_path: PathBuf,
    cfg: Mutex<Config>,
    running: Mutex<Option<Running>>,
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
}

#[derive(Serialize)]
struct StateView {
    up: bool,
    pac_url: String,
    license: String,
    profiles: Vec<ProfileView>,
    tunnels: Vec<Tunnel>,
}

#[tauri::command]
async fn get_state(state: State<'_, AppState>) -> Result<StateView, String> {
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
                })
                .collect(),
        ),
    };
    Ok(StateView {
        up,
        pac_url: format!("http://{}/tailfuse.pac", cfg.router.listen),
        license: license::status(cfg.profiles.len(), &cfg.license),
        profiles,
        tunnels: cfg.tunnels.clone(),
    })
}

/// Start profiles + router + PAC. Shared by the Start command and tray toggle.
async fn start_inner(state: &AppState) -> Result<(), String> {
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
    if let Err(e) = r.clone().serve(&cfg.router.listen, rx).await {
        return Err(format!("router: {e:#}"));
    }
    // Best-effort system PAC; the window shows the manual URL regardless.
    let pac_url = format!("http://{}/tailfuse.pac", cfg.router.listen);
    if let Err(e) = sysproxy::apply(&pac_url) {
        tracing::warn!("system PAC: {e:#}");
    }
    *running = Some(Running { daemons, shutdown });
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
async fn start(state: State<'_, AppState>) -> Result<(), String> {
    start_inner(&state).await
}

#[tauri::command]
async fn stop(state: State<'_, AppState>) -> Result<(), String> {
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
    let mut items: Vec<Box<dyn tauri::menu::IsMenuItem<tauri::Wry>>> = Vec::new();
    for name in cfg.profiles.keys() {
        let online = stats.iter().any(|s| s.name == *name && s.online);
        if let Ok(item) = CheckMenuItem::with_id(
            app,
            format!("profile-{name}"),
            format!("{} {name}", if online { "●" } else { "○" }),
            false,
            online,
            None::<&str>,
        ) {
            items.push(Box::new(item));
        }
    }
    if !cfg.profiles.is_empty() {
        if let Ok(sep) = PredefinedMenuItem::separator(app) {
            items.push(Box::new(sep));
        }
    }
    if let Ok(show) = MenuItem::with_id(app, "show", "Show tailfuse", true, None::<&str>) {
        items.push(Box::new(show));
    }
    let toggle_label = if is_up { "Stop" } else { "Start" };
    if let Ok(toggle) = MenuItem::with_id(app, "toggle", toggle_label, true, None::<&str>) {
        items.push(Box::new(toggle));
    }
    if let Ok(quit) = MenuItem::with_id(app, "quit", "Quit tailfuse", true, None::<&str>) {
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
    state: State<'_, AppState>,
    name: String,
    app: AppHandle,
) -> Result<Option<String>, String> {
    let cfg = state.cfg.lock().await.clone();
    if state.running.lock().await.is_none() {
        return Err("start tailfuse first".into());
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

#[tauri::command]
async fn import_from_tailscale(state: State<'_, AppState>) -> Result<String, String> {
    let suffix = daemon::detect_official_tailnet_suffix().await.ok_or_else(|| {
        "could not read the official Tailscale client's tailnet — is Tailscale installed, running and logged in?"
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
        "imported tailnet {suffix} as profile {name:?} — press Log in to authenticate it"
    ))
}

#[tauri::command]
async fn add_profile(
    state: State<'_, AppState>,
    name: String,
    suffixes: Vec<String>,
) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    if cfg.profiles.contains_key(&name) {
        return Err(format!("profile {name:?} already exists"));
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

#[tauri::command]
async fn remove_profile(state: State<'_, AppState>, name: String) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    if cfg.profiles.remove(&name).is_none() {
        return Err(format!("no such profile {name:?}"));
    }
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn add_tunnel(
    state: State<'_, AppState>,
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
async fn remove_tunnel(state: State<'_, AppState>, name: String) -> Result<(), String> {
    let mut cfg = state.cfg.lock().await;
    let before = cfg.tunnels.len();
    cfg.tunnels.retain(|t| t.name != name);
    if cfg.tunnels.len() == before {
        return Err(format!("no tunnel {name:?}"));
    }
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn activate_license(state: State<'_, AppState>, key: String) -> Result<String, String> {
    let mut cfg = state.cfg.lock().await;
    let lic = license::verify(&key).map_err(|e| format!("activation failed: {e:#}"))?;
    license::check(cfg.profiles.len(), &key).map_err(|e| e.to_string())?;
    cfg.license = key;
    cfg.save(&state.cfg_path).map_err(|e| e.to_string())?;
    Ok(format!(
        "activated for {} (updates until {})",
        lic.email, lic.updates_until
    ))
}

#[tauri::command]
async fn test_host(state: State<'_, AppState>, host: String) -> Result<String, String> {
    let cfg = state.cfg.lock().await.clone();
    Ok(match cfg.owner(&host) {
        Some((name, _)) => format!("{host} → profile {name:?}"),
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

fn show_main_window(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.show();
        let _ = w.unminimize();
        let _ = w.set_focus();
    }
}

fn build_tray(app: &AppHandle) -> Result<()> {
    let show = MenuItem::with_id(app, "show", "Show tailfuse", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit tailfuse", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;
    let mut builder = TrayIconBuilder::with_id(TRAY_ID)
        .menu(&menu)
        .tooltip("tailfuse")
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
                        }
                        refresh_tray(&app);
                    });
                }
            }
            id if id.starts_with("profile-") => show_main_window(app),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                show_main_window(tray.app_handle());
            }
        });
    if let Some(icon) = app.default_window_icon().cloned() {
        builder = builder.icon(icon);
    }
    let _tray = builder.build(app)?;
    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let cfg_path = config::default_config_path();
    let cfg = config::load(&cfg_path).unwrap_or_default();

    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .manage(Arc::new(AppState {
            cfg_path,
            cfg: Mutex::new(cfg),
            running: Mutex::new(None),
        }))
        .setup(|app| {
            // Menu-bar behavior on macOS: no Dock icon, window hidden at
            // launch — the tray is the app. Other platforms keep a window.
            #[cfg(target_os = "macos")]
            {
                app.set_activation_policy(tauri::ActivationPolicy::Accessory);
                if let Some(w) = app.get_webview_window("main") {
                    let _ = w.hide();
                }
            }
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
            build_tray(app.handle())?;
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
            start,
            stop,
            login,
            add_profile,
            import_from_tailscale,
            remove_profile,
            add_tunnel,
            remove_tunnel,
            activate_license,
            test_host,
            get_autostart,
            set_autostart,
        ])
        .run(tauri::generate_context!())
        .expect("error while running tailfuse");
}
