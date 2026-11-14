//! PAC file generation: owned suffixes → the tailfuse router, everything
//! else direct. Served live at `/tailfuse.pac` so the browser always has
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

pub fn generate(cfg: &Config, proxy_addr: &str) -> String {
    generate_from(&config_suffixes(cfg), &[], &[], proxy_addr)
}

/// A PAC for these domains (each also matching its subdomains), these exact
/// hosts and these Tailscale addresses (matched only when the URL uses the
/// address itself). Every check is a string comparison: the PAC never makes the
/// browser look a name up, so ordinary browsing pays nothing for it.
pub fn generate_from(suffixes: &[String], hosts: &[String], ips: &[String], proxy_addr: &str) -> String {
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
    let mut exact: Vec<String> = hosts.iter().chain(ips.iter()).map(|h| h.to_lowercase()).collect();
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
    format!(
        "// tailfuse PAC: generated; served live at http://{proxy_addr}/tailfuse.pac\n\
         function FindProxyForURL(url, host) {{\n  \
         host = host.toLowerCase().replace(/^\\[|\\]$/g, \"\");\n\
         {check}  return \"DIRECT\";\n}}\n"
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
