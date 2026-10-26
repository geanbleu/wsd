// Package api implements the wsd HTTP API handlers exposed on the Unix socket.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"wsd/internal/agentproto"
	"wsd/internal/config"
	"wsd/internal/disk"
	"wsd/internal/fc"
	"wsd/internal/netns"
	"wsd/internal/store"
)

// Deps holds the dependencies injected into the handler.
type Deps struct {
	Store *store.Store
	Cfg   *config.Config
	Log   *slog.Logger
}

// Handler implements all /v1/* routes.
type Handler struct {
	Deps
}

// New creates a Handler.
func New(d Deps) *Handler { return &Handler{Deps: d} }

// RegisterRoutes registers all API routes on mux (Go ≥ 1.22 method patterns).
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/workspaces", h.createWorkspace)
	mux.HandleFunc("GET /v1/workspaces", h.listWorkspaces)
	mux.HandleFunc("GET /v1/workspaces/{id}", h.getWorkspace)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", h.deleteWorkspace)
	mux.HandleFunc("GET /v1/health", h.health)
}

// ── request / response types ─────────────────────────────────────────────────

type createReq struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

type wsResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Owner        string `json:"owner"`
	State        string `json:"state"`
	HostIP       string `json:"host_ip,omitempty"`
	NSIP         string `json:"ns_ip,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	LastActivity int64  `json:"last_activity"`
}

func toResponse(w *store.Workspace, alloc *store.IPAllocation) wsResponse {
	r := wsResponse{
		ID:           w.ID,
		Name:         w.Name,
		Owner:        w.Owner,
		State:        w.State,
		CreatedAt:    w.CreatedAt.Unix(),
		LastActivity: w.LastActivity.Unix(),
	}
	if alloc != nil {
		r.HostIP = alloc.HostIP
		r.NSIP = alloc.NSIP
	}
	return r
}

// ── handlers ─────────────────────────────────────────────────────────────────

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body invalide: "+err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name requis")
		return
	}

	wsID := store.NewID()
	log := h.Log.With("ws_id", wsID)
	now := time.Now()

	ws := store.Workspace{
		ID: wsID, Name: req.Name, Owner: req.Owner,
		State: store.StateCreating, ImageID: h.Cfg.ImageID,
		CreatedAt: now, LastActivity: now,
	}
	if err := h.Store.CreateWorkspace(ws); err != nil {
		log.Error("create workspace", "err", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	alloc, err := h.Store.AllocateIP(wsID, h.Cfg.Net.Pool)
	if err != nil {
		h.Store.MarkError(wsID)
		writeErr(w, http.StatusInternalServerError, "allocation IP: "+err.Error())
		return
	}

	// Lancement dans une goroutine, la requête HTTP bloque jusqu'à running.
	result := make(chan error, 1)
	go func() { result <- h.bootWorkspace(r.Context(), wsID, alloc) }()

	select {
	case err := <-result:
		if err != nil {
			log.Error("boot workspace", "err", err)
			h.Store.Transition(wsID, store.StateCreating, store.StateError)
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case <-r.Context().Done():
		writeErr(w, http.StatusGatewayTimeout, "timeout création workspace")
		return
	}

	final, _ := h.Store.GetWorkspace(wsID)
	writeJSON(w, http.StatusCreated, toResponse(final, alloc))
}

func (h *Handler) bootWorkspace(ctx context.Context, wsID string, alloc *store.IPAllocation) error {
	cfg := h.Cfg
	id8 := store.ID8(wsID)
	log := h.Log.With("ws_id", wsID)

	// 1. Réseau
	egressRules := make([]netns.EgressRule, len(cfg.Egress.Allow))
	for i, r := range cfg.Egress.Allow {
		egressRules[i] = netns.EgressRule{CIDR: r.CIDR, Ports: r.Ports}
	}
	if err := netns.Setup(id8, alloc.HostIP, alloc.NSIP,
		cfg.Net.EgressIface, cfg.Net.DNS, egressRules); err != nil {
		return fmt.Errorf("netns setup: %w", err)
	}

	// 2. Disque workspace
	wsDir := cfg.WorkspaceDir(wsID)
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return err
	}
	wsDisk := filepath.Join(wsDir, "workspace.ext4")
	if err := disk.CreateWorkspaceDisk(wsDisk, cfg.VM.WorkspaceDiskGiB); err != nil {
		return fmt.Errorf("disque workspace: %w", err)
	}

	// 3. Chroot + jailer
	imgDir := cfg.ImageDir(cfg.ImageID)
	uid, gid := cfg.UIDForBlock(alloc.BlockIdx)
	res, err := fc.Launch(ctx, fc.LaunchParams{
		WorkspaceID:    wsID,
		JailerBin:      cfg.JailerBin,
		FirecrackerBin: cfg.FirecrackerBin,
		JailerDir:      cfg.JailerDir,
		NetnsPath:      "/var/run/netns/" + netns.NSName(id8),
		UID:            uid, GID: gid,
		MemMiB:       cfg.VM.MemMiB,
		KernelSrc:    filepath.Join(imgDir, "vmlinux"),
		RootfsSrc:    filepath.Join(imgDir, "rootfs.ext4"),
		WorkspaceSrc: wsDisk,
	})
	if err != nil {
		return fmt.Errorf("jailer launch: %w", err)
	}

	h.Store.SetPID(wsID, res.PID)

	// 4. Configurer et démarrer la VM
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off nomodule ro"
	fcc := res.Client
	if err := fcc.PutMachineConfig(ctx, fc.MachineConfig{
		VcpuCount: cfg.VM.VCPUs, MemSizeMiB: cfg.VM.MemMiB,
	}); err != nil {
		return err
	}
	if err := fcc.PutBootSource(ctx, fc.BootSource{
		KernelImagePath: "/vmlinux", BootArgs: bootArgs,
	}); err != nil {
		return err
	}
	if err := fcc.PutDrive(ctx, fc.Drive{
		DriveID: "rootfs", PathOnHost: "/rootfs.ext4",
		IsRootDevice: true, IsReadOnly: false,
	}); err != nil {
		return err
	}
	if err := fcc.PutDrive(ctx, fc.Drive{
		DriveID: "workspace", PathOnHost: "/workspace.ext4",
		IsRootDevice: false, IsReadOnly: false,
	}); err != nil {
		return err
	}
	if err := fcc.PutNetworkInterface(ctx, fc.NetworkInterface{
		IfaceID: "eth0", HostDevName: "tap0",
		GuestMAC: "06:00:AC:10:00:02",
	}); err != nil {
		return err
	}
	if err := fcc.PutVsock(ctx, fc.Vsock{
		GuestCID: 3, UDSPath: "/v.sock",
	}); err != nil {
		return err
	}
	if err := fcc.Start(ctx); err != nil {
		return fmt.Errorf("InstanceStart: %w", err)
	}
	log.Info("VM démarrée, attente wsagent...")

	// 5. Attendre wsagent
	if err := waitAgent(ctx, res.VsockPath); err != nil {
		return fmt.Errorf("wsagent: %w", err)
	}

	return h.Store.Transition(wsID, store.StateCreating, store.StateRunning)
}

func waitAgent(ctx context.Context, vsockPath string) error {
	tryPing := func() error {
		conn, err := agentproto.Dial(vsockPath)
		if err != nil {
			return err
		}
		defer conn.Close()
		resp, err := conn.Ping()
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("ping réponse not OK")
		}
		return nil
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := tryPing(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("agent non disponible après 60 s")
}

func (h *Handler) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	workspaces, err := h.Store.ListWorkspaces(owner)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]wsResponse, 0, len(workspaces))
	for _, ws := range workspaces {
		alloc, _ := h.Store.GetIPAllocation(ws.ID)
		out = append(out, toResponse(ws, alloc))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.Store.GetWorkspace(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "workspace introuvable")
		return
	}
	alloc, _ := h.Store.GetIPAllocation(id)
	writeJSON(w, http.StatusOK, toResponse(ws, alloc))
}

func (h *Handler) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	log := h.Log.With("ws_id", id)

	ws, err := h.Store.GetWorkspace(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "workspace introuvable")
		return
	}
	if ws.State == store.StateDeleted {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h.Store.Transition(id, ws.State, store.StateDeleting)

	go func() {
		if err := h.destroyWorkspace(id, ws); err != nil {
			log.Error("destroy workspace", "err", err)
		}
	}()

	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) destroyWorkspace(wsID string, ws *store.Workspace) error {
	cfg := h.Cfg
	id8 := store.ID8(wsID)
	log := h.Log.With("ws_id", wsID)

	// 1. Tuer le process firecracker
	if ws.PID > 0 {
		if p, err := os.FindProcess(ws.PID); err == nil {
			p.Kill()
			p.Wait()
		}
	}

	// 2. Supprimer le chroot
	if err := fc.DestroyChroot(cfg.JailerDir, wsID); err != nil {
		log.Warn("destroy chroot", "err", err)
	}

	// 3. Supprimer le netns (et toutes ses interfaces)
	if err := netns.Teardown(id8, cfg.Net.EgressIface); err != nil {
		log.Warn("netns teardown", "err", err)
	}

	// 4. Supprimer le disque workspace (sauf si --keep_data)
	wsDir := cfg.WorkspaceDir(wsID)
	os.RemoveAll(wsDir)

	// 5. Libérer l'IP
	h.Store.ReleaseIP(wsID)

	return h.Store.Transition(wsID, store.StateDeleting, store.StateDeleted)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
