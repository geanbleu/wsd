// Package api implements the wsd HTTP API handlers exposed on the Unix socket.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	mux.HandleFunc("POST /v1/workspaces/{id}/ensure", h.ensureWorkspace)
	mux.HandleFunc("POST /v1/workspaces/{id}/activity", h.updateActivity)
	mux.HandleFunc("POST /v1/workspaces/{id}/token", h.createToken)
	mux.HandleFunc("POST /v1/tokens/{token}", h.consumeToken)
	mux.HandleFunc("POST /v1/golden", h.handleBuildGolden)
	mux.HandleFunc("GET /v1/golden", h.handleGetGolden)
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

	golden, err := h.Store.ActiveGolden()
	if err != nil {
		return fmt.Errorf("golden lookup: %w", err)
	}

	// 1. Réseau
	egressRules := make([]netns.EgressRule, len(cfg.Egress.Allow))
	for i, r := range cfg.Egress.Allow {
		egressRules[i] = netns.EgressRule{CIDR: r.CIDR, Ports: r.Ports}
	}
	if err := netns.Setup(id8, alloc.HostIP, alloc.NSIP,
		cfg.Net.EgressIface, cfg.Net.DNS, egressRules); err != nil {
		return fmt.Errorf("netns setup: %w", err)
	}

	// 2. Répertoire workspace + chemin disque
	wsDir := cfg.WorkspaceDir(wsID)
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return err
	}
	wsDisk := filepath.Join(wsDir, "workspace.ext4")
	nsName := netns.NSName(id8)

	if golden != nil {
		return h.bootFromGolden(ctx, wsID, id8, nsName, wsDisk, alloc, golden)
	}
	// Cold boot : créer le disque workspace vierge
	if err := disk.CreateWorkspaceDisk(wsDisk, cfg.VM.WorkspaceDiskGiB); err != nil {
		return fmt.Errorf("disque workspace: %w", err)
	}
	return h.coldBoot(ctx, wsID, id8, nsName, wsDisk, alloc)
}

