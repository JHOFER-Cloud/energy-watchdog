package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDryRunModeUnmarshal(t *testing.T) {
	ok := map[string]DryRunMode{
		"false":     DryRunFull,
		"true":      DryRunLog,
		"alert":     DryRunAlert,
		`"alert"`:   DryRunAlert,
		"  Alert  ": DryRunAlert,
	}
	for in, want := range ok {
		var got struct {
			D DryRunMode `yaml:"d"`
		}
		if err := yaml.Unmarshal([]byte("d: "+in), &got); err != nil {
			t.Fatalf("%q: unexpected error: %v", in, err)
		}
		if got.D != want {
			t.Errorf("%q -> %v, want %v", in, got.D, want)
		}
	}

	var bad struct {
		D DryRunMode `yaml:"d"`
	}
	if err := yaml.Unmarshal([]byte("d: maybe"), &bad); err == nil {
		t.Error("expected an error for an invalid dryRun value")
	}
}
