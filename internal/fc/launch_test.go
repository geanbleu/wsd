package fc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestConsoleTail : la console invitée est la seule trace d'un invité qui ne
// démarre pas. Elle enrichit les erreurs, donc elle ne doit jamais en produire.
func TestConsoleTail(t *testing.T) {
	chroot := t.TempDir()

	if got := ConsoleTail(chroot); got != "" {
		t.Errorf("sans journal, ConsoleTail doit être vide, obtenu %q", got)
	}

	if err := os.MkdirAll(filepath.Join(chroot, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(chroot, "run", "console.log")
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ConsoleTail(chroot); got != "" {
		t.Errorf("journal vide : ConsoleTail doit être vide, obtenu %q", got)
	}

	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, "ligne "+strconv.Itoa(i))
	}
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ConsoleTail(chroot)
	if !strings.Contains(got, "ligne 40") {
		t.Error("la dernière ligne doit apparaître")
	}
	if strings.Contains(got, "ligne 25") {
		t.Error("ConsoleTail doit se limiter aux dernières lignes")
	}
	if !strings.Contains(got, log) {
		t.Error("le chemin du journal doit être indiqué pour pouvoir le relire en entier")
	}
}

// fakeJailer écrit un faux jailer qui crée le socket API attendu puis reste
// vivant, afin d'exercer Launch sans KVM ni privilèges.
func fakeJailer(t *testing.T, apiSock string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-jailer")
	script := "#!/bin/sh\nmkdir -p \"$(dirname \"" + apiSock + "\")\"\n" +
		"touch \"" + apiSock + "\"\nexec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLaunchSurvitAuContexte : la VM ne doit pas mourir avec le contexte passé
// à Launch, qui est celui de la requête HTTP. Avec exec.CommandContext, Go tuait
// jailer dès que le handler répondait : un workspace créé était annoncé
// « running » puis mourait dans la seconde.
func TestLaunchSurvitAuContexte(t *testing.T) {
	base := t.TempDir()
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := func(name string) string {
		path := filepath.Join(srcDir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	wsID := "aaaaaaaa-0000-0000-0000-000000000000"
	jailerDir := filepath.Join(base, "jailer")
	apiSock := filepath.Join(jailerDir, "firecracker", wsID, "root", "run", "firecracker.sock")

	ctx, cancel := context.WithCancel(context.Background())
	res, err := Launch(ctx, LaunchParams{
		WorkspaceID:    wsID,
		JailerBin:      fakeJailer(t, apiSock),
		FirecrackerBin: "/bin/true",
		JailerDir:      jailerDir,
		NetnsPath:      "/var/run/netns/ws-aaaaaaaa",
		UID:            os.Getuid(),
		GID:            os.Getgid(),
		MemMiB:         2048,
		KernelSrc:      src("vmlinux"),
		RootfsSrc:      src("rootfs.ext4"),
		WorkspaceSrc:   src("workspace.ext4"),
		HardlinkRootfs: true,
	})
	if err != nil {
		cancel()
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(res.PID); err == nil {
			p.Kill()
			p.Wait()
		}
	})

	// kill(pid, 0) réussit aussi sur un zombie — un process tué mais pas encore
	// récolté par wsd garderait donc son PID. L'état est lu dans /proc.
	if st := procState(t, res.PID); st == "Z" || st == "" {
		t.Fatalf("le process devrait être vivant juste après Launch, état %q", st)
	}

	// Ce que fait le handler HTTP quand il a répondu.
	cancel()
	time.Sleep(300 * time.Millisecond)

	if st := procState(t, res.PID); st == "Z" || st == "" {
		t.Errorf("le process a été tué par l'annulation du contexte de la requête (état %q)", st)
	}
	if res.ConsoleLog == "" {
		t.Error("ConsoleLog doit être renseigné")
	}
	if _, err := os.Stat(res.ConsoleLog); err != nil {
		t.Errorf("la console invitée doit être créée : %v", err)
	}
}

// procState renvoie l'état du process dans /proc/<pid>/stat (« R », « S »,
// « Z »…), ou la chaîne vide s'il n'existe plus.
func procState(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// Le nom de la commande est entre parenthèses et peut contenir des espaces :
	// l'état est le premier champ après la dernière parenthèse fermante.
	i := strings.LastIndex(string(data), ")")
	if i < 0 {
		return ""
	}
	fields := strings.Fields(string(data)[i+1:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
