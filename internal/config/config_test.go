package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIDSetContains(t *testing.T) {
	var s IDSet
	if err := yaml.Unmarshal([]byte(`[101, "300-309", 601]`), &s); err != nil {
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
