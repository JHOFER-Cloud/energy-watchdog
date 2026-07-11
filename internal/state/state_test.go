package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewFileStore(path)
	ctx := context.Background()

	// Missing file -> fresh running state.
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeRunning {
		t.Errorf("fresh mode = %q, want running", got.Mode)
	}

	want := State{
		Mode:     ModeShed,
		Stopped:  []GuestRef{{VMID: 301, Type: "qemu"}, {VMID: 311, Type: "lxc"}},
		Silences: []SilenceRef{{URL: "http://am", ID: "sil-1"}},
	}
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != want.Mode || len(got.Silences) != 1 || got.Silences[0].ID != "sil-1" || len(got.Stopped) != 2 {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
	if got.Stopped[0].VMID != 301 || got.Stopped[1].Type != "lxc" {
		t.Errorf("stopped = %+v", got.Stopped)
	}
}
