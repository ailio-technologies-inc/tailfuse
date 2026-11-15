package engine

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{"ailio.dev": ".ailio.dev", " Ailio.DEV. ": ".ailio.dev", "*.ailio.dev": ".ailio.dev", "https://App.Ailio.dev/x": ".app.ailio.dev", ".example.co.uk": ".example.co.uk"} {
		got, err := NormalizeDomain(in)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "dev", "100.74.194.18", "fd7a:115c:a1e0::1", "co.uk", "a b.com", "ts.net", "x.tail1.ts.net", "-a.com", "a..com", "a_b.com"} {
		if got, err := NormalizeDomain(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestSuggestDomain(t *testing.T) {
	for in, want := range map[string]string{"ailio.dev": "ailio.dev", "app.ailio.dev": "ailio.dev", "a.b.ailio.dev": "ailio.dev", "x.example.co.uk": "example.co.uk", "dev": "", "10.0.0.1": "", "box.tail1.ts.net": ""} {
		if got := SuggestDomain(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func domainEngine(t *testing.T) (*Engine, string, *fakeNet) {
	e, dir, fn := offEngine(t)
	writeConfig(t, dir, &Config{Version: 1, Profiles: map[string]*Profile{
		"home": {Suffixes: []string{".home.ts.net"}}, "work": {Suffixes: []string{".work.ts.net"}},
	}, Tunnels: []Tunnel{}})
	return e, dir, fn
}

func TestAddRemoveDomainWorksOff(t *testing.T) {
	e, dir, _ := domainEngine(t)
	mustOK(t, e, "add_domain", map[string]any{"name": "home", "domain": "Ailio.dev"})
	mustOK(t, e, "add_domain", map[string]any{"name": "home", "domain": "ailio.dev"}) // again: nothing changes
	c := readConfig(t, dir)
	if !reflect.DeepEqual(c.Profiles["home"].Suffixes, []string{".home.ts.net", ".ailio.dev"}) {
		t.Fatalf("suffixes %v", c.Profiles["home"].Suffixes)
	}
	st := getState(t, e)
	if !reflect.DeepEqual(st.MatchDomains, []string{"ailio.dev", "home.ts.net", "work.ts.net"}) {
		t.Fatalf("match domains %v", st.MatchDomains)
	}
	for host, want := range map[string]string{"ailio.dev": "home", "www.ailio.dev": "home", "notailio.dev": ""} {
		var r struct{ Owner *string }
		json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": host}), &r)
		got := ""
		if r.Owner != nil {
			got = *r.Owner
		}
		if got != want {
			t.Errorf("%s -> %q want %q", host, got, want)
		}
	}
	mustFail(t, e, "add_domain", map[string]any{"name": "work", "domain": "ailio.dev"}, "already belongs to the tailnet \"home\"")
	mustFail(t, e, "add_domain", map[string]any{"name": "nope", "domain": "x.dev"}, "there is no tailnet")
	mustFail(t, e, "add_domain", map[string]any{"name": "home", "domain": "com"}, "top-level")
	mustFail(t, e, "add_domain", map[string]any{"name": "home", "domain": "100.74.194.18"}, "address")
	mustFail(t, e, "add_domain", map[string]any{"name": "home", "domain": "x.work.ts.net"}, "ts.net")
	mustFail(t, e, "remove_domain", map[string]any{"name": "home", "domain": "home.ts.net"}, "cannot be removed")
	mustFail(t, e, "remove_domain", map[string]any{"name": "work", "domain": "ailio.dev"}, "does not own")
	mustOK(t, e, "remove_domain", map[string]any{"name": "home", "domain": "ailio.dev"})
	if got := readConfig(t, dir).Profiles["home"].Suffixes; !reflect.DeepEqual(got, []string{".home.ts.net"}) {
		t.Fatalf("after remove %v", got)
	}
}

func TestSuggestDomainFromTestHost(t *testing.T) {
	e, dir, fn := domainEngine(t)
	res := &fakeResolver{}
	e.opts.Resolver = res
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	fn.node("home").signIn(fakeState{Suffix: "home.ts.net", Account: "a@b.example"})
	fn.node("home").learnIPs = []netip.Addr{netip.MustParseAddr("100.74.194.18")}
	res.set("ailio.dev", "100.74.194.18")
	eventually(t, "learned", func() bool {
		var r struct{ Owner *string }
		json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": "ailio.dev"}), &r)
		return r.Owner != nil
	})
	var r map[string]any
	json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": "ailio.dev"}), &r)
	if r["via"] != "address" || r["suggest_domain"] != "ailio.dev" {
		t.Fatalf("result %v", r)
	}
	mustOK(t, e, "add_domain", map[string]any{"name": r["owner"], "domain": "ailio.dev"})
	r = nil
	json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": "ailio.dev"}), &r)
	if _, ok := r["via"]; ok || r["owner"] != "home" {
		t.Fatalf("after adding: %v", r)
	}
}
