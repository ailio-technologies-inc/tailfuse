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
use tailfuse_core::config::Config;

#[derive(Serialize, Deserialize, Default)]
struct Saved {
    on: bool,
}

fn file(cfg_path: &Path) -> PathBuf {
    cfg_path.with_file_name("all-apps.json")
}

/// This build carries the tunnel: Tailfuse.app/Contents/Library/SystemExtensions holds it. A build
/// without it (an unsigned one, or a release whose lane had no provisioning profiles,
/// macos/README.md) hides the switch rather than offering something macOS will refuse.
pub fn available() -> bool {
    cfg!(target_os = "macos")
        && std::env::current_exe()
            .ok()
            .and_then(|exe| {
                exe.parent()?.parent().map(|contents| {
                    contents
                        .join("Library/SystemExtensions/com.ailio.tailfuse.tunnel.systemextension")
                })
            })
            .is_some_and(|ext| ext.is_dir())
}

/// Whether all-apps mode is chosen and this build can run it.
pub fn is_on(cfg_path: &Path) -> bool {
    available() && saved_on(cfg_path)
}

/// The saved choice alone.
fn saved_on(cfg_path: &Path) -> bool {
    std::fs::read(file(cfg_path))
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
                connecting: p["connecting"].as_bool().unwrap_or(false),
            }
        })
        .collect()
}

/// The engine's tailnets as the tray reads them (`daemon::ProfileStatus`).
pub fn statuses(engine_state: &serde_json::Value) -> Vec<tailfuse_core::daemon::ProfileStatus> {
    engine_state["profiles"]
        .as_array()
        .into_iter()
        .flatten()
        .map(|p| tailfuse_core::daemon::ProfileStatus {
            name: p["name"].as_str().unwrap_or("").to_string(),
            running: p["online"].as_bool().unwrap_or(false)
                || p["connecting"].as_bool().unwrap_or(false),
            online: p["online"].as_bool().unwrap_or(false),
            hostname: p["hostname"].as_str().unwrap_or("").to_string(),
            tailscale_ips: p["ips"]
                .as_array()
                .into_iter()
                .flatten()
                .filter_map(|x| x.as_str().map(str::to_string))
                .collect(),
            error: p["error"].as_str().unwrap_or("").to_string(),
            account: p["account"].as_str().unwrap_or("").to_string(),
            paused: p["paused"].as_bool().unwrap_or(false),
            connecting: p["connecting"].as_bool().unwrap_or(false),
        })
        .collect()
}

/// Starts a fresh sign-in for a tailnet in the tunnel and answers its page (a few seconds at
/// most), or None when it was already signed in.
pub async fn login(name: &str) -> Result<Option<String>, String> {
    call("login", serde_json::json!({ "name": name })).await?;
    login_url(name).await
}

/// A new tailnet in the tunnel, its sign-in started; answers the name the engine gave it.
pub async fn add_profile() -> Result<String, String> {
    let made = call("add_profile", serde_json::json!({})).await?;
    made["name"]
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| "the tunnel made a tailnet without a name".into())
}

/// The sign-in page the engine opened for `name`, once it appears (10 s at most).
pub async fn login_url(name: &str) -> Result<Option<String>, String> {
    for _ in 0..20 {
        let state = call("state", serde_json::Value::Null).await?;
        if let Some((_, url)) = auth_urls(&state).into_iter().find(|(n, _)| n == name) {
            return Ok(Some(url));
        }
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    }
    Ok(None)
}

