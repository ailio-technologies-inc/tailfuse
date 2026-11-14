//! Loopback router: one listener multiplexing SOCKS5 and HTTP CONNECT.
//! Every connection is dispatched to the per-profile upstream SOCKS5 proxy
//! of the profile owning the destination hostname suffix. Strict no-fallback:
//! unowned names are refused — nothing ever leaks direct or cross-profile.

use anyhow::{bail, Context, Result};
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::net::IpAddr;
use std::sync::{Arc, RwLock};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::watch;

use crate::pac;

/// Routing table. A hostname goes to a tailnet by, in order:
/// 1. a suffix in that profile's config (longest match wins);
/// 2. a domain the tailnet itself serves through Tailscale DNS (split DNS or a
///    search domain), learned from the running daemon;
/// 3. its address: an IP literal of a device on that tailnet, or a name that
///    resolves (in ordinary DNS) to one, like a custom domain pointed at a
///    tailnet machine. Such a name is remembered as an exact host (never its
///    whole domain) once Tailfuse has resolved it, so browsers can be sent to
///    it from the PAC without a DNS lookup per site; it is re-checked each
///    learning round and forgotten when it stops pointing at that tailnet.
///
/// Learned domains and addresses only count when exactly one tailnet claims
/// them: tailnets hand out addresses from the same 100.64.0.0/10 range, and an
/// ambiguous match is refused, never guessed.
#[derive(Debug, Clone, Default)]
pub struct RouteTable {
    by_suffix: BTreeMap<String, (String, u16)>,
    ports: BTreeMap<String, u16>,
    learned: Arc<RwLock<Learned>>,
    /// Where remembered hosts are kept between sessions (None in tests).
    hosts_file: Option<std::path::PathBuf>,
}

/// What each running tailnet reported about itself (DaemonSet::learn), plus
/// the exact hosts found pointing at its devices.
#[derive(Debug, Default)]
struct Learned {
    domains: BTreeMap<String, BTreeSet<String>>,
    ips: HashMap<IpAddr, BTreeSet<String>>,
    /// Exact hostname -> the one tailnet it points at.
    hosts: BTreeMap<String, String>,
}

