//! Sync to a phone: the `tailfuse://sync?v=1&d=...` link a QR code carries.
//!
//! Setup only, never credentials: no auth keys, node keys or license. The
//! format is specified in `engines/mobile/API.md`; the reader lives in the
//! phone engine.

use crate::config::Config;
use anyhow::{anyhow, bail, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use qrcode::render::{svg, unicode};
use qrcode::{EcLevel, QrCode};
use serde::Serialize;

/// The longest link a code may carry, so it stays easy to scan.
pub const MAX_LINK_LEN: usize = 2000;
const PREFIX: &str = "tailfuse://sync?v=1&d=";

/// A link ready to show as a QR code.
#[derive(Debug, Clone)]
pub struct SyncLink {
    pub link: String,
    pub profiles: usize,
    /// Tunnels in the link (0 when they were dropped).
    pub tunnels: usize,
    /// The setup was too big, so the local ports were left out.
    pub tunnels_dropped: bool,
}

#[derive(Serialize)]
struct Payload<'a> {
    v: u32,
    from: &'a str,
    profiles: Vec<ProfileOut<'a>>,
    tunnels: Vec<TunnelOut<'a>>,
}

// Only these fields ever leave the computer: there is deliberately no
// auth_key, license or node state here.
#[derive(Serialize)]
struct ProfileOut<'a> {
    name: &'a str,
    account: &'a str,
    suffixes: &'a [String],
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    accept_routes: bool,
    #[serde(skip_serializing_if = "str::is_empty")]
    exit_node: &'a str,
    /// The tailnet requires Tailscale's logging, and this computer's person allowed it when they
    /// signed in to it. A setup fact, not a credential: without it the other device's node is
    /// refused ("requires logging") and the tailnet has to be removed and added again there.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    tailscale_logs: bool,
}

#[derive(Serialize)]
struct TunnelOut<'a> {
    name: &'a str,
    listen: &'a str,
    target: &'a str,
    profile: &'a str,
}

fn encode(cfg: &Config, from: &str, with_tunnels: bool) -> Result<String> {
    let payload = Payload {
        v: 1,
        from,
        profiles: cfg
            .profiles
            .iter()
            .map(|(name, p)| ProfileOut {
                name,
                account: &p.account,
                suffixes: &p.suffixes,
                accept_routes: p.accept_routes,
                exit_node: &p.exit_node,
                tailscale_logs: p.tailscale_logs,
            })
            .collect(),
        tunnels: if with_tunnels {
            cfg.tunnels
                .iter()
                .map(|t| TunnelOut {
                    name: &t.name,
                    listen: &t.listen,
                    target: &t.target,
                    profile: &t.profile,
                })
                .collect()
        } else {
            Vec::new()
        },
    };
    let json = serde_json::to_string(&payload)?;
    Ok(format!("{PREFIX}{}", URL_SAFE_NO_PAD.encode(json)))
}

/// Build the sync link for `cfg`. `from` is the computer's name, shown on the
/// phone's confirmation screen. Over the length cap the tunnels go first.
pub fn link(cfg: &Config, from: &str) -> Result<SyncLink> {
    if cfg.profiles.is_empty() {
        bail!("there are no tailnets to sync yet; add one first");
    }
    let full = encode(cfg, from, true)?;
    if full.len() <= MAX_LINK_LEN {
        return Ok(SyncLink {
            link: full,
            profiles: cfg.profiles.len(),
            tunnels: cfg.tunnels.len(),
            tunnels_dropped: false,
        });
    }
    let slim = encode(cfg, from, false)?;
    if slim.len() <= MAX_LINK_LEN {
        return Ok(SyncLink {
            link: slim,
            profiles: cfg.profiles.len(),
            tunnels: 0,
            tunnels_dropped: !cfg.tunnels.is_empty(),
        });
    }
    bail!(
        "this setup is too big for one code ({} characters, the limit is {MAX_LINK_LEN}); remove some tailnets or suffixes and try again",
        slim.len()
    )
}

/// This computer's name, for the phone's confirmation screen ("" if unknown).
pub fn computer_name() -> String {
    fn run(cmd: &str, args: &[&str]) -> Option<String> {
        let out = std::process::Command::new(cmd).args(args).output().ok()?;
        let s = String::from_utf8(out.stdout).ok()?.trim().to_string();
        (out.status.success() && !s.is_empty()).then_some(s)
    }
    #[cfg(target_os = "macos")]
    if let Some(n) = run("scutil", &["--get", "ComputerName"]) {
        return n;
    }
    if let Ok(n) = std::env::var("COMPUTERNAME") {
        if !n.trim().is_empty() {
            return n.trim().to_string();
        }
    }
    run("hostname", &[]).unwrap_or_default()
}

fn code(link: &str) -> Result<QrCode> {
    QrCode::with_error_correction_level(link.as_bytes(), EcLevel::L)
        .map_err(|e| anyhow!("could not make a code from the link: {e}"))
}

/// A crisp SVG of the code: dark modules on white with a 4-module quiet zone.
pub fn qr_svg(link: &str) -> Result<String> {
    Ok(code(link)?
        .render::<svg::Color>()
        .quiet_zone(true)
        .min_dimensions(240, 240)
        .dark_color(svg::Color("#18191b"))
        .light_color(svg::Color("#ffffff"))
        .build())
}

