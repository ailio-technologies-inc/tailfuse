//! All-apps mode on the Mac: Tailfuse the way the Tailscale app works, for every tailnet at once.
//!
//! Off (the default), the app runs one userspace `tailscaled` per tailnet behind its router and
//! system PAC, so only proxy-aware programs (browsers, mostly) reach the tailnets. On, those stand
//! down and the packet-tunnel system extension (`tunnel.rs`) runs the phones' engine instead: it
//! holds 100.64.0.0/10 and Tailfuse's own 198.18.0.0/15, answers the tailnets' names from a DNS
//! server inside the tunnel, and so ssh, Node, database clients and curl reach every tailnet with
//! no settings.
//!
//! Turning it on carries the setup over with the same link a phone scans (`sync::link`, engine
//! `import_setup`): every tailnet, its domains and its local ports. Keys never travel: the tunnel
//! is its own device on each tailnet, so each one asks for a sign-in once.
//!
//! The choice is saved beside the config (`all-apps.json`), so `start_inner` / `stop_inner` (the
//! window, the tray and launch) start and stop the tunnel instead of the app's own nodes.

use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

use crate::tunnel;

#[derive(Serialize, Deserialize, Default)]
struct Saved {
    on: bool,
}

fn file(cfg_path: &Path) -> PathBuf {
    cfg_path.with_file_name("all-apps.json")
}

/// Whether all-apps mode is chosen (macOS only; elsewhere always false).
pub fn is_on(cfg_path: &Path) -> bool {
    cfg!(target_os = "macos")
        && std::fs::read(file(cfg_path))
            .ok()
            .and_then(|b| serde_json::from_slice::<Saved>(&b).ok())
            .map(|s| s.on)
            .unwrap_or(false)
}

pub fn save(cfg_path: &Path, on: bool) -> Result<(), String> {
    let body = serde_json::to_vec(&Saved { on }).map_err(|e| e.to_string())?;
    std::fs::write(file(cfg_path), body)
        .map_err(|e| format!("could not save the all-apps setting: {e}"))
}

async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> Result<T, String> {
    tauri::async_runtime::spawn_blocking(f)
        .await
        .map_err(|e| e.to_string())
}

/// One engine request, its `result` on success.
pub async fn call(cmd: &str, args: serde_json::Value) -> Result<serde_json::Value, String> {
    let req = serde_json::json!({ "cmd": cmd, "args": args }).to_string();
    let raw = blocking(move || tunnel::call(&req)).await??;
    let v: serde_json::Value = serde_json::from_str(&raw)
        .map_err(|e| format!("the tunnel answered something that is not JSON: {e}"))?;
    if v["ok"].as_bool() == Some(true) {
        Ok(v["result"].clone())
    } else {
        Err(v["error"]
            .as_str()
            .unwrap_or("the tunnel refused the request")
            .to_string())
    }
}

/// Starts the tunnel (installing or updating the extension first). `on_approval` runs when macOS
/// wants the person to allow it in System Settings.
pub async fn start(
    device_name: String,
    on_approval: impl Fn() + Send + 'static,
) -> Result<(), String> {
    let activated = blocking(move || tunnel::activate(on_approval)).await??;
    if activated == "will_activate_after_reboot" {
        return Err("macOS will finish installing Tailfuse's tunnel after a restart".into());
    }
    blocking(move || tunnel::start(&device_name)).await??;
    Ok(())
}

pub async fn stop() -> Result<(), String> {
    blocking(tunnel::stop).await??;
    Ok(())
}

pub async fn status() -> String {
    blocking(tunnel::status)
        .await
        .ok()
        .and_then(Result::ok)
        .unwrap_or_else(|| "none".into())
}

/// Hands the computer's setup to the tunnel's engine. Answers the engine's report
/// (`added`, `already`, `needs_unlock`, `tunnels_added`, `skipped`).
pub async fn import(
    cfg: &tailfuse_core::config::Config,
    computer: &str,
) -> Result<serde_json::Value, String> {
    let out = tailfuse_core::sync::link(cfg, computer).map_err(|e| format!("{e:#}"))?;
    call("import_setup", serde_json::json!({ "link": out.link })).await
}

