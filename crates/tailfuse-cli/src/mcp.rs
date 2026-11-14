//! `tailfuse mcp`: a Model Context Protocol server on stdio, so an AI assistant
//! (Claude, Cursor, ...) can see your tailnets and reach their services through
//! Tailfuse.
//!
//! It works against the running Tailfuse (the menu-bar app or `tailfuse up`):
//! it reads each tailnet's node over its private control socket and sends
//! traffic through the local router, so the same isolation rules apply: a
//! hostname no tailnet owns is refused, never sent anywhere.
//!
//! Register it with a client, e.g. Claude Code:
//!   claude mcp add tailfuse -- tailfuse mcp

use anyhow::{bail, Context, Result};
use serde_json::{json, Value};
use std::io::{BufRead, Read, Write};
use std::net::{SocketAddr, TcpStream, ToSocketAddrs};
use std::path::PathBuf;
use std::time::Duration;
use tailfuse_core::config::{self, Config};
use tailfuse_core::daemon;

const PROTOCOL_VERSION: &str = "2025-06-18";
/// Most of a response body handed back to the model.
const MAX_BODY: usize = 64 * 1024;

pub fn serve(cfg_path: PathBuf) -> Result<()> {
    // Called on a blocking thread inside the CLI's runtime.
    let rt = tokio::runtime::Handle::current();
    let stdin = std::io::stdin();
    let mut out = std::io::stdout();
    for line in stdin.lock().lines() {
        let line = line?;
        if line.trim().is_empty() {
            continue;
        }
        let msg: Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(e) => {
                write(
                    &mut out,
                    &error(Value::Null, -32700, &format!("parse error: {e}")),
                )?;
                continue;
            }
        };
        // Notifications (no id) need no answer.
        let Some(id) = msg.get("id").cloned() else {
            continue;
        };
        let method = msg["method"].as_str().unwrap_or_default();
        let reply = match method {
            "initialize" => ok(
                id,
                json!({
                    "protocolVersion": msg["params"]["protocolVersion"].as_str().unwrap_or(PROTOCOL_VERSION),
                    "capabilities": { "tools": {} },
                    "serverInfo": { "name": "tailfuse", "version": env!("CARGO_PKG_VERSION") },
                    "instructions": "Tailfuse connects this computer to several Tailscale tailnets at once. Use tailfuse_status to see which tailnets are connected, tailfuse_list_devices to find machines, tailfuse_which_tailnet to see where a hostname goes, and tailfuse_http_request or tailfuse_check_port to reach a service on a tailnet. Only hostnames a tailnet owns are reachable.",
                }),
            ),
            "ping" => ok(id, json!({})),
            "tools/list" => ok(id, json!({ "tools": tools() })),
            "tools/call" => {
                let name = msg["params"]["name"].as_str().unwrap_or_default();
                let args = msg["params"]["arguments"].clone();
                let cfg = config::load(&cfg_path);
                let result = match cfg {
                    Ok(cfg) => rt.block_on(call(name, &args, &cfg)),
                    Err(e) => Err(e),
                };
                match result {
                    Ok(v) => ok(
                        id,
                        json!({
                            "content": [{ "type": "text", "text": serde_json::to_string_pretty(&v)? }],
                            "structuredContent": v,
                        }),
                    ),
                    Err(e) => ok(
                        id,
                        json!({ "content": [{ "type": "text", "text": format!("{e:#}") }], "isError": true }),
                    ),
                }
            }
            _ => error(id, -32601, &format!("unknown method {method}")),
        };
        write(&mut out, &reply)?;
    }
    Ok(())
}

