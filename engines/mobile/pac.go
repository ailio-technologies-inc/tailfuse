package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PAC returns a proxy auto-config script for these domains (each also
// matching its subdomains), these exact hosts and these Tailscale addresses
// (matched only when the URL uses the address itself). Every check is a string
// comparison: the PAC never makes the browser look a name up.
//
// Same text as the desktop core's pac.rs generate_from, except the proxy
// line: phones are given an HTTP proxy, so it offers the router as an HTTP
// proxy first and as SOCKS5 second (the router speaks both on one port).
func PAC(suffixes, hosts, ips []string, proxyAddr string) string {
	var domains []string
	for _, s := range suffixes {
		s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "."))
		if s != "" {
			domains = append(domains, s)
		}
	}
	domains = sortedUnique(domains)
	var conds []string
	for _, d := range domains {
		// dnsDomainIs is a plain "ends with": match ".domain" so that
		// "notailio.dev" never counts as under "ailio.dev", plus the domain.
		conds = append(conds, fmt.Sprintf("dnsDomainIs(host, %s) || host == %s", strconv.Quote("."+d), strconv.Quote(d)))
	}
	var exact []string
	for _, h := range append(append([]string(nil), hosts...), ips...) {
		exact = append(exact, strings.ToLower(h))
	}
	for _, h := range sortedUnique(exact) {
		conds = append(conds, "host == "+strconv.Quote(h))
	}
	check := ""
	if len(conds) > 0 {
		check = fmt.Sprintf("  if (\n      %s\n  ) {\n    return \"PROXY %s; SOCKS5 %s\";\n  }\n",
			strings.Join(conds, " ||\n      "), proxyAddr, proxyAddr)
	}
	return fmt.Sprintf("// tailfuse PAC: generated; served live at http://%s/tailfuse.pac\n"+
		"function FindProxyForURL(url, host) {\n"+
		"  host = host.toLowerCase().replace(/^\\[|\\]$/g, \"\");\n"+
		"%s  return \"DIRECT\";\n}\n", proxyAddr, check)
}

func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
