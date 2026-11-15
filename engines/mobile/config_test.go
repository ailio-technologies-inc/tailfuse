package engine

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFile)
	c := NewConfig()
	c.Unlocked = true
	c.Profiles["work"] = &Profile{Suffixes: []string{".work.ts.net"}, Paused: true, Account: "ada@example.com", TailscaleLogs: true}
	c.Profiles["new-tailnet"] = &Profile{Suffixes: []string{}, Pending: true, Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	c.Tunnels = []Tunnel{{Name: "db", Listen: "127.0.0.1:5432", Target: "db.work.ts.net:5432", Profile: "work"}}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Unlocked || !back.Profiles["work"].Paused || !back.Profiles["work"].TailscaleLogs || back.Profiles["work"].Account != "ada@example.com" {
		t.Fatalf("lost fields: %+v", back.Profiles["work"])
	}
	if !back.Profiles["new-tailnet"].Pending || !back.Profiles["new-tailnet"].Created.Equal(c.Profiles["new-tailnet"].Created) {
		t.Fatalf("pending lost: %+v", back.Profiles["new-tailnet"])
	}
	if len(back.Tunnels) != 1 || back.Tunnels[0] != c.Tunnels[0] {
		t.Fatalf("tunnels: %+v", back.Tunnels)
	}
	// Overwriting keeps 0600 and leaves no temp file.
	back.Profiles["work"].Paused = false
	if err := back.Save(path); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	again, _ := LoadConfig(path)
	if again.Profiles["work"].Paused {
		t.Fatal("the switch choice did not persist")
	}
}

func TestConfigMissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "nope.json"))
	if err != nil || len(c.Profiles) != 0 || c.Tunnels == nil {
		t.Fatalf("missing file: %v %+v", err, c)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{"), 0o600)
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("broken file accepted")
	}
	// Unknown and null fields are tolerated; versions are normalised.
	odd := filepath.Join(dir, "odd.json")
	os.WriteFile(odd, []byte(`{"version":0,"profiles":{"x":null,"w":{"suffixes":[".w.ts.net"],"future":1}},"tunnels":null}`), 0o600)
	c, err = LoadConfig(odd)
	if err != nil || c.Version != ConfigVersion || len(c.Profiles) != 1 || c.Tunnels == nil {
		t.Fatalf("odd file: %v %+v", err, c)
	}
}

func TestConfigClone(t *testing.T) {
	c := NewConfig()
	c.Profiles["w"] = &Profile{Suffixes: []string{".w.ts.net"}}
	c.Tunnels = append(c.Tunnels, Tunnel{Name: "a"})
	d := c.Clone()
	d.Profiles["w"].Suffixes[0] = ".x"
	d.Profiles["w"].Paused = true
	d.Tunnels[0].Name = "b"
	if c.Profiles["w"].Suffixes[0] != ".w.ts.net" || c.Profiles["w"].Paused || c.Tunnels[0].Name != "a" {
		t.Fatal("clone shares memory")
	}
}

func TestProfileNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"work": true, "new-tailnet-2": true, "a": true, "0x": true,
		"": false, "Work": false, "a b": false, "../x": false, "a.b": false,
		strings.Repeat("a", 40): true, strings.Repeat("a", 41): false,
	} {
		if ValidProfileName(name) != ok {
			t.Errorf("ValidProfileName(%q) != %v", name, ok)
		}
	}
}

func TestTunnelListenForms(t *testing.T) {
	for in, want := range map[string]string{
		"5432":           "127.0.0.1:5432",
		":5432":          "127.0.0.1:5432",
		"127.0.0.1:5432": "127.0.0.1:5432",
		"localhost:5432": "127.0.0.1:5432",
		"[::1]:5432":     "[::1]:5432",
	} {
		got, err := normalizeTunnelListen(in)
		if err != nil || got != want {
			t.Errorf("normalizeTunnelListen(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:5432", "192.168.1.2:80", "127.0.0.1:0", "127.0.0.1:99999", "nope"} {
		if _, err := normalizeTunnelListen(bad); err == nil {
			t.Errorf("normalizeTunnelListen(%q) accepted", bad)
		}
	}
	if err := checkLoopbackListen("0.0.0.0:43100"); err == nil {
		t.Error("router on 0.0.0.0 accepted")
	}
	if err := checkLoopbackListen(net.JoinHostPort("::1", "43100")); err != nil {
		t.Error(err)
	}
}

func TestNameForTailnet(t *testing.T) {
	none := func(string) bool { return false }
	cases := []struct {
		found SignedIn
		want  string
	}{
		{SignedIn{Account: "someone@example.com", Suffix: "tail1.ts.net"}, "example"},
		{SignedIn{Account: "someone@gmail.com", Suffix: "tail1.ts.net"}, "someone"},
		{SignedIn{Account: "Ada.Lovelace@ICloud.com"}, "adalovelace"},
		{SignedIn{Account: "x@mail.bigcorp.co.uk"}, "mail"},
		{SignedIn{Account: "octo@github", Tailnet: "octo@github"}, "octo"},
		{SignedIn{Tailnet: "example.org"}, "example"},
		{SignedIn{Tailnet: "person@gmail.com"}, "gmail"},
		{SignedIn{Suffix: "tail8e2dc7.ts.net"}, "tail8e2dc7"},
		{SignedIn{}, "tailnet"},
		{SignedIn{Account: "averyveryveryverylongusername@gmail.com"}, "averyveryveryverylonguse"},
	}
	for _, c := range cases {
		if got := NameForTailnet(none, c.found); got != c.want {
			t.Errorf("NameForTailnet(%+v) = %q, want %q", c.found, got, c.want)
		}
	}
	taken := map[string]bool{"example": true, "example-2": true}
	if got := NameForTailnet(func(n string) bool { return taken[n] }, SignedIn{Account: "a@example.com"}); got != "example-3" {
		t.Errorf("unique name = %q", got)
	}
}

func TestHostFromInput(t *testing.T) {
	for in, want := range map[string]string{
		"nas.home-lab.ts.net":                 "nas.home-lab.ts.net",
		"https://Wiki.Work.ts.net:8443/a?b=c": "wiki.work.ts.net",
		"db.work.ts.net:5432":                 "db.work.ts.net",
		"db.work.ts.net/path":                 "db.work.ts.net",
		"http://[fd7a:115c:a1e0::1]:80/":      "fd7a:115c:a1e0::1",
		"  NAS.home-lab.ts.net.  ":            "nas.home-lab.ts.net",
		"":                                    "",
	} {
		if got := hostFromInput(in); got != want {
			t.Errorf("hostFromInput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostnameFromDeviceName(t *testing.T) {
	long := strings.Repeat("a", 62) + " b" + strings.Repeat("c", 10)
	for in, want := range map[string]string{
		"Ada's iPhone":          "adas-iphone",
		"Ada’s iPad":            "adas-ipad",
		"  iPhone (2)  ":        "iphone-2",
		"Zoë's Pixel 8 Pro":     "zos-pixel-8-pro",
		"---":                   "",
		"":                      "",
		"日本":                    "",
		"日本 iPhone":             "iphone",
		"my_phone.local":        "my-phone-local",
		long:                    strings.Repeat("a", 62),
		strings.Repeat("x", 80): strings.Repeat("x", 63),
	} {
		got := HostnameFromDeviceName(in)
		if got != want {
			t.Errorf("HostnameFromDeviceName(%q) = %q, want %q", in, got, want)
		}
		if len(got) > 63 || strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("HostnameFromDeviceName(%q) = %q is not a DNS label", in, got)
		}
	}
}
