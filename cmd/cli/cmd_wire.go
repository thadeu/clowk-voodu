// cmd_wire.go is `vd wire`: how two voodu hosts are wired together over
// WireGuard without anyone editing wg0.conf.
//
//	vd wire show                 # this host, as a ready-to-paste `add` line
//	vd wire add --key … --address … [--endpoint …]
//	vd wire remove <address>
//	vd wire list                 # peers and their link health
//	vd wire ufw [enable|disable] # the host firewall rules the mesh needs
//
// Wiring two hosts is `show` on one, `add` on the other, then the same
// the other way round. Every verb runs against the controller of the host
// it targets, so `-r` does it from anywhere.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type wirePeer struct {
	PublicKey string `json:"public_key"`
	Address   string `json:"address"`
	Endpoint  string `json:"endpoint,omitempty"`
	AddedAt   string `json:"added_at,omitempty"`

	Applied bool `json:"applied"`
	Link    *struct {
		Endpoint      string `json:"endpoint,omitempty"`
		LastHandshake string `json:"last_handshake,omitempty"`
		RxBytes       int64  `json:"rx_bytes"`
		TxBytes       int64  `json:"tx_bytes"`
	} `json:"link,omitempty"`
}

type wireIdentity struct {
	PublicKey  string `json:"public_key"`
	Address    string `json:"address"`
	ListenPort int    `json:"listen_port"`
	Endpoint   string `json:"endpoint,omitempty"`
}

type wireStatus struct {
	Identity wireIdentity `json:"identity"`
	Peers    []wirePeer   `json:"peers"`
}

func newWireCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wire",
		Short: "Wire this host to other voodu hosts over WireGuard",
		Long: `wire manages the WireGuard peers of this host — the other voodu hosts
whose containers this one can reach by name.

Wiring two hosts, from anywhere:

  vd wire show -r vm-1          # prints the add line for vm-1
  vd wire add  -r vm-2 --key … --endpoint … --address …
  vd wire show -r vm-2
  vd wire add  -r vm-1 --key … --endpoint … --address …

Peers are applied to wg0 at once, without dropping the tunnel, and come
back after a reboot. wg0.conf is never edited. Open UDP 51820 on both.`,
	}

	cmd.AddCommand(newWireShowCmd(), newWireAddCmd(), newWireRemoveCmd(), newWireListCmd(), newWireUFWCmd())

	return cmd
}

func newWireShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print this host as an `add` line for another host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := wireFetch(cmd)
			if err != nil {
				return err
			}

			if outputIsJSON(cmd) {
				return json.NewEncoder(os.Stdout).Encode(st.Identity)
			}

			fmt.Fprintln(os.Stdout, wireAddLine(st.Identity))

			return nil
		},
	}
}

// wireAddLine is the command the other host runs. The endpoint is the
// outbound IP; a host behind NAT needs it replaced.
func wireAddLine(id wireIdentity) string {
	line := fmt.Sprintf("vd wire add --key %s --address %s", id.PublicKey, id.Address)

	if id.Endpoint != "" {
		line += " --endpoint " + id.Endpoint
	}

	return line
}

func newWireAddCmd() *cobra.Command {
	var p wirePeer

	cmd := &cobra.Command{
		Use:   "add --key <public key> --address <10.254.X.Y> [--endpoint <host:port>]",
		Short: "Add a peer, as printed by `vd wire show` on it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, _ := json.Marshal(p)

			var out struct {
				Peer wirePeer `json:"peer"`
			}

			if err := wireCall(cmd, http.MethodPost, "/wire/peers", bytes.NewReader(body), &out); err != nil {
				return err
			}

			if outputIsJSON(cmd) {
				return json.NewEncoder(os.Stdout).Encode(out.Peer)
			}

			fmt.Fprintf(os.Stdout, "%s peer %s added to wg0\n", check(), out.Peer.Address)

			return nil
		},
	}

	cmd.Flags().StringVar(&p.PublicKey, "key", "", "the peer's WireGuard public key")
	cmd.Flags().StringVar(&p.Address, "address", "", "the peer's tunnel address (10.254.X.Y)")
	cmd.Flags().StringVar(&p.Endpoint, "endpoint", "", "where the peer listens, host:port (omit for a peer behind NAT)")
	_ = cmd.MarkFlagRequired("key")
	_ = cmd.MarkFlagRequired("address")

	return cmd
}

func newWireRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <address>",
		Short: "Remove a peer by tunnel address",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := wireCall(cmd, http.MethodDelete, "/wire/peers/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}

			fmt.Fprintf(os.Stdout, "%s peer %s removed from wg0\n", check(), args[0])

			return nil
		},
	}
}

func newWireListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List peers and their link health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := wireFetch(cmd)
			if err != nil {
				return err
			}

			if outputIsJSON(cmd) {
				return json.NewEncoder(os.Stdout).Encode(st)
			}

			renderWireList(os.Stdout, st, time.Now())

			return nil
		},
	}
}

