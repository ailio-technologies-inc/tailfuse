//! tailfuse desktop app: menu-bar companion to the CLI, backed by
//! tailfuse-core. Shows per-tailnet state, drives login, manages the router
//! and system PAC, and keeps everything inside one user-process — no root
//! helpers, no kernel extensions.

use anyhow::Result;
use serde::Serialize;
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;
use tailfuse_core::config::{self, Config, Profile, Tunnel};
use tailfuse_core::{daemon, license, router, sysproxy};
use tauri::menu::{Menu, MenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Manager, State};
use tauri_plugin_opener::OpenerExt;
use tokio::sync::{watch, Mutex};

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

#[tauri::command]
async fn start(state: State<'_, AppState>) -> Result<(), String> {
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
    let listen = cfg.router.listen.clone();
    tokio::spawn(async move {
        let _ = r.run(&listen, rx).await;
    });
    // Best-effort system PAC; the window shows the manual URL regardless.
    let pac_url = format!("http://{}/tailfuse.pac", cfg.router.listen);
    if let Err(e) = sysproxy::apply(&pac_url) {
        tracing::warn!("system PAC: {e:#}");
    }
    *running = Some(Running { daemons, shutdown });
    Ok(())
}

#[tauri::command]
async fn stop(state: State<'_, AppState>) -> Result<(), String> {
    let mut running = state.running.lock().await;
    if let Some(r) = running.take() {
        let _ = r.shutdown.send(true);
        r.daemons.stop().await;
        let _ = sysproxy::clear();
    }
    Ok(())
}

#[tauri::command]
async fn login(
    state: State<'_, AppState>,
    name: String,
    app: AppHandle,
) -> Result<Option<String>, String> {
    let cfg = state.cfg.lock().await.clone();
    {
        let running = state.running.lock().await;
        if running.is_none() {
            return Err("start tailfuse first".into());
        }
    }
    let url = daemon::login_start(&cfg, &name)
        .await
        .map_err(|e| e.to_string())?;
    if let Some(u) = &url {
        let _ = app.opener().open_url(u, None::<&str>);
        // Wait for completion in the background; the frontend polls state.
        tokio::spawn(async move {
            if let Err(e) = daemon::login_wait(&name).await {
                tracing::warn!(profile = %name, "login: {e:#}");
            }
        });
    }
    Ok(url)
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

fn build_tray(app: &AppHandle) -> Result<()> {
    let show = MenuItem::with_id(app, "show", "Show tailfuse", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;
    let icon = app.default_window_icon().cloned();
    let mut builder = TrayIconBuilder::new()
        .menu(&menu)
        .show_menu_on_left_click(true)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "quit" => {
                // Stop everything we started, then exit.
                if let Some(state) = app.try_state::<SharedState>() {
                    let state: Arc<AppState> = Arc::clone(&state);
                    let app = AppHandle::clone(app);
                    tauri::async_runtime::spawn(async move {
                        let mut running = state.running.lock().await;
                        if let Some(r) = running.take() {
                            let _ = r.shutdown.send(true);
                            r.daemons.stop().await;
                            let _ = sysproxy::clear();
                        }
                        app.exit(0);
                    });
                } else {
                    app.exit(0);
                }
            }
            "show" => {
                if let Some(w) = app.get_webview_window("main") {
                    let _ = w.show();
                    let _ = w.set_focus();
                }
            }
            _ => {}
        });
    if let Some(icon) = icon {
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
        .manage(Arc::new(AppState {
            cfg_path,
            cfg: Mutex::new(cfg),
            running: Mutex::new(None),
        }))
        .setup(|app| {
            build_tray(app.handle())?;
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            get_state,
            start,
            stop,
            login,
            add_profile,
            remove_profile,
            add_tunnel,
            remove_tunnel,
            activate_license,
            test_host,
        ])
        .run(tauri::generate_context!())
        .expect("error while running tailfuse");
}
