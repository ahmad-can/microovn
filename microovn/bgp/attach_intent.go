package bgp

// A bgp-attach group is the answer to "how should these members peer": one leaf
// peer, an optional IPv4 pool, the advertise table, the uplink name pattern and
// the members that use it (in practice, one rack). A group is identified by its
// peer, since every rack has its own leaf. Each group lives in dqlite under its
// own key together with the addresses allocated to its members, so every
// member finds its group and derives its own per-node keys from it on each
// reconcile tick.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"path"
	"slices"
	"sort"
	"strings"

	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/database"
)

// maxAttachPoolBits is the widest pool accepted (/16), so allocation stays a
// simple walk over at most 65536 addresses.
const maxAttachPoolBits = 16

// AttachIntent is a parsed, validated bgp-attach group.
type AttachIntent struct {
	// Members are the group's cluster members, sorted and unique.
	Members []string
	Peer    types.Peer
	// Pool is the IPv4 address pool (numbered), kept exactly as given: its
	// address is where allocation starts and its length is the subnet, so
	// 192.0.2.18/28 hands out .18, .19, ... within 192.0.2.16/28. The zero
	// value means unnumbered.
	Pool           netip.Prefix
	AdvertiseTable uint32
	UplinkPattern  string
	// Addresses are the members' allocated addresses (numbered only). They are
	// assigned by MicroOVN when the group is set and stored with it.
	Addresses map[string]netip.Prefix
	// Ports records, per member, the uplink router port MicroOVN wrote the
	// attach keys on and that port's values before the first write, so they
	// can be put back when the member leaves the group.
	Ports map[string]PortRecord
}

// PortRecord is one member's uplink router port and its pre-attach values.
type PortRecord struct {
	RouterPort string   `json:"router_port"`
	Prior      lrpPrior `json:"prior"`
}

// Numbered reports whether node addresses are allocated from a pool.
func (i AttachIntent) Numbered() bool { return i.Pool.IsValid() }

// PeerString renders the peer in the microovn-bgp-peers format.
func (i AttachIntent) PeerString() string {
	return fmt.Sprintf("%d@%s", i.Peer.ASN, i.Peer.Address)
}

// Key identifies the group: its peer.
func (i AttachIntent) Key() string { return i.PeerString() }

// Request renders the group back to the request form (for output).
func (i AttachIntent) Request() types.SetupBgpAttachRequest {
	req := types.SetupBgpAttachRequest{
		Members:        slices.Clone(i.Members),
		Peer:           i.PeerString(),
		AdvertiseTable: i.AdvertiseTable,
		UplinkPattern:  i.UplinkPattern,
	}
	if i.Numbered() {
		req.Networks = i.Pool.String()
	}
	return req
}