func (h *Handler) coldBoot(ctx context.Context, wsID, id8, nsName, wsDisk string, alloc *store.IPAllocation) error {
	cfg := h.Cfg
	log := h.Log.With("ws_id", wsID)
	imgDir := cfg.ImageDir(cfg.ImageID)
	uid, gid := cfg.UIDForBlock(alloc.BlockIdx)

	res, err := fc.Launch(ctx, fc.LaunchParams{
		WorkspaceID:    wsID,
		JailerBin:      cfg.JailerBin,
		FirecrackerBin: cfg.FirecrackerBin,
		JailerDir:      cfg.JailerDir,
		NetnsPath:      "/var/run/netns/" + nsName,
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

	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off nomodule rw"
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

	if err := waitAgent(ctx, res.VsockPath); err != nil {
		return fmt.Errorf("wsagent: %w", err)
	}
	if err := callPostRestore(ctx, res.VsockPath, wsID, false); err != nil {
		return fmt.Errorf("post_restore: %w", err)
	}
	if err := waitCodeServer(ctx, nsName, alloc.NSIP); err != nil {
		return fmt.Errorf("code-server: %w", err)
	}

	return h.Store.Transition(wsID, store.StateCreating, store.StateRunning)
}

func (h *Handler) bootFromGolden(ctx context.Context, wsID, id8, nsName, wsDisk string, alloc *store.IPAllocation, golden *store.Golden) error {
	cfg := h.Cfg
	log := h.Log.With("ws_id", wsID)
	imgDir := cfg.ImageDir(cfg.ImageID)
	uid, gid := cfg.UIDForBlock(alloc.BlockIdx)

	// Créer le disque workspace depuis le template golden (reflink) ou de zéro
	template := filepath.Join(golden.Dir, "workspace-template.ext4")
	if _, err := os.Stat(template); err == nil {
		if err := disk.Reflink(template, wsDisk); err != nil {
			return fmt.Errorf("reflink workspace template: %w", err)
		}
	} else {
		if err := disk.CreateWorkspaceDisk(wsDisk, cfg.VM.WorkspaceDiskGiB); err != nil {
			return fmt.Errorf("disque workspace: %w", err)
		}
	}

	res, err := fc.Launch(ctx, fc.LaunchParams{
		WorkspaceID:    wsID,
		JailerBin:      cfg.JailerBin,
		FirecrackerBin: cfg.FirecrackerBin,
		JailerDir:      cfg.JailerDir,
		NetnsPath:      "/var/run/netns/" + nsName,
		UID:            uid, GID: gid,
		MemMiB:       cfg.VM.MemMiB,
		KernelSrc:    filepath.Join(imgDir, "vmlinux"),
		RootfsSrc:    filepath.Join(golden.Dir, "rootfs.ext4"),
		WorkspaceSrc: wsDisk,
		VMStateSrc:   filepath.Join(golden.Dir, "vmstate"),
		MemSrc:       filepath.Join(golden.Dir, "mem"),
	})
	if err != nil {
		return fmt.Errorf("jailer launch: %w", err)
	}
	h.Store.SetPID(wsID, res.PID)

	fcc := res.Client
	if err := fcc.LoadSnapshot(ctx, fc.SnapshotLoadParams{
		SnapshotPath: "/vmstate",
		MemBackend: fc.MemoryBackend{
			BackendType: "File",
			BackendPath: "/mem",
		},
		ResumeVM: false,
	}); err != nil {
		return fmt.Errorf("LoadSnapshot: %w", err)
	}

	// Rediriger le disque workspace vers le disque frais de cet espace de travail
	if err := fcc.PatchDrive(ctx, "workspace", "/workspace.ext4"); err != nil {
		return fmt.Errorf("PatchDrive workspace: %w", err)
	}

	if err := fcc.Resume(ctx); err != nil {
		return fmt.Errorf("Resume: %w", err)
	}
	log.Info("VM restaurée depuis golden, attente wsagent...")

	if err := waitAgent(ctx, res.VsockPath); err != nil {
		return fmt.Errorf("wsagent: %w", err)
	}
	// mount_workspace=true : wsagent démonte le /workspace du golden et monte le disque frais
	if err := callPostRestore(ctx, res.VsockPath, wsID, true); err != nil {
		return fmt.Errorf("post_restore: %w", err)
	}
	if err := waitCodeServer(ctx, nsName, alloc.NSIP); err != nil {
		return fmt.Errorf("code-server: %w", err)
	}

	return h.Store.Transition(wsID, store.StateCreating, store.StateRunning)
}

// ── ensure / activity / token ─────────────────────────────────────────────────

func (h *Handler) ensureWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ws, err := h.Store.GetWorkspace(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "workspace introuvable")
			return
		}
		if ws.State == store.StateRunning {
			alloc, err := h.Store.GetIPAllocation(id)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "allocation IP introuvable")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"upstream": alloc.NSIP + ":8080"})
			return
		}
		if ws.State == store.StateCreating || ws.State == store.StateResuming {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		writeErr(w, http.StatusServiceUnavailable, "workspace non disponible: "+ws.State)
		return
	}
	writeErr(w, http.StatusGatewayTimeout, "timeout attente workspace running")
}

func (h *Handler) updateActivity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.Store.SetLastActivity(id, time.Now()); err != nil {
		writeErr(w, http.StatusNotFound, "workspace introuvable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.Store.GetWorkspace(id); err != nil {
		writeErr(w, http.StatusNotFound, "workspace introuvable")
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	if err := h.Store.CreateToken(token, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	url := "http://" + id + ".ws." + h.Cfg.Domain + "/?tkn=" + token
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "url": url})
}

func (h *Handler) consumeToken(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	wsID, err := h.Store.ConsumeToken(token)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"workspace_id": wsID})
}

// ── golden ────────────────────────────────────────────────────────────────────

