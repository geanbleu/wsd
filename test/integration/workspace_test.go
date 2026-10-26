//go:build integration

// Tests d'intégration Phase 2 — nécessitent root, /dev/kvm, xfs+reflink, firecracker+jailer.
// Lancer avec : sudo make test-integration
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const wsdSocket = "/run/wsd/wsd.sock"

// client HTTP sur le socket unix de wsd
func wsdClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", wsdSocket)
			},
		},
		Timeout: 120 * time.Second,
	}
}

func TestCreateListDelete(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root requis")
	}
	if _, err := os.Stat(wsdSocket); err != nil {
		t.Skipf("wsd non démarré (%s absent)", wsdSocket)
	}
	c := wsdClient()

	// ── créer 3 workspaces ────────────────────────────────────────────────────
	var ids []string
	for i := 1; i <= 3; i++ {
		body := fmt.Sprintf(`{"name":"test-%d","owner":"integration"}`, i)
		resp, err := c.Post("http://wsd/v1/workspaces",
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("créer workspace %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("créer workspace %d: HTTP %d", i, resp.StatusCode)
		}
		var ws map[string]any
		json.NewDecoder(resp.Body).Decode(&ws)
		resp.Body.Close()
		id := ws["id"].(string)
		ids = append(ids, id)
		if ws["state"] != "running" {
			t.Errorf("workspace %s : état=%v, attendu running", id, ws["state"])
		}
		t.Logf("workspace %d créé : id=%s ns_ip=%v", i, id, ws["ns_ip"])
	}

	// ── joindre code-server via l'IP veth ────────────────────────────────────
	for _, id := range ids {
		resp, err := c.Get("http://wsd/v1/workspaces/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var ws map[string]any
		json.NewDecoder(resp.Body).Decode(&ws)
		resp.Body.Close()

		nsIP, _ := ws["ns_ip"].(string)
		if nsIP == "" {
			t.Errorf("workspace %s : ns_ip absent", id)
			continue
		}

		// Ping code-server depuis le netns du workspace
		id8 := id[:8]
		nsName := "ws-" + id8
		out, err := exec.Command("ip", "netns", "exec", nsName,
			"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", "5",
			fmt.Sprintf("http://%s:8080", nsIP)).Output()
		if err != nil {
			t.Errorf("code-server %s inaccessible: %v", id, err)
			continue
		}
		code := strings.TrimSpace(string(out))
		if code != "200" && code != "302" {
			t.Errorf("code-server %s : HTTP %s", id, code)
		} else {
			t.Logf("code-server %s : HTTP %s ✓", id, code)
		}
	}

	// ── isolement réseau ──────────────────────────────────────────────────────
	if len(ids) >= 2 {
		id1, id2 := ids[0], ids[1]
		resp1, _ := c.Get("http://wsd/v1/workspaces/" + id1)
		resp2, _ := c.Get("http://wsd/v1/workspaces/" + id2)
		var ws1, ws2 map[string]any
		json.NewDecoder(resp1.Body).Decode(&ws1)
		json.NewDecoder(resp2.Body).Decode(&ws2)
		resp1.Body.Close()
		resp2.Body.Close()

		ns1IP, _ := ws2["ns_ip"].(string) // IP du workspace 2, depuis le netns du workspace 1
		nsName1 := "ws-" + id1[:8]

		// Workspace 1 ne doit pas joindre le workspace 2
		err := exec.Command("ip", "netns", "exec", nsName1,
			"ping", "-c", "1", "-W", "2", ns1IP).Run()
		if err == nil {
			t.Errorf("isolement KO : workspace %s peut joindre %s", id1, ns1IP)
		} else {
			t.Logf("isolement OK : workspace %s ne peut pas joindre %s ✓", id1, ns1IP)
		}

		// Link-local doit être bloqué
		err = exec.Command("ip", "netns", "exec", nsName1,
			"ping", "-c", "1", "-W", "2", "169.254.169.254").Run()
		if err == nil {
			t.Errorf("isolement KO : workspace %s peut joindre 169.254.169.254", id1)
		} else {
			t.Logf("link-local bloqué ✓")
		}
	}

	// ── supprimer les workspaces ──────────────────────────────────────────────
	for _, id := range ids {
		req, _ := http.NewRequest("DELETE", "http://wsd/v1/workspaces/"+id, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Errorf("supprimer %s: %v", id, err)
			continue
		}
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
			t.Errorf("supprimer %s: HTTP %d", id, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Attendre que la suppression se termine (max 30 s)
	time.Sleep(5 * time.Second)

	// ── vérifier l'absence de résidus ─────────────────────────────────────────
	for _, id := range ids {
		id8 := id[:8]
		// Plus de netns
		out, _ := exec.Command("ip", "netns", "list").Output()
		if bytes.Contains(out, []byte("ws-"+id8)) {
			t.Errorf("netns ws-%s toujours présent après suppression", id8)
		}
		// Plus de veth
		out, _ = exec.Command("ip", "link", "show").Output()
		if bytes.Contains(out, []byte("vh-"+id8)) {
			t.Errorf("veth vh-%s toujours présent après suppression", id8)
		}
		// Workspace en état deleted
		resp, _ := c.Get("http://wsd/v1/workspaces/" + id)
		if resp != nil {
			var ws map[string]any
			json.NewDecoder(resp.Body).Decode(&ws)
			resp.Body.Close()
			if state, ok := ws["state"].(string); ok && state != "deleted" {
				t.Errorf("workspace %s : état=%s après suppression", id, state)
			}
		}
	}
	t.Log("Phase 2 — critères d'acceptation ✓")
}
