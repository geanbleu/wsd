// Package netns manages per-workspace network namespaces, veth pairs, tap devices and nftables rules.
// All network operations use os/exec (ip, nft) as required by SPEC §6.
package netns

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
)

// NSName returns the netns name for a workspace id8.
func NSName(id8 string) string { return "ws-" + id8 }

// VethHost returns the host-side veth name for a workspace id8.
func VethHost(id8 string) string { return "vh-" + id8 }

// VethNS returns the netns-side veth name for a workspace id8.
func VethNS(id8 string) string { return "vn-" + id8 }

// Setup creates the full network stack for a workspace:
// netns, veth pair, tap0 inside netns, static routes, nftables rules.
func Setup(id8, hostIP, nsIP, egressIface, dns string, egress []EgressRule) error {
	ns := NSName(id8)
	vh := VethHost(id8)
	vn := VethNS(id8)

	steps := []func() error{
		func() error { return ip("netns", "add", ns) },
		func() error {
			return ip("link", "add", vh, "type", "veth", "peer", "name", vn)
		},
		func() error { return ip("link", "set", vn, "netns", ns) },
		func() error { return ip("addr", "add", hostIP+"/30", "dev", vh) },
		func() error { return ip("link", "set", vh, "up") },
		func() error { return ipns(ns, "addr", "add", nsIP+"/30", "dev", vn) },
		func() error { return ipns(ns, "link", "set", vn, "up") },
		func() error { return ipns(ns, "tuntap", "add", "name", "tap0", "mode", "tap") },
		func() error { return ipns(ns, "addr", "add", "172.16.0.1/30", "dev", "tap0") },
		func() error { return ipns(ns, "link", "set", "tap0", "up") },
		func() error { return ipns(ns, "link", "set", "lo", "up") },
		func() error { return ipns(ns, "route", "add", "default", "via", hostIP) },
		// Le forwarding est un sysctl par netns, et un netns neuf le recopie
		// depuis le netns initial du host, pas depuis son créateur. Il est donc
		// réglé explicitement ici : sans lui, la chaîne forward ne voit rien
		// passer et la VM n'a aucun accès réseau, sans erreur visible.
		func() error { return setNetnsForwarding(ns) },
		func() error {
			return applyNft(id8, vn, dns, egress)
		},
	}

	for _, step := range steps {
		if err := step(); err != nil {
			// Best-effort cleanup on error
			Teardown(id8, egressIface)
			return err
		}
	}
	return nil
}

// Teardown removes the netns and veth pair for a workspace.
// Deleting the netns removes all interfaces inside it automatically.
func Teardown(id8, egressIface string) error {
	ns := NSName(id8)
	vh := VethHost(id8)

	// Flush nft table (best-effort, may fail if it was never created)
	nft(fmt.Sprintf("delete table ip wsd-%s", id8))

	// Delete veth on host (the peer inside the netns is deleted automatically)
	ip("link", "del", vh)

	// Delete netns
	if err := ip("netns", "del", ns); err != nil {
		return fmt.Errorf("netns del %s: %w", ns, err)
	}
	return nil
}

// SetupHostMasquerade installs a one-time masquerade rule on the host for
// the 10.200.0.0/16 pool egressing through egressIface.
// Must be called once at wsd startup. Idempotent (nft handles duplicates).
// NOTE: modifie les règles nft du host — ne pas appeler hors tests d'intégration
// sans confirmation de l'opérateur.
func SetupHostMasquerade(pool, egressIface string) error {
	script := fmt.Sprintf(`
table ip wsd-host {
  chain postrouting {
    type nat hook postrouting priority srcnat;
    ip saddr %s oif %s masquerade
  }
}`, pool, egressIface)
	return nft(script)
}

// TeardownHostMasquerade removes the host masquerade table.
func TeardownHostMasquerade() error {
	return nft("delete table ip wsd-host")
}

// ── nftables ──────────────────────────────────────────────────────────────────

