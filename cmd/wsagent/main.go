package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"wsd/internal/agentproto"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := mountWorkspace(log); err != nil {
		log.Warn("montage /workspace au démarrage échoué", "err", err)
	}

	log.Info("wsagent démarré", "vsock_port", agentproto.Port)
	for {
		if err := serveOnce(log); err != nil {
			log.Info("listener vsock relancé après erreur", "err", err)
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// serveOnce crée un listener vsock et boucle sur Accept.
// Retourne une erreur si le listener doit être recréé (ex. après restore).
func serveOnce(log *slog.Logger) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	if err := unix.Bind(fd, &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: agentproto.Port,
	}); err != nil {
		return err
	}
	if err := unix.Listen(fd, 16); err != nil {
		return err
	}

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			return err // déclenche la recréation du listener
		}
		go handleConn(log, nfd)
	}
}

func handleConn(log *slog.Logger, fd int) {
	f := os.NewFile(uintptr(fd), "vsock")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		log.Error("FileConn", "err", err)
		return
	}
	defer conn.Close()

	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)

	for {
		// Décoder d'abord en brut pour pouvoir re-décoder dans le bon type
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return
		}
		var req agentproto.Req
		json.Unmarshal(raw, &req)

		switch req.Op {
		case agentproto.OpPing:
			enc.Encode(agentproto.PingResp{ID: req.ID, OK: true, Ready: true})

		case agentproto.OpPostRestore:
			var pr agentproto.PostRestoreReq
			if err := json.Unmarshal(raw, &pr); err != nil {
				enc.Encode(agentproto.OKResp{ID: req.ID, OK: false, Error: err.Error()})
				continue
			}
			err := handlePostRestore(log, pr)
			resp := agentproto.OKResp{ID: req.ID, OK: err == nil}
			if err != nil {
				resp.Error = err.Error()
				log.Error("post_restore", "err", err)
			}
			enc.Encode(resp)

		case agentproto.OpPrepareSnapshot:
			err := prepareSnapshot()
			resp := agentproto.OKResp{ID: req.ID, OK: err == nil}
			if err != nil {
				resp.Error = err.Error()
			}
			enc.Encode(resp)

		case agentproto.OpStats:
			enc.Encode(collectStats(req.ID))

		default:
			enc.Encode(agentproto.OKResp{ID: req.ID, OK: false,
				Error: "op inconnue: " + req.Op})
		}
	}
}

// ── post_restore ──────────────────────────────────────────────────────────────

func handlePostRestore(log *slog.Logger, req agentproto.PostRestoreReq) error {
	// 1. Horloge
	if req.TimeUnixNano > 0 {
		ts := unix.NsecToTimespec(req.TimeUnixNano)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			log.Warn("clock_settime", "err", err)
		}
	}

	// 2. Réensemencement RNG
	if len(req.RandomBytes) > 0 {
		if f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0); err == nil {
			f.Write(req.RandomBytes)
			f.Close()
		}
	}

	// 3. machine-id
	if req.MachineID != "" {
		os.WriteFile("/etc/machine-id", []byte(req.MachineID+"\n"), 0o444)
	}

	// 4. hostname
	if req.Hostname != "" {
		unix.Sethostname([]byte(req.Hostname))
		os.WriteFile("/etc/hostname", []byte(req.Hostname+"\n"), 0o644)
	}

	// 5. Fichiers poussés par le host
	for _, f := range req.Files {
		if err := writeFile(f); err != nil {
			return fmt.Errorf("écriture %s: %w", f.Path, err)
		}
	}

	// 6. Montage /workspace
	if req.MountWorkspace {
		if isMounted("/workspace") {
			// Drive was patched after snapshot restore — force unmount stale mount
			unix.Unmount("/workspace", unix.MNT_FORCE)
		}
		if err := mountWorkspace(log); err != nil {
			return fmt.Errorf("montage workspace: %w", err)
		}
	}

	// 7. Démarrage des unités
	for _, unit := range req.RestartUnits {
		log.Info("démarrage unité", "unit", unit)
		exec.Command("systemctl", "start", unit).Run()
	}

	// 8. Attente des unités actives
	for _, unit := range req.RestartUnits {
		if err := waitUnit(unit, 30*time.Second); err != nil {
			log.Warn("unité non active", "unit", unit, "err", err)
		}
	}

	log.Info("post_restore terminé", "hostname", req.Hostname)
	return nil
}

func writeFile(f agentproto.FileSpec) error {
	content, err := base64.StdEncoding.DecodeString(f.ContentB64)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(parentDir(f.Path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(f.Path, content, os.FileMode(f.Mode)); err != nil {
		return err
	}
	if f.Owner != "" {
		if u, err := user.Lookup(f.Owner); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			os.Chown(f.Path, uid, gid)
		}
	}
	return nil
}

func waitUnit(unit string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("systemctl", "is-active", "--quiet", unit).Output()
		if strings.TrimSpace(string(out)) == "" {
			// is-active --quiet ne sort rien ; code retour 0 = active
			if exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("unité %s non active après %v", unit, timeout)
}

// ── workspace mount ───────────────────────────────────────────────────────────

func mountWorkspace(log *slog.Logger) error {
	if isMounted("/workspace") {
		return nil
	}
	os.MkdirAll("/workspace", 0o755)
	if err := run("mount", "-t", "ext4", "/dev/vdb", "/workspace"); err != nil {
		log.Info("format /dev/vdb (disque vierge détecté)")
		if err2 := run("mkfs.ext4", "-q", "/dev/vdb"); err2 != nil {
			return err2
		}
		if err2 := run("mount", "-t", "ext4", "/dev/vdb", "/workspace"); err2 != nil {
			return err2
		}
	}
	run("chown", "coder:coder", "/workspace")
	log.Info("/workspace monté")
	return nil
}

func isMounted(path string) bool {
	data, _ := os.ReadFile("/proc/mounts")
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) >= 2 && f[1] == path {
			return true
		}
	}
	return false
}

// ── prepare_snapshot ──────────────────────────────────────────────────────────

func prepareSnapshot() error {
	run("sync")
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o200)
}

// ── stats ─────────────────────────────────────────────────────────────────────

func collectStats(id string) map[string]any {
	data, _ := os.ReadFile("/proc/meminfo")
	return map[string]any{"id": id, "ok": true, "meminfo_lines": strings.Count(string(data), "\n")}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func parentDir(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "/"
}
