package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wsd/internal/api"
	"wsd/internal/store"
)

func main() {
	configPath := flag.String("config", "/etc/wsd/wsd.json", "chemin vers wsd.json")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	dbPath := cfg.DataDir + "/wsd.db"
	st, err := store.Open(dbPath)
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := reconcile(log, st); err != nil {
		log.Warn("réconciliation", "err", err)
	}

	h := api.New(api.Deps{Store: st, Cfg: cfg, Log: log})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	sockPath := cfg.SocketPath
	os.MkdirAll(sockPath[:len(sockPath)-len("/wsd.sock")], 0o750)
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	os.Chmod(sockPath, 0o660)

	srv := &http.Server{Handler: mux}
	log.Info("wsd démarré", "socket", sockPath)

	go idleReaper(log, st, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func reconcile(log *slog.Logger, st *store.Store) error {
	workspaces, err := st.ActiveWorkspaces()
	if err != nil {
		return err
	}
	for _, w := range workspaces {
		switch w.State {
		case store.StateRunning, store.StateCreating, store.StateResuming, store.StateSuspending:
			if w.PID == 0 || !pidAlive(w.PID) {
				log.Warn("workspace orphelin → error", "ws_id", w.ID, "state", w.State, "pid", w.PID)
				st.MarkError(w.ID)
			}
		}
	}
	return nil
}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func idleReaper(log *slog.Logger, st *store.Store, cfg interface{ IdleTimeoutDuration() time.Duration }) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		// Phase 5 : suspendre les workspaces inactifs depuis > idle_timeout
		_ = log
		_ = st
	}
}