/// True for an address Tailscale assigns: 100.64.0.0/10 or fd7a:115c:a1e0::/48.
pub fn is_tailscale_ip(ip: &IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => {
            let o = v4.octets();
            o[0] == 100 && (o[1] & 0xc0) == 64
        }
        IpAddr::V6(v6) => {
            let s = v6.segments();
            s[0] == 0xfd7a && s[1] == 0x115c && s[2] == 0xa1e0
        }
    }
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
        let hosts_file = crate::config::state_dir().join("learned-hosts.json");
        let hosts: BTreeMap<String, String> = std::fs::read_to_string(&hosts_file)
            .ok()
            .and_then(|t| serde_json::from_str(&t).ok())
            .unwrap_or_default();
        let hosts = hosts
            .into_iter()
            .filter(|(_, p)| socks_ports.contains_key(p))
            .collect();
        Self {
            by_suffix,
            ports: socks_ports.clone(),
            learned: Arc::new(RwLock::new(Learned {
                hosts,
                ..Default::default()
            })),
            hosts_file: Some(hosts_file),
        }
    }

    /// The tailnet a hostname (or IP literal) goes to, without a DNS lookup:
    /// configured suffix, then learned domain, then a known Tailscale address.
    pub fn owner(&self, host: &str) -> Option<(String, u16)> {
        let host = host.trim_end_matches('.').trim_matches(['[', ']']).to_lowercase();
        let mut best: Option<(&str, u16, usize)> = None;
        for (suffix, (profile, port)) in &self.by_suffix {
            if host.ends_with(suffix.as_str())
                && best.map(|(_, _, l)| suffix.len() > l).unwrap_or(true)
            {
                best = Some((profile.as_str(), *port, suffix.len()));
            }
        }
        if let Some((p, port, _)) = best {
            return Some((p.to_string(), port));
        }
        let learned = self.learned.read().ok()?;
        // Longest learned domain that the host is, or is under.
        let mut found: Option<(&BTreeSet<String>, usize)> = None;
        for (domain, owners) in &learned.domains {
            let hit = host == *domain || host.ends_with(&format!(".{domain}"));
            if hit && found.map(|(_, l)| domain.len() > l).unwrap_or(true) {
                found = Some((owners, domain.len()));
            }
        }
        if let Some((owners, _)) = found {
            return self.only(owners);
        }
        if let Some(profile) = learned.hosts.get(&host) {
            return self.ports.get(profile).map(|p| (profile.clone(), *p));
        }
        if let Ok(ip) = host.parse::<IpAddr>() {
            return learned.ips.get(&ip).and_then(|o| self.only(o));
        }
        None
    }

    /// Like [`owner`](Self::owner), and when nothing matches, resolve the name
    /// and route it if it points at a device on exactly one tailnet; such a
    /// name is then remembered, so browsers reach it too.
    pub async fn resolve_owner(&self, host: &str) -> Option<(String, u16)> {
        if let Some(found) = self.owner(host) {
            return Some(found);
        }
        let host = host.trim_end_matches('.').to_lowercase();
        if host.parse::<IpAddr>().is_ok() || !self.has_learned_ips() {
            return None;
        }
        let found = self.lookup_tailnet(&host).await?;
        self.remember(&host, &found.0);
        Some(found)
    }

    /// The one tailnet whose online devices `host` resolves to, if any. Every
    /// Tailscale address it resolves to must belong to that same tailnet.
    async fn lookup_tailnet(&self, host: &str) -> Option<(String, u16)> {
        let addrs = tokio::time::timeout(
            std::time::Duration::from_secs(3),
            tokio::net::lookup_host((host, 0)),
        )
        .await
        .ok()?
        .ok()?;
        let learned = self.learned.read().ok()?;
        let mut owners = BTreeSet::new();
        for a in addrs {
            let ip = a.ip();
            if !is_tailscale_ip(&ip) {
                continue;
            }
            match learned.ips.get(&ip) {
                Some(o) => owners.extend(o.iter().cloned()),
                None => return None,
            }
        }
        self.only(&owners)
    }

    fn remember(&self, host: &str, profile: &str) {
        let Ok(mut l) = self.learned.write() else {
            return;
        };
        if l.hosts.get(host).map(String::as_str) == Some(profile) {
            return;
        }
        l.hosts.insert(host.to_string(), profile.to_string());
        tracing::info!(%host, %profile, "remembered: points at a device on this tailnet");
        self.save_hosts(&l.hosts);
    }

    fn save_hosts(&self, hosts: &BTreeMap<String, String>) {
        if let (Some(path), Ok(text)) = (&self.hosts_file, serde_json::to_string_pretty(hosts)) {
            let _ = std::fs::write(path, text);
        }
    }

    /// Re-check the remembered hosts: keep each only while it still resolves
    /// to a device on the same tailnet. Called after every learning round.
    pub async fn recheck_hosts(&self) {
        let hosts: Vec<(String, String)> = match self.learned.read() {
            Ok(l) => l.hosts.iter().map(|(h, p)| (h.clone(), p.clone())).collect(),
            Err(_) => return,
        };
        let mut gone = Vec::new();
        for (host, profile) in hosts {
            if self.lookup_tailnet(&host).await.map(|f| f.0) != Some(profile) {
                gone.push(host);
            }
        }
        if gone.is_empty() {
            return;
        }
        if let Ok(mut l) = self.learned.write() {
            for h in &gone {
                l.hosts.remove(h);
                tracing::info!(host = %h, "forgotten: no longer points at that tailnet");
            }
            self.save_hosts(&l.hosts);
        }
    }

    fn has_learned_ips(&self) -> bool {
        self.learned.read().map(|l| !l.ips.is_empty()).unwrap_or(false)
    }

    /// The single profile in `owners` and its port, or None when it is empty
    /// or ambiguous.
    fn only(&self, owners: &BTreeSet<String>) -> Option<(String, u16)> {
        if owners.len() != 1 {
            return None;
        }
        let name = owners.iter().next()?;
        self.ports.get(name).map(|p| (name.clone(), *p))
    }

    /// Replace what one tailnet serves: its devices' addresses and its DNS domains.
    pub fn set_learned(&self, profile: &str, ips: &[IpAddr], domains: &[String]) {
        let Ok(mut l) = self.learned.write() else {
            return;
        };
        for set in l.ips.values_mut() {
            set.remove(profile);
        }
        l.ips.retain(|_, s| !s.is_empty());
        for set in l.domains.values_mut() {
            set.remove(profile);
        }
        l.domains.retain(|_, s| !s.is_empty());
        for ip in ips {
            l.ips.entry(*ip).or_default().insert(profile.to_string());
        }
        for d in domains {
            l.domains.entry(d.clone()).or_default().insert(profile.to_string());
        }
    }

    /// For the PAC: learned domains and addresses that one tailnet alone
    /// claims, and the remembered exact hosts.
    pub fn pac_extras(&self) -> (Vec<String>, Vec<String>, Vec<String>) {
        let Ok(l) = self.learned.read() else {
            return (Vec::new(), Vec::new(), Vec::new());
        };
        let hosts = l.hosts.keys().cloned().collect();
        let domains = l
            .domains
            .iter()
            .filter(|(_, o)| o.len() == 1)
            .map(|(d, _)| d.clone())
            .collect();
        let ips = l
            .ips
            .iter()
            .filter(|(_, o)| o.len() == 1)
            .map(|(ip, _)| ip.to_string())
            .collect();
        (domains, hosts, ips)
    }

    /// Upstream SOCKS5 port for a named profile (for tunnels).
    pub fn profile_port(&self, profile: &str) -> Option<u16> {
        self.ports.get(profile).copied()
    }
}