fn tools() -> Value {
    json!([
        {
            "name": "tailfuse_status",
            "title": "Tailnet status",
            "description": "List the tailnets Tailfuse manages: whether each is connected, the account it is signed in with, the hostname suffixes it owns, and this computer's name and Tailscale IPs on it.",
            "inputSchema": { "type": "object", "properties": {} },
            "annotations": { "readOnlyHint": true, "openWorldHint": false },
        },
        {
            "name": "tailfuse_list_devices",
            "title": "Devices on a tailnet",
            "description": "List the devices (peers) on one tailnet, or on all of them: name, DNS name, Tailscale IPs, operating system and whether each is online.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "tailnet": { "type": "string", "description": "Tailnet name as shown by tailfuse_status. Omit for all." },
                    "online_only": { "type": "boolean", "description": "Only devices that are online now.", "default": false },
                    "query": { "type": "string", "description": "Only devices whose name or DNS name contains this text." },
                },
            },
            "annotations": { "readOnlyHint": true, "openWorldHint": false },
        },
        {
            "name": "tailfuse_which_tailnet",
            "title": "Which tailnet owns a hostname",
            "description": "Say which tailnet a hostname or URL is routed to, or that no tailnet owns it (Tailfuse then refuses it).",
            "inputSchema": {
                "type": "object",
                "properties": { "host": { "type": "string", "description": "Hostname or URL, e.g. wiki.work.ts.net." } },
                "required": ["host"],
            },
            "annotations": { "readOnlyHint": true, "openWorldHint": false },
        },
        {
            "name": "tailfuse_http_request",
            "title": "HTTP request to a tailnet service",
            "description": "Make an HTTP(S) request to a service on one of your tailnets, routed by Tailfuse. Only URLs whose hostname a tailnet owns are allowed. Returns the status, headers and up to 64 KB of the body.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "url": { "type": "string", "description": "Full URL, e.g. http://wiki.work.ts.net/health." },
                    "method": { "type": "string", "enum": ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"], "default": "GET" },
                    "headers": { "type": "object", "additionalProperties": { "type": "string" } },
                    "body": { "type": "string", "description": "Request body, for POST, PUT and PATCH." },
                    "timeout_seconds": { "type": "number", "default": 20 },
                },
                "required": ["url"],
            },
            "annotations": { "readOnlyHint": false, "destructiveHint": true, "openWorldHint": false },
        },
        {
            "name": "tailfuse_check_port",
            "title": "Check a port on a tailnet",
            "description": "Check whether a TCP port on a tailnet host accepts connections (through Tailfuse), e.g. a database on db.work.ts.net:5432.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "host": { "type": "string" },
                    "port": { "type": "integer", "minimum": 1, "maximum": 65535 },
                },
                "required": ["host", "port"],
            },
            "annotations": { "readOnlyHint": true, "openWorldHint": false },
        },
        {
            "name": "tailfuse_set_connected",
            "title": "Connect or disconnect a tailnet",
            "description": "Connect or disconnect one tailnet. Tailfuse itself must be running (the menu-bar app is on, or `tailfuse up`).",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "tailnet": { "type": "string" },
                    "connected": { "type": "boolean" },
                },
                "required": ["tailnet", "connected"],
            },
            "annotations": { "readOnlyHint": false, "destructiveHint": false, "openWorldHint": false },
        },
    ])
}

async fn call(name: &str, args: &Value, cfg: &Config) -> Result<Value> {
    match name {
        "tailfuse_status" => status(cfg).await,
        "tailfuse_list_devices" => devices(cfg, args).await,
        "tailfuse_which_tailnet" => {
            let host = host_of(args["host"].as_str().context("host is required")?);
            Ok(match cfg.owner(&host) {
                Some((t, _)) => json!({ "host": host, "tailnet": t, "routed": true }),
                None => {
                    json!({ "host": host, "tailnet": null, "routed": false, "note": "No tailnet owns this hostname; Tailfuse refuses it." })
                }
            })
        }
        "tailfuse_http_request" => {
            let args = args.clone();
            let cfg = cfg.clone();
            tokio::task::spawn_blocking(move || http(&cfg, &args)).await?
        }
        "tailfuse_check_port" => {
            let host = host_of(args["host"].as_str().context("host is required")?);
            let port = args["port"].as_u64().context("port is required")? as u16;
            let tailnet = owned(cfg, &host)?;
            let router = router_addr(cfg)?;
            let started = std::time::Instant::now();
            let res =
                tokio::task::spawn_blocking(move || socks_connect(router, &host, port).map(|_| ()))
                    .await?;
            Ok(json!({
                "tailnet": tailnet,
                "open": res.is_ok(),
                "detail": res.err().map(|e| format!("{e:#}")),
                "milliseconds": started.elapsed().as_millis() as u64,
            }))
        }
        "tailfuse_set_connected" => {
            let t = args["tailnet"].as_str().context("tailnet is required")?;
            let on = args["connected"]
                .as_bool()
                .context("connected is required")?;
            if !cfg.profiles.contains_key(t) {
                bail!("no tailnet called {t:?}; see tailfuse_status");
            }
            daemon::set_connected(t, on).await?;
            Ok(json!({ "tailnet": t, "connected": on }))
        }
        _ => bail!("unknown tool {name}"),
    }
}

