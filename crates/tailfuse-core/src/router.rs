//! Loopback router: one listener multiplexing SOCKS5 and HTTP CONNECT.
//! Every connection is dispatched to the per-profile upstream SOCKS5 proxy
//! of the profile owning the destination hostname suffix. Strict no-fallback:
//! unowned names are refused — nothing ever leaks direct or cross-profile.

use anyhow::{bail, Context, Result};
use std::collections::BTreeMap;
use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::watch;

use crate::pac;

/// Routing table: hostname suffix -> (profile name, upstream SOCKS5 port).
#[derive(Debug, Clone, Default)]
pub struct RouteTable {
    by_suffix: BTreeMap<String, (String, u16)>,
}

impl RouteTable {
    pub fn build(cfg: &crate::config::Config, socks_ports: &BTreeMap<String, u16>) -> Self {
        let mut by_suffix = BTreeMap::new();
        for (name, prof) in &cfg.profiles {
            let Some(&port) = socks_ports.get(name) else {
                continue;
            };
            for s in &prof.suffixes {
                by_suffix.insert(s.trim().to_lowercase(), (name.clone(), port));
            }
        }
        Self { by_suffix }
    }

    /// Longest matching suffix wins.
    pub fn owner(&self, host: &str) -> Option<(&str, u16)> {
        let host = host.trim_end_matches('.').to_lowercase();
        let mut best: Option<(&str, u16, usize)> = None;
        for (suffix, (profile, port)) in &self.by_suffix {
            if host.ends_with(suffix.as_str())
                && best.map(|(_, _, l)| suffix.len() > l).unwrap_or(true)
            {
                best = Some((profile.as_str(), *port, suffix.len()));
            }
        }
        best.map(|(p, port, _)| (p, port))
    }

    /// Upstream SOCKS5 port for a named profile (for tunnels).
    pub fn profile_port(&self, profile: &str) -> Option<u16> {
        self.by_suffix
            .values()
            .find(|(name, _)| name == profile)
            .map(|(_, port)| *port)
    }
}

pub struct Router {
    routes: Arc<RouteTable>,
    pac_body: Arc<String>,
}

impl Router {
    pub fn new(cfg: &crate::config::Config, listen_addr: &str, routes: RouteTable) -> Self {
        Self {
            routes: Arc::new(routes),
            pac_body: Arc::new(pac::generate(cfg, listen_addr)),
        }
    }

    pub fn routes(&self) -> Arc<RouteTable> {
        self.routes.clone()
    }

    /// Serve until `shutdown` fires.
    pub async fn run(
        self: Arc<Self>,
        listen: &str,
        mut shutdown: watch::Receiver<bool>,
    ) -> Result<()> {
        let listener = TcpListener::bind(listen)
            .await
            .with_context(|| format!("router bind {listen}"))?;
        let addr = listener.local_addr()?;
        tracing::info!(%addr, "router listening");
        println!("router listening on {addr} (PAC: http://{addr}/tailfuse.pac)");
        loop {
            tokio::select! {
                _ = shutdown.changed() => return Ok(()),
                accept = listener.accept() => {
                    let Ok((conn, _)) = accept else { continue };
                    let this = self.clone();
                    tokio::spawn(async move {
                        if let Err(e) = this.handle(conn).await {
                            tracing::debug!(error = %e, "connection closed");
                        }
                    });
                }
            }
        }
    }

    async fn handle(self: Arc<Self>, mut conn: TcpStream) -> Result<()> {
        let _ = conn.set_nodelay(true);
        let mut peek = [0u8; 1];
        let n = conn.peek(&mut peek).await?;
        if n == 0 {
            return Ok(());
        }

        if peek[0] == 0x05 {
            self.handle_socks5(&mut conn).await
        } else {
            self.handle_http(&mut conn).await
        }
    }

