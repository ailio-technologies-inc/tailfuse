package engine

import (
	"strings"
	"testing"
)

func TestPACRoutesOwnedOnly(t *testing.T) {
	pac := PAC([]string{".work.ts.net", "Ailio.dev", ".work.ts.net"}, []string{"app.example.com"}, []string{"100.74.194.18"}, "127.0.0.1:43100")
	for _, want := range []string{
		`dnsDomainIs(host, ".work.ts.net") || host == "work.ts.net"`,
		`dnsDomainIs(host, ".ailio.dev") || host == "ailio.dev"`,
		`host == "app.example.com"`,
		`host == "100.74.194.18"`,
		`return "PROXY 127.0.0.1:43100; SOCKS5 127.0.0.1:43100";`,
		`return "DIRECT";`,
		"function FindProxyForURL(url, host)",
	} {
		if !strings.Contains(pac, want) {
			t.Errorf("PAC lacks %q:\n%s", want, pac)
		}
	}
	if strings.Count(pac, "work.ts.net\")") != 1 {
		t.Errorf("duplicate suffix not removed:\n%s", pac)
	}
}

func TestPACEmpty(t *testing.T) {
	pac := PAC(nil, nil, nil, "127.0.0.1:43100")
	if strings.Contains(pac, "PROXY") || !strings.Contains(pac, `return "DIRECT";`) {
		t.Fatalf("empty PAC routes something:\n%s", pac)
	}
}
