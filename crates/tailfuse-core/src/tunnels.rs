//! Local TCP tunnels: pin `127.0.0.1:PORT` to a remote `host:port` through a
//! specific profile, so GUI clients (database apps, RDP, Finder/SMB) connect
//! to localhost.

use anyhow::{Context, Result};
use std::sync::Arc;
use tokio::io::copy_bidirectional;
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::watch;
use tokio::task::JoinHandle;

use crate::config::Tunnel;
use crate::router::RouteTable;

pub struct TunnelHandle {
    pub name: String,
    pub listen: String,
    pub target: String,
    pub profile: String,
    pub task: JoinHandle<()>,
}

/// Forward `t.listen` → `t.target` via the profile's upstream SOCKS5.
pub async fn spawn(
    t: &Tunnel,
    routes: Arc<RouteTable>,
    mut shutdown: watch::Receiver<bool>,
) -> Result<TunnelHandle> {
    let listener = TcpListener::bind(&t.listen)
        .await
        .with_context(|| format!("tunnel {:?} bind {}", t.name, t.listen))?;
    let target = t.target.clone();
    let name = t.name.clone();
    let profile = t.profile.clone();

    let task = tokio::spawn(async move {
        loop {
            tokio::select! {
                _ = shutdown.changed() => break,
                accept = listener.accept() => {
                    let Ok((client, _)) = accept else { continue };
                    let target = target.clone();
                    let routes = routes.clone();
                    let profile = profile.clone();
                    tokio::spawn({
                        let name = name.clone();
                        async move {
                            if let Err(e) = forward(client, &routes, &profile, &target).await {
                                tracing::debug!(tunnel = %name, error = %e, "tunnel connection ended");
                            }
                        }
                    });
                }
            }
        }
    });
    Ok(TunnelHandle {
        name: t.name.clone(),
        listen: t.listen.clone(),
        target: t.target.clone(),
        profile: t.profile.clone(),
        task,
    })
}

async fn forward(
    mut client: TcpStream,
    routes: &RouteTable,
    profile: &str,
    target: &str,
) -> Result<()> {
    let port = routes
        .profile_port(profile)
        .with_context(|| format!("profile {profile:?} has no SOCKS5 route"))?;
    let (host, dest_port) = target
        .rsplit_once(':')
        .and_then(|(h, p)| p.parse::<u16>().ok().map(|p| (h.to_string(), p)))
        .unwrap_or((target.to_string(), 443));
    let mut upstream = super::router::socks5_connect_pub(port, &host, dest_port).await?;
    copy_bidirectional(&mut client, &mut upstream).await?;
    Ok(())
}
