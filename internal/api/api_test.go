package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestWaitCodeServerPret : code-server prêt doit être reconnu, et le sondage
// porter sur /healthz, que code-server exclut de son heartbeat.
func TestWaitCodeServerPret(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				t.Errorf("chemin sondé = %q, attendu /healthz", r.URL.Path)
			}
			w.Write([]byte(`{"status":"alive"}`))
		}))
	srv.Start()
	defer srv.Close()

	if err := waitCodeServer(context.Background(), srv.Listener.Addr().String(), 3*time.Second); err != nil {
		t.Fatalf("code-server prêt mais refusé : %v", err)
	}
}

// Une version sans /healthz répond 404 : le serveur répond, donc il est prêt.
func TestWaitCodeServer404EstPret(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	srv.Start()
	defer srv.Close()

	if err := waitCodeServer(context.Background(), srv.Listener.Addr().String(), 3*time.Second); err != nil {
		t.Fatalf("un 404 prouve que le serveur répond : %v", err)
	}
}

// TestWaitCodeServerInjoignable : l'erreur doit nommer l'adresse sondée et la
// cause réseau, pas seulement « non disponible ». C'est ce qui manquait pour
// distinguer une VM muette d'un DNAT absent.
func TestWaitCodeServerInjoignable(t *testing.T) {
	// 192.0.2.0/24 est réservé à la documentation : jamais routé.
	err := waitCodeServer(context.Background(), "192.0.2.1:8080", 300*time.Millisecond)
	if err == nil {
		t.Fatal("une adresse injoignable doit échouer")
	}
	if !strings.Contains(err.Error(), "192.0.2.1:8080") {
		t.Errorf("l'erreur doit nommer l'adresse sondée : %v", err)
	}
}

// TestWaitCodeServerContexteAnnule : l'attente doit rendre la main dès que le
// contexte est annulé, sans consommer les 30 s.
func TestWaitCodeServerContexteAnnule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitCodeServer(ctx, "192.0.2.1:8080", 30*time.Second); err == nil {
		t.Fatal("un contexte annulé doit interrompre l'attente")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("attente de %s après annulation du contexte", elapsed)
	}
}

func TestUpstreamFor(t *testing.T) {
	if got := upstreamFor("10.200.0.6"); got != "10.200.0.6:8080" {
		t.Errorf("upstreamFor = %q, attendu 10.200.0.6:8080", got)
	}
}
