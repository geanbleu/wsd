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
)

const defaultSocket = "/run/wsd/wsd.sock"

func main() {
	socket := flag.String("socket", defaultSocket, "socket unix de wsd")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: wsctl [-socket <path>] <commande> [args]\n\n")
		fmt.Fprintf(os.Stderr, "Commandes:\n")
		fmt.Fprintf(os.Stderr, "  create <nom> [--owner <owner>]  Créer un workspace\n")
		fmt.Fprintf(os.Stderr, "  ls [--owner <owner>]             Lister les workspaces\n")
		fmt.Fprintf(os.Stderr, "  rm <id>                          Supprimer un workspace\n")
		fmt.Fprintf(os.Stderr, "  get <id>                         Afficher un workspace\n")
		fmt.Fprintf(os.Stderr, "  golden build                     Construire le golden snapshot\n")
		fmt.Fprintf(os.Stderr, "  golden get                       Afficher le golden actif\n")
		flag.PrintDefaults()
	}
	flag.Parse()

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
	case "golden":
		err = cmdGolden(ctx, client, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue: %s\n", args[0])
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "erreur: %v\n", err)
		os.Exit(1)
	}
}

func cmdCreate(ctx context.Context, c *http.Client, args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	owner := fs.String("owner", "", "propriétaire du workspace")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("nom requis")
	}
	name := fs.Arg(0)

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
	fs.Parse(args)

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
	if len(args) == 0 {
		return fmt.Errorf("id requis")
	}
	return call(ctx, c, "DELETE", "/v1/workspaces/"+args[0], "", nil)
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
