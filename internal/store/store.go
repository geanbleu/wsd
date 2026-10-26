// Package store manages workspace state in SQLite.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	_ "embed"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// States
const (
	StateCreating  = "creating"
	StateRunning   = "running"
	StateSuspending = "suspending"
	StateSuspended = "suspended"
	StateResuming  = "resuming"
	StateError     = "error"
	StateDeleting  = "deleting"
	StateDeleted   = "deleted"
)

var validTransitions = map[string][]string{
	StateCreating:  {StateRunning, StateError, StateDeleting},
	StateRunning:   {StateSuspending, StateError, StateDeleting},
	StateSuspending: {StateSuspended, StateError},
	StateSuspended: {StateResuming, StateDeleting},
	StateResuming:  {StateRunning, StateError},
	StateError:     {StateDeleting},
	StateDeleting:  {StateDeleted},
}

func validTransition(from, to string) bool {
	for _, s := range validTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Workspace holds the state of a single workspace.
type Workspace struct {
	ID           string
	Name         string
	Owner        string
	State        string
	ImageID      string
	GoldenID     string
	PID          int
	CreatedAt    time.Time
	LastActivity time.Time
}

// IPAllocation holds the network allocation for a workspace.
type IPAllocation struct {
	WorkspaceID string
	BlockIdx    int
	HostIP      string
	NSIP        string
}

// Store is the wsd database.
type Store struct {
	db    *sql.DB
	wsMu  sync.Map // map[workspace-id]*sync.Mutex
	ipMu  sync.Mutex
}

// Open opens or creates the SQLite database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("store open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite WAL: single writer
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// wsLock returns (or creates) the per-workspace mutex.
func (s *Store) wsLock(id string) *sync.Mutex {
	v, _ := s.wsMu.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// NewID generates a random workspace ID.
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ID8 returns the first 8 chars of an ID (used for interface names).
func ID8(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// ── workspace CRUD ────────────────────────────────────────────────────────────

func (s *Store) CreateWorkspace(w Workspace) error {
	_, err := s.db.Exec(
		`INSERT INTO workspaces (id,name,owner,state,image_id,golden_id,pid,created_at,last_activity)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Name, w.Owner, w.State, w.ImageID, nilStr(w.GoldenID),
		nilInt(w.PID), w.CreatedAt.Unix(), w.LastActivity.Unix(),
	)
	return err
}

func (s *Store) GetWorkspace(id string) (*Workspace, error) {
	row := s.db.QueryRow(
		`SELECT id,name,owner,state,image_id,COALESCE(golden_id,''),COALESCE(pid,0),created_at,last_activity
		 FROM workspaces WHERE id=?`, id)
	return scanWorkspace(row)
}

func (s *Store) ListWorkspaces(owner string) ([]*Workspace, error) {
	q := `SELECT id,name,owner,state,image_id,COALESCE(golden_id,''),COALESCE(pid,0),created_at,last_activity
	      FROM workspaces WHERE state != ?`
	args := []any{StateDeleted}
	if owner != "" {
		q += " AND owner=?"
		args = append(args, owner)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Transition atomically moves workspace id from state `from` to `to`.
func (s *Store) Transition(id, from, to string) error {
	if !validTransition(from, to) {
		return fmt.Errorf("transition invalide %s → %s", from, to)
	}
	mu := s.wsLock(id)
	mu.Lock()
	defer mu.Unlock()

	res, err := s.db.Exec(
		`UPDATE workspaces SET state=? WHERE id=? AND state=?`, to, id, from)
	if err != nil {
		return fmt.Errorf("transition %s → %s: %w", from, to, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("transition %s → %s: état actuel != %s", from, to, from)
	}
	return nil
}

// SetPID stores the firecracker/jailer PID for a workspace.
func (s *Store) SetPID(id string, pid int) error {
	_, err := s.db.Exec(`UPDATE workspaces SET pid=? WHERE id=?`, pid, id)
	return err
}

// SetLastActivity updates last_activity for a workspace.
func (s *Store) SetLastActivity(id string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE workspaces SET last_activity=? WHERE id=?`, t.Unix(), id)
	return err
}

// ── IP allocation ─────────────────────────────────────────────────────────────

// AllocateIP finds the lowest free /30 block in the pool and records it.
func (s *Store) AllocateIP(wsID string, poolCIDR string) (*IPAllocation, error) {
	_, ipNet, err := net.ParseCIDR(poolCIDR)
	if err != nil {
		return nil, fmt.Errorf("pool CIDR invalide %q: %w", poolCIDR, err)
	}
	ones, bits := ipNet.Mask.Size()
	maxBlocks := (1 << (bits - ones)) / 4

	s.ipMu.Lock()
	defer s.ipMu.Unlock()

	// Find lowest unused block_idx
	rows, err := s.db.Query(`SELECT block_idx FROM ip_allocations ORDER BY block_idx`)
	if err != nil {
		return nil, err
	}
	used := make(map[int]bool)
	for rows.Next() {
		var idx int
		rows.Scan(&idx)
		used[idx] = true
	}
	rows.Close()

	free := -1
	for i := 0; i < maxBlocks; i++ {
		if !used[i] {
			free = i
			break
		}
	}
	if free < 0 {
		return nil, errors.New("pool d'IP épuisé")
	}

	hostIP, nsIP := ipsForBlock(ipNet.IP, free)
	_, err = s.db.Exec(
		`INSERT INTO ip_allocations (workspace_id,block_idx,host_ip,ns_ip) VALUES (?,?,?,?)`,
		wsID, free, hostIP, nsIP)
	if err != nil {
		return nil, err
	}
	return &IPAllocation{WorkspaceID: wsID, BlockIdx: free, HostIP: hostIP, NSIP: nsIP}, nil
}

// GetIPAllocation returns the IP allocation for a workspace.
func (s *Store) GetIPAllocation(wsID string) (*IPAllocation, error) {
	row := s.db.QueryRow(
		`SELECT workspace_id,block_idx,host_ip,ns_ip FROM ip_allocations WHERE workspace_id=?`, wsID)
	var a IPAllocation
	if err := row.Scan(&a.WorkspaceID, &a.BlockIdx, &a.HostIP, &a.NSIP); err != nil {
		return nil, err
	}
	return &a, nil
}

// ReleaseIP removes the IP allocation for a workspace.
func (s *Store) ReleaseIP(wsID string) error {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	_, err := s.db.Exec(`DELETE FROM ip_allocations WHERE workspace_id=?`, wsID)
	return err
}

// ── golden ───────────────────────────────────────────────────────────────────

// Golden represents a saved Firecracker snapshot used as a boot baseline.
type Golden struct {
	ID      string
	ImageID string
	Dir     string // directory containing vmstate, mem, rootfs.ext4
}

// ActiveGolden returns the current active golden, or nil if none.
func (s *Store) ActiveGolden() (*Golden, error) {
	row := s.db.QueryRow(`SELECT id,image_id,dir FROM goldens WHERE is_active=1 LIMIT 1`)
	var g Golden
	if err := row.Scan(&g.ID, &g.ImageID, &g.Dir); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &g, nil
}

// CreateGolden records a new (inactive) golden snapshot.
func (s *Store) CreateGolden(id, imageID, dir string) error {
	_, err := s.db.Exec(
		`INSERT INTO goldens (id,image_id,dir,is_active,created_at) VALUES (?,?,?,0,?)`,
		id, imageID, dir, time.Now().Unix())
	return err
}

// SetActiveGolden atomically marks id as the active golden (deactivates all others).
func (s *Store) SetActiveGolden(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE goldens SET is_active=0`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE goldens SET is_active=1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("golden %s introuvable", id)
	}
	return tx.Commit()
}

// ── reconciliation ────────────────────────────────────────────────────────────

// ActiveWorkspaces returns all workspaces that are not deleted/error/deleting.
func (s *Store) ActiveWorkspaces() ([]*Workspace, error) {
	rows, err := s.db.Query(
		`SELECT id,name,owner,state,image_id,COALESCE(golden_id,''),COALESCE(pid,0),created_at,last_activity
		 FROM workspaces WHERE state NOT IN (?,?,?)`,
		StateDeleted, StateError, StateDeleting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkError sets a workspace to the error state unconditionally.
func (s *Store) MarkError(id string) error {
	_, err := s.db.Exec(`UPDATE workspaces SET state=? WHERE id=?`, StateError, id)
	return err
}

// ── helpers ───────────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanWorkspace(r scanner) (*Workspace, error) {
	var w Workspace
	var ca, la int64
	if err := r.Scan(&w.ID, &w.Name, &w.Owner, &w.State, &w.ImageID,
		&w.GoldenID, &w.PID, &ca, &la); err != nil {
		return nil, err
	}
	w.CreatedAt = time.Unix(ca, 0)
	w.LastActivity = time.Unix(la, 0)
	return &w, nil
}

func nilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

// ipsForBlock returns host and ns IPs for /30 block at index idx in pool.
func ipsForBlock(base net.IP, idx int) (hostIP, nsIP string) {
	b4 := base.To4()
	u := binary.BigEndian.Uint32(b4)
	u += uint32(idx * 4)
	host := make(net.IP, 4)
	ns := make(net.IP, 4)
	binary.BigEndian.PutUint32(host, u+1)
	binary.BigEndian.PutUint32(ns, u+2)
	return host.String(), ns.String()
}
