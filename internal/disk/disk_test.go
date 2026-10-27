package disk

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestHardlinkCrossDevice : data_dir et jailer_dir sur deux filesystems
// différents donnent EXDEV. L'erreur doit nommer le remède, pas seulement
// « invalid cross-device link ».
func TestHardlinkCrossDevice(t *testing.T) {
	// /dev/shm est un tmpfs, donc un autre filesystem que le répertoire temporaire.
	other := "/dev/shm"
	if _, err := os.Stat(other); err != nil {
		t.Skipf("%s indisponible", other)
	}
	src := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sameDevice(t, src, other) {
		t.Skipf("%s est sur le même filesystem que le répertoire temporaire", other)
	}

	dst := filepath.Join(other, "wsd-test-vmlinux")
	t.Cleanup(func() { os.Remove(dst) })

	err := Hardlink(src, dst)
	if err == nil {
		t.Fatal("un hardlink inter-filesystems doit échouer")
	}
	for _, want := range []string{"même filesystem", "data_dir", "jailer_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("erreur sans %q : %v", want, err)
		}
	}
}

func TestHardlinkSameDevice(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "sous", "repertoire", "vmlinux")
	if err := Hardlink(src, dst); err != nil {
		t.Fatalf("hardlink sur le même filesystem refusé : %v", err)
	}
	// Idempotence : un lien déjà en place est remplacé sans erreur.
	if err := Hardlink(src, dst); err != nil {
		t.Fatalf("hardlink non idempotent : %v", err)
	}
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if !os.SameFile(si, di) {
		t.Error("src et dst devraient être le même inode")
	}
}

// sameDevice indique si deux chemins sont sur le même filesystem.
func sameDevice(t *testing.T, a, b string) bool {
	t.Helper()
	dev := func(path string) uint64 {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("Stat_t indisponible sur cette plateforme")
		}
		return uint64(st.Dev)
	}
	return dev(a) == dev(b)
}
