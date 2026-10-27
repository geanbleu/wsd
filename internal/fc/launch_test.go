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

// TestSetChrootPermissions : jailer ne chown que ses répertoires, pas les
// fichiers que wsd place dans le chroot. Sans ces droits, Firecracker échoue
// sur « Permission denied (os error 13) /rootfs.ext4 » à la configuration du
// drive, qui est en lecture-écriture.
//
// Le test utilise l'uid courant : chown vers son propre uid est permis sans
// privilège, ce qui rend le test exécutable par `make test`.
func TestSetChrootPermissions(t *testing.T) {
	chroot := t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()

	for _, name := range []string{"rootfs.ext4", "workspace.ext4", "vmlinux", "mem", "vmstate"} {
		if err := os.WriteFile(filepath.Join(chroot, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := setChrootPermissions(chroot, uid, gid, true); err != nil {
		t.Fatalf("setChrootPermissions: %v", err)
	}

	// Les drives en lecture-écriture doivent appartenir à l'uid de la VM.
	for _, name := range []string{"rootfs.ext4", "workspace.ext4"} {
		fi, err := os.Stat(filepath.Join(chroot, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s : mode %04o, attendu 0600", name, got)
		}
	}

	// Les fichiers partagés doivent rester lisibles par n'importe quel uid.
	for _, name := range []string{"vmlinux", "mem", "vmstate"} {
		fi, err := os.Stat(filepath.Join(chroot, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("%s : mode %04o, attendu 0644 (lisible par l'uid jailé)", name, got)
		}
	}
}

// TestSetChrootPermissionsBootAFroid : au boot à froid il n'y a ni mem ni
// vmstate, leur absence ne doit pas faire échouer l'étape.
func TestSetChrootPermissionsBootAFroid(t *testing.T) {
	chroot := t.TempDir()
	for _, name := range []string{"rootfs.ext4", "workspace.ext4", "vmlinux"} {
		if err := os.WriteFile(filepath.Join(chroot, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := setChrootPermissions(chroot, os.Getuid(), os.Getgid(), false); err != nil {
		t.Fatalf("setChrootPermissions sans snapshot: %v", err)
	}
}
