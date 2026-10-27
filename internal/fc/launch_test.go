package fc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckSourcesImageAbsente : c'est le cas rencontré quand l'image invitée
// n'a pas été déposée dans data_dir/images/<image_id>. L'erreur doit nommer le
// fichier manquant, là où os.Link ne renvoie qu'un « no such file or
// directory » portant les deux chemins.
func TestCheckSourcesImageAbsente(t *testing.T) {
	dir := t.TempDir()
	p := LaunchParams{
		KernelSrc:    filepath.Join(dir, "images", "debian-bookworm-v1", "vmlinux"),
		RootfsSrc:    filepath.Join(dir, "images", "debian-bookworm-v1", "rootfs.ext4"),
		WorkspaceSrc: filepath.Join(dir, "workspace.ext4"),
	}
	err := checkSources(p)
	if err == nil {
		t.Fatal("une image absente doit être signalée")
	}
	if !strings.Contains(err.Error(), "kernel invité") || !strings.Contains(err.Error(), p.KernelSrc) {
		t.Errorf("erreur peu explicite : %v", err)
	}
}

func TestCheckSourcesCheminVide(t *testing.T) {
	if err := checkSources(LaunchParams{}); err == nil {
		t.Fatal("un chemin vide doit être signalé")
	}
}

// TestCheckSourcesCompletes vérifie qu'un jeu de sources présent passe, et que
// les sources de snapshot ne sont contrôlées que si elles sont demandées.
func TestCheckSourcesCompletes(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	p := LaunchParams{
		KernelSrc:    write("vmlinux"),
		RootfsSrc:    write("rootfs.ext4"),
		WorkspaceSrc: write("workspace.ext4"),
	}
	if err := checkSources(p); err != nil {
		t.Fatalf("sources complètes refusées : %v", err)
	}

	p.MemSrc = filepath.Join(dir, "mem")
	err := checkSources(p)
	if err == nil || !strings.Contains(err.Error(), "mem") {
		t.Errorf("un mem demandé mais absent doit être signalé, obtenu : %v", err)
	}
}