    async fn handle_socks5(self: Arc<Self>, conn: &mut TcpStream) -> Result<()> {
        // Greeting: ver=5, offer no-auth only.
        let mut head = [0u8; 2];
        conn.read_exact(&mut head).await?;
        if head[0] != 0x05 {
            bail!("bad socks version");
        }
        let mut methods = vec![0u8; head[1] as usize];
        conn.read_exact(&mut methods).await?;
        conn.write_all(&[0x05, 0x00]).await?; // no auth

        // Request.
        let mut req = [0u8; 4];
        conn.read_exact(&mut req).await?;
        if req[0] != 0x05 || req[1] != 0x01 {
            bail!("only CONNECT supported");
        }
        let host = match req[3] {
            0x01 => {
                let mut b = [0u8; 4];
                conn.read_exact(&mut b).await?;
                std::net::Ipv4Addr::from(b).to_string()
            }
            0x03 => {
                let len = conn.read_u8().await?;
                let mut b = vec![0u8; len as usize];
                conn.read_exact(&mut b).await?;
                String::from_utf8_lossy(&b).to_string()
            }
            0x04 => {
                let mut b = [0u8; 16];
                conn.read_exact(&mut b).await?;
                std::net::Ipv6Addr::from(b).to_string()
            }
            _ => bail!("bad address type"),
        };
        let dest_port = conn.read_u16().await?;

        let Some((_, upstream_port)) = self.routes.owner(&host) else {
            tracing::info!(%host, "refused: no profile owns hostname");
            let _ = conn
                .write_all(&[0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
                .await;
            return Ok(());
        };
        // Reply success (optimistic; upstream failure surfaces as a reset).
        conn.write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
            .await?;

        let mut upstream = socks5_connect_pub(upstream_port, &host, dest_port).await?;
        pipe_bidirectional(conn, &mut upstream).await
    }

    async fn handle_http(self: Arc<Self>, conn: &mut TcpStream) -> Result<()> {
        // Read headers.
        let mut buf = Vec::with_capacity(1024);
        let mut tmp = [0u8; 1024];
        while !buf.windows(4).any(|w| w == b"\r\n\r\n") && buf.len() < 64 * 1024 {
            let n = conn.read(&mut tmp).await?;
            if n == 0 {
                return Ok(());
            }
            buf.extend_from_slice(&tmp[..n]);
        }
        let header_end = buf
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|p| p + 4)
            .unwrap_or(buf.len());
        let head = String::from_utf8_lossy(&buf[..header_end]).to_string();
        let body_rest = buf[header_end..].to_vec();

        let first_line = head.lines().next().unwrap_or("");
        let mut parts = first_line.split_whitespace();
        let method = parts.next().unwrap_or("");
        let target = parts.next().unwrap_or("");

        if method == "GET" && target.starts_with("/tailfuse.pac") {
            let body = self.pac_body.as_str().as_bytes().to_vec();
            let head = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/x-ns-proxy-autoconfig\r\nContent-Length: {}\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n",
                body.len()
            );
            conn.write_all(head.as_bytes()).await?;
            conn.write_all(&body).await?;
            return Ok(());
        }
        if method == "GET" && target == "/healthz" {
            conn.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
                .await?;
            return Ok(());
        }
        if method != "CONNECT" {
            conn.write_all(b"HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\ntailfuse router only supports CONNECT").await?;
            return Ok(());
        }
        let host = target.split(':').next().unwrap_or(target).to_string();
        let Some((_, upstream_port)) = self.routes.owner(&host) else {
            tracing::info!(%host, "refused: no profile owns hostname");
            let msg = format!("no tailfuse profile owns hostname {host}");
            let head = format!(
                "HTTP/1.1 403 Forbidden\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                msg.len(),
                msg
            );
            conn.write_all(head.as_bytes()).await?;
            return Ok(());
        };
        conn.write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            .await?;

        // Port from the CONNECT target when present, else 443.
        let dest_port = target
            .rsplit(':')
            .next()
            .and_then(|p| p.parse::<u16>().ok())
            .unwrap_or(443);
        let mut upstream = socks5_connect_pub(upstream_port, &host, dest_port).await?;
        if !body_rest.is_empty() {
            upstream.write_all(&body_rest).await?;
        }
        pipe_bidirectional(conn, &mut upstream).await
    }
}

/// Dial `host:dest_port` through the per-profile upstream SOCKS5 proxy.
pub(crate) async fn socks5_connect_pub(
    upstream_port: u16,
    host: &str,
    dest_port: u16,
) -> Result<TcpStream> {
    let mut s = TcpStream::connect(("127.0.0.1", upstream_port))
        .await
        .with_context(|| format!("connecting upstream SOCKS5 on {upstream_port}"))?;
    s.write_all(&[0x05, 0x01, 0x00]).await?;
    let mut resp = [0u8; 2];
    s.read_exact(&mut resp).await?;
    if resp != [0x05, 0x00] {
        bail!("upstream SOCKS5 rejected no-auth");
    }
    let host_bytes = host.as_bytes();
    let mut req = Vec::with_capacity(7 + host_bytes.len());
    req.extend_from_slice(&[0x05, 0x01, 0x00, 0x03, host_bytes.len() as u8]);
    req.extend_from_slice(host_bytes);
    req.extend_from_slice(&dest_port.to_be_bytes());
    s.write_all(&req).await?;
    let mut head = [0u8; 4];
    s.read_exact(&mut head).await?;
    if head[1] != 0x00 {
        bail!("upstream SOCKS5 CONNECT failed (code {})", head[1]);
    }
    // Consume bound address.
    match head[3] {
        0x01 => {
            let mut b = [0u8; 6];
            s.read_exact(&mut b).await?;
        }
        0x03 => {
            let l = s.read_u8().await?;
            let mut b = vec![0u8; l as usize + 2];
            s.read_exact(&mut b).await?;
        }
        0x04 => {
            let mut b = [0u8; 18];
            s.read_exact(&mut b).await?;
        }
        _ => bail!("bad upstream reply"),
    }
    Ok(s)
}

async fn pipe_bidirectional(a: &mut TcpStream, b: &mut TcpStream) -> Result<()> {
    let (mut ar, mut aw) = a.split();
    let (mut br, mut bw) = b.split();
    let a_to_b = tokio::io::copy(&mut ar, &mut bw);
    let b_to_a = tokio::io::copy(&mut br, &mut aw);
    tokio::select! {
        r = a_to_b => { r?; }
        r = b_to_a => { r?; }
    }
    Ok(())
}
