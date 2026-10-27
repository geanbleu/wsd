package config

import "testing"

// TestWorkspaceURL : le lien de wsctl token était écrit en dur en « http:// »
// sans port, donc inutilisable derrière un terminateur TLS comme derrière un
// tunnel SSH sur un port non standard.
func TestWorkspaceURL(t *testing.T) {
	const id = "fae81881-3583-f5a9-99b9-61be49052456"
	cas := []struct {
		nom    string
		scheme string
		port   int
		domain string
		want   string
	}{
		{"défaut", "", 0, "example.internal",
			"http://" + id + ".ws.example.internal"},
		{"tunnel ssh sur 8080", "http", 8080, "localhost",
			"http://" + id + ".ws.localhost:8080"},
		{"derrière un terminateur TLS", "https", 0, "example.internal",
			"https://" + id + ".ws.example.internal"},
		{"port par défaut du schéma omis", "https", 443, "example.internal",
			"https://" + id + ".ws.example.internal"},
		{"http sur 80 omis", "http", 80, "example.internal",
			"http://" + id + ".ws.example.internal"},
		{"https sur un port exotique", "https", 8443, "example.internal",
			"https://" + id + ".ws.example.internal:8443"},
	}
	for _, c := range cas {
		cfg := &Config{Domain: c.domain, ExternalScheme: c.scheme, ExternalPort: c.port}
		cfg.applyDefaults()
		if got := cfg.WorkspaceURL(id); got != c.want {
			t.Errorf("%s : %q, attendu %q", c.nom, got, c.want)
		}
	}
}

// TestExternalSchemeDefaut vérifie que l'absence du champ garde le
// comportement antérieur.
func TestExternalSchemeDefaut(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	if cfg.ExternalScheme != "http" {
		t.Errorf("external_scheme par défaut = %q, attendu http", cfg.ExternalScheme)
	}
}
