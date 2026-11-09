package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	lxdCmd "github.com/canonical/lxd/shared/cmd"
	"github.com/canonical/microcluster/v3/microcluster"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"
	"github.com/spf13/cobra"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/client"
)

type cmdSetupBgpAttach struct {
	common *CmdControl

	flagMembers        string
	flagPeer           string
	flagNetworks       string
	flagAdvertiseTable uint32
	flagUplinkPattern  string
	flagDryRun         bool
	flagDisablePeer    string
}

func (c *cmdSetupBgpAttach) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bgp-attach",
		Short: "Attach BIRD to the BGP uplink of every member that peers with one leaf",
		Long: `Attach BIRD to the BGP uplink router port of every listed member, peering
with one leaf (in practice, one rack's members and that rack's leaf).

Run it on any member. The setting is identified by its --peer, and --members
is its complete member list: members of that setting not listed are removed,
listed ones are added, and repeating the same call changes nothing. Send the
whole rack every time (for example the rack's machines from MAAS). Members
that stay keep their address. A member may peer with only one leaf. Use
"disable" to remove a whole setting.

The member handling the call writes the router-port keys for every member in
one OVN Northbound transaction; each member sets its own Open_vSwitch knobs.

  on the uplink router port   networks (numbered only),
                              options:dynamic-routing-maintain-vrf=true,
                              external_ids:microovn-bgp-attach=true,
                              external_ids:microovn-bgp-peers=<peer>
  on Open_vSwitch             microovn-bgp=enabled,
                              microovn-bgp-advertise-table=<table>

Previous router-port values are recorded and put back when a member leaves the
setting or the setting is disabled.

Numbered: give the leaf's IPv4 --peer and the --networks pool as
<first-ip>/<len>. Addresses are handed out from <first-ip> upward within that
subnet, skipping the network, broadcast and peer addresses: 192.0.2.0/28
starts at .1, while 192.0.2.18/28 leaves .16-.17 alone and starts at .18. Each
member gets the lowest free address and keeps it while it stays listed, so the
pool needs one address per listed member, and a member that stays keeps its
address. Each leaf needs its own subnet. The
leaf can accept the subnet with "neighbor range".

Unnumbered: give an IPv6 link-local --peer and no --networks.

The uplink is the router port matching --uplink-pattern on a router pinned to
the member (options:chassis) whose switch has a localnet port.`,
		Example: `  microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --members node-1,node-2,node-3
  microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --members node-1,node-3,node-4
  microovn setup bgp-attach --peer 65102@192.0.2.33 --networks 192.0.2.34/28 --members node-5 --dry-run
  microovn setup bgp-attach --peer 65103@fe80::1 --members node-6
  microovn setup bgp-attach status
  microovn setup bgp-attach disable --peer 65102@192.0.2.33
  microovn setup bgp-attach disable`,
		Args: cobra.NoArgs,
		RunE: c.Run,
	}

	cmd.Flags().StringVar(&c.flagPeer, "peer", "", "Leaf peer as <asn>@<ip>; identifies the setting (required)")
	cmd.Flags().StringVar(&c.flagMembers, "members", "", "Complete, comma-separated list of the cluster members that peer with this leaf, as in 'microovn cluster list' (required)")
	cmd.Flags().StringVar(&c.flagNetworks, "networks", "", "IPv4 pool as <first-ip>/<len>; addresses are handed out from <first-ip> upward, e.g. 192.0.2.18/28 (numbered only)")
	cmd.Flags().Uint32Var(&c.flagAdvertiseTable, "advertise-table", types.DefaultBgpAttachAdvertiseTable, "Kernel table BIRD reads advertised /32s from")
	cmd.Flags().StringVar(&c.flagUplinkPattern, "uplink-pattern", types.DefaultBgpAttachUplinkPattern, "Glob matched against router port names to find each member's uplink")
	cmd.Flags().BoolVar(&c.flagDryRun, "dry-run", false, "Validate and print the per-member plan without writing anything")

	disable := &cobra.Command{
		Use:   "disable",
		Short: "Remove the bgp-attach setting for one leaf (or all) and restore its members' previous values",
		Args:  cobra.NoArgs,
		RunE:  c.runDisable,
	}
	disable.Flags().StringVar(&c.flagDisablePeer, "peer", "", "Leaf peer (<asn>@<ip>) whose setting to remove (default: all)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show every bgp-attach setting and each member's uplink, address and state",
		Args:  cobra.NoArgs,
		RunE:  c.runStatus,
	}
	cmd.AddCommand(disable, status)

	return cmd
}

