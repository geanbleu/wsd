package main

import (
	"flag"
	"testing"
)

// TestParseArgsOptionsApresPositionnel : flag.Parse s'arrête au premier
// argument non-flag. « create recette --owner alice » perdait donc l'option, et
// « rm <id> --keep-data » supprimait le disque malgré la demande inverse.
func TestParseArgsOptionsApresPositionnel(t *testing.T) {
	cas := []struct {
		args      []string
		wantOwner string
		wantPos   []string
	}{
		{[]string{"recette", "--owner", "alice@example.com"}, "alice@example.com", []string{"recette"}},
		{[]string{"--owner", "alice@example.com", "recette"}, "alice@example.com", []string{"recette"}},
		{[]string{"recette", "-owner", "alice@example.com"}, "alice@example.com", []string{"recette"}},
		{[]string{"recette"}, "", []string{"recette"}},
		{[]string{}, "", nil},
	}
	for _, c := range cas {
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		owner := fs.String("owner", "", "")
		pos, err := parseArgs(fs, c.args)
		if err != nil {
			t.Fatalf("%v : %v", c.args, err)
		}
		if *owner != c.wantOwner {
			t.Errorf("%v → owner %q, attendu %q", c.args, *owner, c.wantOwner)
		}
		if len(pos) != len(c.wantPos) {
			t.Fatalf("%v → positionnels %v, attendu %v", c.args, pos, c.wantPos)
		}
		for i := range pos {
			if pos[i] != c.wantPos[i] {
				t.Errorf("%v → positionnels %v, attendu %v", c.args, pos, c.wantPos)
			}
		}
	}
}

// TestParseArgsDrapeauBooleenApresPositionnel couvre le cas dangereux :
// --keep-data doit être vu, sinon le disque de l'utilisateur est détruit.
func TestParseArgsDrapeauBooleenApresPositionnel(t *testing.T) {
	for _, args := range [][]string{
		{"fae81881-3583-f5a9-99b9-61be49052456", "--keep-data"},
		{"--keep-data", "fae81881-3583-f5a9-99b9-61be49052456"},
	} {
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		keep := fs.Bool("keep-data", false, "")
		pos, err := parseArgs(fs, args)
		if err != nil {
			t.Fatalf("%v : %v", args, err)
		}
		if !*keep {
			t.Errorf("%v → keep-data non vu : le disque serait supprimé", args)
		}
		if len(pos) != 1 || pos[0] != "fae81881-3583-f5a9-99b9-61be49052456" {
			t.Errorf("%v → positionnels %v", args, pos)
		}
	}
}

// TestParseArgsOptionInconnue : une option inconnue doit remonter une erreur,
// pas être silencieusement rangée dans les positionnels.
func TestParseArgsOptionInconnue(t *testing.T) {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	fs.String("owner", "", "")
	if _, err := parseArgs(fs, []string{"recette", "--fictive", "x"}); err == nil {
		t.Error("une option inconnue doit être refusée")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