// EgressRule is one entry in the egress allow list.
type EgressRule struct {
	CIDR  string
	Ports []int
}

var nftTmpl = template.Must(template.New("nft").Funcs(template.FuncMap{
	"isCatchAll": func(cidr string) bool { return cidr == "" || cidr == "0.0.0.0/0" },
}).Parse(`
# La table est déclarée vide avant d'être vidée : « flush » sur une table
# absente fait échouer tout le script (« Error: No such file or directory »),
# et le netns est neuf à la première application. Déclarer puis vider rend le
# script rejouable, ce dont un resume a besoin.
table ip wsd-{{.ID}} { }
flush table ip wsd-{{.ID}}

table ip wsd-{{.ID}} {
    chain prerouting {
        type nat hook prerouting priority dstnat;
        iif {{.VethNS}} tcp dport 8080 dnat to 172.16.0.2
    }

    chain postrouting {
        type nat hook postrouting priority srcnat;
        ip saddr 172.16.0.2 oif {{.VethNS}} masquerade
    }

    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        iif {{.VethNS}} oif tap0 ct state new accept
        iif tap0 oif {{.VethNS}} jump vm_egress
    }

    chain vm_egress {
        ip daddr 169.254.0.0/16 drop
        ip daddr 10.200.0.0/16 drop
        ip daddr 100.64.0.0/10 drop
        ip daddr {{.DNS}} udp dport 53 accept
        ip daddr {{.DNS}} tcp dport 53 accept
{{- range .EgressRules}}
{{- $cidr := .CIDR}}
{{- range .Ports}}
        {{if not (isCatchAll $cidr)}}ip daddr {{$cidr}} {{end}}tcp dport {{.}} accept
        {{if not (isCatchAll $cidr)}}ip daddr {{$cidr}} {{end}}udp dport {{.}} accept
{{- end}}
{{- end}}
        drop
    }
}
`))

type nftParams struct {
	ID          string
	VethNS      string
	DNS         string
	EgressRules []EgressRule
}

func applyNft(id8, vethNS, dns string, egress []EgressRule) error {
	if dns == "" {
		return fmt.Errorf("nft: net.dns est vide, les règles DNS seraient invalides")
	}
	var buf bytes.Buffer
	if err := nftTmpl.Execute(&buf, nftParams{
		ID:          id8,
		VethNS:      vethNS,
		DNS:         dns,
		EgressRules: egress,
	}); err != nil {
		return fmt.Errorf("nft template: %w", err)
	}
	// Run nft inside the netns
	cmd := exec.Command("ip", "netns", "exec", NSName(id8), "nft", "-f", "-")
	cmd.Stdin = &buf
	return run(cmd, "nft apply")
}

// setNetnsForwarding active le routage IPv4 à l'intérieur du netns.
func setNetnsForwarding(ns string) error {
	cmd := exec.Command("ip", "netns", "exec", ns,
		"sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
	return run(cmd, "activation du forwarding dans "+ns)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// run exécute cmd en rapportant sa sortie d'erreur dans l'erreur retournée.
// Sans cela, un échec de ip ou de nft ne remonte qu'un « exit status 1 »
// jusqu'à l'API, et la vraie cause reste dans le journal du daemon.
func run(cmd *exec.Cmd, what string) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := cleanOutput(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", what, err, msg)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// cleanOutput met la sortie d'erreur sur une seule ligne lisible : les lignes
// de soulignement « ^^^^ » de nft n'ont aucun sens hors de leur contexte.
func cleanOutput(out string) string {
	var parts []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Trim(line, "^") == "" {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " | ")
}

func ip(args ...string) error {
	return run(exec.Command("ip", args...), "ip "+strings.Join(args, " "))
}

func ipns(ns string, args ...string) error {
	a := append([]string{"netns", "exec", ns, "ip"}, args...)
	return ip(a...)
}

func nft(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = bytes.NewBufferString(script)
	return run(cmd, "nft")
}
