package types

// BgpAttachGroupKeyPrefix prefixes the dqlite key holding one
// `microovn setup bgp-attach` group: "microovn-bgp-attach-group.<asn>@<ip>"
// holds the group's request and its members' allocated addresses as JSON. A
// group is identified by its leaf peer, since every rack has its own leaf. The
// prefix never collides with the managed path's keys (microovn-bgp-asn, ...);
// "microovn-bgp-attach" (no suffix) is a separate Logical_Router_Port marker.
const BgpAttachGroupKeyPrefix = "microovn-bgp-attach-group."

// DefaultBgpAttachAdvertiseTable is the main-router VRF id Neutron's ovn-bgp
// plugin uses by default.
const DefaultBgpAttachAdvertiseTable uint32 = 42

// DefaultBgpAttachUplinkPattern matches the uplink LRP name Neutron's ovn-bgp
// plugin creates for each chassis
// (bgp-lrp-bgp-lr-<chassis>-to-bgp-ls-<chassis>-br-bgp).
const DefaultBgpAttachUplinkPattern = "*br-bgp"

// SetupBgpAttachRequest is the `microovn setup bgp-attach` request for one
// group: the members that peer with one leaf (in practice, one rack). A group
// is identified by its peer. Each call states the group's complete member
// list: calling again with the same peer makes the group match it (members not
// listed are removed), so repeating a call changes nothing. The node the operator talks to validates it, allocates the members'
// addresses, stores it cluster-wide and asks the affected members to converge;
// members then keep converging on their own every reconcile tick.
type SetupBgpAttachRequest struct {
	// Members is the complete list of cluster members that peer with this
	// leaf, as shown by `microovn cluster list`. Members of the group not in it
	// are removed. A member may be in only one group.
	Members []string `json:"members" yaml:"members"`
	// Peer is "<asn>@<ip>", the leaf peer, the same for every member of the
	// group: an IPv4 address (numbered, requires Networks) or an IPv6
	// link-local (unnumbered).
	Peer string `json:"peer" yaml:"peer"`
	// Networks is the IPv4 pool as "<first-ip>/<len>", e.g. "192.0.2.0/28" or
	// "192.0.2.18/28". Each member gets the lowest free address from
	// <first-ip> upward within that subnet; it keeps it while it stays in the
	// group. Empty for unnumbered.
	Networks string `json:"networks,omitempty" yaml:"networks,omitempty"`
	// AdvertiseTable is the kernel table BIRD reads advertised /32s from.
	// Zero means DefaultBgpAttachAdvertiseTable.
	AdvertiseTable uint32 `json:"advertise_table,omitempty" yaml:"advertise_table,omitempty"`
	// UplinkPattern is a glob matched against LRP names on routers pinned to
	// each node. Empty means DefaultBgpAttachUplinkPattern.
	UplinkPattern string `json:"uplink_pattern,omitempty" yaml:"uplink_pattern,omitempty"`
	// DryRun validates and returns the plan without writing anything.
	DryRun bool `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
}

// BgpAttachNodePlan is what bgp-attach does (or would do) on one member.
type BgpAttachNodePlan struct {
	Member   string `json:"member" yaml:"member"`
	MemberID int64  `json:"member_id" yaml:"member_id"`
	// RouterPort is the discovered uplink LRP, empty if none was found.
	RouterPort string `json:"router_port,omitempty" yaml:"router_port,omitempty"`
	// Address is the allocated "<ip>/<len>" (numbered) or empty (unnumbered).
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	Peer    string `json:"peer" yaml:"peer"`
	// Attached reports whether the LRP currently carries this member's
	// routing-protocol-redirect (i.e. BIRD is attached to it).
	Attached bool `json:"attached" yaml:"attached"`
	// Error explains why this member cannot be attached.
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
}

// SetupBgpAttachResponse is returned by apply, dry-run, disable and status.
//
// Success (and the CLI's exit code) answers "was the request carried out":
// false only when it was refused (nothing stored) or the router-port write
// failed. Per-member problems that resolve on their own (a member not reached,
// its router port not created yet, BIRD not up yet) do not fail it; they are
// listed in Warnings and on each member's row.
type SetupBgpAttachResponse struct {
	Success bool   `json:"success" yaml:"success"`
	Message string `json:"message" yaml:"message"`
	// Groups are the stored groups that this response is about.
	Groups []SetupBgpAttachRequest `json:"groups,omitempty" yaml:"groups,omitempty"`
	// Added and Removed are the members a call added to and removed from the
	// group (or would, for a dry run).
	Added   []string `json:"added,omitempty" yaml:"added,omitempty"`
	Removed []string `json:"removed,omitempty" yaml:"removed,omitempty"`
	// Nodes is the per-member plan or status.
	Nodes []BgpAttachNodePlan `json:"nodes,omitempty" yaml:"nodes,omitempty"`
	// Errors make the request fail (Success false, non-zero exit).
	Errors []string `json:"errors,omitempty" yaml:"errors,omitempty"`
	// Warnings are per-member problems that resolve on their own; they do not
	// fail the request.
	Warnings []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}
