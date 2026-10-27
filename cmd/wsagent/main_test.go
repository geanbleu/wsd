package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"wsd/internal/agentproto"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestServeConnPing : le host attend une réponse au ping, pas une fermeture.
// Un agent qui ferme sans répondre donne côté host
// « agent non disponible après 60 s (dernière erreur : ping recv: EOF) ».
func TestServeConnPing(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()

	go func() {
		serveConn(discardLogger(), guest)
		guest.Close()
	}()

	host.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(host).Encode(agentproto.Req{ID: "1", Op: agentproto.OpPing}); err != nil {
		t.Fatal(err)
	}
	var resp agentproto.PingResp
	if err := json.NewDecoder(host).Decode(&resp); err != nil {
		t.Fatalf("réponse au ping : %v", err)
	}
	if !resp.OK || !resp.Ready {
		t.Errorf("ping → %+v, attendu ok et ready", resp)
	}
	if resp.ID != "1" {
		t.Errorf("id de corrélation = %q, attendu \"1\"", resp.ID)
	}
}

// TestServeConnOpInconnue : une op non reconnue doit donner une erreur
// applicative, pas une connexion fermée.
func TestServeConnOpInconnue(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	go func() {
		serveConn(discardLogger(), guest)
		guest.Close()
	}()

	host.SetDeadline(time.Now().Add(5 * time.Second))
	json.NewEncoder(host).Encode(agentproto.Req{ID: "2", Op: "fictive"})
	var resp agentproto.OKResp
	if err := json.NewDecoder(host).Decode(&resp); err != nil {
		t.Fatalf("réponse : %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "op inconnue") {
		t.Errorf("op inconnue → %+v", resp)
	}
}

// TestServeConnPlusieursRequetes : la connexion est réutilisée, le host
// enchaîne des requêtes dessus.
func TestServeConnPlusieursRequetes(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	go func() {
		serveConn(discardLogger(), guest)
		guest.Close()
	}()

	host.SetDeadline(time.Now().Add(5 * time.Second))
	enc := json.NewEncoder(host)
	dec := json.NewDecoder(host)
	for i := 0; i < 3; i++ {
		if err := enc.Encode(agentproto.Req{Op: agentproto.OpPing}); err != nil {
			t.Fatal(err)
		}
		var resp agentproto.PingResp
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("requête %d : %v", i, err)
		}
		if !resp.OK {
			t.Fatalf("requête %d refusée", i)
		}
	}
}

// TestVsockFdEnveloppe verrouille la raison pour laquelle handleConn n'utilise
// pas net.FileConn : celui-ci rejette la famille AF_VSOCK, alors qu'os.NewFile
// l'accepte. C'est la cause de l'EOF vu par le host sur chaque requête.
func TestVsockFdEnveloppe(t *testing.T) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("AF_VSOCK indisponible : %v", err)
	}

	dup, err := unix.Dup(fd)
	if err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}

	f := os.NewFile(uintptr(fd), "vsock")
	defer f.Close()
	if _, err := net.FileConn(f); err == nil {
		t.Error("net.FileConn accepte désormais AF_VSOCK : handleConn peut être simplifié")
	}

	g := os.NewFile(uintptr(dup), "vsock")
	defer g.Close()
	if g.Fd() == ^uintptr(0) {
		t.Error("os.NewFile doit accepter un descripteur AF_VSOCK")
	}
}