/// The sign-in links the engine is waiting on, by tailnet.
pub fn auth_urls(engine_state: &serde_json::Value) -> Vec<(String, String)> {
    engine_state["profiles"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|p| {
            let url = p["auth_url"].as_str().filter(|u| !u.is_empty())?;
            Some((
                p["name"].as_str().unwrap_or("").to_string(),
                url.to_string(),
            ))
        })
        .collect()
}

/// The engine's tailnets as the window shows them (`ProfileView`). A tailnet waiting for a
/// sign-in says so in `error`, where the window already shows a tailnet's problem.
pub fn profiles(engine_state: &serde_json::Value) -> Vec<crate::ProfileView> {
    let strs = |v: &serde_json::Value| -> Vec<String> {
        v.as_array()
            .into_iter()
            .flatten()
            .filter_map(|x| x.as_str().map(str::to_string))
            .collect()
    };
    engine_state["profiles"]
        .as_array()
        .into_iter()
        .flatten()
        .map(|p| {
            let s = |k: &str| p[k].as_str().unwrap_or("").to_string();
            let mut error = s("error");
            if error.is_empty() && p["needs_login"].as_bool() == Some(true) {
                error = "Sign in to connect this tailnet".into();
            }
            crate::ProfileView {
                name: s("name"),
                suffixes: strs(&p["suffixes"]),
                online: p["online"].as_bool().unwrap_or(false),
                hostname: s("hostname"),
                ips: strs(&p["ips"]),
                error,
                account: s("account"),
                paused: p["paused"].as_bool().unwrap_or(false),
            }
        })
        .collect()
}

/// Starts a fresh sign-in for a tailnet in the tunnel and answers its page (a few seconds at
/// most), or None when it was already signed in.
pub async fn login(name: &str) -> Result<Option<String>, String> {
    call("login", serde_json::json!({ "name": name })).await?;
    for _ in 0..20 {
        let state = call("state", serde_json::Value::Null).await?;
        if let Some((_, url)) = auth_urls(&state).into_iter().find(|(n, _)| n == name) {
            return Ok(Some(url));
        }
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    }
    Ok(None)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_setting_is_off_until_saved_and_reads_back() {
        let dir = std::env::temp_dir().join(format!("tf-allapps-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cfg = dir.join("config.yaml");
        assert!(!is_on(&cfg));
        save(&cfg, true).unwrap();
        assert_eq!(is_on(&cfg), cfg!(target_os = "macos"));
        save(&cfg, false).unwrap();
        assert!(!is_on(&cfg));
        std::fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn engine_tailnets_read_as_the_window_shows_them() {
        let state = serde_json::json!({ "profiles": [
            { "name": "ailio", "suffixes": [".ailio.dev"], "online": true, "ips": ["100.64.0.7"], "account": "a@ailio.ca", "hostname": "mac" },
            { "name": "home", "needs_login": true, "auth_url": "https://login.tailscale.com/a/2" }
        ]});
        let views = profiles(&state);
        assert_eq!(views[0].suffixes, vec![".ailio.dev"]);
        assert!(views[0].online && views[0].error.is_empty());
        assert_eq!(views[1].error, "Sign in to connect this tailnet");
    }

    #[test]
    fn auth_urls_lists_only_tailnets_waiting_for_a_sign_in() {
        let state = serde_json::json!({ "profiles": [
            { "name": "ailio", "auth_url": "https://login.tailscale.com/a/1" },
            { "name": "home", "auth_url": "" },
            { "name": "work" }
        ]});
        assert_eq!(
            auth_urls(&state),
            vec![(
                "ailio".to_string(),
                "https://login.tailscale.com/a/1".to_string()
            )]
        );
    }
}