// ParseAttachIntent validates a request and returns the group it describes,
// with defaults filled in. Addresses are not part of a request; they are
// allocated separately. It does not touch the system.
func ParseAttachIntent(req types.SetupBgpAttachRequest) (AttachIntent, error) {
	var intent AttachIntent

	seen := map[string]bool{}
	for _, m := range req.Members {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if seen[m] {
			return intent, fmt.Errorf("--members lists %s twice", m)
		}
		seen[m] = true
		intent.Members = append(intent.Members, m)
	}
	if len(intent.Members) == 0 {
		return intent, fmt.Errorf("--members is required: list the cluster members that peer with this leaf (as shown by 'microovn cluster list')")
	}
	sort.Strings(intent.Members)

	peers, err := types.ParsePeers(req.Peer)
	if err != nil {
		return intent, fmt.Errorf("--peer: %w", err)
	}
	if len(peers) != 1 {
		return intent, fmt.Errorf("--peer: exactly one peer is required, as <asn>@<ip> (e.g. 65101@192.0.2.1)")
	}
	intent.Peer = peers[0]
	peerAddr, err := netip.ParseAddr(intent.Peer.Address)
	if err != nil {
		return intent, fmt.Errorf("--peer: invalid address %q", intent.Peer.Address)
	}

	networks := strings.TrimSpace(req.Networks)
	if peerAddr.Is4() {
		if networks == "" {
			return intent, fmt.Errorf("--networks is required with an IPv4 peer (numbered peering): give the pool node addresses are allocated from, e.g. 192.0.2.0/28")
		}
		pool, err := netip.ParsePrefix(networks)
		if err != nil {
			return intent, fmt.Errorf("--networks: invalid CIDR %q: %w", networks, err)
		}
		if !pool.Addr().Is4() {
			return intent, fmt.Errorf("--networks: the pool must be IPv4, got %s", pool)
		}
		if pool.Bits() < maxAttachPoolBits || pool.Bits() > 31 {
			return intent, fmt.Errorf("--networks: pool length must be between /%d and /31, got /%d", maxAttachPoolBits, pool.Bits())
		}
		if !pool.Contains(peerAddr) {
			return intent, fmt.Errorf("--peer %s is outside the --networks pool %s; the leaf must be reachable on the shared transit network", peerAddr, pool)
		}
		if isIPv4NetOrBroadcast(peerAddr, pool) {
			return intent, fmt.Errorf("--peer %s is the network/broadcast address of %s", peerAddr, pool)
		}
		intent.Pool = pool
	} else {
		if !peerAddr.Is6() || !peerAddr.IsLinkLocalUnicast() {
			return intent, fmt.Errorf("--peer must be IPv4 (numbered) or IPv6 link-local (unnumbered), got %s", peerAddr)
		}
		if networks != "" {
			return intent, fmt.Errorf("--networks is only used with an IPv4 peer; unnumbered peering needs no addresses")
		}
	}

	intent.AdvertiseTable = req.AdvertiseTable
	if intent.AdvertiseTable == 0 {
		intent.AdvertiseTable = types.DefaultBgpAttachAdvertiseTable
	}
	// 253-255 are the kernel's default/main/local tables, never an OVN VRF.
	if intent.AdvertiseTable >= 253 && intent.AdvertiseTable <= 255 {
		return intent, fmt.Errorf("--advertise-table %d is a reserved kernel table", intent.AdvertiseTable)
	}

	intent.UplinkPattern = strings.TrimSpace(req.UplinkPattern)
	if intent.UplinkPattern == "" {
		intent.UplinkPattern = types.DefaultBgpAttachUplinkPattern
	}
	if _, err := path.Match(intent.UplinkPattern, ""); err != nil {
		return intent, fmt.Errorf("--uplink-pattern %q is not a valid glob: %w", intent.UplinkPattern, err)
	}

	return intent, nil
}

// Equal reports whether two groups are the same, including allocations.
func (i AttachIntent) Equal(o AttachIntent) bool {
	return slices.Equal(i.Members, o.Members) && i.Peer == o.Peer && i.Pool == o.Pool &&
		i.AdvertiseTable == o.AdvertiseTable && i.UplinkPattern == o.UplinkPattern &&
		maps.Equal(i.Addresses, o.Addresses) &&
		maps.EqualFunc(i.Ports, o.Ports, func(a, b PortRecord) bool {
			return a.RouterPort == b.RouterPort && a.Prior.equal(b.Prior)
		})
}

// HasMember reports whether member is in the group.
func (i AttachIntent) HasMember(member string) bool {
	return slices.Contains(i.Members, member)
}

// groupRecord is the stored form of a group: the request, the allocations and
// the members' router ports with their pre-attach values.
type groupRecord struct {
	types.SetupBgpAttachRequest
	Addresses map[string]string     `json:"addresses,omitempty"`
	Ports     map[string]PortRecord `json:"ports,omitempty"`
}

