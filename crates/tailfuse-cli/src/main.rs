//! tailfuse CLI: connect to multiple Tailscale tailnets at once.

use anyhow::{bail, Context, Result};
use clap::{Parser, Subcommand};
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;
use tailfuse_core::config::{self, Config, Profile, Tunnel};
use tailfuse_core::{daemon, license, router, sysproxy, tunnels};
use tokio::sync::watch;

mod mcp;

#[derive(Parser)]
#[command(name = "tailfuse", version, about = "Connect to multiple Tailscale tailnets at once", long_about = None)]
struct Cli {
    /// Config file path (default ~/.config/tailfuse/config.yaml).
    #[arg(long, global = true)]
    config: Option<PathBuf>,

    /// Verbose logging.
    #[arg(long, global = true)]
    debug: bool,

    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Write a starter config.
    Init,
    /// Start all profiles, the router, and tunnels (blocks until Ctrl-C).
    Up {
        /// Don't configure the system PAC (configure your browser manually).
        #[arg(long)]
        no_pac: bool,
    },
    /// Stop: clear the system PAC (embedded nodes exit with `up`).
    Down,
    /// Show profile and router status.
    Status,
    /// Show which profile owns a hostname.
    Test { hostname: String },
    /// Print proxy environment variables for shell use.
    Env,
    /// Run a command with tailfuse proxy env (curl, git, npm…).
    Run { command: Vec<String> },
    /// SSH to a host through the profile that owns it.
    Ssh { target: String, args: Vec<String> },
    /// Print the PAC URL and serve it (manual browser setup).
    ServePac,
    /// Run an MCP server on stdio, for AI assistants (e.g. `claude mcp add tailfuse -- tailfuse mcp`).
    Mcp,
    /// ssh ProxyCommand backend (internal).
    #[command(hide = true)]
    DialProfile {
        profile: String,
        host: String,
        port: u16,
    },
    /// Manage tailnet profiles.
    Profile {
        #[command(subcommand)]
        action: ProfileAction,
    },
    /// Manage local TCP tunnels (db clients, RDP, SMB…).
    Tunnel {
        #[command(subcommand)]
        action: TunnelAction,
    },
    /// Manage your tailfuse license.
    License {
        #[command(subcommand)]
        action: LicenseAction,
    },
}

#[derive(Subcommand)]
enum ProfileAction {
    /// List profiles and their suffixes.
    List,
    /// Add a profile.
    Add {
        name: String,
        /// Hostname suffix this profile owns (repeatable).
        #[arg(long = "suffix", required = true)]
        suffixes: Vec<String>,
        /// Accept subnet routes advertised in this tailnet.
        #[arg(long)]
        accept_routes: bool,
        /// Use this tailnet peer as an exit node (MagicDNS name).
        #[arg(long)]
        exit_node: Option<String>,
        /// Tailscale auth key for unattended login.
        #[arg(long)]
        auth_key: Option<String>,
        /// Ephemeral node (removed from tailnet on logout).
        #[arg(long)]
        ephemeral: bool,
    },
    /// Remove a profile (keeps tailnet node state on disk).
    Remove { name: String },
    /// Authenticate a profile with its tailnet (opens a browser).
    Login { name: String },
    /// Import the tailnet this machine's official Tailscale app is logged into
    /// as a new profile, and authenticate it (browser OAuth with a localhost
    /// callback — the node is ready when the command returns).
    Import { name: String },
}

#[derive(Subcommand)]
enum TunnelAction {
    /// List tunnels.
    List,
    /// Add a tunnel.
    Add {
        name: String,
        #[arg(long)]
        listen: String,
        #[arg(long)]
        target: String,
        #[arg(long)]
        profile: String,
    },
    /// Remove a tunnel.
    Remove { name: String },
}

#[derive(Subcommand)]
enum LicenseAction {
    /// Activate a license key.
    Activate { key: String },
    /// Show licensing status.
    Status,
}

fn load() -> Result<(Config, PathBuf)> {
    let path = flag_config_path()?;
    Ok((config::load(&path)?, path))
}

fn flag_config_path() -> Result<PathBuf> {
    Ok(cli_config_path().unwrap_or_else(config::default_config_path))
}

// Set once at startup from parsed args.
static CONFIG_PATH: std::sync::OnceLock<Option<PathBuf>> = std::sync::OnceLock::new();

