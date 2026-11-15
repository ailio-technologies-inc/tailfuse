package engine

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func syncVector(t *testing.T) (link, raw string) {
	t.Helper()
	b, err := os.ReadFile("testdata/sync.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Link, JSON string }
	if err := json.Unmarshal([]byte(strings.ReplaceAll(strings.ReplaceAll(string(b), `"json"`, `"JSON"`), `"link"`, `"Link"`)), &v); err != nil {
		t.Fatal(err)
	}
	return v.Link, v.JSON
}

func linkFor(v any) string {
	b, _ := json.Marshal(v)
	return "tailfuse://sync?v=1&d=" + base64.RawURLEncoding.EncodeToString(b)
}

type syncOut = SyncResult

func doImport(t *testing.T, e *Engine, link string, dry bool) syncOut {
	t.Helper()
	var r syncOut
	if err := json.Unmarshal(mustOK(t, e, "import_setup", map[string]any{"link": link, "dry_run": dry}), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func offEngine(t *testing.T) (*Engine, string, *fakeNet) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, dir)
	return e, dir, fn
}

func TestSyncVectorDecodes(t *testing.T) {
	link, raw := syncVector(t)
	s, err := ParseSyncLink(link)
	if err != nil {
		t.Fatal(err)
	}
	var want syncSetup
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, &want) || s.From != "Ada's MacBook" || len(s.Profiles) != 2 || len(s.Tunnels) != 1 {
		t.Fatalf("decoded %+v", s)
	}
	e, dir, _ := offEngine(t)
	r := doImport(t, e, link, false)
	want2 := syncOut{From: "Ada's MacBook", Added: []string{"personal", "work"}, Already: []string{}, NeedsUnlock: []string{}, TunnelsAdded: []string{"prod-db"}, Skipped: []SyncSkip{}}
	if !reflect.DeepEqual(r, want2) {
		t.Fatalf("result %+v", r)
	}
	c := readConfig(t, dir)
	if len(c.Profiles) != 2 || !c.Profiles["work"].SignedOut || c.Profiles["work"].Paused || c.Profiles["work"].Pending {
		t.Fatalf("config %+v", c.Profiles["work"])
	}
	if !reflect.DeepEqual(c.Profiles["work"].Suffixes, []string{".work.ts.net", ".internal.example"}) {
		t.Fatalf("suffixes %v", c.Profiles["work"].Suffixes)
	}
	if len(c.Tunnels) != 1 || c.Tunnels[0].Listen != "127.0.0.1:5432" || c.Tunnels[0].Profile != "work" {
		t.Fatalf("tunnels %+v", c.Tunnels)
	}
	st := getState(t, e)
	if p := profile(st, "work"); p == nil || p.Online || p.Paused || p.AuthURL != "" {
		t.Fatalf("state %+v", p)
	}
}

func TestSyncDryRunChangesNothing(t *testing.T) {
	link, _ := syncVector(t)
	e, dir, _ := offEngine(t)
	r := doImport(t, e, link, true)
	if len(r.Added) != 2 || len(r.TunnelsAdded) != 1 {
		t.Fatalf("dry run %+v", r)
	}
	if _, err := os.Stat(dir + "/" + ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote a config: %v", err)
	}
	if len(getState(t, e).Profiles) != 0 {
		t.Fatal("dry run added tailnets")
	}
}

func TestSyncAlreadyAndRepeat(t *testing.T) {
	link, _ := syncVector(t)
	e, _, _ := offEngine(t)
	doImport(t, e, link, false)
	r := doImport(t, e, link, false)
	if len(r.Added) != 0 || !reflect.DeepEqual(r.Already, []string{"personal", "work"}) || len(r.TunnelsAdded) != 0 {
		t.Fatalf("second import %+v", r)
	}
	// the tunnel is skipped: its name is taken
	if len(r.Skipped) != 1 || r.Skipped[0].Name != "prod-db" {
		t.Fatalf("skipped %+v", r.Skipped)
	}
	// a tailnet owned under another name is already here, and tunnels follow it
	e2, dir2, _ := offEngine(t)
	writeConfig(t, dir2, &Config{Version: 1, Profiles: map[string]*Profile{"mine": {Suffixes: []string{".work.ts.net"}}}, Tunnels: []Tunnel{}})
	r = doImport(t, e2, link, false)
	if !reflect.DeepEqual(r.Already, []string{"work"}) || !reflect.DeepEqual(r.Added, []string{"personal"}) {
		t.Fatalf("result %+v", r)
	}
	if c := readConfig(t, dir2); c.Tunnels[0].Profile != "mine" {
		t.Fatalf("tunnel went to %q", c.Tunnels[0].Profile)
	}
}