/// The code in half-block characters for a terminal. Light modules are drawn
/// as bright white blocks on a black background, so the code is dark-on-light
/// whatever the terminal's own theme (a code that reads inverted often fails
/// to scan).
pub fn qr_terminal(link: &str) -> Result<String> {
    let art = code(link)?
        .render::<unicode::Dense1x2>()
        .quiet_zone(true)
        .dark_color(unicode::Dense1x2::Light)
        .light_color(unicode::Dense1x2::Dark)
        .build();
    Ok(art
        .lines()
        .map(|l| format!("\x1b[97;40m{l}\x1b[0m"))
        .collect::<Vec<_>>()
        .join("\n"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{Profile, Tunnel};

    const VECTOR: &str = include_str!("../../../engines/mobile/testdata/sync.json");

    fn sample() -> Config {
        let mut cfg = Config::default();
        cfg.profiles.insert(
            "personal".into(),
            Profile {
                account: "ada@example.com".into(),
                suffixes: vec![".home-lab.ts.net".into()],
                ..Default::default()
            },
        );
        cfg.profiles.insert(
            "work".into(),
            Profile {
                account: "ada@work.example".into(),
                suffixes: vec![".work.ts.net".into(), ".internal.example".into()],
                accept_routes: true,
                exit_node: "exit-us".into(),
                ..Default::default()
            },
        );
        cfg.tunnels.push(Tunnel {
            name: "prod-db".into(),
            profile: "work".into(),
            listen: "127.0.0.1:5432".into(),
            target: "db.work.ts.net:5432".into(),
        });
        cfg
    }

    fn decode(link: &str) -> String {
        let d = link.strip_prefix(PREFIX).expect("prefix");
        String::from_utf8(URL_SAFE_NO_PAD.decode(d).unwrap()).unwrap()
    }

    #[test]
    fn matches_the_shared_test_vector() {
        let v: serde_json::Value = serde_json::from_str(VECTOR).unwrap();
        let out = link(&sample(), "Ada's MacBook").unwrap();
        assert_eq!(out.link, v["link"].as_str().unwrap());
        assert_eq!(decode(&out.link), v["json"].as_str().unwrap());
        assert_eq!(
            (out.profiles, out.tunnels, out.tunnels_dropped),
            (2, 1, false)
        );
    }

    #[test]
    fn drops_tunnels_over_the_cap() {
        let mut cfg = sample();
        for i in 0..60 {
            cfg.tunnels.push(Tunnel {
                name: format!("tunnel-number-{i}"),
                profile: "work".into(),
                listen: format!("127.0.0.1:{}", 6000 + i),
                target: format!("some-long-host-{i}.work.ts.net:5432"),
            });
        }
        let out = link(&cfg, "Ada").unwrap();
        assert!(out.tunnels_dropped);
        assert_eq!(out.tunnels, 0);
        assert!(out.link.len() <= MAX_LINK_LEN);
        assert!(decode(&out.link).contains("\"tunnels\":[]"));
    }

    #[test]
    fn errors_when_profiles_alone_are_too_big() {
        let mut cfg = sample();
        cfg.profiles.get_mut("work").unwrap().suffixes = (0..200)
            .map(|i| format!(".host-number-{i}.example.com"))
            .collect();
        let err = link(&cfg, "Ada").unwrap_err().to_string();
        assert!(err.contains("too big"), "{err}");
    }

    #[test]
    fn errors_with_no_tailnets() {
        assert!(link(&Config::default(), "Ada").is_err());
    }

    #[test]
    fn never_carries_secrets() {
        let mut cfg = sample();
        cfg.license = "LICENSE-SECRET-123".into();
        cfg.profiles.get_mut("work").unwrap().auth_key = "tskey-auth-SECRET".into();
        let out = link(&cfg, "Ada").unwrap();
        let json = decode(&out.link);
        assert!(!json.contains("tskey"));
        assert!(!json.contains("SECRET"));
        assert!(!json.contains("auth_key"));
        assert!(!json.contains("license"));
        assert!(!out.link.contains("SECRET"));
    }

    #[test]
    fn carries_tailscale_logs_only_when_the_tailnet_needs_it() {
        let mut cfg = sample();
        assert!(!decode(&link(&cfg, "Mac").unwrap().link).contains("tailscale_logs"));
        let name = cfg.profiles.keys().next().unwrap().clone();
        cfg.profiles.get_mut(&name).unwrap().tailscale_logs = true;
        assert!(decode(&link(&cfg, "Mac").unwrap().link).contains(r#""tailscale_logs":true"#));
    }

    #[test]
    fn svg_is_dark_on_white() {
        let s = qr_svg(&link(&sample(), "Ada").unwrap().link).unwrap();
        assert!(s.contains("<svg") && s.trim_end().ends_with("</svg>"));
        assert!(s.contains("#18191b"));
        assert!(s.contains("#ffffff"));
    }

    #[test]
    fn terminal_code_is_half_blocks() {
        let t = qr_terminal(&link(&sample(), "Ada").unwrap().link).unwrap();
        assert!(t.lines().count() > 10);
        assert!(t.contains('\u{2580}') || t.contains('\u{2584}') || t.contains('\u{2588}'));
    }
}