fn cli_config_path() -> Option<PathBuf> {
    CONFIG_PATH.get().and_then(|p| p.clone())
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    let _ = CONFIG_PATH.set(cli.config.clone());
    tracing_subscriber::fmt()
        .with_max_level(if cli.debug {
            tracing::Level::DEBUG
        } else {
            tracing::Level::INFO
        })
        .with_target(false)
        .without_time()
        .init();

    match cli.command {
        Commands::Init => cmd_init(),
        Commands::Up { no_pac } => cmd_up(no_pac).await,
        Commands::Down => cmd_down(),
        Commands::Status => cmd_status().await,
        Commands::Mcp => {
            let path = flag_config_path()?;
            tokio::task::spawn_blocking(move || mcp::serve(path)).await?
        }
        Commands::Test { hostname } => cmd_test(&hostname),
        Commands::Env => cmd_env(),
        Commands::Run { command } => cmd_run(command).await,
        Commands::Ssh { target, args } => cmd_ssh(&target, args),
        Commands::ServePac => cmd_serve_pac().await,
        Commands::DialProfile {
            profile,
            host,
            port,
        } => cmd_dial_profile(&profile, &host, port).await,
        Commands::Profile { action } => cmd_profile(action).await,
        Commands::Tunnel { action } => cmd_tunnel(action),
        Commands::License { action } => cmd_license(action),
    }
}

fn cmd_init() -> Result<()> {
    let (_, path) = load()?;
    if path.exists() {
        bail!("{} already exists", path.display());
    }
    let mut cfg = Config::default();
    cfg.profiles.insert(
        "personal".into(),
        Profile {
            suffixes: vec![".home-lab.ts.net".into()],
            ..Default::default()
        },
    );
    cfg.profiles.insert(
        "work".into(),
        Profile {
            suffixes: vec![".work.ts.net".into()],
            ..Default::default()
        },
    );
    cfg.save(&path)?;
    println!(
        "wrote {} — edit suffixes to match your tailnets, then `tailfuse up`",
        path.display()
    );
    Ok(())
}

async fn cmd_up(no_pac: bool) -> Result<()> {
    let (cfg, _) = load()?;
    license::check(cfg.profiles.len(), &cfg.license)?;
    cfg.validate()?;

    let set = daemon::DaemonSet::start(&cfg).await?;

    // Per-profile SOCKS5 ports, in config order.
    let mut socks_ports = BTreeMap::new();
    for (i, name) in cfg.profiles.keys().enumerate() {
        let port = config::profile_socks_port(i);
        socks_ports.insert(name.clone(), port);
    }
    let routes = Arc::new(router::RouteTable::build(&cfg, &socks_ports));
    let router = Arc::new(router::Router::new(
        &cfg,
        &cfg.router.listen,
        (*routes).clone(),
    ));
    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    let router_addr = router
        .clone()
        .serve(&cfg.router.listen, shutdown_rx)
        .await?;
    println!("router listening on {router_addr} (PAC: http://{router_addr}/tailfuse.pac)");

    // Tunnels.
    let (tun_tx, tun_rx) = watch::channel(false);
    let mut tunnel_handles = Vec::new();
    for t in &cfg.tunnels {
        match tunnels::spawn(t, routes.clone(), tun_rx.clone()).await {
            Ok(h) => {
                println!(
                    "tunnel {:?}: {} → {} via {:?}",
                    h.name, h.listen, h.target, h.profile
                );
                tunnel_handles.push(h);
            }
            Err(e) => eprintln!("tunnel {:?}: {e:#}", t.name),
        }
    }

    let pac_url = format!("http://{}/tailfuse.pac", cfg.router.listen);
    let mut pac_set = false;
    if !no_pac {
        match sysproxy::apply(&pac_url) {
            Ok(()) => {
                pac_set = true;
                println!("system PAC configured — Ctrl-C removes it");
            }
            Err(e) => eprintln!("warning: {e:#}"),
        }
    }

    println!("tailfuse is up. Ctrl-C to stop.");
    wait_for_ctrl_c().await;
    println!("\nshutting down…");
    let _ = shutdown_tx.send(true);
    let _ = tun_tx.send(true);
    if pac_set {
        if let Err(e) = sysproxy::clear() {
            eprintln!("warning: clearing PAC: {e:#}");
        }
    }
    for h in tunnel_handles {
        h.task.abort();
    }
    set.stop().await;
    Ok(())
}

