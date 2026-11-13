package dispatch

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestHostContractPin is the consumer side of idp.telos.host (idp-telos-go ADR-0001 T3): the copy in
// tests/contracts/idp.telos.host/<version> is byte-identical to the registered one (checked by
// tools/platform.py matrix), every field the workspace reads of an AgentRun is required there, and
// the published examples decode into what the workspace reads.
func TestHostContractPin(t *testing.T) {
	dir := "../../tests/contracts/idp.telos.host/1.8.0/"
	var schema struct {
		Defs map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	raw, err := os.ReadFile(dir + "schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	run := schema.Defs["AgentRun"]
	required := map[string]bool{}
	for _, r := range run.Required {
		required[r] = true
	}
	rt := reflect.TypeOf(hostRun{})
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if !required[name] {
			t.Errorf("the workspace reads AgentRun.%s, which idp.telos.host does not require", name)
		}
	}
	// the run statuses the workspace's follower treats as ends are the contract's terminal ones
	var status struct {
		Enum []string `json:"enum"`
	}
	_ = json.Unmarshal(run.Properties["status"], &status)
	for _, s := range []string{"QUEUED", "COMPLETED", "FAILED", "CANCELED"} {
		if !strings.Contains(strings.Join(status.Enum, ","), s) {
			t.Errorf("status %s is not in the contract: %v", s, status.Enum)
		}
	}
	var examples struct {
		Examples []struct {
			Definition string          `json:"definition"`
			Value      json.RawMessage `json:"value"`
		} `json:"examples"`
	}
	raw, err = os.ReadFile(dir + "examples.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, x := range examples.Examples {
		if x.Definition != "AgentRun" {
			continue
		}
		var r hostRun
		if err := json.Unmarshal(x.Value, &r); err != nil || r.RunID == "" || r.Status == "" || r.ThreadID == "" {
			t.Errorf("a published AgentRun does not give the workspace what it reads: %+v %v", r, err)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no AgentRun example in the pinned contract")
	}
}