pub struct Router {
    routes: Arc<RouteTable>,
    pac_suffixes: Vec<String>,
    listen: String,
}

impl Router {
    pub fn new(cfg: &crate::config::Config, listen_addr: &str, routes: RouteTable) -> Self {
        Self {
            routes: Arc::new(routes),
            pac_suffixes: pac::config_suffixes(cfg),
            listen: listen_addr.to_string(),
        }
    }

    pub fn routes(&self) -> Arc<RouteTable> {
        self.routes.clone()
    }

    /// Bind and serve in the background until `shutdown` fires.
    /// Returns the bound address (useful when `listen` specifies port 0).
    pub async fn serve(
        self: Arc<Self>,
        listen: &str,
        mut shutdown: watch::Receiver<bool>,
    ) -> Result<std::net::SocketAddr> {
        let listener = TcpListener::bind(listen)
            .await
            .with_context(|| format!("router bind {listen}"))?;
        let addr = listener.local_addr()?;
        tracing::info!(%addr, "router listening");
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return;
                        }
                    }
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
        });
        Ok(addr)
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

        let Some((_, upstream_port)) = self.routes.resolve_owner(&host).await else {
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
            // Built per request, so browsers always get what the tailnets serve now.
            let (domains, hosts, ips) = self.routes.pac_extras();
            let mut suffixes = self.pac_suffixes.clone();
            suffixes.extend(domains);
            let body = pac::generate_from(&suffixes, &hosts, &ips, &self.listen).into_bytes();
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
        let Some((_, upstream_port)) = self.routes.resolve_owner(&host).await else {
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

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::sync::mpsc;

    /// A fake per-profile SOCKS5 upstream: completes the handshake, records
    /// the requested destination, then sends a marker byte proving which
    /// upstream the client landed on.
    async fn fake_upstream(marker: u8, seen: mpsc::Sender<String>) -> u16 {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            loop {
                let Ok((mut s, _)) = listener.accept().await else {
                    return;
                };
                let seen = seen.clone();
                tokio::spawn(async move {
                    // Greeting.
                    let mut h = [0u8; 2];
                    if s.read_exact(&mut h).await.is_err() {
                        return;
                    }
                    let mut m = vec![0u8; h[1] as usize];
                    let _ = s.read_exact(&mut m).await;
                    let _ = s.write_all(&[0x05, 0x00]).await;
                    // Request.
                    let mut r = [0u8; 4];
                    if s.read_exact(&mut r).await.is_err() {
                        return;
                    }
                    let len = s.read_u8().await.unwrap_or(0);
                    let mut host = vec![0u8; len as usize];
                    let _ = s.read_exact(&mut host).await;
                    let mut pb = [0u8; 2];
                    let _ = s.read_exact(&mut pb).await;
                    let _ = seen
                        .send(format!(
                            "{}:{}",
                            String::from_utf8_lossy(&host),
                            u16::from_be_bytes(pb)
                        ))
                        .await;
                    let _ = s
                        .write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
                        .await;
                    let _ = s.write_u8(marker).await;
                    let _ = s.flush().await;
                });
            }
        });
        port
    }

    async fn test_router() -> (std::net::SocketAddr, watch::Sender<bool>) {
        let mut cfg = crate::config::Config::default();
        cfg.profiles.insert(
            "work".into(),
            crate::config::Profile {
                suffixes: vec![".work.ts.net".into()],
                ..Default::default()
            },
        );
        cfg.profiles.insert(
            "personal".into(),
            crate::config::Profile {
                suffixes: vec![".home-lab.ts.net".into()],
                ..Default::default()
            },
        );
        let (_tx_work, rx_work) = mpsc::channel(8);
        let (_tx_pers, rx_pers) = mpsc::channel(8);
        let work_port = fake_upstream(b'W', _tx_work).await;
        let pers_port = fake_upstream(b'P', _tx_pers).await;
        let _ = rx_work;
        let _ = rx_pers;
        let routes = RouteTable {
            by_suffix: BTreeMap::from([
                (".work.ts.net".into(), ("work".into(), work_port)),
                (".home-lab.ts.net".into(), ("personal".into(), pers_port)),
            ]),
            ports: BTreeMap::from([("work".into(), work_port), ("personal".into(), pers_port)]),
            ..Default::default()
        };
        let router = Arc::new(Router::new(&cfg, "127.0.0.1:0", routes));
        let (tx, rx) = watch::channel(false);
        let addr = router.serve("127.0.0.1:0", rx).await.unwrap();
        (addr, tx)
    }

    #[tokio::test]
    async fn socks5_routes_by_suffix() {
        let (addr, _tx) = test_router().await;
        let mut s = socks5_connect_pub(addr.port(), "db.work.ts.net", 5432)
            .await
            .unwrap();
        assert_eq!(s.read_u8().await.unwrap(), b'W');

        let mut s2 = socks5_connect_pub(addr.port(), "nas.home-lab.ts.net", 80)
            .await
            .unwrap();
        assert_eq!(s2.read_u8().await.unwrap(), b'P');
    }

    #[tokio::test]
    async fn socks5_refuses_unowned() {
        let (addr, _tx) = test_router().await;
        // Our client errors on the failure reply; assert it does not connect.
        assert!(socks5_connect_pub(addr.port(), "evil.example.com", 443)
            .await
            .is_err());
    }

    #[tokio::test]
    async fn http_connect_routes_and_refuses() {
        let (addr, _tx) = test_router().await;
        let mut s = TcpStream::connect(addr).await.unwrap();
        s.write_all(b"CONNECT wiki.work.ts.net:443 HTTP/1.1\r\nHost: wiki.work.ts.net:443\r\n\r\n")
            .await
            .unwrap();
        let mut buf = [0u8; 39];
        s.read_exact(&mut buf).await.unwrap();
        assert!(String::from_utf8_lossy(&buf).starts_with("HTTP/1.1 200"));
        assert_eq!(s.read_u8().await.unwrap(), b'W');

        let mut s2 = TcpStream::connect(addr).await.unwrap();
        s2.write_all(b"CONNECT evil.example.com:443 HTTP/1.1\r\n\r\n")
            .await
            .unwrap();
        let mut buf2 = vec![0u8; 128];
        let n = s2.read(&mut buf2).await.unwrap();
        let text = String::from_utf8_lossy(&buf2[..n]).to_string();
        assert!(text.starts_with("HTTP/1.1 403"), "got: {text}");
        assert!(text.contains("no tailfuse profile owns"));
    }

    #[tokio::test]
    async fn pac_endpoint_served() {
        let (addr, _tx) = test_router().await;
        let mut s = TcpStream::connect(addr).await.unwrap();
        s.write_all(b"GET /tailfuse.pac HTTP/1.1\r\nHost: x\r\n\r\n")
            .await
            .unwrap();
        // The reply can arrive in more than one read: collect until the PAC body is in.
        let mut text = String::new();
        let mut buf = vec![0u8; 4096];
        for _ in 0..50 {
            let n = s.read(&mut buf).await.unwrap();
            if n == 0 {
                break;
            }
            text.push_str(&String::from_utf8_lossy(&buf[..n]));
            if text.contains("work.ts.net") {
                break;
            }
        }
        assert!(text.contains("200 OK"), "got: {text}");
        assert!(text.contains("work.ts.net"), "got: {text}");
    }

    #[test]
    fn learned_domains_and_addresses_route_only_when_unambiguous() {
        let rt = RouteTable {
            by_suffix: BTreeMap::from([(".work.ts.net".into(), ("work".into(), 1))]),
            ports: BTreeMap::from([("work".into(), 1), ("ailio".into(), 2), ("change".into(), 3)]),
            ..Default::default()
        };
        let ip: IpAddr = "100.74.194.18".parse().unwrap();
        let shared: IpAddr = "100.70.0.9".parse().unwrap();
        rt.set_learned("ailio", &[ip, shared], &["ailio.dev".into()]);
        rt.set_learned("change", &[shared], &[]);
        // Learned domain, and its subdomains.
        assert_eq!(rt.owner("ailio.dev").map(|o| o.0), Some("ailio".into()));
        assert_eq!(rt.owner("litellm.ailio.dev").map(|o| o.0), Some("ailio".into()));
        assert!(rt.owner("notailio.dev").is_none());
        // Configured suffixes still win.
        assert_eq!(rt.owner("db.work.ts.net").map(|o| o.0), Some("work".into()));
        // An address one tailnet has routes there; one two tailnets have is refused.
        assert_eq!(rt.owner("100.74.194.18").map(|o| o.0), Some("ailio".into()));
        assert!(rt.owner("100.70.0.9").is_none());
        let (domains, _hosts, ips) = rt.pac_extras();
        assert_eq!(domains, vec!["ailio.dev".to_string()]);
        assert_eq!(ips, vec!["100.74.194.18".to_string()]);
        // A new report replaces the old one.
        rt.set_learned("ailio", &[], &[]);
        assert!(rt.owner("ailio.dev").is_none());
    }

    #[test]
    fn tailscale_ranges() {
        assert!(is_tailscale_ip(&"100.64.0.1".parse().unwrap()));
        assert!(is_tailscale_ip(&"100.127.255.254".parse().unwrap()));
        assert!(!is_tailscale_ip(&"100.128.0.1".parse().unwrap()));
        assert!(!is_tailscale_ip(&"104.21.71.112".parse().unwrap()));
        assert!(is_tailscale_ip(&"fd7a:115c:a1e0::1".parse().unwrap()));
    }
}
