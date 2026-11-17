//! PAC file generation: owned suffixes → the tailfuse router; any other name
//! that resolves to a Tailscale address (100.64.0.0/10) → the router too, which
//! decides the tailnet and remembers the name; everything else direct. Served live at `/tailfuse.pac` so the browser always has
//! current routes.

use crate::config::Config;

/// The configured suffixes, normalized for `dnsDomainIs`.
pub fn config_suffixes(cfg: &Config) -> Vec<String> {
    cfg.profiles
        .values()
        .flat_map(|p| p.suffixes.iter())
        .map(|s| s.trim().trim_start_matches('.').to_lowercase())
        .collect()
}

/// Names matching nothing above: resolve once, and send those that land in the
/// Tailscale ranges (100.64.0.0/10, fd7a:115c:a1e0::/48) to the router.
const FALLBACK: &str = "  if (host.indexOf(\".\") > 0 && !/^[0-9.]+$/.test(host) && host.indexOf(\":\") < 0) {\n\
    var c = FindProxyForURL.cache || (FindProxyForURL.cache = {});\n\
    if (!(host in c)) {\n\
      var ip = dnsResolve(host);\n\
      c[host] = !!ip && (isInNet(ip, \"100.64.0.0\", \"255.192.0.0\") || ip.indexOf(\"fd7a:115c:a1e0:\") == 0);\n\
    }\n\
    if (c[host]) return PROXY_ADDR;\n\
  }\n";

pub fn generate(cfg: &Config, proxy_addr: &str) -> String {
    generate_from(&config_suffixes(cfg), &[], &[], proxy_addr)
}

/// A PAC for these domains (each also matching its subdomains), these exact
/// hosts and these Tailscale addresses (matched only when the URL uses the
/// address itself). Those checks are string comparisons. Only a name matching
/// none of them is looked up once (the answer is cached in the PAC), to catch
/// public DNS names that point at a tailnet device, such as `ailio.dev`.
pub fn generate_from(
    suffixes: &[String],
    hosts: &[String],
    ips: &[String],
    proxy_addr: &str,
) -> String {
    let mut domains: Vec<String> = suffixes
        .iter()
        .map(|s| s.trim().trim_start_matches('.').to_lowercase())
        .filter(|s| !s.is_empty())
        .collect();
    domains.sort();
    domains.dedup();
    let mut conds: Vec<String> = domains
        .iter()
        // dnsDomainIs is a plain "ends with": match ".domain" so that
        // "notailio.dev" never counts as under "ailio.dev", plus the domain itself.
        .map(|h| format!("dnsDomainIs(host, \".{h}\") || host == {h:?}"))
        .collect();
    let mut exact: Vec<String> = hosts
        .iter()
        .chain(ips.iter())
        .map(|h| h.to_lowercase())
        .collect();
    exact.sort();
    exact.dedup();
    conds.extend(exact.iter().map(|h| format!("host == {h:?}")));
    let check = if conds.is_empty() {
        String::new()
    } else {
        format!(
            "  if (\n      {}\n  ) {{\n    return \"SOCKS5 {proxy_addr}\";\n  }}\n",
            conds.join(" ||\n      ")
        )
    };
    let fallback = FALLBACK.replace("PROXY_ADDR", &format!("\"SOCKS5 {proxy_addr}\""));
    format!(
        "// tailfuse PAC: generated; served live at http://{proxy_addr}/tailfuse.pac\n\
         function FindProxyForURL(url, host) {{\n  \
         host = host.toLowerCase().replace(/^\\[|\\]$/g, \"\");\n\
         {check}{fallback}  return \"DIRECT\";\n}}\n"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pac_routes_owned_only() {
        let mut c = Config::default();
        c.profiles.insert(
            "work".into(),
            crate::config::Profile {
                suffixes: vec![".work.ts.net".into()],
                ..Default::default()
            },
        );
        let pac = generate(&c, "127.0.0.1:43100");
        assert!(pac.contains("dnsDomainIs(host, \".work.ts.net\")"));
        assert!(pac.contains("SOCKS5 127.0.0.1:43100"));
        assert!(pac.contains("DIRECT"));
        // Unlisted names that resolve into the Tailscale range still reach the router.
        assert!(pac.contains("isInNet(ip, \"100.64.0.0\", \"255.192.0.0\")"));
        assert!(pac.contains("if (c[host]) return \"SOCKS5 127.0.0.1:43100\";"));
    }
}

#[cfg(test)]
mod pac_dump {
    #[test]
    #[ignore]
    fn dump() {
        let pac = super::generate_from(
            &["tail8e2dc7.ts.net".into(), "ailio.dev".into()],
            &["app.example.com".into()],
            &["100.74.194.18".into(), "fd7a:115c:a1e0::1".into()],
            "127.0.0.1:43100",
        );
        std::fs::write(std::env::var("PAC_OUT").unwrap(), pac).unwrap();
    }
}