/// `tailscale status --json` for one tailnet's node, if it is running.
async fn node_status(name: &str) -> Option<Value> {
    let socket = config::profile_socket(name);
    if !socket.exists() {
        return None;
    }
    let text = daemon::tailscale_cli(&socket, &["status", "--json"])
        .await
        .ok()?;
    serde_json::from_str(&text).ok()
}

async fn status(cfg: &Config) -> Result<Value> {
    let mut list = Vec::new();
    for (name, prof) in &cfg.profiles {
        let st = node_status(name).await;
        let state = match st.as_ref().and_then(|s| s["BackendState"].as_str()) {
            None => "off",
            Some("Running") => "connected",
            Some("Stopped") => "disconnected",
            Some("NeedsLogin") => "needs sign-in",
            Some(_) => "starting",
        };
        let me = st.as_ref().map(|s| &s["Self"]);
        let account = st
            .as_ref()
            .and_then(|s| {
                let id = s["Self"]["UserID"].to_string();
                s["User"][id.as_str()]["LoginName"]
                    .as_str()
                    .map(String::from)
            })
            .unwrap_or_else(|| prof.account.clone());
        list.push(json!({
            "tailnet": name,
            "state": state,
            "account": account,
            "suffixes": prof.suffixes,
            "this_computer": me.and_then(|m| m["HostName"].as_str()),
            "ips": st.as_ref().map(|s| s["TailscaleIPs"].clone()).unwrap_or(json!([])),
        }));
    }
    Ok(json!({ "router": cfg.router.listen, "tailnets": list }))
}

async fn devices(cfg: &Config, args: &Value) -> Result<Value> {
    let only = args["tailnet"].as_str();
    if let Some(t) = only {
        if !cfg.profiles.contains_key(t) {
            bail!("no tailnet called {t:?}; see tailfuse_status");
        }
    }
    let online_only = args["online_only"].as_bool().unwrap_or(false);
    let query = args["query"].as_str().unwrap_or_default().to_lowercase();
    let mut out = Vec::new();
    for name in cfg.profiles.keys() {
        if only.is_some_and(|t| t != name) {
            continue;
        }
        let Some(st) = node_status(name).await else {
            continue;
        };
        let Some(peers) = st["Peer"].as_object() else {
            continue;
        };
        for p in peers.values() {
            let online = p["Online"].as_bool().unwrap_or(false);
            let host = p["HostName"].as_str().unwrap_or_default();
            let dns = p["DNSName"]
                .as_str()
                .unwrap_or_default()
                .trim_end_matches('.');
            if online_only && !online {
                continue;
            }
            if !query.is_empty()
                && !host.to_lowercase().contains(&query)
                && !dns.to_lowercase().contains(&query)
            {
                continue;
            }
            out.push(json!({
                "tailnet": name,
                "name": host,
                "dns_name": dns,
                "ips": p["TailscaleIPs"],
                "os": p["OS"],
                "online": online,
            }));
        }
    }
    Ok(json!({ "count": out.len(), "devices": out }))
}

fn host_of(input: &str) -> String {
    let t = input.trim();
    let t = t.split_once("://").map(|(_, r)| r).unwrap_or(t);
    let t = t.split(['/', '?', '#']).next().unwrap_or_default();
    let t = t.rsplit_once('@').map(|(_, h)| h).unwrap_or(t);
    let t = if t.starts_with('[') {
        t.split(']')
            .next()
            .unwrap_or_default()
            .trim_start_matches('[')
    } else {
        t.split(':').next().unwrap_or_default()
    };
    t.trim_end_matches('.').to_lowercase()
}

/// The tailnet that owns `host`, or an error: the same refusal the router applies.
fn owned(cfg: &Config, host: &str) -> Result<String> {
    match cfg.owner(host) {
        Some((t, _)) => Ok(t.to_string()),
        None => bail!("no tailnet owns {host}; Tailfuse only reaches hostnames a tailnet owns (see tailfuse_status for the suffixes)"),
    }
}

fn router_addr(cfg: &Config) -> Result<SocketAddr> {
    cfg.router
        .listen
        .to_socket_addrs()?
        .next()
        .context("bad router address")
}

