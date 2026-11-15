package engine

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// personalMail lists mail domains that say nothing about whose tailnet this
// is (same list as the desktop app's name_for_tailnet).
var personalMail = []string{
	"gmail", "googlemail", "outlook", "hotmail", "live", "msn", "icloud", "me",
	"mac", "yahoo", "proton", "protonmail", "pm", "fastmail", "hey", "aol",
	"gmx", "github",
}

// SignedIn is what a finished sign-in tells about the tailnet.
type SignedIn struct {
	Suffix  string // MagicDNS suffix without the leading dot
	Tailnet string // display name, may be empty
	Account string // login name, may be empty
}

// NameForTailnet picks a short profile name from the account that signed in:
// someone@example.com gives "example", someone@gmail.com gives "someone".
// Falls back to the tailnet's own name, then its MagicDNS label, then
// "tailnet". Made unique against taken. Port of the desktop app's rule.
func NameForTailnet(taken func(string) bool, found SignedIn) string {
	clean := func(text string) string {
		var b strings.Builder
		n := 0
		for _, r := range strings.ToLower(text) {
			if n == 24 {
				break
			}
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
				n++
			}
		}
		return b.String()
	}
	firstLabel := func(text string) string {
		label, _, _ := strings.Cut(text, ".")
		return clean(label)
	}
	base := ""
	if user, domain, ok := strings.Cut(found.Account, "@"); ok {
		org := firstLabel(domain)
		if org == "" || slices.Contains(personalMail, org) {
			base = clean(user)
		} else {
			base = org
		}
	}
	if base == "" {
		t := found.Tailnet
		if i := strings.LastIndex(t, "@"); i >= 0 {
			t = t[i+1:]
		}
		base = firstLabel(t)
	}
	if base == "" {
		base = firstLabel(found.Suffix)
	}
	if base == "" {
		base = "tailnet"
	}
	name := base
	for n := 2; taken(name); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

// HostnameFromDeviceName turns a phone's own name into a DNS label for its
// hostname on each tailnet: lower case a-z, 0-9 and '-', at most 63
// characters, no leading or trailing '-'. Apostrophes are dropped ("Ada's
// iPhone" gives "adas-iphone"), as are letters outside a-z; every other run of characters outside the
// label alphabet becomes one '-'. Returns "" when nothing usable is left.
func HostnameFromDeviceName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case r == '\'' || r == '’' || r == '`' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			// dropped, keeping the word whole: "Ada's" is "adas", "Zoë" is "zo"
		default:
			dash = true
		}
		if b.Len() >= 63 {
			break
		}
	}
	return strings.TrimRight(b.String()[:min(b.Len(), 63)], "-")
}
