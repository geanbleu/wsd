package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wsd/internal/api"
	"wsd/internal/config"
	"wsd/internal/store"
	"wsd/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/wsd/wsd.json", "chemin vers wsd.json")
	showVersion := flag.Bool("version", false, "afficher la version et quitter")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("wsd"))
		return
	}

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

	checkImage(log, cfg)

	if err := reconcile(log, st); err != nil {
		log.Warn("réconciliation", "err", err)
	}

	h := api.New(api.Deps{Store: st, Cfg: cfg, Log: log})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	ln, err := listenAPI(log, cfg)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	sockPath := cfg.SocketPath

	srv := &http.Server{Handler: mux}
	log.Info("wsd démarré", "socket", sockPath)

	go idleReaper(log, st, h, cfg)

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

// checkImage signale au démarrage une image invitée absente ou incomplète.
// Ce n'est pas fatal — l'image peut être déposée ensuite, sans redémarrer wsd —
// mais le dire ici évite de le découvrir à la première création de workspace,
// sous la forme d'une erreur de hardlink.
func checkImage(log *slog.Logger, cfg *config.Config) {
	if cfg.ImageID == "" {
		log.Warn("image_id non renseigné : création de workspace et construction de golden impossibles")
		return
	}
	dir := cfg.ImageDir(cfg.ImageID)
	var missing []string
	for _, name := range []string{"vmlinux", "rootfs.ext4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		log.Warn("image invitée incomplète : la création de workspace échouera",
			"image_id", cfg.ImageID, "dir", dir,
			"manquant", strings.Join(missing, ", "),
			"remede", "make image kernel, puis copier rootfs.ext4 et vmlinux dans ce répertoire")
		return
	}
	log.Info("image invitée en place", "image_id", cfg.ImageID, "dir", dir)
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

func idleReaper(log *slog.Logger, st *store.Store, h interface{ SuspendByID(string) error }, cfg interface{ IdleTimeoutDuration() time.Duration }) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		workspaces, err := st.ActiveWorkspaces()
		if err != nil {
			log.Warn("idleReaper: liste workspaces", "err", err)
			continue
		}
		cutoff := time.Now().Add(-cfg.IdleTimeoutDuration())
		for _, w := range workspaces {
			if w.State == store.StateRunning && w.LastActivity.Before(cutoff) {
				log.Info("suspendre workspace inactif", "ws_id", w.ID, "last_activity", w.LastActivity)
				if err := h.SuspendByID(w.ID); err != nil {
					log.Warn("suspension échouée", "ws_id", w.ID, "err", err)
				}
			}
		}
	}
}

// listenAPI prépare le socket unix de l'API : répertoire et socket en 0660,
// groupe cfg.SocketGroup, pour que wsproxy (membre de ce groupe) puisse l'ouvrir
// alors que wsd tourne en root (SPEC §10).
func listenAPI(log *slog.Logger, cfg *config.Config) (net.Listener, error) {
	dir := filepath.Dir(cfg.SocketPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("créer %s: %w", dir, err)
	}

	gid := -1
	if group := *cfg.SocketGroup; group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			log.Warn("groupe du socket introuvable, socket laissé au groupe courant",
				"group", group, "err", err)
		} else if n, err := strconv.Atoi(g.Gid); err == nil {
			gid = n
		}
	}
	if gid >= 0 {
		if err := os.Chown(dir, 0, gid); err != nil {
			log.Warn("chgrp du répertoire du socket", "dir", dir, "err", err)
		}
	}

	if err := os.Remove(cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("supprimer le socket résiduel: %w", err)
	}
	ln, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, err
	}
	if gid >= 0 {
		if err := os.Chown(cfg.SocketPath, 0, gid); err != nil {
			log.Warn("chgrp du socket", "socket", cfg.SocketPath, "err", err)
		}
	}
	if err := os.Chmod(cfg.SocketPath, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod du socket: %w", err)
	}
	return ln, nil
}