// encodeGroup / decodeGroup convert a group to and from its stored JSON.
func encodeGroup(intent AttachIntent) (string, error) {
	rec := groupRecord{SetupBgpAttachRequest: intent.Request(), Ports: intent.Ports}
	if len(intent.Addresses) > 0 {
		rec.Addresses = map[string]string{}
		for m, a := range intent.Addresses {
			rec.Addresses[m] = a.String()
		}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeGroup(key, value string) (AttachIntent, error) {
	var rec groupRecord
	if err := json.Unmarshal([]byte(value), &rec); err != nil {
		return AttachIntent{}, fmt.Errorf("stored %s is not valid JSON: %w", key, err)
	}
	intent, err := ParseAttachIntent(rec.SetupBgpAttachRequest)
	if err != nil {
		return AttachIntent{}, fmt.Errorf("stored %s is invalid: %w", key, err)
	}
	if want := types.BgpAttachGroupKeyPrefix + intent.Key(); want != key {
		return AttachIntent{}, fmt.Errorf("stored %s is for peer %s", key, intent.Key())
	}
	intent.Ports = rec.Ports
	if len(rec.Addresses) > 0 {
		intent.Addresses = map[string]netip.Prefix{}
		for m, a := range rec.Addresses {
			p, err := netip.ParsePrefix(a)
			if err != nil {
				return AttachIntent{}, fmt.Errorf("stored %s has invalid address %q for %s", key, a, m)
			}
			intent.Addresses[m] = p
		}
	}
	return intent, nil
}

// decodeGroups picks the groups out of the config table's rows, sorted by peer.
func decodeGroups(items []database.ConfigItem) ([]AttachIntent, error) {
	var groups []AttachIntent
	for _, item := range items {
		if !strings.HasPrefix(item.Key, types.BgpAttachGroupKeyPrefix) {
			continue
		}
		group, err := decodeGroup(item.Key, item.Value)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(a, b int) bool { return groups[a].Key() < groups[b].Key() })
	return groups, nil
}

// LoadAttachGroups reads every group from dqlite, sorted by peer.
func LoadAttachGroups(ctx context.Context, s microTypes.State) ([]AttachIntent, error) {
	var items []database.ConfigItem
	err := s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		items, err = database.GetConfigItems(ctx, tx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read bgp-attach groups: %w", err)
	}
	return decodeGroups(items)
}

// HasAttachIntent reports whether any bgp-attach group is stored.
func HasAttachIntent(ctx context.Context, s microTypes.State) (bool, error) {
	groups, err := LoadAttachGroups(ctx, s)
	if err != nil {
		return false, err
	}
	return len(groups) > 0, nil
}

// UpdateAttachGroups reads the stored groups, passes them to change and writes
// back what it returns (group key -> new group, or nil to delete the group),
// all in one database transaction. Concurrent updates therefore never lose
// each other's members or hand out the same address twice: a transaction that
// races another is retried by the database layer, re-running change on the
// fresh state, so change must only compute (no side effects).
func UpdateAttachGroups(ctx context.Context, s microTypes.State, change func(groups []AttachIntent) (map[string]*AttachIntent, error)) error {
	err := s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		items, err := database.GetConfigItems(ctx, tx)
		if err != nil {
			return err
		}
		groups, err := decodeGroups(items)
		if err != nil {
			return err
		}
		changes, err := change(groups)
		if err != nil {
			return err
		}
		for key, group := range changes {
			dbKey := types.BgpAttachGroupKeyPrefix + key
			exists, err := database.ConfigItemExists(ctx, tx, dbKey)
			if err != nil {
				return err
			}
			if group == nil {
				if exists {
					if err := database.DeleteConfigItem(ctx, tx, dbKey); err != nil {
						return err
					}
				}
				continue
			}
			value, err := encodeGroup(*group)
			if err != nil {
				return err
			}
			item := database.ConfigItem{Key: dbKey, Value: value}
			if exists {
				err = database.UpdateConfigItem(ctx, tx, dbKey, item)
			} else {
				_, err = database.CreateConfigItem(ctx, tx, item)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to update bgp-attach groups: %w", err)
	}
	return nil
}
