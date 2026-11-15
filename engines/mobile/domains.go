package engine

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// secondLevel: labels that, before a two-letter country code, make a
// public suffix of three parts (co.uk, com.au). A cheap stand-in for the
// public suffix list, enough to tell "ailio.dev" from "example.co.uk".
var secondLevel = map[string]bool{"co": true, "com": true, "org": true, "net": true, "gov": true, "ac": true, "edu": true, "ne": true, "or": true, "go": true}

func multiPartSuffix(a, b string) bool { return len(b) == 2 && secondLevel[a] }

// NormalizeDomain turns what a person typed ("Ailio.dev", "*.ailio.dev",
// "https://ailio.dev/") into a suffix with a leading dot (".ailio.dev"),
// which covers the domain and everything under it.
func NormalizeDomain(input string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(input))
	if _, rest, ok := strings.Cut(d, "://"); ok {
		d, _, _ = strings.Cut(rest, "/")
	}
	d = strings.TrimLeft(d, "*.")
	d = strings.TrimRight(d, ".")
	if d == "" {
		return "", errors.New("enter a domain, such as example.com")
	}
	if _, err := netip.ParseAddr(strings.Trim(d, "[]")); err == nil {
		return "", errors.New("that is an address; enter a domain name, such as example.com")
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return "", fmt.Errorf("%q is not a domain name", input)
		}
	}
	labels := strings.Split(d, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", fmt.Errorf("%q is not a domain name", input)
		}
	}
	if len(labels) < 2 {
		return "", fmt.Errorf("%q is a whole top-level domain; enter a domain of your own, such as example.com", d)
	}
	if len(labels) == 2 && multiPartSuffix(labels[0], labels[1]) {
		return "", fmt.Errorf("%q is a public suffix; enter a domain of your own under it", d)
	}
	if d == "ts.net" || strings.HasSuffix(d, ".ts.net") {
		return "", errors.New("a ts.net name belongs to the tailnet's own sign-in and is added for you")
	}
	return "." + d, nil
}

// SuggestDomain is the domain to offer for a host found through its address:
// the host itself with two labels, else its last two (three under a
// multi-part suffix like co.uk). "" when there is nothing sensible.
func SuggestDomain(host string) string {
	host = normalizeHost(host)
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return ""
	}
	labels := strings.Split(host, ".")
	n := len(labels)
	if n < 2 {
		return ""
	}
	take := 2
	if multiPartSuffix(labels[n-2], labels[n-1]) {
		take = 3
	}
	if take > n {
		take = n
	}
	d := strings.Join(labels[n-take:], ".")
	if _, err := NormalizeDomain(d); err != nil {
		return ""
	}
	return d
}

func isBuiltInSuffix(s string) bool { return strings.HasSuffix(NormalizeSuffix(s), ".ts.net") }

// AddDomain makes a tailnet own a domain (and everything under it).
func (e *Engine) AddDomain(name, domain string) error {
	suffix, err := NormalizeDomain(domain)
	if err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	p := e.cfg.Profiles[name]
	if p == nil {
		return fmt.Errorf("there is no tailnet called %q", name)
	}
	if p.Pending {
		return fmt.Errorf("tailnet %q has not finished signing in", name)
	}
	for owner, op := range e.cfg.Profiles {
		for _, s := range op.Suffixes {
			if NormalizeSuffix(s) == suffix {
				if owner == name {
					return nil
				}
				return fmt.Errorf("%s already belongs to the tailnet %q; remove it there first", suffix[1:], owner)
			}
		}
	}
	cfg := e.cfg.Clone()
	cfg.Profiles[name].Suffixes = append(cfg.Profiles[name].Suffixes, suffix)
	return e.commitLocked(cfg)
}

// RemoveDomain stops a tailnet owning a domain added with AddDomain. The
// tailnet's own ts.net name stays.
func (e *Engine) RemoveDomain(name, domain string) error {
	suffix, err := NormalizeDomain(domain)
	if err != nil && !strings.HasSuffix(strings.ToLower(strings.TrimSpace(domain)), "ts.net") {
		return err
	}
	if err != nil {
		return errors.New("the tailnet's own name cannot be removed")
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	if e.cfg.Profiles[name] == nil {
		return fmt.Errorf("there is no tailnet called %q", name)
	}
	cfg := e.cfg.Clone()
	p := cfg.Profiles[name]
	kept := p.Suffixes[:0]
	found := false
	for _, s := range p.Suffixes {
		if NormalizeSuffix(s) == suffix {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		return fmt.Errorf("%q does not own %s", name, suffix[1:])
	}
	p.Suffixes = kept
	return e.commitLocked(cfg)
}
