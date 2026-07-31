package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIDSetContains(t *testing.T) {
	var s IDSet
	if err := yaml.Unmarshal([]byte(`[101, "300-309", 601, "700"]`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tests := []struct {
		id   int
		want bool
	}{
		{101, true},
		{102, false},
		{299, false},
		{300, true},
		{305, true},
		{309, true},
		{310, false},
		{601, true},
		{700, true}, // bare number as a string
		{701, false},
	}
	for _, tt := range tests {
		if got := s.Contains(tt.id); got != tt.want {
			t.Errorf("Contains(%d) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestIDSetInvalidRange(t *testing.T) {
	var s IDSet
	if err := yaml.Unmarshal([]byte(`["699-600"]`), &s); err == nil {
		t.Fatal("expected error for reversed range, got nil")
	}
}

func TestIDSetOverlap(t *testing.T) {
	parse := func(in string) IDSet {
		var s IDSet
		if err := yaml.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("unmarshal %q: %v", in, err)
		}
		return s
	}
	a := parse(`["100-199", "300-399"]`)
	b := parse(`["350-450"]`)
	c := parse(`["600-699"]`)

	if lo, hi, ok := a.Overlap(b); !ok || lo != 350 || hi != 399 {
		t.Errorf("a.Overlap(b) = %d-%d,%v, want 350-399,true", lo, hi, ok)
	}
	if _, _, ok := a.Overlap(c); ok {
		t.Errorf("a.Overlap(c) = true, want false")
	}
}

// TestReplicationManagedDefault: omitting manageReplication means on, and the zero Proxmox
// reads the same way, so nothing has to remember to call defaults() for this.
func TestReplicationManagedDefault(t *testing.T) {
	if !(Proxmox{}).ReplicationManaged() {
		t.Error("zero Proxmox should manage replication")
	}
	for _, tc := range []struct {
		yaml string
		want bool
	}{
		{"node: pve-1", true},
		{"node: pve-1\nmanageReplication: true", true},
		{"node: pve-1\nmanageReplication: false", false},
	} {
		var p Proxmox
		if err := yaml.Unmarshal([]byte(tc.yaml), &p); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.yaml, err)
		}
		if got := p.ReplicationManaged(); got != tc.want {
			t.Errorf("%q: ReplicationManaged() = %v, want %v", tc.yaml, got, tc.want)
		}
	}
}

func TestValidateRejectsOverlap(t *testing.T) {
	c := &Config{}
	if err := yaml.Unmarshal([]byte(`
prometheus:
  url: http://prom
  productionMetric: p
  consumptionMetric: c
proxmox:
  endpoint: https://pve-2
  node: pve-1
  mac: "aa:bb:cc:dd:ee:ff"
  tokenID: u@pam!t
  tokenSecret: x
  targetNodes: [pve-2]
guests:
  migrate: ["100-199"]
  stop: ["150-250"]
`), c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.defaults()
	if err := c.validate(); err == nil {
		t.Fatal("expected overlap error, got nil")
	}
}
