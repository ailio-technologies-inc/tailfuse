package engine

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// RouteTable decides which tailnet a hostname goes to. In order:
//
//  1. a suffix in that profile's config (longest match wins);
//  2. a domain the tailnet itself serves through Tailscale DNS (split DNS or a
//     search domain), learned from the running node;
//  3. its address: an IP literal of a device on that tailnet, or a name that
//     resolves (in ordinary DNS) to one, like a custom domain pointed at a
//     tailnet machine. Such a name is remembered as an exact host (never its
//     whole domain), re-checked every learning round and forgotten when it
//     stops pointing at that tailnet.
//
// Learned domains and addresses only count when exactly one tailnet claims
// them: tailnets hand out addresses from the same 100.64.0.0/10 range, and an
// ambiguous match is refused, never guessed. A name nothing owns is refused.
//
// Port of the desktop core's router.rs RouteTable.
type RouteTable struct {
	mu        sync.RWMutex
	bySuffix  map[string]string // suffix (leading dot) -> profile
	profiles  map[string]bool   // profiles that can carry traffic
	domains   map[string]map[string]bool
	ips       map[netip.Addr]map[string]bool
	hosts     map[string]string // exact host -> profile
	hostsFile string            // "" in tests
	resolver  Resolver

	// claims: the domains each tailnet last said it serves, kept while it is
	// off (and on disk between runs). They never route anything; they only
	// stop passthrough from sending an off tailnet's names to the internet.
	claims     map[string][]string
	claimsFile string
}

// Resolver looks a name up in ordinary DNS.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// NewRouteTable returns an empty table. hostsFile (may be "") keeps the
// remembered hosts between runs.
func NewRouteTable(hostsFile string, r Resolver) *RouteTable {
	rt := &RouteTable{
		bySuffix:  map[string]string{},
		profiles:  map[string]bool{},
		domains:   map[string]map[string]bool{},
		ips:       map[netip.Addr]map[string]bool{},
		hosts:     map[string]string{},
		hostsFile: hostsFile,
		resolver:  r,
	}
	rt.claims = map[string][]string{}
	if hostsFile != "" {
		if data, err := os.ReadFile(hostsFile); err == nil {
			_ = json.Unmarshal(data, &rt.hosts)
		}
		rt.claimsFile = filepath.Join(filepath.Dir(hostsFile), "learned-domains.json")
		if data, err := os.ReadFile(rt.claimsFile); err == nil {
			_ = json.Unmarshal(data, &rt.claims)
		}
	}
	return rt
}

// SetConfig replaces the configured suffixes. Learned data for profiles that
// are gone is dropped, and remembered hosts pointing at them are forgotten.
func (rt *RouteTable) SetConfig(c *Config) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.bySuffix = map[string]string{}
	rt.profiles = map[string]bool{}
	for name, p := range c.Profiles {
		if p.Pending {
			continue
		}
		rt.profiles[name] = true
		for _, s := range p.Suffixes {
			rt.bySuffix[NormalizeSuffix(s)] = name
		}
	}
	for d, set := range rt.domains {
		for p := range set {
			if !rt.profiles[p] {
				delete(set, p)
			}
		}
		if len(set) == 0 {
			delete(rt.domains, d)
		}
	}
	for ip, set := range rt.ips {
		for p := range set {
			if !rt.profiles[p] {
				delete(set, p)
			}
		}
		if len(set) == 0 {
			delete(rt.ips, ip)
		}
	}
	changed := false
	for h, p := range rt.hosts {
		if !rt.profiles[p] {
			delete(rt.hosts, h)
			changed = true
		}
	}
	if changed {
		rt.saveHostsLocked()
	}
	pruned := false
	for p := range rt.claims {
		if !rt.profiles[p] {
			delete(rt.claims, p)
			pruned = true
		}
	}
	if pruned {
		rt.saveClaimsLocked()
	}
}

func (rt *RouteTable) saveClaimsLocked() {
	if rt.claimsFile == "" {
		return
	}
	data, err := json.MarshalIndent(rt.claims, "", "  ")
	if err != nil {
		return
	}
	tmp := rt.claimsFile + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, rt.claimsFile)
	}
}

// Owner is the tailnet a hostname (or IP literal) goes to without a DNS
// lookup: configured suffix, then learned domain, then a remembered host,
// then a known Tailscale address. "" when nothing owns it.
func (rt *RouteTable) Owner(host string) string {
	host = normalizeHost(host)
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	best, bestLen := "", 0
	for s, p := range rt.bySuffix {
		if suffixCovers(host, s) && (len(s) > bestLen || (len(s) == bestLen && p < best)) {
			best, bestLen = p, len(s)
		}
	}
	if best != "" {
		return best
	}
	var found map[string]bool
	foundLen := -1
	for d, owners := range rt.domains {
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > foundLen {
			found, foundLen = owners, len(d)
		}
	}
	if found != nil {
		return rt.onlyLocked(found)
	}
	if p, ok := rt.hosts[host]; ok && rt.profiles[p] {
		return p
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return rt.onlyLocked(rt.ips[ip.Unmap()])
	}
	return ""
}