func (c *cmdSetupBgpAttach) localClient() (microTypes.Client, error) {
	m, err := microcluster.App(microcluster.Args{StateDir: c.common.FlagStateDir})
	if err != nil {
		return nil, err
	}
	return m.LocalClient()
}

// splitMembers turns the --members value into a list (empty entries dropped).
func splitMembers(raw string) []string {
	var out []string
	for _, m := range strings.Split(raw, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func (c *cmdSetupBgpAttach) Run(_ *cobra.Command, _ []string) error {
	switch {
	case c.flagPeer == "":
		return errors.New("--peer is required (e.g. --peer 65101@192.0.2.17)")
	case c.flagMembers == "":
		return errors.New("--members is required (e.g. --members node-1,node-2)")
	}
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.SetupBgpAttach(context.Background(), cli, types.SetupBgpAttachRequest{
		Members:        splitMembers(c.flagMembers),
		Peer:           c.flagPeer,
		Networks:       c.flagNetworks,
		AdvertiseTable: c.flagAdvertiseTable,
		UplinkPattern:  c.flagUplinkPattern,
		DryRun:         c.flagDryRun,
	})
	if err != nil {
		return fmt.Errorf("bgp-attach failed: %w", err)
	}
	return printAttachResponse(resp)
}

func (c *cmdSetupBgpAttach) runDisable(_ *cobra.Command, _ []string) error {
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.DisableBgpAttach(context.Background(), cli, c.flagDisablePeer)
	if err != nil {
		return fmt.Errorf("bgp-attach disable failed: %w", err)
	}
	return printAttachResponse(resp)
}

func (c *cmdSetupBgpAttach) runStatus(_ *cobra.Command, _ []string) error {
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.GetBgpAttachStatus(context.Background(), cli)
	if err != nil {
		return fmt.Errorf("bgp-attach status failed: %w", err)
	}
	return printAttachResponse(resp)
}

// attachGroupLine renders one setting as a single line.
func attachGroupLine(r types.SetupBgpAttachRequest) string {
	networks := r.Networks
	if networks == "" {
		networks = "(unnumbered)"
	}
	return fmt.Sprintf("peer: %s  networks: %s  advertise-table: %d  uplink-pattern: %s  members: %s",
		r.Peer, networks, r.AdvertiseTable, r.UplinkPattern, strings.Join(r.Members, ","))
}

// attachTableRows renders the per-member plan as table rows.
func attachTableRows(nodes []types.BgpAttachNodePlan) [][]string {
	dash := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	rows := make([][]string, 0, len(nodes))
	for _, n := range nodes {
		id := "-"
		if n.MemberID > 0 {
			id = strconv.FormatInt(n.MemberID, 10)
		}
		rows = append(rows, []string{dash(n.Peer), n.Member, id, dash(n.Address), strconv.FormatBool(n.Attached), dash(n.RouterPort)})
	}
	return rows
}

// printAttachResponse prints the settings, a per-member table, warnings and
// errors. It returns an error (non-zero exit) only when the request failed:
// refused, or the router-port write failed. Per-member warnings (a member not
// reached, its router port not created yet) keep exit 0.
func printAttachResponse(resp types.SetupBgpAttachResponse) error {
	for _, r := range resp.Groups {
		fmt.Printf("%s\n", attachGroupLine(r))
	}
	if len(resp.Groups) > 0 {
		fmt.Printf("\n")
	}

	if len(resp.Nodes) > 0 {
		header := []string{"PEER", "MEMBER", "ID", "ADDRESS", "ATTACHED", "ROUTER PORT"}
		if err := lxdCmd.RenderTable(lxdCmd.TableFormatTable, header, attachTableRows(resp.Nodes), resp.Nodes); err != nil {
			return err
		}
	}

	if len(resp.Added) > 0 {
		fmt.Printf("added: %s\n", strings.Join(resp.Added, ", "))
	}
	if len(resp.Removed) > 0 {
		fmt.Printf("removed: %s\n", strings.Join(resp.Removed, ", "))
	}
	for _, w := range resp.Warnings {
		fmt.Printf("Warning: %s\n", w)
	}
	for _, e := range resp.Errors {
		fmt.Printf("Error: %s\n", e)
	}
	fmt.Printf("%s\n", resp.Message)
	if !resp.Success {
		return errors.New(resp.Message)
	}
	return nil
}