func (h *Handler) handleBuildGolden(w http.ResponseWriter, r *http.Request) {
	log := h.Log.With("op", "build_golden")
	log.Info("construction golden démarrée")
	if err := h.buildGolden(r.Context()); err != nil {
		log.Error("build golden", "err", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	golden, err := h.Store.ActiveGolden()
	if err != nil || golden == nil {
		writeErr(w, http.StatusInternalServerError, "golden introuvable après création")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id": golden.ID, "image_id": golden.ImageID, "dir": golden.Dir,
	})
}

func (h *Handler) handleGetGolden(w http.ResponseWriter, r *http.Request) {
	golden, err := h.Store.ActiveGolden()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if golden == nil {
		writeErr(w, http.StatusNotFound, "aucun golden actif")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id": golden.ID, "image_id": golden.ImageID, "dir": golden.Dir,
	})
}

func (h *Handler) buildGolden(ctx context.Context) error {
	cfg := h.Cfg
	log := h.Log.With("op", "build_golden")

	tmpID := store.NewID()
	id8 := store.ID8(tmpID)
	imgDir := cfg.ImageDir(cfg.ImageID)

	// 1. Allocation IP temporaire
	alloc, err := h.Store.AllocateIP(tmpID, cfg.Net.Pool)
	if err != nil {
		return fmt.Errorf("allocation IP: %w", err)
	}
	defer h.Store.ReleaseIP(tmpID)

	// 2. Réseau
	egressRules := make([]netns.EgressRule, len(cfg.Egress.Allow))
	for i, r := range cfg.Egress.Allow {
		egressRules[i] = netns.EgressRule{CIDR: r.CIDR, Ports: r.Ports}
	}
	nsName := netns.NSName(id8)
	if err := netns.Setup(id8, alloc.HostIP, alloc.NSIP,
		cfg.Net.EgressIface, cfg.Net.DNS, egressRules); err != nil {
		return fmt.Errorf("netns setup: %w", err)
	}
	defer netns.Teardown(id8, cfg.Net.EgressIface)

	// 3. Disque workspace temporaire
	tmpDir := cfg.WorkspaceDir(tmpID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	tmpDisk := filepath.Join(tmpDir, "workspace.ext4")
	if err := disk.CreateWorkspaceDisk(tmpDisk, cfg.VM.WorkspaceDiskGiB); err != nil {
		return fmt.Errorf("disque workspace: %w", err)
	}

	// 4. Lancement VM (cold boot)
	uid, gid := cfg.UIDForBlock(alloc.BlockIdx)
	res, err := fc.Launch(ctx, fc.LaunchParams{
		WorkspaceID:    tmpID,
		JailerBin:      cfg.JailerBin,
		FirecrackerBin: cfg.FirecrackerBin,
		JailerDir:      cfg.JailerDir,
		NetnsPath:      "/var/run/netns/" + nsName,
		UID:            uid, GID: gid,
		MemMiB:       cfg.VM.MemMiB,
		KernelSrc:    filepath.Join(imgDir, "vmlinux"),
		RootfsSrc:    filepath.Join(imgDir, "rootfs.ext4"),
		WorkspaceSrc: tmpDisk,
	})
	if err != nil {
		return fmt.Errorf("jailer launch: %w", err)
	}
	defer func() {
		if p, err := os.FindProcess(res.PID); err == nil {
			p.Kill()
			p.Wait()
		}
		fc.DestroyChroot(cfg.JailerDir, tmpID)
	}()

	// 5. Démarrage VM
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off nomodule rw"
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
	log.Info("VM golden démarrée, attente wsagent...")

	if err := waitAgent(ctx, res.VsockPath); err != nil {
		return fmt.Errorf("wsagent: %w", err)
	}
	if err := callPostRestore(ctx, res.VsockPath, tmpID, false); err != nil {
		return fmt.Errorf("post_restore golden: %w", err)
	}
	if err := waitCodeServer(ctx, nsName, alloc.NSIP); err != nil {
		return fmt.Errorf("code-server golden: %w", err)
	}
	log.Info("code-server golden prêt, préparation snapshot...")

	// 6. Préparer le snapshot
	if err := func() error {
		conn, err := agentproto.Dial(res.VsockPath)
		if err != nil {
			return err
		}
		defer conn.Close()
		resp, err := conn.PrepareSnapshot()
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("prepare_snapshot: %s", resp.Error)
		}
		return nil
	}(); err != nil {
		return fmt.Errorf("prepare_snapshot: %w", err)
	}

	// 7. Pauser la VM et capturer le snapshot
	if err := fcc.Pause(ctx); err != nil {
		return fmt.Errorf("Pause: %w", err)
	}
	if err := fcc.CreateSnapshot(ctx, fc.SnapshotCreateParams{
		SnapshotPath: "/run/vmstate",
		MemFilePath:  "/run/mem",
		SnapshotType: "Full",
	}); err != nil {
		return fmt.Errorf("CreateSnapshot: %w", err)
	}

	// 8. Déplacer les fichiers vers le répertoire golden
	goldenID := store.NewID()
	goldenDir := cfg.GoldenDir(goldenID)
	if err := os.MkdirAll(goldenDir, 0o755); err != nil {
		return err
	}
	chroot := res.ChromeRoot
	if err := os.Rename(
		filepath.Join(chroot, "run", "vmstate"),
		filepath.Join(goldenDir, "vmstate"),
	); err != nil {
		os.RemoveAll(goldenDir)
		return fmt.Errorf("déplacement vmstate: %w", err)
	}
	if err := os.Rename(
		filepath.Join(chroot, "run", "mem"),
		filepath.Join(goldenDir, "mem"),
	); err != nil {
		os.RemoveAll(goldenDir)
		return fmt.Errorf("déplacement mem: %w", err)
	}
	if err := disk.Reflink(
		filepath.Join(chroot, "rootfs.ext4"),
		filepath.Join(goldenDir, "rootfs.ext4"),
	); err != nil {
		os.RemoveAll(goldenDir)
		return fmt.Errorf("reflink rootfs golden: %w", err)
	}
	// Sauvegarder le disque workspace formaté comme template pour les futurs clones
	if err := disk.Reflink(tmpDisk, filepath.Join(goldenDir, "workspace-template.ext4")); err != nil {
		log.Warn("reflink workspace-template ignoré", "err", err)
	}

	// 9. Enregistrer en base
	if err := h.Store.CreateGolden(goldenID, cfg.ImageID, goldenDir); err != nil {
		os.RemoveAll(goldenDir)
		return fmt.Errorf("CreateGolden: %w", err)
	}
	if err := h.Store.SetActiveGolden(goldenID); err != nil {
		return fmt.Errorf("SetActiveGolden: %w", err)
	}

	log.Info("golden créé", "golden_id", goldenID, "dir", goldenDir)
	return nil
}

// ── workspace teardown ────────────────────────────────────────────────────────

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

func callPostRestore(ctx context.Context, vsockPath, wsID string, mountWorkspace bool) error {
	conn, err := agentproto.Dial(vsockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	rnd := make([]byte, 32)
	rand.Read(rnd)

	resp, err := conn.PostRestore(agentproto.PostRestoreReq{
		TimeUnixNano:   time.Now().UnixNano(),
		RandomBytes:    rnd,
		MachineID:      strings.ReplaceAll(wsID, "-", ""),
		Hostname:       "ws-" + store.ID8(wsID),
		MountWorkspace: mountWorkspace,
		RestartUnits:   []string{"code-server.service"},
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("post_restore: %s", resp.Error)
	}
	return nil
}

func waitCodeServer(ctx context.Context, nsName, nsIP string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		out, _ := exec.CommandContext(ctx, "ip", "netns", "exec", nsName,
			"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", "2",
			fmt.Sprintf("http://%s:8080/", nsIP)).Output()
		code := strings.TrimSpace(string(out))
		if code == "200" || code == "302" {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("code-server non disponible après 30 s")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
