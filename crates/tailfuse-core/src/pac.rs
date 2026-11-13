//! PAC file generation: owned suffixes → the tailfuse router, everything
//! else direct. Served live at `/tailfuse.pac` so the browser always has
//! current routes.

use crate::config::Config;

pub fn generate(cfg: &Config, proxy_addr: &str) -> String {
    let mut hosts: Vec<String> = cfg
        .profiles
        .values()
        .flat_map(|p| p.suffixes.iter())
        .map(|s| s.trim().trim_start_matches('.').to_lowercase())
        .collect();
    hosts.sort();
    hosts.dedup();

    let conds: Vec<String> = hosts
        .iter()
        .map(|h| format!("dnsDomainIs(host, {h:?})"))
        .collect();
    format!(
        "// tailfuse PAC — generated; served live at http://{proxy_addr}/tailfuse.pac\n\
         function FindProxyForURL(url, host) {{\n  \
         host = host.toLowerCase();\n  \
         if (\n      {}\n  ) {{\n    \
         return \"SOCKS5 {proxy_addr}\";\n  }}\n  \
         return \"DIRECT\";\n}}\n",
        conds.join(" ||\n      ")
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
        assert!(pac.contains("dnsDomainIs(host, \"work.ts.net\")"));
        assert!(pac.contains("SOCKS5 127.0.0.1:43100"));
        assert!(pac.contains("DIRECT"));
    }
}
