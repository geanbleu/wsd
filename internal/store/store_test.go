package store

import (
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