/// What the tunnel's engine must be told for a change the window made to the app's config while
/// all-apps mode is on: one request per difference. New tailnets and new local ports travel as one
/// `import_setup` of the whole config (it skips what the engine already has); everything else is
/// its own command. The engine names an imported tailnet as the computer does when the name is
/// free, which is what these requests address.
pub fn plan(before: &Config, after: &Config) -> Vec<(&'static str, serde_json::Value)> {
    use serde_json::json;
    let mut out = Vec::new();
    for name in before
        .profiles
        .keys()
        .filter(|n| !after.profiles.contains_key(*n))
    {
        out.push(("remove_profile", json!({ "name": name })));
    }
    for t in before
        .tunnels
        .iter()
        .filter(|t| !after.tunnels.iter().any(|a| a.name == t.name))
    {
        out.push(("remove_tunnel", json!({ "name": t.name })));
    }
    let added_profile = after
        .profiles
        .keys()
        .any(|n| !before.profiles.contains_key(n));
    let added_tunnel = after
        .tunnels
        .iter()
        .any(|t| !before.tunnels.iter().any(|b| b.name == t.name));
    if added_profile || added_tunnel {
        out.push(("import_setup", serde_json::Value::Null)); // the link is made from `after` when sent
    }
    for (name, a) in &after.profiles {
        let Some(b) = before.profiles.get(name) else {
            continue;
        };
        for d in a.suffixes.iter().filter(|d| !b.suffixes.contains(d)) {
            out.push((
                "add_domain",
                json!({ "name": name, "domain": d.trim_start_matches('.') }),
            ));
        }
        for d in b.suffixes.iter().filter(|d| !a.suffixes.contains(d)) {
            out.push((
                "remove_domain",
                json!({ "name": name, "domain": d.trim_start_matches('.') }),
            ));
        }
        if a.exit_node != b.exit_node || a.accept_routes != b.accept_routes {
            let mut args = json!({ "name": name, "accept_routes": a.accept_routes });
            if !a.exit_node.is_empty() {
                args["exit_node"] = json!(a.exit_node);
            }
            out.push(("set_options", args));
        }
        if a.connected != b.connected {
            out.push(("set_connected", json!({ "name": name, "on": a.connected })));
        }
    }
    out
}

/// Sends `plan(before, after)` to the tunnel. Every request is tried; the first refusal is the
/// answer, so the window shows it.
pub async fn mirror(before: &Config, after: &Config) -> Result<(), String> {
    let mut first_error = None;
    for (cmd, args) in plan(before, after) {
        let sent = if cmd == "import_setup" {
            import(after, &tailfuse_core::sync::computer_name())
                .await
                .map(|_| serde_json::Value::Null)
        } else {
            call(cmd, args).await
        };
        if let Err(e) = sent {
            tracing::warn!("all apps: {cmd}: {e}");
            first_error.get_or_insert(format!("the tunnel did not take the change ({cmd}): {e}"));
        }
    }
    first_error.map_or(Ok(()), Err)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_setting_is_off_until_saved_and_reads_back() {
        let dir = std::env::temp_dir().join(format!("tf-allapps-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cfg = dir.join("config.yaml");
        assert!(!saved_on(&cfg));
        save(&cfg, true).unwrap();
        assert!(saved_on(&cfg));
        // A test binary is no app bundle, so it carries no tunnel: chosen is not on.
        assert!(!available() && !is_on(&cfg));
        save(&cfg, false).unwrap();
        assert!(!saved_on(&cfg));
        std::fs::remove_dir_all(dir).unwrap();
    }

    fn profile(suffixes: &[&str]) -> tailfuse_core::config::Profile {
        tailfuse_core::config::Profile {
            suffixes: suffixes.iter().map(|s| s.to_string()).collect(),
            ..Default::default()
        }
    }

    #[test]
    fn a_change_in_the_window_becomes_one_request_per_difference() {
        let mut before = Config::default();
        before
            .profiles
            .insert("ailio".into(), profile(&[".ailio.dev"]));
        before
            .profiles
            .insert("old".into(), profile(&[".old.ts.net"]));
        before.tunnels.push(tailfuse_core::config::Tunnel {
            name: "db".into(),
            profile: "ailio".into(),
            listen: "127.0.0.1:5432".into(),
            target: "db.ailio.dev:5432".into(),
        });
        let mut after = Config::default();
        let mut ailio = profile(&[".ailio.dev", ".clinicly.dev"]);
        ailio.exit_node = "exit-ca".into();
        after.profiles.insert("ailio".into(), ailio);
        after
            .profiles
            .insert("new".into(), profile(&[".new.ts.net"]));
        let cmds: Vec<&str> = plan(&before, &after).into_iter().map(|(c, _)| c).collect();
        assert_eq!(
            cmds,
            vec![
                "remove_profile",
                "remove_tunnel",
                "import_setup",
                "add_domain",
                "set_options"
            ]
        );
        let domain = plan(&before, &after)
            .into_iter()
            .find(|(c, _)| *c == "add_domain")
            .unwrap()
            .1;
        assert_eq!(
            domain,
            serde_json::json!({ "name": "ailio", "domain": "clinicly.dev" })
        );
        assert!(plan(&after, &after).is_empty(), "no change, no request");
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