func renderWireList(out io.Writer, st wireStatus, now time.Time) {
	fmt.Fprintf(out, "this host: %s  key %s\n\n", st.Identity.Address, abbrevKey(st.Identity.PublicKey))

	if len(st.Peers) == 0 {
		fmt.Fprintln(out, "No peers. Run `vd wire show` on another host and paste its line here.")

		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ADDRESS\tENDPOINT\tHANDSHAKE\tRX/TX\tKEY")

	for _, p := range st.Peers {
		endpoint := p.Endpoint

		if p.Link != nil && p.Link.Endpoint != "" {
			endpoint = p.Link.Endpoint
		}

		if endpoint == "" {
			endpoint = "-"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Address, endpoint, wireHandshake(p, now), wireBytes(p), abbrevKey(p.PublicKey))
	}

	tw.Flush()
}

// wireHandshake is the health column: how long since the peer last
// answered, "never" for a peer that has not, and "not applied" when wg0
// does not carry the peer at all.
func wireHandshake(p wirePeer, now time.Time) string {
	if !p.Applied || p.Link == nil {
		return "not applied"
	}

	if p.Link.LastHandshake == "" {
		return "never"
	}

	at, err := time.Parse(time.RFC3339, p.Link.LastHandshake)
	if err != nil || at.IsZero() {
		return "never"
	}

	return humanAgo(now.Sub(at)) + " ago"
}

func wireBytes(p wirePeer) string {
	if p.Link == nil {
		return "-"
	}

	return humanBytes(p.Link.RxBytes) + "/" + humanBytes(p.Link.TxBytes)
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%dB", n)
	}

	div, exp := int64(unit), 0

	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func abbrevKey(key string) string {
	if len(key) <= 12 {
		return key
	}

	return key[:8] + "…"
}

func outputIsJSON(cmd *cobra.Command) bool {
	format, _ := cmd.Root().PersistentFlags().GetString("output")

	return format == "json"
}

func wireFetch(cmd *cobra.Command) (wireStatus, error) {
	var st wireStatus

	err := wireCall(cmd, http.MethodGet, "/wire", nil, &st)

	return st, err
}

// wireCall is one request to the controller, its envelope decoded into
// out when given.
func wireCall(cmd *cobra.Command, method, path string, body io.Reader, out any) error {
	resp, err := controllerDo(cmd.Root(), method, path, "", body)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env patEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return nil
}

type wireUFW struct {
	Bridge string     `json:"bridge"`
	Rules  [][]string `json:"rules"`
}

func newWireUFWCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ufw [enable|disable]",
		Short: "Print, apply or remove the ufw rules the mesh needs on this host",
		Long: `ufw prints the host firewall rules cross-VM networking needs — the
tunnel port, DNS from the other hosts and from this host's containers, and
forwarding from wg0 into voodu0 — ready to paste.

  vd wire ufw                 # print (works with -r)
  sudo vd wire ufw enable     # apply, on the host itself
  sudo vd wire ufw disable    # remove the same rules

enable and disable run ufw here, so they need root on the host: the
controller only computes the rules, it cannot write the firewall.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"enable", "disable"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var u wireUFW

			if err := wireCall(cmd, http.MethodGet, "/wire/ufw", nil, &u); err != nil {
				return err
			}

			verb := ""
			if len(args) == 1 {
				verb = args[0]
			}

			if verb == "" {
				if outputIsJSON(cmd) {
					return json.NewEncoder(os.Stdout).Encode(u)
				}

				fmt.Fprint(os.Stdout, renderUFWRules(u, "sudo "))

				return nil
			}

			if verb != "enable" && verb != "disable" {
				return fmt.Errorf("ufw: want enable or disable, got %q", verb)
			}

			return applyUFW(os.Stdout, u, verb == "disable", os.Geteuid(), ufwRunner)
		},
	}
}

// renderUFWRules is the rules as shell lines. prefix is what goes before
// `ufw` — "sudo " when printing for a human to paste.
func renderUFWRules(u wireUFW, prefix string) string {
	var b strings.Builder

	for _, r := range u.Rules {
		fmt.Fprintf(&b, "%sufw %s\n", prefix, strings.Join(r, " "))
	}

	return b.String()
}

// deleteArgs turns a rule into the ufw arguments that remove it: `delete`
// goes after `route` when there is one, before the rule otherwise.
func deleteArgs(rule []string) []string {
	if len(rule) > 0 && rule[0] == "route" {
		return append([]string{"route", "delete"}, rule[1:]...)
	}

	return append([]string{"delete"}, rule...)
}

// ufwRunner runs one ufw invocation. A seam for tests.
var ufwRunner = func(args ...string) (string, error) {
	out, err := exec.Command("ufw", args...).CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// applyUFW runs the rules — or their deletes — through ufw. Without root
// it prints them instead and says so: the forward over -r runs as the SSH
// user, and a `sudo` typed on the host is the only way this gets root.
func applyUFW(out io.Writer, u wireUFW, remove bool, euid int, run func(args ...string) (string, error)) error {
	if euid != 0 {
		fmt.Fprint(out, renderUFWRules(u, "sudo "))

		return fmt.Errorf("ufw: needs root on the host — run `sudo vd wire ufw enable` there, or apply the lines above")
	}

	if _, err := exec.LookPath("ufw"); err != nil {
		return fmt.Errorf("ufw: not installed on this host")
	}

	for _, r := range u.Rules {
		args := r
		if remove {
			args = deleteArgs(r)
		}

		res, err := run(args...)
		if err != nil {
			return fmt.Errorf("ufw %s: %v: %s", strings.Join(args, " "), err, res)
		}

		fmt.Fprintf(out, "%s ufw %s — %s\n", check(), strings.Join(args, " "), res)
	}

	return nil
}