async fn wait_for_ctrl_c() {
    let _ = tokio::signal::ctrl_c().await;
}

fn cmd_down() -> Result<()> {
    sysproxy::clear()?;
    println!("system PAC cleared (embedded nodes exit with `tailfuse up`; none run as a daemon)");
    Ok(())
}

async fn cmd_status() -> Result<()> {
    let (cfg, _) = load()?;
    let set = daemon::DaemonSet::start(&cfg).await?;
    println!(
        "{:<12} {:<12} {:<24} TAILSCALE IPs",
        "PROFILE", "STATE", "HOSTNAME"
    );
    for ps in set.status().await {
        let state = if !ps.error.is_empty() {
            format!("error: {}", ps.error)
        } else if ps.online {
            "online".to_string()
        } else {
            "logged out".to_string()
        };
        println!(
            "{:<12} {:<12} {:<24} {}",
            ps.name,
            state,
            ps.hostname,
            ps.tailscale_ips.join(", ")
        );
    }
    set.stop().await;
    Ok(())
}

fn cmd_test(hostname: &str) -> Result<()> {
    let (cfg, _) = load()?;
    match cfg.owner(hostname) {
        Some((name, _)) => println!("{hostname} → profile {name:?}"),
        None => println!("{hostname} → not owned by any profile (would be refused)"),
    }
    Ok(())
}

fn cmd_env() -> Result<()> {
    let (cfg, _) = load()?;
    let addr = &cfg.router.listen;
    println!("export all_proxy=socks5h://{addr}");
    println!("export ALL_PROXY=socks5h://{addr}");
    println!("# only owned hostnames resolve through the proxy; everything else bypasses");
    println!("export no_proxy=localhost,127.0.0.1");
    println!("export NO_PROXY=localhost,127.0.0.1");
    Ok(())
}

async fn cmd_run(command: Vec<String>) -> Result<()> {
    let (cmd, args) = command
        .split_first()
        .context("usage: tailfuse run <command> [args...]")?;
    let (cfg, _) = load()?;
    cfg.validate()?;

    let set = daemon::DaemonSet::start(&cfg).await?;
    let mut socks_ports = BTreeMap::new();
    for (i, name) in cfg.profiles.keys().enumerate() {
        socks_ports.insert(name.clone(), config::profile_socks_port(i));
    }
    let routes = router::RouteTable::build(&cfg, &socks_ports);
    let router = Arc::new(router::Router::new(&cfg, &cfg.router.listen, routes));
    let (_tx, rx) = watch::channel(false);
    router.clone().serve(&cfg.router.listen, rx).await?;
    let addr = cfg.router.listen.clone();

    let mut child = tokio::process::Command::new(cmd)
        .args(args)
        .env("all_proxy", format!("socks5h://{addr}"))
        .env("ALL_PROXY", format!("socks5h://{addr}"))
        .env("http_proxy", format!("socks5h://{addr}"))
        .env("HTTP_PROXY", format!("socks5h://{addr}"))
        .env("https_proxy", format!("socks5h://{addr}"))
        .env("HTTPS_PROXY", format!("socks5h://{addr}"))
        .env("no_proxy", "localhost,127.0.0.1")
        .env("NO_PROXY", "localhost,127.0.0.1")
        .stdin(std::process::Stdio::inherit())
        .stdout(std::process::Stdio::inherit())
        .stderr(std::process::Stdio::inherit())
        .spawn()?;
    let status = child.wait().await?;
    set.stop().await;
    std::process::exit(status.code().unwrap_or(1));
}

fn cmd_ssh(target: &str, args: Vec<String>) -> Result<()> {
    let (cfg, path) = load()?;
    let host = target
        .rsplit('@')
        .next()
        .unwrap_or(target)
        .split(':')
        .next()
        .unwrap_or(target);
    let Some((name, _)) = cfg.owner(host) else {
        bail!("no profile owns hostname {host:?}");
    };
    let self_exe = std::env::current_exe()?;
    let proxy = format!(
        "ProxyCommand={} --config {} dial-profile {} %h %p",
        shell_quote(&self_exe.display().to_string()),
        shell_quote(&path.display().to_string()),
        shell_quote(name)
    );
    let mut cmd = std::process::Command::new("ssh");
    cmd.arg("-o").arg(proxy).arg(target).args(args);
    cmd.stdin(std::process::Stdio::inherit())
        .stdout(std::process::Stdio::inherit())
        .stderr(std::process::Stdio::inherit());
    let status = cmd.status()?;
    std::process::exit(status.code().unwrap_or(1));
}

