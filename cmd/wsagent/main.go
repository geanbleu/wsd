package main

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"wsd/internal/agentproto"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := mountWorkspace(log); err != nil {
		log.Warn("montage /workspace échoué", "err", err)
	}

	if err := serve(log); err != nil {
		log.Error("vsock serve", "err", err)
		os.Exit(1)
	}
}

// mountWorkspace monte /dev/vdb sur /workspace.
// Si le disque est vierge (pas de superbloc ext4), il le formate d'abord.
// Idempotent : si /workspace est déjà un point de montage, ne fait rien.
func mountWorkspace(log *slog.Logger) error {
	if isMounted("/workspace") {
		return nil
	}
	if err := os.MkdirAll("/workspace", 0o755); err != nil {
		return err
	}
	// Essai de montage direct ; si le disque est vierge, on formate.
	if err := run("mount", "-t", "ext4", "/dev/vdb", "/workspace"); err != nil {
		log.Info("format /dev/vdb (disque vierge)")
		if err2 := run("mkfs.ext4", "-q", "/dev/vdb"); err2 != nil {
			return err2
		}
		if err2 := run("mount", "-t", "ext4", "/dev/vdb", "/workspace"); err2 != nil {
			return err2
		}
	}
	// chown coder
	_ = run("chown", "coder:coder", "/workspace")
	log.Info("/workspace monté")
	return nil
}

func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// serve ouvre un listener vsock sur le port 1024 et traite les connexions.
func serve(log *slog.Logger) error {
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

	log.Info("wsagent prêt", "vsock_port", agentproto.Port)

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			log.Error("accept vsock", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
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
		var req agentproto.Req
		if err := dec.Decode(&req); err != nil {
			return
		}
		switch req.Op {
		case agentproto.OpPing:
			_ = enc.Encode(agentproto.PingResp{ID: req.ID, OK: true, Ready: true})
		case agentproto.OpPrepareSnapshot:
			err := prepareSnapshot()
			resp := agentproto.OKResp{ID: req.ID, OK: err == nil}
			if err != nil {
				resp.Error = err.Error()
			}
			_ = enc.Encode(resp)
		default:
			_ = enc.Encode(agentproto.OKResp{ID: req.ID, OK: false, Error: "op inconnue: " + req.Op})
		}
	}
}

func prepareSnapshot() error {
	if err := run("sync"); err != nil {
		return err
	}
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o200)
}
