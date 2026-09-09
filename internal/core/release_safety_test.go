package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConfigurationRejectsCaseFoldAliases(t *testing.T) {
	b, e := json.Marshal(Example())
	if e != nil {
		t.Fatal(e)
	}
	for _, pair := range [][2]string{{`"apiVersion":`, `"APIVERSION":`}, {`"vcpu":14`, `"vCPU":14`}, {`"enabled":true`, `"ENABLED":true`}, {`"memoryMiB":24576`, `"memoryMiB":24576,"MemoryMiB":24576`}} {
		text := strings.Replace(string(b), pair[0], pair[1], 1)
		if text == string(b) {
			t.Fatal("fixture not mutated", pair)
		}
		var c Config
		if Decode(strings.NewReader(text), &c) == nil {
			t.Fatal("case alias accepted", pair)
		}
	}
}
func TestExactWireTypesAndMapKeysRoundTrip(t *testing.T) {
	type wire struct {
		At     time.Time         `json:"at"`
		Bytes  []byte            `json:"bytes"`
		Labels map[string]string `json:"labels"`
	}
	want := wire{time.Now().UTC().Truncate(time.Second), []byte("test"), map[string]string{"A": "upper", "a": "lower"}}
	b, _ := json.Marshal(want)
	var got wire
	if e := Decode(strings.NewReader(string(b)), &got); e != nil {
		t.Fatal(e)
	}
	if Fingerprint(want) != Fingerprint(got) {
		t.Fatal("wire type changed")
	}
}
func TestNoopConfigurationPlanPreservesRevision(t *testing.T) {
	s, c := testStore(t)
	_, before, e := s.Config(ctx)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Plan(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	rev, e := s.Apply(ctx, p.ID)
	if e != nil || rev != before {
		t.Fatalf("no-op mutated revision: %d -> %d (%v)", before, rev, e)
	}
	c.Pools[0].WarmIdle = 1
	p, e = s.Plan(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	rev, e = s.Apply(ctx, p.ID)
	if e != nil || rev != before+1 {
		t.Fatal(rev, e)
	}
	var count int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM audit WHERE event='config.apply'").Scan(&count); e != nil || count != 2 {
		t.Fatal("config mutation audit missing or duplicated", count, e)
	}
}
