package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestSetOptionsSavedWhileOff(t *testing.T) {
	e, dir, _ := domainEngine(t)
	want := Prefs{ExitNode: "gateway", AcceptRoutes: true, AdvertiseRoutes: []string{"10.0.0.0/24"}, AdvertiseExitNode: true, ShieldsUp: true}
	mustOK(t, e, "set_options", map[string]any{
		"name": "home", "exit_node": "gateway", "accept_routes": true,
		"advertise_routes": []string{"10.0.0.0/24"}, "advertise_exit_node": true, "shields_up": true,
	})
	if got := readConfig(t, dir).Profiles["home"].Prefs; !reflect.DeepEqual(got, want) {
		t.Fatalf("saved %+v, want %+v", got, want)
	}
	var st State
	json.Unmarshal(mustOK(t, e, "state", nil), &st)
	for _, p := range st.Profiles {
		if p.Name == "home" && !reflect.DeepEqual(p.Prefs, want) {
			t.Fatalf("state shows %+v", p.Prefs)
		}
		if p.Name == "work" && !p.Prefs.IsZero() {
			t.Fatalf("work changed: %+v", p.Prefs)
		}
	}
	// Sending nothing clears them.
	mustOK(t, e, "set_options", map[string]any{"name": "home"})
	if got := readConfig(t, dir).Profiles["home"].Prefs; !got.IsZero() {
		t.Fatalf("not cleared: %+v", got)
	}
}

func TestSetOptionsRejectsBadInput(t *testing.T) {
	e, _, _ := domainEngine(t)
	mustFail(t, e, "set_options", map[string]any{"name": "home", "advertise_routes": []string{"not-a-subnet"}}, "is not a subnet")
	mustFail(t, e, "set_options", map[string]any{"name": "nowhere"}, "there is no tailnet called")
}

func TestSetOptionsAppliedToRunningNodeAndOnStart(t *testing.T) {
	e, dir, fn := domainEngine(t)
	mustOK(t, e, "set_options", map[string]any{"name": "home", "shields_up": true})
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	// Saved settings reach a node when it starts.
	eventually(t, "applied at start", func() bool {
		p, n := fn.node("home").lastPrefs()
		return n > 0 && p.ShieldsUp
	})
	if _, n := fn.node("work").lastPrefs(); n != 0 {
		t.Fatalf("work got settings it never asked for")
	}
	// And a change reaches the running node.
	mustOK(t, e, "set_options", map[string]any{"name": "home", "exit_node": "gateway"})
	eventually(t, "applied live", func() bool {
		p, _ := fn.node("home").lastPrefs()
		return p.ExitNode == "gateway" && !p.ShieldsUp
	})
}

func TestApplyPrefsRetriesUntilTheExitNodeIsKnown(t *testing.T) {
	e, dir, fn := domainEngine(t)
	mustOK(t, e, "set_options", map[string]any{"name": "home", "exit_node": "gateway"})
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	n := fn.node("home")
	n.mu.Lock()
	n.prefsErr = errors.New("no device called \"gateway\" on this tailnet yet")
	n.mu.Unlock()
	n.mu.Lock()
	n.prefsErr = nil // the device list arrives
	n.mu.Unlock()
	eventually(t, "applied after retry", func() bool {
		_, c := n.lastPrefs()
		return c > 0
	})
}