func TestSyncFreeLimit(t *testing.T) {
	link, _ := syncVector(t)
	e, dir, _ := offEngine(t)
	writeConfig(t, dir, &Config{Version: 1, Profiles: map[string]*Profile{"a": {Suffixes: []string{".a.ts.net"}}}, Tunnels: []Tunnel{}})
	r := doImport(t, e, link, false)
	if !reflect.DeepEqual(r.Added, []string{"personal"}) || !reflect.DeepEqual(r.NeedsUnlock, []string{"work"}) {
		t.Fatalf("result %+v", r)
	}
	// work's tunnel is left out, and says why
	if len(r.TunnelsAdded) != 0 || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Reason, "unlock") {
		t.Fatalf("tunnels %+v", r)
	}
	if len(readConfig(t, dir).Profiles) != 2 {
		t.Fatal("limit not applied")
	}
	mustOK(t, e, "set_unlocked", map[string]any{"on": true})
	r = doImport(t, e, link, false)
	if !reflect.DeepEqual(r.Added, []string{"work"}) || !reflect.DeepEqual(r.TunnelsAdded, []string{"prod-db"}) {
		t.Fatalf("after unlock %+v", r)
	}
}

func TestSyncTunnelRules(t *testing.T) {
	e, dir, _ := offEngine(t)
	writeConfig(t, dir, &Config{Version: 1, Profiles: map[string]*Profile{}, Tunnels: []Tunnel{}})
	prof := []map[string]any{{"name": "work", "suffixes": []string{".work.ts.net"}}}
	tun := func(name, listen, target, p string) map[string]any {
		return map[string]any{"name": name, "listen": listen, "target": target, "profile": p}
	}
	r := doImport(t, e, linkFor(map[string]any{"v": 1, "profiles": prof, "tunnels": []any{
		tun("ok", "5432", "db.work.ts.net:5432", "work"),
		tun("same-name", "5433", "db:1", "work"),
		tun("ok", "5434", "db:1", "work"),
		tun("same-port", "127.0.0.1:5432", "db:1", "work"),
		tun("public", "0.0.0.0:6000", "db:1", "work"),
		tun("no-target", "6001", "db", "work"),
		tun("ghost", "6002", "db:1", "nope"),
		tun("", "6003", "db:1", "work"),
	}}), false)
	if !reflect.DeepEqual(r.TunnelsAdded, []string{"ok", "same-name"}) {
		t.Fatalf("added %v", r.TunnelsAdded)
	}
	if len(r.Skipped) != 6 {
		t.Fatalf("skipped %+v", r.Skipped)
	}
	for _, s := range r.Skipped {
		if s.Reason == "" {
			t.Fatalf("no reason for %q", s.Name)
		}
	}
	if got := readConfig(t, dir).Tunnels[0].Listen; got != "127.0.0.1:5432" {
		t.Fatalf("listen %q", got)
	}
}

func TestSyncBadLinks(t *testing.T) {
	e, dir, _ := offEngine(t)
	for _, l := range []string{
		"", "hello", "https://example.com/?d=abc", "tailfuse://other?v=1&d=e30", "tailfuse://sync?v=1", "tailfuse://sync?v=1&d=***",
		"tailfuse://sync?v=1&d=bm90IGpzb24", linkFor(map[string]any{"profiles": []any{}}), linkFor([]int{1}),
		"tailfuse://sync?v=zzz&d=e30",
	} {
		mustFail(t, e, "import_setup", map[string]any{"link": l}, "that is not a tailfuse sync code")
	}
	mustFail(t, e, "import_setup", map[string]any{"link": "tailfuse://sync?v=2&d=e30"}, "that code is from a newer tailfuse")
	b, _ := json.Marshal(map[string]any{"v": 2, "profiles": []any{}})
	mustFail(t, e, "import_setup", map[string]any{"link": "tailfuse://sync?v=1&d=" + base64.RawURLEncoding.EncodeToString(b)}, "newer tailfuse")
	if _, err := os.Stat(dir + "/" + ConfigFile); !os.IsNotExist(err) {
		t.Fatal("a bad link wrote a config")
	}
	// a bad profile is skipped, the rest still import
	r := doImport(t, e, linkFor(map[string]any{"v": 1, "profiles": []any{
		map[string]any{"name": "bad", "suffixes": []string{"no-dot.com"}},
		map[string]any{"name": "none", "suffixes": []string{}},
		map[string]any{"name": "fine", "suffixes": []string{".fine.ts.net"}},
	}}), false)
	if !reflect.DeepEqual(r.Added, []string{"fine"}) || len(r.Skipped) != 2 {
		t.Fatalf("result %+v", r)
	}
}