// Verdict is how the rules see a name, for passthrough.
type Verdict int

const (
	// Unowned: no tailnet has anything to do with the name.
	Unowned Verdict = iota
	// Owned: one tailnet owns it (it may be off; it still goes nowhere else).
	Owned
	// Conflict: two or more tailnets claim it; refused.
	Conflict
	// TailnetSpace: a Tailscale address no running tailnet has; refused.
	TailnetSpace
	// Claimed: under a domain a tailnet served when it last ran; that tailnet
	// is off now, so the name is refused rather than looked up publicly.
	Claimed
)

// Classify is Owner with the reason when nothing routes the name: it tells
// a conflict, Tailscale address space and an off tailnet's domain apart from
// a name no tailnet has anything to do with. Owner's answers are unchanged.
func (rt *RouteTable) Classify(host string) (string, Verdict) {
	if p := rt.Owner(host); p != "" {
		return p, Owned
	}
	host = normalizeHost(host)
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	foundLen := -1
	var found map[string]bool
	for d, owners := range rt.domains {
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > foundLen {
			found, foundLen = owners, len(d)
		}
	}
	if len(found) > 1 {
		return "", Conflict
	}
	if p, ok := rt.hosts[host]; ok {
		return p, Claimed // remembered for a profile that cannot carry traffic
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if len(rt.ips[ip]) > 1 {
			return "", Conflict
		}
		if IsTailscaleIP(ip) {
			return "", TailnetSpace
		}
		return "", Unowned
	}
	for p, ds := range rt.claims {
		for _, d := range ds {
			if host == d || strings.HasSuffix(host, "."+d) {
				return p, Claimed
			}
		}
	}
	return "", Unowned
}

// ResolveOwner is Owner, and when nothing matches, resolves the name and
// routes it if it points at a device on exactly one tailnet; such a name is
// then remembered, so the PAC and match_domains carry it too.
func (rt *RouteTable) ResolveOwner(ctx context.Context, host string) string {
	if p := rt.Owner(host); p != "" {
		return p
	}
	host = normalizeHost(host)
	if host == "" {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil || !rt.hasLearnedIPs() {
		return ""
	}
	p := rt.lookupTailnet(ctx, host)
	if p != "" {
		rt.remember(host, p)
	}
	return p
}

// lookupTailnet is the one tailnet whose online devices host resolves to.
// Every Tailscale address it resolves to must belong to that same tailnet.
func (rt *RouteTable) lookupTailnet(ctx context.Context, host string) string {
	if rt.resolver == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := rt.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return ""
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	owners := map[string]bool{}
	for _, a := range addrs {
		a = a.Unmap()
		if !IsTailscaleIP(a) {
			continue
		}
		o, ok := rt.ips[a]
		if !ok {
			return ""
		}
		for p := range o {
			owners[p] = true
		}
	}
	return rt.onlyLocked(owners)
}

func (rt *RouteTable) remember(host, profile string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.hosts[host] == profile {
		return
	}
	rt.hosts[host] = profile
	rt.saveHostsLocked()
}

func (rt *RouteTable) saveHostsLocked() {
	if rt.hostsFile == "" {
		return
	}
	data, err := json.MarshalIndent(rt.hosts, "", "  ")
	if err != nil {
		return
	}
	tmp := rt.hostsFile + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, rt.hostsFile)
	}
}

// RecheckHosts keeps each remembered host only while it still resolves to a
// device on the same tailnet. Called after every learning round. Reports
// whether anything was forgotten.
func (rt *RouteTable) RecheckHosts(ctx context.Context) bool {
	rt.mu.RLock()
	hosts := make(map[string]string, len(rt.hosts))
	for h, p := range rt.hosts {
		hosts[h] = p
	}
	rt.mu.RUnlock()
	var gone []string
	for h, p := range hosts {
		if rt.lookupTailnet(ctx, h) != p {
			gone = append(gone, h)
		}
	}
	if len(gone) == 0 {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, h := range gone {
		delete(rt.hosts, h)
	}
	rt.saveHostsLocked()
	return true
}

func (rt *RouteTable) hasLearnedIPs() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.ips) > 0
}

// onlyLocked is the single profile in owners, or "" when it is empty,
// ambiguous, or that profile cannot carry traffic.
func (rt *RouteTable) onlyLocked(owners map[string]bool) string {
	if len(owners) != 1 {
		return ""
	}
	for p := range owners {
		if rt.profiles[p] {
			return p
		}
	}
	return ""
}

// SetLearned replaces what one tailnet serves: its online devices'
// addresses and its DNS domains. Only configured profiles can learn.
func (rt *RouteTable) SetLearned(profile string, ips []netip.Addr, domains []string) {
	rt.setLearned(profile, ips, domains, true)
}