/// Open a TCP connection to `host:port` through the router's SOCKS5 port,
/// with the hostname resolved on the tailnet side.
fn socks_connect(router: SocketAddr, host: &str, port: u16) -> Result<TcpStream> {
    let mut s = TcpStream::connect_timeout(&router, Duration::from_secs(3))
        .context("Tailfuse is not running (nothing on its router port); turn it on first")?;
    s.set_read_timeout(Some(Duration::from_secs(15)))?;
    s.set_write_timeout(Some(Duration::from_secs(15)))?;
    s.write_all(&[5, 1, 0])?;
    let mut hello = [0u8; 2];
    s.read_exact(&mut hello)?;
    if hello != [5, 0] {
        bail!("unexpected answer from the Tailfuse router");
    }
    let mut req = vec![5, 1, 0, 3, host.len() as u8];
    req.extend_from_slice(host.as_bytes());
    req.extend_from_slice(&port.to_be_bytes());
    s.write_all(&req)?;
    let mut head = [0u8; 4];
    s.read_exact(&mut head)?;
    if head[1] != 0 {
        bail!(
            "{host}:{port} did not accept the connection (SOCKS reply {})",
            head[1]
        );
    }
    let skip = match head[3] {
        1 => 4 + 2,
        4 => 16 + 2,
        3 => {
            let mut n = [0u8; 1];
            s.read_exact(&mut n)?;
            n[0] as usize + 2
        }
        _ => bail!("unexpected answer from the Tailfuse router"),
    };
    let mut rest = vec![0u8; skip];
    s.read_exact(&mut rest)?;
    s.set_read_timeout(None)?;
    s.set_write_timeout(None)?;
    Ok(s)
}

fn http(cfg: &Config, args: &Value) -> Result<Value> {
    let url = args["url"].as_str().context("url is required")?;
    if !(url.starts_with("http://") || url.starts_with("https://")) {
        bail!("only http:// and https:// URLs");
    }
    let host = host_of(url);
    let tailnet = owned(cfg, &host)?;
    let method = args["method"].as_str().unwrap_or("GET").to_uppercase();
    if !["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"].contains(&method.as_str()) {
        bail!("unsupported method {method}");
    }
    let timeout = args["timeout_seconds"]
        .as_f64()
        .unwrap_or(20.0)
        .clamp(1.0, 120.0);
    let agent = ureq::AgentBuilder::new()
        .proxy(ureq::Proxy::new(format!("socks5://{}", cfg.router.listen))?)
        .timeout(Duration::from_secs_f64(timeout))
        .redirects(5)
        .build();
    let mut req = agent.request(&method, url);
    if let Some(h) = args["headers"].as_object() {
        for (k, v) in h {
            if let Some(v) = v.as_str() {
                req = req.set(k, v);
            }
        }
    }
    let started = std::time::Instant::now();
    let res = match args["body"].as_str() {
        Some(b) if matches!(method.as_str(), "POST" | "PUT" | "PATCH") => req.send_string(b),
        _ => req.call(),
    };
    let resp = match res {
        Ok(r) | Err(ureq::Error::Status(_, r)) => r,
        Err(e) => bail!("request failed: {e}"),
    };
    let status = resp.status();
    let headers: serde_json::Map<String, Value> = resp
        .headers_names()
        .into_iter()
        .filter_map(|n| resp.header(&n).map(|v| (n, Value::String(v.to_string()))))
        .collect();
    let mut buf = Vec::new();
    resp.into_reader()
        .take(MAX_BODY as u64 + 1)
        .read_to_end(&mut buf)?;
    let truncated = buf.len() > MAX_BODY;
    buf.truncate(MAX_BODY);
    Ok(json!({
        "tailnet": tailnet,
        "status": status,
        "headers": headers,
        "body": String::from_utf8_lossy(&buf),
        "truncated": truncated,
        "milliseconds": started.elapsed().as_millis() as u64,
    }))
}

fn ok(id: Value, result: Value) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "result": result })
}

fn error(id: Value, code: i64, message: &str) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "error": { "code": code, "message": message } })
}

fn write(out: &mut impl Write, v: &Value) -> Result<()> {
    writeln!(out, "{}", serde_json::to_string(v)?)?;
    out.flush()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hosts_from_urls() {
        assert_eq!(
            host_of("https://Wiki.Work.ts.net:8443/a?b"),
            "wiki.work.ts.net"
        );
        assert_eq!(host_of("db.work.ts.net"), "db.work.ts.net");
        assert_eq!(
            host_of("http://user@nas.home-lab.ts.net/"),
            "nas.home-lab.ts.net"
        );
    }
}
