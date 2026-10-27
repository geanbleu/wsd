package netns

import (
	"bytes"
	"strings"
	"testing"
)

func render(t *testing.T, p nftParams) string {
	t.Helper()
	var buf bytes.Buffer
	if err := nftTmpl.Execute(&buf, p); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// statements renvoie les instructions du script, commentaires et lignes vides
// retirés, pour raisonner sur l'ordre des directives de tête.
func statements(script string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestNftDeclareAvantFlush : « flush table » sur une table absente fait échouer
// tout le script, et le netns d'un workspace neuf n'en a aucune. C'est l'erreur
// « netns setup: nft apply: exit status 1 » rencontrée à la création.
func TestNftDeclareAvantFlush(t *testing.T) {
	script := render(t, nftParams{ID: "a1b2c3d4", VethNS: "vn-a1b2c3d4", DNS: "1.1.1.1"})
	st := statements(script)
	if len(st) < 2 {
		t.Fatalf("script trop court : %q", script)
	}
	if st[0] != "table ip wsd-a1b2c3d4 { }" {
		t.Errorf("première instruction = %q, attendu la déclaration de la table vide", st[0])
	}
	if st[1] != "flush table ip wsd-a1b2c3d4" {
		t.Errorf("deuxième instruction = %q, attendu le flush", st[1])
	}
}

// TestNftReglesEgress vérifie la forme des règles générées : une règle
// attrape-tout ne doit pas produire de « ip daddr 0.0.0.0/0 », et un CIDR
// précis doit bien apparaître.
func TestNftReglesEgress(t *testing.T) {
	script := render(t, nftParams{
		ID: "a1b2c3d4", VethNS: "vn-a1b2c3d4", DNS: "1.1.1.1",
		EgressRules: []EgressRule{
			{CIDR: "0.0.0.0/0", Ports: []int{443}},
			{CIDR: "192.0.2.10/32", Ports: []int{8080}},
		},
	})
	for _, want := range []string{
		"tcp dport 443 accept",
		"ip daddr 192.0.2.10/32 tcp dport 8080 accept",
		"ip daddr 1.1.1.1 udp dport 53 accept",
		"ip daddr 169.254.0.0/16 drop",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("règle absente : %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "ip daddr 0.0.0.0/0") {
		t.Error("une règle attrape-tout ne doit pas être restreinte par ip daddr")
	}
	// La politique par défaut doit rester le drop final de vm_egress.
	st := statements(script)
	if st[len(st)-1] != "}" || st[len(st)-2] != "}" || st[len(st)-3] != "drop" {
		t.Errorf("la chaîne vm_egress doit finir par un drop, fin du script : %q", st[len(st)-3:])
	}
}

// TestApplyNftRefuseDNSVide : sans résolveur, le template produirait
// « ip daddr  udp dport 53 accept », que nft rejette. L'erreur doit être
// explicite et arriver avant l'appel à nft.
func TestApplyNftRefuseDNSVide(t *testing.T) {
	err := applyNft("a1b2c3d4", "vn-a1b2c3d4", "", nil)
	if err == nil {
		t.Fatal("un net.dns vide doit être refusé")
	}
	if !strings.Contains(err.Error(), "net.dns") {
		t.Errorf("erreur peu explicite : %v", err)
	}
}

func TestCleanOutput(t *testing.T) {
	in := "/dev/stdin:1:16-27: Error: No such file or directory\n" +
		"flush table ip wsd-x\n" +
		"               ^^^^^^^^^^^^\n\n"
	got := cleanOutput(in)
	want := "/dev/stdin:1:16-27: Error: No such file or directory | flush table ip wsd-x"
	if got != want {
		t.Errorf("cleanOutput = %q, attendu %q", got, want)
	}
}
