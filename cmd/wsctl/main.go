package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"wsd/internal/version"
)

const defaultSocket = "/run/wsd/wsd.sock"

func main() {
	socket := flag.String("socket", defaultSocket, "socket unix de wsd")
	showVersion := flag.Bool("version", false, "afficher la version et quitter")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: wsctl [-socket <path>] <commande> [args]\n\n")
		fmt.Fprintf(os.Stderr, "Commandes:\n")
		fmt.Fprintf(os.Stderr, "  create <nom> [--owner <owner>]  Créer un workspace\n")
		fmt.Fprintf(os.Stderr, "  ls [--owner <owner>]             Lister les workspaces\n")
		fmt.Fprintf(os.Stderr, "  rm <id> [--keep-data]            Supprimer un workspace\n")
		fmt.Fprintf(os.Stderr, "  get <id>                         Afficher un workspace\n")
		fmt.Fprintf(os.Stderr, "  golden build                     Construire le golden snapshot\n")
		fmt.Fprintf(os.Stderr, "  golden get                       Afficher le golden actif\n")
		fmt.Fprintf(os.Stderr, "  suspend <id>                     Suspendre un workspace\n")
		fmt.Fprintf(os.Stderr, "  resume <id>                      Réveiller un workspace\n")
		fmt.Fprintf(os.Stderr, "  token <id>                       Générer un lien d'accès à usage unique\n")
		fmt.Fprintf(os.Stderr, "  top                              Afficher les VMs avec RSS et activité\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("wsctl"))
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	client := newClient(*socket)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var err error
	switch args[0] {
	case "create":
		err = cmdCreate(ctx, client, args[1:])
	case "ls":
		err = cmdList(ctx, client, args[1:])
	case "rm":
		err = cmdRM(ctx, client, args[1:])
	case "get":
		err = cmdGet(ctx, client, args[1:])
	case "suspend":
		err = cmdSuspend(ctx, client, args[1:])
	case "resume":
		err = cmdResume(ctx, client, args[1:])
	case "top":
		err = cmdTop(ctx, client, args[1:])
	case "golden":
		err = cmdGolden(ctx, client, args[1:])
	case "token":
		err = cmdToken(ctx, client, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue: %s\n", args[0])
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "erreur: %v\n", err)
		os.Exit(1)
	}
}

// parseArgs analyse les options d'une sous-commande où qu'elles soient, et
// renvoie les arguments positionnels.
//
// flag.FlagSet.Parse s'arrête au premier argument non-flag : avec
// « create recette --owner alice », l'option était purement ignorée, et avec
// « rm <id> --keep-data » le disque de l'utilisateur était supprimé malgré la
// demande explicite de le garder. L'usage documenté place les options après le
// positionnel, on parse donc par passes successives.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func cmdCreate(ctx context.Context, c *http.Client, args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	owner := fs.String("owner", "", "propriétaire du workspace")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("nom requis")
	}
	name := rest[0]

	body := fmt.Sprintf(`{"name":%q,"owner":%q}`, name, *owner)
	var result map[string]any
	if err := call(ctx, c, "POST", "/v1/workspaces", body, &result); err != nil {
		return err
	}
	printJSON(result)
	return nil
}

func cmdList(ctx context.Context, c *http.Client, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	owner := fs.String("owner", "", "filtrer par owner")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	url := "/v1/workspaces"
	if *owner != "" {
		url += "?owner=" + *owner
	}
	var result []map[string]any
	if err := call(ctx, c, "GET", url, "", &result); err != nil {
		return err
	}
	if len(result) == 0 {
		fmt.Println("aucun workspace")
		return nil
	}
	fmt.Printf("%-36s  %-20s  %-12s  %s\n", "ID", "NOM", "ÉTAT", "OWNER")
	for _, ws := range result {
		fmt.Printf("%-36s  %-20s  %-12s  %s\n",
			strVal(ws, "id"), strVal(ws, "name"),
			strVal(ws, "state"), strVal(ws, "owner"))
	}
	return nil
}

func cmdRM(ctx context.Context, c *http.Client, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	keepData := fs.Bool("keep-data", false, "garder workspace.ext4 après suppression")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("id requis")
	}
	path := "/v1/workspaces/" + rest[0]
	if *keepData {
		path += "?keep_data=1"
	}
	return call(ctx, c, "DELETE", path, "", nil)
}

func cmdGet(ctx context.Context, c *http.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("id requis")
	}
	var result map[string]any
	if err := call(ctx, c, "GET", "/v1/workspaces/"+args[0], "", &result); err != nil {
		return err
	}
	printJSON(result)
	return nil
}

func cmdToken(ctx context.Context, c *http.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("id requis")
	}
	var result map[string]any
	if err := call(ctx, c, "POST", "/v1/workspaces/"+args[0]+"/token", "", &result); err != nil {
		return err
	}
	if url, ok := result["url"].(string); ok {
		fmt.Println(url)
	} else {
		printJSON(result)
	}
	return nil
}

func cmdSuspend(ctx context.Context, c *http.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("id requis")
	}
	var result map[string]any
	if err := call(ctx, c, "POST", "/v1/workspaces/"+args[0]+"/suspend", "", &result); err != nil {
		return err
	}
	printJSON(result)
	return nil
}

func cmdResume(ctx context.Context, c *http.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("id requis")
	}
	var result map[string]any
	if err := call(ctx, c, "POST", "/v1/workspaces/"+args[0]+"/resume", "", &result); err != nil {
		return err
	}
	printJSON(result)
	return nil
}

func cmdTop(ctx context.Context, c *http.Client, _ []string) error {
	var result []map[string]any
	if err := call(ctx, c, "GET", "/v1/top", "", &result); err != nil {
		return err
	}
	if len(result) == 0 {
		fmt.Println("aucun workspace")
		return nil
	}
	fmt.Printf("%-36s  %-20s  %-12s  %8s  %8s  %s\n",
		"ID", "NOM", "ÉTAT", "RSS(MiB)", "PSS(MiB)", "DERNIÈRE ACTIVITÉ")
	for _, e := range result {
		rss := int64Val(e, "rss_kib") / 1024
		pss := int64Val(e, "pss_kib") / 1024
		la := int64Val(e, "last_activity")
		var ago string
		if la > 0 {
			ago = time.Since(time.Unix(la, 0)).Truncate(time.Second).String()
		}
		fmt.Printf("%-36s  %-20s  %-12s  %8d  %8d  il y a %s\n",
			strVal(e, "id"), strVal(e, "name"), strVal(e, "state"), rss, pss, ago)
	}
	return nil
}

func int64Val(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		}
	}
	return 0
}

func cmdGolden(ctx context.Context, c *http.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sous-commande requise: build | get")
	}
	switch args[0] {
	case "build":
		var result map[string]any
		if err := call(ctx, c, "POST", "/v1/golden", "", &result); err != nil {
			return err
		}
		printJSON(result)
		return nil
	case "get":
		var result map[string]any
		if err := call(ctx, c, "GET", "/v1/golden", "", &result); err != nil {
			return err
		}
		printJSON(result)
		return nil
	default:
		return fmt.Errorf("sous-commande inconnue: %s (build | get)", args[0])
	}
}

// ── HTTP client sur unix socket ───────────────────────────────────────────────

func newClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func call(ctx context.Context, c *http.Client, method, path, body string, out any) error {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	var req *http.Request
	var err error
	if bodyReader != nil {
		req, err = http.NewRequestWithContext(ctx, method, "http://wsd"+path, bodyReader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, "http://wsd"+path, nil)
	}
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("appel API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e["error"])
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}