func TestSyncUnknownFieldsIgnored(t *testing.T) {
	e, _, _ := offEngine(t)
	r := doImport(t, e, linkFor(map[string]any{
		"v": 1, "future": []int{1}, "from": "",
		"profiles": []any{map[string]any{"name": "x", "suffixes": []string{".x.ts.net"}, "colour": "red", "accept_routes": true, "exit_node": "e"}},
		"tunnels":  []any{},
	}), false)
	if !reflect.DeepEqual(r.Added, []string{"x"}) {
		t.Fatalf("result %+v", r)
	}
}

func TestSyncNameClashes(t *testing.T) {
	e, dir, _ := offEngine(t)
	writeConfig(t, dir, &Config{Version: 1, Unlocked: true, Profiles: map[string]*Profile{"work": {Suffixes: []string{".old.ts.net"}}}, Tunnels: []Tunnel{}})
	r := doImport(t, e, linkFor(map[string]any{"v": 1, "profiles": []any{
		map[string]any{"name": "work", "account": "ada@work.example", "suffixes": []string{".new.ts.net"}},
		map[string]any{"name": "Bad Name", "account": "bo@gmail.com", "suffixes": []string{".b.ts.net"}},
		map[string]any{"name": "work", "suffixes": []string{".third.ts.net"}},
	}, "tunnels": []any{map[string]any{"name": "t", "listen": "7000", "target": "h:1", "profile": "work"}}}), false)
	if !reflect.DeepEqual(r.Added, []string{"work", "Bad Name"}) || len(r.Skipped) != 1 {
		t.Fatalf("result %+v", r)
	}
	c := readConfig(t, dir)
	if c.Profiles["work-2"] == nil || c.Profiles["bo"] == nil || len(c.Profiles) != 3 {
		t.Fatalf("names %v", c.Names())
	}
	if c.Profiles["work"].SignedOut {
		t.Fatal("existing tailnet changed")
	}
	// the code's "work" names the one that was just added, not the old one
	if c.Tunnels[0].Profile != "work-2" {
		t.Fatalf("tunnel profile %q", c.Tunnels[0].Profile)
	}
}

func TestSyncedTailnetsWaitForLogin(t *testing.T) {
	link, _ := syncVector(t)
	e, dir, fn := offEngine(t)
	doImport(t, e, link, false)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	if fn.count("work") != 0 || fn.count("personal") != 0 {
		t.Fatal("signed-out tailnets started a node before login")
	}
	if p := profile(getState(t, e), "work"); p == nil || p.Online || p.Paused {
		t.Fatalf("state %+v", p)
	}
	mustOK(t, e, "login", map[string]any{"name": "work"})
	if fn.count("work") != 1 || fn.node("work").logins != 1 {
		t.Fatalf("login did not start the node: %d", fn.count("work"))
	}
	if readConfig(t, dir).Profiles["work"].SignedOut {
		t.Fatal("still signed out after login")
	}
	eventually(t, "auth url", func() bool { return profile(getState(t, e), "work").AuthURL != "" })
	if fn.count("personal") != 0 {
		t.Fatal("the other tailnet started too")
	}
	// importing while on adds the port without a restart
	doImport(t, e, linkFor(map[string]any{"v": 1, "profiles": []any{map[string]any{"name": "work", "suffixes": []string{".work.ts.net"}}},
		"tunnels": []any{map[string]any{"name": "web", "listen": "127.0.0.1:0", "target": "h:1", "profile": "work"}}}), false)
}

func TestNeedsLoginAndConnecting(t *testing.T) {
	link, _ := syncVector(t)
	e, dir, fn := offEngine(t)
	doImport(t, e, link, false)
	// off: a never-signed-in (imported) tailnet needs a sign-in
	if p := profile(getState(t, e), "work"); !p.NeedsLogin || p.Connecting {
		t.Fatalf("off, signed out: %+v", p)
	}
	// off: one that has signed in before (node state on disk) does not
	nd := filepath.Join(dir, "nodes", "work")
	os.MkdirAll(nd, 0o700)
	cfg := readConfig(t, dir)
	cfg.Profiles["work"].SignedOut = false
	if err := cfg.Save(filepath.Join(dir, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	if p := profile(getState(t, e), "work"); !p.NeedsLogin {
		t.Fatalf("off, no node state: %+v", p)
	}
	os.WriteFile(filepath.Join(nd, nodeStateFile), []byte("{}"), 0o600)
	if p := profile(getState(t, e), "work"); p.NeedsLogin || p.Connecting || p.Online {
		t.Fatalf("off, signed in before: %+v", p)
	}
	// on: it connects (never needing a login), and the other waits for one
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	if p := profile(getState(t, e), "personal"); !p.NeedsLogin || p.Connecting {
		t.Fatalf("on, signed out: %+v", p)
	}
	_ = fn
}
