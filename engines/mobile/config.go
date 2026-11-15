package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ConfigVersion is the version written to config.json.
const ConfigVersion = 1

// DefaultListen is where the router listens unless told otherwise.
const DefaultListen = "127.0.0.1:43100"

// FreeProfiles is how many tailnets may be added without the purchase.
const FreeProfiles = 2

// ConfigFile is the config's file name inside the state folder.
const ConfigFile = "config.json"

// Config is config.json: one profile per tailnet, plus local ports.
// It is the mobile counterpart of the desktop config.yaml, and follows the
// same rules (see Validate).
type Config struct {
	Version int `json:"version"`
	// Hostname is this device's name on every tailnet; "" picks one.
	Hostname string `json:"hostname,omitempty"`
	// Unlocked: the unlimited-tailnets in-app purchase is recorded. The app
	// owns the store check; the engine only keeps the flag.
	Unlocked bool                `json:"unlocked,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
	Tunnels  []Tunnel            `json:"tunnels"`
}

// Profile is one tailnet.
type Profile struct {
	// Suffixes this tailnet owns, each with a leading dot (".work.ts.net").
	Suffixes []string `json:"suffixes"`
	// Paused: switched off on purpose; the node is not started.
	Paused bool `json:"paused,omitempty"`
	// Account is the login that signed in ("" until known).
	Account string `json:"account,omitempty"`
	// Tailnet is the tailnet's display name ("" until known).
	Tailnet string `json:"tailnet,omitempty"`
	// AuthKey signs the node in without a browser; cleared once signed in.
	AuthKey string `json:"auth_key,omitempty"`
	// TailscaleLogs: this tailnet requires Tailscale's own logging.
	TailscaleLogs bool `json:"tailscale_logs,omitempty"`
	// Pending: a provisional profile whose sign-in has not finished. It has
	// no suffixes yet and owns nothing.
	Pending bool `json:"pending,omitempty"`
	// Created is when a pending profile was made (for its 10 minute limit).
	Created time.Time `json:"created,omitzero"`
}

// Tunnel is a local port: 127.0.0.1:PORT pinned to host:port on one tailnet.
type Tunnel struct {
	Name    string `json:"name"`
	Listen  string `json:"listen"`
	Target  string `json:"target"`
	Profile string `json:"profile"`
}

// NewConfig returns an empty config.
func NewConfig() *Config {
	return &Config{Version: ConfigVersion, Profiles: map[string]*Profile{}, Tunnels: []Tunnel{}}
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	out := &Config{Version: c.Version, Hostname: c.Hostname, Unlocked: c.Unlocked, Profiles: make(map[string]*Profile, len(c.Profiles))}
	for n, p := range c.Profiles {
		cp := *p
		cp.Suffixes = append([]string(nil), p.Suffixes...)
		out.Profiles[n] = &cp
	}
	out.Tunnels = append([]Tunnel{}, c.Tunnels...)
	return out
}

// Names returns the profile names, sorted.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadConfig reads path; a missing file is an empty config.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	c := NewConfig()
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]*Profile{}
	}
	for n, p := range c.Profiles {
		if p == nil {
			delete(c.Profiles, n)
		}
	}
	if c.Tunnels == nil {
		c.Tunnels = []Tunnel{}
	}
	c.Version = ConfigVersion
	return c, nil
}

// Save writes the config atomically, readable only by this user.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

var profileNameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ValidProfileName reports whether name may name a profile: 1 to 40 of
// a-z, 0-9 and '-'. Names are folder names under nodes/.
func ValidProfileName(name string) bool { return profileNameRE.MatchString(name) }

// NormalizeSuffix trims and lower-cases a suffix, the way the desktop
// core compares them.
func NormalizeSuffix(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Validate checks structure and isolation. Suffix ownership is strict: every
// suffix belongs to exactly one profile; a suffix listed twice (by two
// profiles or the same one) is rejected, so a name on one tailnet can never
// fall back to another. Ports desktop config.rs validate(); an empty profile
// list is fine on mobile (the app starts with none), and a pending profile
// may have no suffixes.
func (c *Config) Validate() error {
	seen := map[string]string{}
	for _, name := range c.Names() {
		p := c.Profiles[name]
		if !ValidProfileName(name) {
			return fmt.Errorf("profile name %q is not valid: use 1 to 40 of a-z, 0-9 and '-'", name)
		}
		if len(p.Suffixes) == 0 && !p.Pending {
			return fmt.Errorf("profile %q needs at least one suffix", name)
		}
		for _, raw := range p.Suffixes {
			s := NormalizeSuffix(raw)
			if s == "" {
				return fmt.Errorf("profile %q has an empty suffix", name)
			}
			if !strings.HasPrefix(s, ".") {
				return fmt.Errorf("profile %q: suffix %q must start with '.'", name, s)
			}
			if s == "." {
				return fmt.Errorf("profile %q: suffix %q names no domain", name, s)
			}
			if owner, ok := seen[s]; ok {
				return fmt.Errorf("suffix %q is owned by both %q and %q; overlaps are rejected to guarantee isolation", s, owner, name)
			}
			seen[s] = name
		}
	}
	names := map[string]bool{}
	listens := map[string]string{}
	for _, t := range c.Tunnels {
		if t.Name == "" || t.Profile == "" || t.Listen == "" || t.Target == "" {
			return fmt.Errorf("local port %q needs name, profile, listen and target", t.Name)
		}
		if names[t.Name] {
			return fmt.Errorf("duplicate local port name %q", t.Name)
		}
		names[t.Name] = true
		if _, ok := c.Profiles[t.Profile]; !ok {
			return fmt.Errorf("local port %q references unknown profile %q", t.Name, t.Profile)
		}
		listen, err := normalizeTunnelListen(t.Listen)
		if err != nil {
			return fmt.Errorf("local port %q: %w", t.Name, err)
		}
		if other, ok := listens[listen]; ok {
			return fmt.Errorf("local ports %q and %q both listen on %s", other, t.Name, listen)
		}
		listens[listen] = t.Name
		if _, _, err := splitTarget(t.Target); err != nil {
			return fmt.Errorf("local port %q: %w", t.Name, err)
		}
	}
	return nil
}

// Owner is the profile owning host by configured suffix, longest match
// winning; "" when none. Pending profiles own nothing.
func (c *Config) Owner(host string) string {
	host = normalizeHost(host)
	best, bestLen := "", 0
	for _, name := range c.Names() {
		for _, s := range c.Profiles[name].Suffixes {
			s = NormalizeSuffix(s)
			if strings.HasSuffix(host, s) && len(s) > bestLen {
				best, bestLen = name, len(s)
			}
		}
	}
	return best
}

// normalizeHost lower-cases a host and drops a trailing dot and IPv6 brackets.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimRight(host, ".")
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.ToLower(host)
}

// isLoopbackHost: "localhost" or a loopback IP literal.
func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// checkLoopbackListen requires host:port with a loopback host: the router and
// local ports carry traffic into tailnets with no password, so only this
// device may reach them.
func checkLoopbackListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen %q is not host:port", addr)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("listen %q is not a loopback address; Tailfuse only listens on this device (127.0.0.1)", addr)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("listen %q has no valid port", addr)
	}
	return nil
}

// normalizeTunnelListen accepts "PORT", ":PORT" or "127.0.0.1:PORT" and
// returns host:port on loopback.
func normalizeTunnelListen(listen string) (string, error) {
	listen = strings.TrimSpace(listen)
	if _, err := strconv.ParseUint(listen, 10, 16); err == nil {
		listen = "127.0.0.1:" + listen
	} else if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	if err := checkLoopbackListen(listen); err != nil {
		return "", err
	}
	host, port, _ := net.SplitHostPort(listen)
	if port == "0" {
		return "", fmt.Errorf("listen %q needs a fixed port", listen)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// splitTarget splits a local port's target into host and port.
func splitTarget(target string) (string, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil || host == "" {
		return "", "", fmt.Errorf("target %q is not host:port", target)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", "", fmt.Errorf("target %q has no valid port", target)
	}
	return host, port, nil
}
