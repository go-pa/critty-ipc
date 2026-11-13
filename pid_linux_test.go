package critty

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPID(t *testing.T) {
	// The fake server listens in this process, so its PID is ours.
	c, _ := fakeServer(t, "")
	pid, err := c.PID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getpid(); pid != want {
		t.Errorf("PID() = %d, want %d", pid, want)
	}
}

func TestPIDNoServer(t *testing.T) {
	c := &Client{Socket: filepath.Join(t.TempDir(), "missing.sock")}
	if pid, err := c.PID(t.Context()); err == nil {
		t.Errorf("PID() = %d, want error", pid)
	}
}