async fn cmd_serve_pac() -> Result<()> {
    let (cfg, _) = load()?;
    let routes = router::RouteTable::build(&cfg, &BTreeMap::new());
    let router = Arc::new(router::Router::new(&cfg, &cfg.router.listen, routes));
    let (_tx, rx) = watch::channel(false);
    let addr = router.clone().serve(&cfg.router.listen, rx).await?;
    println!("PAC URL: http://{addr}/tailfuse.pac");
    println!("Press Ctrl-C to stop.");
    wait_for_ctrl_c().await;
    Ok(())
}

async fn cmd_dial_profile(profile: &str, host: &str, port: u16) -> Result<()> {
    // Standalone dial used as ssh ProxyCommand: start just this profile's
    // daemon, wait for it to connect, then `tailscale nc` pipes stdio over
    // the tailnet.
    let (cfg, _) = load()?;
    let prof = cfg
        .profiles
        .get(profile)
        .with_context(|| format!("no such profile {profile:?}"))?
        .clone();
    let mut single = cfg.clone();
    single.profiles = BTreeMap::from([(profile.to_string(), prof)]);
    let set = daemon::DaemonSet::start(&single).await?;
    let socket = config::profile_socket(profile);

    let mut ready = false;
    for _ in 0..120 {
        if daemon::tailscale_cli(&socket, &["status"]).await.is_ok() {
            ready = true;
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    }
    if !ready {
        set.stop().await;
        bail!("profile {profile:?} did not become ready");
    }
    let bin = daemon::find_tailscale().context("tailscale CLI not found")?;
    let status = std::process::Command::new(bin)
        .arg("--socket")
        .arg(&socket)
        .arg("nc")
        .arg(format!("{host}:{port}"))
        .stdin(std::process::Stdio::inherit())
        .stdout(std::process::Stdio::inherit())
        .stderr(std::process::Stdio::null())
        .status()?;
    set.stop().await;
    std::process::exit(status.code().unwrap_or(1));
}

fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

async fn cmd_profile(action: ProfileAction) -> Result<()> {
    let (mut cfg, path) = load()?;
    match action {
        ProfileAction::List => {
            for (n, p) in &cfg.profiles {
                let mut line = format!("{n}: {}", p.suffixes.join(", "));
                if p.accept_routes {
                    line.push_str(" (accept-routes)");
                }
                if !p.exit_node.is_empty() {
                    line.push_str(&format!(" (exit-node: {})", p.exit_node));
                }
                println!("{line}");
            }
            println!();
            println!("{}", license::status(cfg.profiles.len(), &cfg.license));
        }
        ProfileAction::Add {
            name,
            suffixes,
            accept_routes,
            exit_node,
            auth_key,
            ephemeral,
        } => {
            if cfg.profiles.contains_key(&name) {
                bail!("profile {name:?} already exists");
            }
            cfg.profiles.insert(
                name.clone(),
                Profile {
                    suffixes,
                    accept_routes,
                    exit_node: exit_node.unwrap_or_default(),
                    auth_key: auth_key.unwrap_or_default(),
                    ephemeral,
                    ..Default::default()
                },
            );
            cfg.validate()?;
            license::check(cfg.profiles.len(), &cfg.license)?;
            cfg.save(&path)?;
            println!("added profile {name:?} — run `tailfuse profile login {name}`");
        }
        ProfileAction::Remove { name } => {
            if cfg.profiles.remove(&name).is_none() {
                bail!("no such profile {name:?}");
            }
            cfg.save(&path)?;
            println!(
                "removed profile {name:?} (state kept at {})",
                config::profile_state_dir(&name).display()
            );
        }
        ProfileAction::Login { name } => match daemon::login(&cfg, &name).await? {
            Some(url) => {
                open_browser(&url);
                println!(
                    "open this URL to authenticate profile {name:?}:\n\n  {url}\n\nauthenticated."
                )
            }
            None => println!("profile {name:?} authenticated via auth key"),
        },
        ProfileAction::Import { name } => cmd_profile_import(&mut cfg, &path, &name).await?,
    }
    Ok(())
}

/// Open a URL in the user's browser (best effort, every platform).
fn open_browser(url: &str) {
    #[cfg(target_os = "macos")]
    let _ = std::process::Command::new("open").arg(url).status();
    #[cfg(target_os = "windows")]
    let _ = std::process::Command::new("rundll32")
        .args(["url.dll,FileProtocolHandler", url])
        .status();
    #[cfg(all(unix, not(target_os = "macos")))]
    let _ = std::process::Command::new("xdg-open").arg(url).status();
}

/// `tailfuse profile import <name>`: read the tailnet the official Tailscale
/// app is logged into, create a profile owning its MagicDNS suffix, then
/// authenticate a fresh node in that tailnet through the browser OAuth flow
/// (the daemon catches the localhost callback itself; this returns once the
/// control plane answers).
async fn cmd_profile_import(cfg: &mut Config, path: &std::path::Path, name: &str) -> Result<()> {
    if cfg.profiles.contains_key(name) {
        bail!("profile {name:?} already exists");
    }
    let suffix = daemon::detect_official_tailnet_suffix()
        .await
        .context("could not read the official Tailscale client's tailnet — is Tailscale installed, running and logged in?")?;
    let suffix = format!(".{suffix}");
    cfg.profiles.insert(
        name.to_string(),
        Profile {
            suffixes: vec![suffix.clone()],
            ..Default::default()
        },
    );
    cfg.validate()?;
    license::check(cfg.profiles.len(), &cfg.license)?;
    cfg.save(path)?;
    println!(
        "created profile {name:?} owning {suffix} — authenticating a new node in that tailnet"
    );

    // Spin up just this profile's daemon and run the browser login; the
    // official client's tailnet is unaffected (userspace node, own state).
    let mut single = cfg.clone();
    single.profiles = BTreeMap::from([(name.to_string(), cfg.profiles[name].clone())]);
    let set = daemon::DaemonSet::start(&single).await?;
    let url = daemon::login_start(cfg, name).await;
    match url {
        Ok(Some(url)) => {
            open_browser(&url);
            println!("finish sign-in in the browser: {url}");
            daemon::login_wait(name).await?;
        }
        Ok(None) => (),
        Err(e) => {
            set.stop().await;
            return Err(e);
        }
    }
    set.stop().await;
    println!("profile {name:?} imported and authenticated — `tailfuse up` connects it");
    Ok(())
}

fn cmd_tunnel(action: TunnelAction) -> Result<()> {
    let (mut cfg, path) = load()?;
    match action {
        TunnelAction::List => {
            for t in &cfg.tunnels {
                println!(
                    "{}: {} → {} (profile {:?})",
                    t.name, t.listen, t.target, t.profile
                );
            }
        }
        TunnelAction::Add {
            name,
            listen,
            target,
            profile,
        } => {
            cfg.tunnels.push(Tunnel {
                name: name.clone(),
                profile,
                listen,
                target,
            });
            cfg.validate()?;
            cfg.save(&path)?;
            println!("tunnel {name:?} added — active after `tailfuse up`");
        }
        TunnelAction::Remove { name } => {
            let before = cfg.tunnels.len();
            cfg.tunnels.retain(|t| t.name != name);
            if cfg.tunnels.len() == before {
                bail!("no tunnel {name:?}");
            }
            cfg.save(&path)?;
        }
    }
    Ok(())
}

fn cmd_license(action: LicenseAction) -> Result<()> {
    let (mut cfg, path) = load()?;
    match action {
        LicenseAction::Activate { key } => {
            // A short code is exchanged for a signed key at tailfuse.com first.
            let key = if license::is_code(&key) {
                license::redeem(&key)?
            } else {
                key
            };
            let lic = license::verify(&key).context("activation failed")?;
            license::check(cfg.profiles.len(), &key)?;
            cfg.license = key;
            cfg.save(&path)?;
            println!(
                "activated for {} (updates until {})",
                lic.email, lic.updates_until
            );
        }
        LicenseAction::Status => println!("{}", license::status(cfg.profiles.len(), &cfg.license)),
    }
    Ok(())
}
