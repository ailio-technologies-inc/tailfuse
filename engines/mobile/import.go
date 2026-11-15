package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// SyncVersion is the setup-code format this engine reads.
const SyncVersion = 1

const maxSyncLink = 64 << 10

var (
	errNotSyncCode = errors.New("that is not a tailfuse sync code")
	errNewerCode   = errors.New("that code is from a newer tailfuse")
)

type syncProfile struct {
	Name     string   `json:"name"`
	Account  string   `json:"account"`
	Suffixes []string `json:"suffixes"`
	// accept_routes and exit_node are in the format too; the engine has no
	// such settings yet, so they are ignored.
}

type syncSetup struct {
	V        int           `json:"v"`
	From     string        `json:"from"`
	Profiles []syncProfile `json:"profiles"`
	Tunnels  []Tunnel      `json:"tunnels"`
}

// SyncSkip is one thing an import left out, and why.
type SyncSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// SyncResult is the import_setup result. Names are the ones in the code
// (the computer's); an added tailnet may be saved under another name when
// the computer's is taken or not valid here.
type SyncResult struct {
	From         string     `json:"from"`
	Added        []string   `json:"added"`
	Already      []string   `json:"already"`
	NeedsUnlock  []string   `json:"needs_unlock"`
	TunnelsAdded []string   `json:"tunnels_added"`
	Skipped      []SyncSkip `json:"skipped"`
}

// ParseSyncLink decodes a tailfuse://sync link.
func ParseSyncLink(link string) (*syncSetup, error) {
	link = strings.TrimSpace(link)
	if len(link) > maxSyncLink {
		return nil, errNotSyncCode
	}
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(u.Scheme, "tailfuse") || !strings.EqualFold(u.Host, "sync") {
		return nil, errNotSyncCode
	}
	q := u.Query()
	if v := q.Get("v"); v != "" && v != "1" {
		if n, err := strconv.Atoi(v); err == nil && n > SyncVersion {
			return nil, errNewerCode
		}
		return nil, errNotSyncCode
	}
	d := strings.TrimRight(q.Get("d"), "=")
	if d == "" {
		return nil, errNotSyncCode
	}
	raw, err := base64.RawURLEncoding.DecodeString(d)
	if err != nil {
		return nil, errNotSyncCode
	}
	var s syncSetup
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errNotSyncCode
	}
	switch {
	case s.V > SyncVersion:
		return nil, errNewerCode
	case s.V != SyncVersion:
		return nil, errNotSyncCode
	}
	return &s, nil
}

// ImportSetup sets this phone up like the computer that made the code:
// new tailnets are added signed out, ports are added where they can be.
// With dryRun it reports and changes nothing. Works while off.
func (e *Engine) ImportSetup(link string, dryRun bool) (*SyncResult, error) {
	setup, err := ParseSyncLink(link)
	if err != nil {
		return nil, err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return nil, err
	}
	cfg := e.cfg.Clone()
	res := &SyncResult{From: setup.From, Added: []string{}, Already: []string{}, NeedsUnlock: []string{}, TunnelsAdded: []string{}, Skipped: []SyncSkip{}}

	// link profile name -> name on this phone, for the profiles that exist after the import
	local := map[string]string{}
	locked := map[string]bool{}
	seenLink := map[string]bool{}
	owners := func() map[string]string {
		m := map[string]string{}
		for n, p := range cfg.Profiles {
			for _, s := range p.Suffixes {
				m[NormalizeSuffix(s)] = n
			}
		}
		return m
	}
	for _, sp := range setup.Profiles {
		label := sp.Name
		if seenLink[sp.Name] && sp.Name != "" {
			res.Skipped = append(res.Skipped, SyncSkip{label, "it is in the code twice"})
			continue
		}
		seenLink[sp.Name] = true
		var suffixes []string
		valid := true
		for _, raw := range sp.Suffixes {
			s := NormalizeSuffix(raw)
			if len(s) < 2 || !strings.HasPrefix(s, ".") || strings.ContainsAny(s, " /:@") {
				valid = false
				break
			}
			dup := false
			for _, x := range suffixes {
				dup = dup || x == s
			}
			if !dup {
				suffixes = append(suffixes, s)
			}
		}
		if !valid || len(suffixes) == 0 {
			res.Skipped = append(res.Skipped, SyncSkip{label, "its addresses are not valid"})
			continue
		}
		own, existing := owners(), ""
		for _, s := range suffixes {
			if n, ok := own[s]; ok {
				existing = n
				break
			}
		}
		if existing != "" {
			res.Already = append(res.Already, label)
			local[sp.Name] = existing
			continue
		}
		if len(cfg.Profiles) >= FreeProfiles && !cfg.Unlocked {
			res.NeedsUnlock = append(res.NeedsUnlock, label)
			locked[sp.Name] = true
			continue
		}
		name := sp.Name
		if !ValidProfileName(name) || cfg.Profiles[name] != nil {
			name = NameForTailnet(func(n string) bool { return cfg.Profiles[n] != nil }, SignedIn{
				Suffix: strings.TrimPrefix(suffixes[0], "."), Account: strings.TrimSpace(sp.Account),
			})
		}
		cfg.Profiles[name] = &Profile{Suffixes: suffixes, SignedOut: true}
		local[sp.Name] = name
		res.Added = append(res.Added, label)
	}

	taken := map[string]bool{}
	listens := map[string]bool{}
	for _, t := range cfg.Tunnels {
		taken[t.Name] = true
		if l, err := normalizeTunnelListen(t.Listen); err == nil {
			listens[l] = true
		}
	}
	own := e.opts.Listen
	e.mu.RLock()
	if e.router != nil {
		own = e.router.Addr()
	}
	e.mu.RUnlock()
	for _, t := range setup.Tunnels {
		skip := func(reason string) { res.Skipped = append(res.Skipped, SyncSkip{t.Name, reason}) }
		t.Name, t.Target = strings.TrimSpace(t.Name), strings.TrimSpace(t.Target)
		prof, ok := local[t.Profile]
		switch {
		case t.Name == "":
			skip("it has no name")
			continue
		case locked[t.Profile]:
			skip("its tailnet needs the unlock")
			continue
		case !ok:
			skip("its tailnet is not on this phone")
			continue
		case taken[t.Name]:
			skip("a local port with that name already exists")
			continue
		}
		listen, err := normalizeTunnelListen(t.Listen)
		if err != nil {
			skip("its port is not valid here")
			continue
		}
		if listen == own || listen == e.opts.Listen {
			skip("that port is Tailfuse's own")
			continue
		}
		if listens[listen] {
			skip("that port is already used by another local port")
			continue
		}
		if _, _, err := splitTarget(t.Target); err != nil {
			skip("its service is not valid")
			continue
		}
		taken[t.Name], listens[listen] = true, true
		cfg.Tunnels = append(cfg.Tunnels, Tunnel{Name: t.Name, Listen: listen, Target: t.Target, Profile: prof})
		res.TunnelsAdded = append(res.TunnelsAdded, t.Name)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if dryRun || (len(res.Added) == 0 && len(res.TunnelsAdded) == 0) {
		return res, nil
	}
	if err := e.commitLocked(cfg); err != nil {
		return nil, err
	}
	if e.Running() {
		e.tunnels.sync(cfg.Tunnels)
	}
	return res, nil
}
