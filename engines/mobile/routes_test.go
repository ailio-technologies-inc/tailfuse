package engine

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type routingVectors struct {
	OwnerScenarios []struct {
		Name     string              `json:"name"`
		Profiles map[string][]string `json:"profiles"`
		Steps    []struct {
			Learn []struct {
				Profile string   `json:"profile"`
				IPs     []string `json:"ips"`
				Domains []string `json:"domains"`
			} `json:"learn"`
			DNS     map[string][]string `json:"dns"`
			Recheck bool                `json:"recheck"`
			Cases   []struct {
				Host    string  `json:"host"`
				Owner   *string `json:"owner"`
				Resolve bool    `json:"resolve"`
			} `json:"cases"`
			Extras *struct {
				Domains []string `json:"domains"`
				Hosts   []string `json:"hosts"`
				IPs     []string `json:"ips"`
			} `json:"extras"`
		} `json:"steps"`
	} `json:"owner_scenarios"`
	TailscaleIP []struct {
		IP   string `json:"ip"`
		Want bool   `json:"want"`
	} `json:"tailscale_ip"`
	ServedDomains []struct {
		Name   string   `json:"name"`
		Routes []string `json:"routes"`
		Search []string `json:"search"`
		Want   []string `json:"want"`
	} `json:"served_domains"`
	Validate []struct {
		Name     string              `json:"name"`
		Profiles map[string]*Profile `json:"profiles"`
		Tunnels  []Tunnel            `json:"tunnels"`
		Error    string              `json:"error"`
	} `json:"validate"`
}

func loadVectors(t *testing.T) routingVectors {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "routing.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v routingVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestRoutingVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.OwnerScenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, sc := range v.OwnerScenarios {
		t.Run(sc.Name, func(t *testing.T) {
			cfg := NewConfig()
			for name, suffixes := range sc.Profiles {
				cfg.Profiles[name] = &Profile{Suffixes: suffixes}
			}
			res := &fakeResolver{}
			rt := NewRouteTable("", res)
			rt.SetConfig(cfg)
			static := true
			for i, step := range sc.Steps {
				for _, l := range step.Learn {
					var ips []netip.Addr
					for _, s := range l.IPs {
						ips = append(ips, netip.MustParseAddr(s))
					}
					rt.SetLearned(l.Profile, ips, l.Domains)
					static = false
				}
				for h, ips := range step.DNS {
					res.set(h, ips...)
					static = false
				}
				if step.Recheck {
					rt.RecheckHosts(context.Background())
				}
				for _, c := range step.Cases {
					want := ""
					if c.Owner != nil {
						want = *c.Owner
					}
					var got string
					if c.Resolve {
						got = rt.ResolveOwner(context.Background(), c.Host)
					} else {
						got = rt.Owner(c.Host)
					}
					if got != want {
						t.Errorf("step %d: owner(%q, resolve=%v) = %q, want %q", i, c.Host, c.Resolve, got, want)
					}
					if static && !c.Resolve {
						if got := cfg.Owner(c.Host); got != want {
							t.Errorf("step %d: Config.Owner(%q) = %q, want %q", i, c.Host, got, want)
						}
					}
				}
				if step.Extras != nil {
					d, h, ips := rt.Extras()
					if !reflect.DeepEqual(nz(d), nz(step.Extras.Domains)) || !reflect.DeepEqual(nz(h), nz(step.Extras.Hosts)) || !reflect.DeepEqual(nz(ips), nz(step.Extras.IPs)) {
						t.Errorf("step %d: extras = %v %v %v, want %v %v %v", i, d, h, ips, step.Extras.Domains, step.Extras.Hosts, step.Extras.IPs)
					}
				}
			}
		})
	}
}

func TestTailscaleIPVectors(t *testing.T) {
	for _, c := range loadVectors(t).TailscaleIP {
		if got := IsTailscaleIP(netip.MustParseAddr(c.IP)); got != c.Want {
			t.Errorf("IsTailscaleIP(%s) = %v", c.IP, got)
		}
	}
}

func TestServedDomainsVectors(t *testing.T) {
	for _, c := range loadVectors(t).ServedDomains {
		if got := ServedDomains(c.Routes, c.Search); !reflect.DeepEqual(got, c.Want) {
			t.Errorf("%s: got %v want %v", c.Name, got, c.Want)
		}
	}
}

func TestValidateVectors(t *testing.T) {
	for _, c := range loadVectors(t).Validate {
		t.Run(c.Name, func(t *testing.T) {
			cfg := NewConfig()
			cfg.Profiles = c.Profiles
			cfg.Tunnels = c.Tunnels
			err := cfg.Validate()
			switch {
			case c.Error == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.Error != "" && err == nil:
				t.Fatalf("want error containing %q", c.Error)
			case c.Error != "" && !strings.Contains(err.Error(), c.Error):
				t.Fatalf("error %q does not contain %q", err, c.Error)
			}
		})
	}
}

func TestRemoveProfileForgetsItsRoutes(t *testing.T) {
	cfg := NewConfig()
	cfg.Profiles["a"] = &Profile{Suffixes: []string{".a.ts.net"}}
	cfg.Profiles["b"] = &Profile{Suffixes: []string{".b.ts.net"}}
	res := &fakeResolver{}
	res.set("app.example.com", "100.64.0.9")
	dir := t.TempDir()
	hosts := filepath.Join(dir, "learned-hosts.json")
	rt := NewRouteTable(hosts, res)
	rt.SetConfig(cfg)
	rt.SetLearned("a", []netip.Addr{netip.MustParseAddr("100.64.0.9")}, []string{"corp.example"})
	if got := rt.ResolveOwner(context.Background(), "app.example.com"); got != "a" {
		t.Fatalf("got %q", got)
	}
	// Remembered hosts survive a restart.
	rt2 := NewRouteTable(hosts, res)
	rt2.SetConfig(cfg)
	if got := rt2.Owner("app.example.com"); got != "a" {
		t.Fatalf("after reload: %q", got)
	}
	delete(cfg.Profiles, "a")
	rt2.SetConfig(cfg)
	if got := rt2.Owner("app.example.com"); got != "" {
		t.Fatalf("host of a removed profile still routes to %q", got)
	}
	if got := rt.MatchDomains(); !reflect.DeepEqual(got, []string{"a.ts.net", "app.example.com", "b.ts.net", "corp.example"}) {
		t.Fatalf("match domains %v", got)
	}
	// Learned data from a profile the table does not know is ignored.
	rt2.SetLearned("ghost", nil, []string{"ghost.example"})
	if got := rt2.Owner("x.ghost.example"); got != "" {
		t.Fatalf("ghost routes: %q", got)
	}
}
