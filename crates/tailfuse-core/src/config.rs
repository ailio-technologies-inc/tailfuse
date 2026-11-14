//! Config file: `config.yaml` with one profile per tailnet.

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::PathBuf;

pub const CONFIG_VERSION: u32 = 1;
pub const DEFAULT_ROUTER_LISTEN: &str = "127.0.0.1:43100";
/// First per-profile SOCKS5 port; each profile takes `base + 2*index`.
pub const PROFILE_PORT_BASE: u16 = 43101;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    #[serde(default)]
    pub version: u32,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub license: String,
    #[serde(default)]
    pub router: Router,
    #[serde(default)]
    pub profiles: BTreeMap<String, Profile>,
    #[serde(default)]
    pub tunnels: Vec<Tunnel>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Router {
    #[serde(default = "default_listen")]
    pub listen: String,
}

impl Default for Router {
    fn default() -> Self {
        Self {
            listen: default_listen(),
        }
    }
}

fn default_listen() -> String {
    DEFAULT_ROUTER_LISTEN.to_string()
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, Default)]
pub struct Profile {
    /// Hostname suffixes this profile owns (e.g. `.work.ts.net`).
    #[serde(default)]
    pub suffixes: Vec<String>,
    /// Accept subnet routes advertised in this tailnet.
    #[serde(default)]
    pub accept_routes: bool,
    /// MagicDNS name of a peer to use as exit node.
    #[serde(default)]
    pub exit_node: String,
    /// Tailscale auth key for unattended login.
    #[serde(default)]
    pub auth_key: String,
    #[serde(default)]
    pub ephemeral: bool,
    /// The login this tailnet was signed in with (shown in the app).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub account: String,
    /// This tailnet requires Tailscale's own logging, so its node runs
    /// without `--no-logs-no-support`. Set by the app's browser sign-in.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub tailscale_logs: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Tunnel {
    pub name: String,
    pub profile: String,
    pub listen: String,
    pub target: String,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            version: CONFIG_VERSION,
            license: String::new(),
            router: Router::default(),
            profiles: BTreeMap::new(),
            tunnels: Vec::new(),
        }
    }
}

pub fn default_config_path() -> PathBuf {
    if let Ok(dir) = std::env::var("TAILFUSE_CONFIG_DIR") {
        return PathBuf::from(dir).join("config.yaml");
    }
    // Unix (macOS and Linux): ~/.config/tailfuse, the path the README
    // documents and the CLI user expects. Windows: the platform config dir.
    #[cfg(unix)]
    {
        dirs::home_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join(".config")
            .join("tailfuse")
            .join("config.yaml")
    }
    #[cfg(not(unix))]
    {
        dirs::config_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join("tailfuse")
            .join("config.yaml")
    }
}

pub fn state_dir() -> PathBuf {
    if let Ok(dir) = std::env::var("TAILFUSE_STATE_DIR") {
        return PathBuf::from(dir);
    }
    dirs::data_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join("tailfuse")
}

pub fn profile_state_dir(name: &str) -> PathBuf {
    state_dir().join("profiles").join(name)
}

pub fn profile_socket(name: &str) -> PathBuf {
    state_dir().join(format!("{name}.sock"))
}

/// SOCKS5 port for a profile by its index (stable across runs).
pub fn profile_socks_port(index: usize) -> u16 {
    PROFILE_PORT_BASE + 2 * index as u16
}

pub fn load(path: &std::path::Path) -> Result<Config> {
    match std::fs::read_to_string(path) {
        Ok(text) => {
            let mut cfg: Config = serde_yml::from_str(&text)
                .with_context(|| format!("parsing {}", path.display()))?;
            if cfg.router.listen.is_empty() {
                cfg.router.listen = default_listen();
            }
            cfg.version = CONFIG_VERSION;
            Ok(cfg)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Config::default()),
        Err(e) => Err(e).with_context(|| format!("reading {}", path.display())),
    }
}

