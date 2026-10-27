package store

import (
	"database/sql"
	"os"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	f, err := os.CreateTemp("", "wsd-store-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	s, err := Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTransitions(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	w := Workspace{ID: "aa112233-0000-0000-0000-000000000000", Name: "test",
		State: StateCreating, ImageID: "img1", CreatedAt: now, LastActivity: now}
	if err := s.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}

	// valid: creating → running
	if err := s.Transition(w.ID, StateCreating, StateRunning); err != nil {
		t.Fatalf("transition valide échouée: %v", err)
	}
	// invalid: running → creating
	if err := s.Transition(w.ID, StateRunning, StateCreating); err == nil {
		t.Fatal("transition invalide acceptée")
	}
	// valid: running → deleting → deleted
	if err := s.Transition(w.ID, StateRunning, StateDeleting); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(w.ID, StateDeleting, StateDeleted); err != nil {
		t.Fatal(err)
	}
}

func TestIPAllocation(t *testing.T) {
	s := openTemp(t)
	now := time.Now()

	// Créer deux workspaces et allouer des IP
	for i, id := range []string{
		"bb000001-0000-0000-0000-000000000000",
		"bb000002-0000-0000-0000-000000000000",
	} {
		_ = i
		s.CreateWorkspace(Workspace{ID: id, Name: id, State: StateCreating,
			ImageID: "img1", CreatedAt: now, LastActivity: now})
	}

	a1, err := s.AllocateIP("bb000001-0000-0000-0000-000000000000", "10.200.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.AllocateIP("bb000002-0000-0000-0000-000000000000", "10.200.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if a1.BlockIdx == a2.BlockIdx {
		t.Error("deux allocations sur le même bloc")
	}
	if a1.HostIP != "10.200.0.1" {
		t.Errorf("hostIP bloc 0 = %q, attendu 10.200.0.1", a1.HostIP)
	}
	if a1.NSIP != "10.200.0.2" {
		t.Errorf("nsIP bloc 0 = %q, attendu 10.200.0.2", a1.NSIP)
	}
	if a2.HostIP != "10.200.0.5" {
		t.Errorf("hostIP bloc 1 = %q, attendu 10.200.0.5", a2.HostIP)
	}

	// Release et réallocation : doit récupérer le bloc 0
	if err := s.ReleaseIP("bb000001-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	s.CreateWorkspace(Workspace{ID: "bb000003-0000-0000-0000-000000000000", Name: "x",
		State: StateCreating, ImageID: "img1", CreatedAt: now, LastActivity: now})
	a3, err := s.AllocateIP("bb000003-0000-0000-0000-000000000000", "10.200.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if a3.BlockIdx != 0 {
		t.Errorf("réallocation après release : bloc %d, attendu 0", a3.BlockIdx)
	}
}

// TestIPAllocationSansWorkspace reproduit « wsctl golden build » : l'allocation
// d'un /30 temporaire pour le netns de construction, sans ligne dans workspaces.
// La contrainte FOREIGN KEY faisait échouer ce cas avec
// « constraint failed: FOREIGN KEY constraint failed (787) ».
func TestIPAllocationSansWorkspace(t *testing.T) {
	s := openTemp(t)

	tmpID := NewID()
	a, err := s.AllocateIP(tmpID, "10.200.0.0/16")
	if err != nil {
		t.Fatalf("allocation pour un id hors workspaces: %v", err)
	}
	if a.HostIP != "10.200.0.1" {
		t.Errorf("hostIP = %q, attendu 10.200.0.1", a.HostIP)
	}
	if err := s.ReleaseIP(tmpID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIPAllocation(tmpID); err == nil {
		t.Error("l'allocation devait être libérée")
	}
}

// TestMigrationIPAllocations ouvre une base à l'ancien schéma — ip_allocations
// avec la contrainte FOREIGN KEY — et vérifie que Open la migre sans perdre les
// allocations existantes, et qu'une allocation sans workspace passe ensuite.
func TestMigrationIPAllocations(t *testing.T) {
	f, err := os.CreateTemp("", "wsd-store-old-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	old := `
CREATE TABLE workspaces (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, owner TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL, image_id TEXT NOT NULL DEFAULT '', golden_id TEXT,
    pid INTEGER, created_at INTEGER NOT NULL, last_activity INTEGER NOT NULL);
CREATE TABLE ip_allocations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    block_idx INTEGER NOT NULL UNIQUE,
    host_ip TEXT NOT NULL, ns_ip TEXT NOT NULL);
CREATE TABLE goldens (
    id TEXT PRIMARY KEY, image_id TEXT NOT NULL, dir TEXT NOT NULL DEFAULT '',
    is_active INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE tokens (
    token TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    created_at INTEGER NOT NULL);
INSERT INTO workspaces VALUES
    ('cc000001-0000-0000-0000-000000000000','w1','',	'running','img1',NULL,0,1,1);
INSERT INTO ip_allocations (workspace_id,block_idx,host_ip,ns_ip) VALUES
    ('cc000001-0000-0000-0000-000000000000',0,'10.200.0.1','10.200.0.2');
`
	db, err := sql.Open("sqlite", f.Name()+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}

	// L'ancien schéma rejette bien une allocation sans workspace : c'est
	// l'erreur remontée par « wsctl golden build ».
	_, err = db.Exec(
		`INSERT INTO ip_allocations (workspace_id,block_idx,host_ip,ns_ip)
		 VALUES ('ff000099-0000-0000-0000-000000000000',9,'10.200.0.37','10.200.0.38')`)
	if err == nil {
		t.Fatal("l'ancien schéma aurait dû refuser l'allocation (FOREIGN KEY)")
	}
	t.Logf("ancien schéma, erreur attendue : %v", err)
	db.Close()

	s, err := Open(f.Name())
	if err != nil {
		t.Fatalf("Open sur une base à l'ancien schéma: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// L'allocation préexistante survit à la reconstruction de la table.
	a, err := s.GetIPAllocation("cc000001-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("allocation préexistante perdue: %v", err)
	}
	if a.BlockIdx != 0 || a.HostIP != "10.200.0.1" {
		t.Errorf("allocation altérée: %+v", a)
	}

	// Et le cas qui échouait fonctionne.
	if _, err := s.AllocateIP(NewID(), "10.200.0.0/16"); err != nil {
		t.Fatalf("allocation sans workspace après migration: %v", err)
	}

	// Rejouer Open ne doit pas rejouer la migration.
	s.Close()
	s2, err := Open(f.Name())
	if err != nil {
		t.Fatalf("seconde ouverture: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d, attendu %d", version, len(migrations))
	}
}
