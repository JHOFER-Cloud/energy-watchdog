package proxmox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTLSConfig(t *testing.T) {
	t.Run("insecure", func(t *testing.T) {
		c, err := TLSConfig("", true)
		if err != nil || c == nil || !c.InsecureSkipVerify {
			t.Fatalf("got %+v, %v", c, err)
		}
	})

	t.Run("system defaults", func(t *testing.T) {
		c, err := TLSConfig("", false)
		if err != nil || c != nil {
			t.Fatalf("got %+v, %v, want nil,nil", c, err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := TLSConfig("/nope/ca.crt", false); err == nil {
			t.Fatal("expected error for missing CA file")
		}
	})

	t.Run("invalid pem", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.crt")
		if err := os.WriteFile(path, []byte("not a cert"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := TLSConfig(path, false); err == nil {
			t.Fatal("expected error for unparseable CA")
		}
	})
}