impl Config {
    pub fn save(&self, path: &std::path::Path) -> Result<()> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let text = serde_yml::to_string(self)?;
        let tmp = path.with_extension("yaml.tmp");
        std::fs::write(&tmp, text)?;
        std::fs::rename(&tmp, path)?;
        Ok(())
    }

    /// Structural + isolation validation. Suffix ownership is strict: every
    /// suffix belongs to exactly one profile; overlaps are rejected so a name
    /// on one tailnet can never fall back to another.
    pub fn validate(&self) -> Result<()> {
        if self.profiles.is_empty() {
            bail!("no profiles defined: add one with `tailfuse profile add <name> --suffix .example.ts.net`");
        }
        let mut seen: std::collections::HashMap<String, String> = Default::default();
        for (name, p) in &self.profiles {
            if p.suffixes.is_empty() {
                bail!("profile {name:?} needs at least one suffix");
            }
            for s in &p.suffixes {
                let s = s.trim().to_lowercase();
                if s.is_empty() {
                    bail!("profile {name:?} has an empty suffix");
                }
                if !s.starts_with('.') {
                    bail!("profile {name:?}: suffix {s:?} must start with '.'");
                }
                if let Some(owner) = seen.get(&s) {
                    bail!("suffix {s:?} is owned by both {owner:?} and {name:?} — overlaps are rejected to guarantee isolation");
                }
                seen.insert(s, name.clone());
            }
        }
        let mut names = std::collections::HashSet::new();
        for t in &self.tunnels {
            if t.name.is_empty()
                || t.profile.is_empty()
                || t.listen.is_empty()
                || t.target.is_empty()
            {
                bail!("tunnel {:?} needs name, profile, listen and target", t.name);
            }
            if !names.insert(t.name.clone()) {
                bail!("duplicate tunnel name {:?}", t.name);
            }
            if !self.profiles.contains_key(&t.profile) {
                bail!(
                    "tunnel {:?} references unknown profile {:?}",
                    t.name,
                    t.profile
                );
            }
        }
        Ok(())
    }

    /// Which profile owns `host` — longest matching suffix wins.
    pub fn owner(&self, host: &str) -> Option<(&str, &Profile)> {
        let host = host.trim_end_matches('.').to_lowercase();
        let mut best: Option<(&str, &Profile, usize)> = None;
        for (name, p) in &self.profiles {
            for s in &p.suffixes {
                let s = s.to_lowercase();
                if host.ends_with(s.as_str()) {
                    let best_len = best.map(|(_, _, l)| l).unwrap_or(0);
                    if s.len() > best_len {
                        best = Some((name.as_str(), p, s.len()));
                    }
                }
            }
        }
        best.map(|(n, p, _)| (n, p))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> Config {
        let mut c = Config::default();
        c.profiles.insert(
            "personal".into(),
            Profile {
                suffixes: vec![".home-lab.ts.net".into()],
                ..Default::default()
            },
        );
        c.profiles.insert(
            "work".into(),
            Profile {
                suffixes: vec![".work.ts.net".into(), ".internal.example".into()],
                accept_routes: true,
                ..Default::default()
            },
        );
        c
    }

    #[test]
    fn owner_longest_suffix() {
        let c = cfg();
        assert_eq!(
            c.owner("nas.home-lab.ts.net").map(|(n, _)| n),
            Some("personal")
        );
        assert_eq!(c.owner("db.work.ts.net").map(|(n, _)| n), Some("work"));
        assert_eq!(
            c.owner("vault.internal.example").map(|(n, _)| n),
            Some("work")
        );
        assert_eq!(c.owner("example.com"), None);
    }

    #[test]
    fn rejects_overlap() {
        let mut c = cfg();
        c.profiles.insert(
            "evil".into(),
            Profile {
                suffixes: vec![".work.ts.net".into()],
                ..Default::default()
            },
        );
        assert!(c.validate().is_err());
    }

    #[test]
    fn roundtrip_yaml() {
        let c = cfg();
        let text = serde_yml::to_string(&c).unwrap();
        let back: Config = serde_yml::from_str(&text).unwrap();
        assert_eq!(back.profiles.len(), 2);
    }
}