func (rt *RouteTable) setLearned(profile string, ips []netip.Addr, domains []string, claim bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for ip, set := range rt.ips {
		delete(set, profile)
		if len(set) == 0 {
			delete(rt.ips, ip)
		}
	}
	for d, set := range rt.domains {
		delete(set, profile)
		if len(set) == 0 {
			delete(rt.domains, d)
		}
	}
	if !rt.profiles[profile] {
		return // a profile the table does not know (removed meanwhile)
	}
	if claim {
		var norm []string
		for _, d := range domains {
			if d = normalizeHost(d); d != "" && !slices.Contains(norm, d) {
				norm = append(norm, d)
			}
		}
		sort.Strings(norm)
		if !slices.Equal(norm, rt.claims[profile]) {
			if len(norm) == 0 {
				delete(rt.claims, profile)
			} else {
				rt.claims[profile] = norm
			}
			rt.saveClaimsLocked()
		}
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if rt.ips[ip] == nil {
			rt.ips[ip] = map[string]bool{}
		}
		rt.ips[ip][profile] = true
	}
	for _, d := range domains {
		d = normalizeHost(d)
		if d == "" {
			continue
		}
		if rt.domains[d] == nil {
			rt.domains[d] = map[string]bool{}
		}
		rt.domains[d][profile] = true
	}
}

// Forget drops everything learned from one tailnet (it stopped). Its
// claimed domains are kept, for passthrough to refuse.
func (rt *RouteTable) Forget(profile string) { rt.setLearned(profile, nil, nil, false) }

// Extras returns, sorted: learned domains one tailnet alone claims, the
// remembered exact hosts, and addresses one tailnet alone has. For the PAC
// and match_domains.
func (rt *RouteTable) Extras() (domains, hosts, ips []string) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for d, o := range rt.domains {
		if len(o) == 1 {
			domains = append(domains, d)
		}
	}
	for h := range rt.hosts {
		hosts = append(hosts, h)
	}
	for ip, o := range rt.ips {
		if len(o) == 1 {
			ips = append(ips, ip.String())
		}
	}
	sort.Strings(domains)
	sort.Strings(hosts)
	sort.Strings(ips)
	return domains, hosts, ips
}

// Suffixes returns the configured suffixes without their leading dot, sorted.
func (rt *RouteTable) Suffixes() []string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := make([]string, 0, len(rt.bySuffix))
	for s := range rt.bySuffix {
		out = append(out, strings.TrimPrefix(s, "."))
	}
	sort.Strings(out)
	return out
}

// MatchDomains is every name the router owns, without a leading dot:
// configured suffixes, learned domains one tailnet alone serves, and
// remembered hosts. Sorted and de-duplicated.
func (rt *RouteTable) MatchDomains() []string {
	domains, hosts, _ := rt.Extras()
	all := append(rt.Suffixes(), domains...)
	all = append(all, hosts...)
	sort.Strings(all)
	out := all[:0]
	for i, d := range all {
		if d != "" && (i == 0 || d != all[i-1]) {
			out = append(out, d)
		}
	}
	return out
}

// IsTailscaleIP reports whether ip is in 100.64.0.0/10 or fd7a:115c:a1e0::/48.
func IsTailscaleIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		b := ip.As4()
		return b[0] == 100 && b[1]&0xc0 == 64
	}
	if ip.Is6() {
		b := ip.As16()
		return b[0] == 0xfd && b[1] == 0x7a && b[2] == 0x11 && b[3] == 0x5c && b[4] == 0xa1 && b[5] == 0xe0
	}
	return false
}

// ServedDomains is the domains a tailnet's DNS answers: its split-DNS route
// keys (sorted) then its search domains. Tailscale's own "ts.net" route (the
// same on every tailnet) and single-label names are left out, so no tailnet
// claims another's MagicDNS names or a whole top-level domain. Port of the
// desktop core's served_domains.
func ServedDomains(routes []string, search []string) []string {
	routes = append([]string(nil), routes...)
	sort.Strings(routes)
	var out []string
	for _, d := range append(routes, search...) {
		d = normalizeHost(d)
		if d == "" || !strings.Contains(d, ".") || d == "ts.net" {
			continue
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// ViaAddress reports whether host is routed only because it was found to
// point at a device on a tailnet (a remembered exact host), not because a
// suffix or a served domain covers it.
func (rt *RouteTable) ViaAddress(host string) bool {
	host = normalizeHost(host)
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for s := range rt.bySuffix {
		if suffixCovers(host, s) {
			return false
		}
	}
	for d := range rt.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return false
		}
	}
	p, ok := rt.hosts[host]
	return ok && rt.profiles[p]
}

// PublicFallback reports whether an owned name may be looked up in ordinary
// DNS when its tailnet's own DNS has no answer: a domain of the person's own
// (a configured suffix outside ts.net, added with add_domain or synced) or a
// remembered host. MagicDNS names and domains a tailnet serves never are:
// they are private to the tailnet. The proxy path does the same (the node's
// DNS first, then the device's).
func (rt *RouteTable) PublicFallback(host string) bool {
	host = normalizeHost(host)
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	best := ""
	for s := range rt.bySuffix {
		if suffixCovers(host, s) && len(s) > len(best) {
			best = s
		}
	}
	if best != "" {
		return best != ".ts.net" && !strings.HasSuffix(best, ".ts.net")
	}
	for d := range rt.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return false
		}
	}
	p, ok := rt.hosts[host]
	return ok && rt.profiles[p]
}
