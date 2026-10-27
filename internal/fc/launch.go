package fc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"wsd/internal/disk"
)

// LaunchParams carries everything needed to start a Firecracker VM under jailer.
type LaunchParams struct {
	WorkspaceID    string
	JailerBin      string
	FirecrackerBin string
	JailerDir      string // chroot base (e.g. /srv/jailer)
	NetnsPath      string // /var/run/netns/ws-<id8>
	UID, GID       int
	MemMiB         int
	// Paths to the source files (before chroot)
	KernelSrc    string // /var/lib/wsd/images/<img>/vmlinux
	RootfsSrc    string // /var/lib/wsd/images/<img>/rootfs.ext4   (or golden)
	WorkspaceSrc string // /var/lib/wsd/workspaces/<ws-id>/workspace.ext4
	// Snapshot restore (Phase 3). Leave empty for cold boot.
	VMStateSrc     string // golden dir/vmstate — hardlinked as /vmstate in chroot
	MemSrc         string // golden dir/mem     — hardlinked as /mem in chroot
	HardlinkRootfs bool   // hardlink rootfs instead of reflink (resume from own rootfs)
	// cgroup cpu.max value (Phase 6), e.g. "200000 100000". Empty = no cpu.max cgroup.
	CPUMax string
}

// LaunchResult holds the running VM handle.
type LaunchResult struct {
	Client     *Client
	PID        int
	APISocket  string
	VsockPath  string
	ChromeRoot string
}

// Launch prepares the jailer chroot and starts Firecracker. It returns once the
// API socket is available (up to 5 s).
func Launch(ctx context.Context, p LaunchParams) (*LaunchResult, error) {
	// ── chroot paths ──────────────────────────────────────────────────────────
	chroot := filepath.Join(p.JailerDir, "firecracker", p.WorkspaceID, "root")
	apiSockInChroot := "/run/firecracker.sock"
	apiSock := filepath.Join(chroot, "run", "firecracker.sock")
	vsockPath := filepath.Join(chroot, "v.sock")
	logPath := filepath.Join(chroot, "run", "firecracker.log")

	if err := checkSources(p); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join(chroot, "run"), 0o755); err != nil {
		return nil, fmt.Errorf("chroot mkdir: %w", err)
	}

	// ── populer le chroot ─────────────────────────────────────────────────────
	if err := disk.Hardlink(p.KernelSrc, filepath.Join(chroot, "vmlinux")); err != nil {
		return nil, fmt.Errorf("hardlink vmlinux: %w", err)
	}
	if p.HardlinkRootfs {
		if err := disk.Hardlink(p.RootfsSrc, filepath.Join(chroot, "rootfs.ext4")); err != nil {
			return nil, fmt.Errorf("hardlink rootfs: %w", err)
		}
	} else {
		if err := disk.Reflink(p.RootfsSrc, filepath.Join(chroot, "rootfs.ext4")); err != nil {
			return nil, fmt.Errorf("reflink rootfs: %w", err)
		}
	}
	if err := disk.Hardlink(p.WorkspaceSrc, filepath.Join(chroot, "workspace.ext4")); err != nil {
		return nil, fmt.Errorf("hardlink workspace: %w", err)
	}
	if p.VMStateSrc != "" {
		if err := disk.Hardlink(p.VMStateSrc, filepath.Join(chroot, "vmstate")); err != nil {
			return nil, fmt.Errorf("hardlink vmstate: %w", err)
		}
	}
	if p.MemSrc != "" {
		if err := disk.Hardlink(p.MemSrc, filepath.Join(chroot, "mem")); err != nil {
			return nil, fmt.Errorf("hardlink mem: %w", err)
		}
	}

	// ── lancer jailer ─────────────────────────────────────────────────────────
	// --cgroup-version 2 est obligatoire (défaut jailer = 1)
	memMax := strconv.Itoa((p.MemMiB + 256) * 1024 * 1024)
	jailerArgs := []string{
		"--id", p.WorkspaceID,
		"--exec-file", p.FirecrackerBin,
		"--uid", strconv.Itoa(p.UID),
		"--gid", strconv.Itoa(p.GID),
		"--chroot-base-dir", p.JailerDir,
		"--netns", p.NetnsPath,
		"--cgroup-version", "2",
		"--cgroup", "memory.max=" + memMax,
	}
	if p.CPUMax != "" {
		jailerArgs = append(jailerArgs, "--cgroup", "cpu.max="+p.CPUMax)
	}
	jailerArgs = append(jailerArgs, "--",
		"--api-sock", apiSockInChroot,
		"--log-path", "/run/firecracker.log",
		"--level", "Info",
	)
	cmd := exec.CommandContext(ctx, p.JailerBin, jailerArgs...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("jailer start: %w", err)
	}

	// ── attendre le socket API ────────────────────────────────────────────────
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(apiSock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(apiSock); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("socket API non créé dans les 5 s (log: %s)", logPath)
	}

	return &LaunchResult{
		Client:     New(apiSock),
		PID:        cmd.Process.Pid,
		APISocket:  apiSock,
		VsockPath:  vsockPath,
		ChromeRoot: chroot,
	}, nil
}

// checkSources vérifie que les fichiers à lier dans le chroot existent.
// Sans ce contrôle, l'échec arrive sous la forme d'un « link <src> <dst>:
// no such file or directory », qui ne dit pas laquelle des deux extrémités
// manque — alors que c'est presque toujours l'image invitée qui n'est pas en
// place. Un hardlink entre filesystems différents donne, lui, un
// « invalid cross-device link ».
func checkSources(p LaunchParams) error {
	sources := []struct{ what, path string }{
		{"kernel invité (vmlinux)", p.KernelSrc},
		{"rootfs invité", p.RootfsSrc},
		{"disque workspace", p.WorkspaceSrc},
	}
	if p.VMStateSrc != "" {
		sources = append(sources, struct{ what, path string }{"état VM du snapshot (vmstate)", p.VMStateSrc})
	}
	if p.MemSrc != "" {
		sources = append(sources, struct{ what, path string }{"mémoire du snapshot (mem)", p.MemSrc})
	}

	for _, src := range sources {
		if src.path == "" {
			return fmt.Errorf("%s : chemin non renseigné", src.what)
		}
		if _, err := os.Stat(src.path); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%s introuvable : %s", src.what, src.path)
			}
			return fmt.Errorf("%s inaccessible (%s) : %w", src.what, src.path, err)
		}
	}
	return nil
}

// DestroyChroot removes the jailer chroot directory for a workspace.
func DestroyChroot(jailerDir, workspaceID string) error {
	chroot := filepath.Join(jailerDir, "firecracker", workspaceID)
	if err := os.RemoveAll(chroot); err != nil {
		return fmt.Errorf("destroy chroot %s: %w", workspaceID, err)
	}
	return nil
}
