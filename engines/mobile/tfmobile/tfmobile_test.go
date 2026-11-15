package tfmobile

import (
	"encoding/json"
	"testing"
)

func TestCallBeforeStart(t *testing.T) {
	if Running() {
		t.Fatal("running before Start")
	}
	var r struct {
		OK     bool `json:"ok"`
		Result struct {
			Up bool `json:"up"`
		} `json:"result"`
		Error string `json:"error"`
	}
	SetStateDir(t.TempDir())
	if err := json.Unmarshal([]byte(Call(`{"cmd":"state"}`)), &r); err != nil || !r.OK || r.Result.Up {
		t.Fatalf("state off: %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(Call(`{"cmd":"add_profile"}`)), &r); err != nil || r.OK || r.Error != "tailfuse is off" {
		t.Fatalf("add_profile off: %+v", r)
	}
	SetListener(nil)
	Stop()
}
