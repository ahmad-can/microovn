package bgpapi

// `microovn setup bgp-attach` endpoints. Settings are per group: the members
// that peer with one leaf (in practice, one rack), identified by the peer.
//
//	POST   bgp/attach            set one group to the given members (or dry-run
//	                             it): validate, allocate addresses, store it,
//	                             write the router ports, ask the added and
//	                             removed members to converge
//	DELETE bgp/attach?peer=P     remove group P (no peer: every group)
//	GET    bgp/attach            every group and each member's plan/state
//
// Each call states a group's complete member list, so repeating it changes
// nothing. Every write is a read-modify-write in one database transaction, so
// overlapping calls never hand out an address twice.
// The member handling the call then writes (or restores) every affected
// compute's router-port keys in one NB transaction, and asks those computes to
// set (or clear) their own Open_vSwitch knobs.
//
// Members also converge on their own every reconcile tick, so a member that is
// down during a change catches up without a rerun. When a request arrives as a
// cluster notification, the member only converges and reports its own state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/bgp"
	microOvnClient "github.com/canonical/microovn/microovn/client"
	"github.com/canonical/microovn/microovn/config"
)

// BgpAttachSetupEndpoint backs `microovn setup bgp-attach`.
var BgpAttachSetupEndpoint = microTypes.Endpoint{
	Path:   "bgp/attach",
	Get:    microTypes.EndpointAction{Handler: getBgpAttachHandler, AllowUntrusted: false, ProxyTarget: true},
	Post:   microTypes.EndpointAction{Handler: postBgpAttachHandler, AllowUntrusted: false, ProxyTarget: true},
	Delete: microTypes.EndpointAction{Handler: deleteBgpAttachHandler, AllowUntrusted: false, ProxyTarget: true},
}

// planGroup computes the plan for each member of a group, in member order.
// Members that are not cluster members, or do not run the switch service, get
// a plan carrying that error.
func planGroup(ctx context.Context, s microTypes.State, group bgp.AttachIntent, ids map[string]int64, chassis map[string]bool) []types.BgpAttachNodePlan {
	plans := make([]types.BgpAttachNodePlan, 0, len(group.Members))
	for _, m := range group.Members {
		id, ok := ids[m]
		switch {
		case !ok:
			plans = append(plans, types.BgpAttachNodePlan{Member: m, Peer: group.PeerString(),
				Error: "not a cluster member (see 'microovn cluster list')"})
		case !chassis[m]:
			plans = append(plans, types.BgpAttachNodePlan{Member: m, MemberID: id, Peer: group.PeerString(),
				Error: "does not run the switch service, so it cannot attach"})
		default:
			plans = append(plans, bgp.PlanAttachForMember(ctx, s, group, m, id))
		}
	}
	return plans
}

// planMembers is planGroup for a subset of the group's members.
func planMembers(ctx context.Context, s microTypes.State, group bgp.AttachIntent, members []string, ids map[string]int64, chassis map[string]bool) []types.BgpAttachNodePlan {
	sub := group
	sub.Members = members
	return planGroup(ctx, s, sub, ids, chassis)
}

// attachWriteMu serialises group writes made through this member. Writes
// through different members are serialised by the database transaction.
var attachWriteMu sync.Mutex

// convergeMembers runs one attach pass on each named member (locally when the
// name is this member) and returns each member's reported state by name.
// Members that can't be reached are returned in unreached with the error.
// Members not named are not contacted, so nodes outside the change (including
// ones running an older MicroOVN without this endpoint) are left alone.
func convergeMembers(ctx context.Context, s microTypes.State, members map[string]bool, disable bool) (map[string]types.BgpAttachNodePlan, map[string]string, error) {
	addresses, err := bgp.ClusterMemberAddresses(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	byAddr := map[string]string{}
	for name, addr := range addresses {
		byAddr[addr] = name
	}
	cluster, err := s.Connect().Cluster(true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get cluster clients: %w", err)
	}

	var mu sync.Mutex
	reported := map[string]types.BgpAttachNodePlan{}
	unreached := map[string]string{}

	var local sync.WaitGroup
	if members[s.Name()] {
		local.Add(1)
		go func() {
			defer local.Done()
			if plan, ok := localConverge(ctx, s); ok {
				mu.Lock()
				reported[s.Name()] = plan
				mu.Unlock()
			}
		}()
	}

	err = cluster.Query(ctx, true, func(ctx context.Context, c microTypes.Client) error {
		name, ok := byAddr[c.URL().Host]
		if !ok || !members[name] {
			return nil
		}
		var resp types.SetupBgpAttachResponse
		var err error
		if disable {
			resp, err = microOvnClient.DisableBgpAttach(ctx, c, "")
		} else {
			resp, err = microOvnClient.SetupBgpAttach(ctx, c, types.SetupBgpAttachRequest{})
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			unreached[name] = err.Error()
			return nil
		}
		for _, n := range resp.Nodes {
			reported[n.Member] = n
		}
		return nil
	})
	local.Wait()
	if err != nil {
		return nil, nil, err
	}
	return reported, unreached, nil
}

// localConverge runs one attach pass on this member and reports its own state.
// ok is false for members that are not a chassis (nothing to report).
func localConverge(ctx context.Context, s microTypes.State) (types.BgpAttachNodePlan, bool) {
	chassis, err := bgp.SwitchMembers(ctx, s)
	if err != nil {
		return types.BgpAttachNodePlan{Member: s.Name(), Error: err.Error()}, true
	}
	if !chassis[s.Name()] {
		return types.BgpAttachNodePlan{}, false
	}

	passErr := bgp.ReconcileAttach(ctx, s)

	groups, err := bgp.LoadAttachGroups(ctx, s)
	if err != nil {
		return types.BgpAttachNodePlan{Member: s.Name(), Error: err.Error()}, true
	}
	group := bgp.GroupForMember(groups, s.Name())

	if group == nil {
		// In no group: report whatever is still attached.
		plan := types.BgpAttachNodePlan{Member: s.Name()}
		attached, err := bgp.AttachLocalState(ctx, s)
		switch {
		case err != nil:
			plan.Error = err.Error()
		case len(attached) > 0:
			// Hand-stamped markers (not from a group) keep working.
			plan.Attached = true
			plan.RouterPort = attached[0]
		}
		if passErr != nil && plan.Error == "" {
			plan.Error = passErr.Error()
		}
		return plan, true
	}

	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return types.BgpAttachNodePlan{Member: s.Name(), Peer: group.PeerString(), Error: err.Error()}, true
	}
	plan := bgp.PlanAttachForMember(ctx, s, *group, s.Name(), ids[s.Name()])
	if plan.Error == "" && !plan.Attached {
		if passErr != nil {
			plan.Error = passErr.Error()
		} else {
			plan.Error = "keys set but not attached yet; check 'sudo snap logs microovn.daemon'"
		}
	}
	return plan, true
}

func respond(resp types.SetupBgpAttachResponse) microTypes.Response {
	return microTypes.SyncResponse(true, &resp)
}

// summariseWarnings fills Success/Message/Warnings for a request that was
// carried out: per-member problems (not reached, router port not created yet,
// not attached yet) resolve on their own, so they are warnings and the request
// still succeeds. Only addNbErrors, called after it, can fail the request.
func summariseWarnings(resp *types.SetupBgpAttachResponse, okMessage string) {
	for _, n := range resp.Nodes {
		if n.Error != "" {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("%s: %s", n.Member, n.Error))
		}
	}
	resp.Success = true
	resp.Message = okMessage
	if len(resp.Warnings) > 0 {
		resp.Message = fmt.Sprintf("%s, with %d warning(s)", okMessage, len(resp.Warnings))
	}
}

// summarise fills Success/Message/Errors from the per-node plans. It is used
// for a member's own reply to a cluster notification.
func summarise(resp *types.SetupBgpAttachResponse, okMessage string) {
	for _, n := range resp.Nodes {
		if n.Error != "" {
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %s", n.Member, n.Error))
		}
	}
	resp.Success = len(resp.Errors) == 0
	if resp.Success {
		resp.Message = okMessage
	} else {
		resp.Message = fmt.Sprintf("%s, with %d node error(s)", okMessage, len(resp.Errors))
	}
}

// sortNodes orders node rows by peer (group), then member.
func sortNodes(nodes []types.BgpAttachNodePlan) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Peer != nodes[j].Peer {
			return nodes[i].Peer < nodes[j].Peer
		}
		return nodes[i].Member < nodes[j].Member
	})
}

// collectReports turns convergeMembers results into node rows for members,
// filling in a row for members that did not report.
func collectReports(members map[string]bool, reported map[string]types.BgpAttachNodePlan, unreached map[string]string, fallback map[string]types.BgpAttachNodePlan) []types.BgpAttachNodePlan {
	var nodes []types.BgpAttachNodePlan
	for m := range members {
		if got, ok := reported[m]; ok {
			nodes = append(nodes, got)
			continue
		}
		row, ok := fallback[m]
		if !ok {
			row = types.BgpAttachNodePlan{Member: m}
		}
		if msg, ok := unreached[m]; ok {
			row.Error = fmt.Sprintf("not reached (%s); it will converge on its next reconcile tick", msg)
		} else if row.Error == "" {
			row.Error = "no report from this member; it will converge on its next reconcile tick"
		}
		nodes = append(nodes, row)
	}
	sortNodes(nodes)
	return nodes
}

func postBgpAttachHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()

	if microTypes.IsNotification(r) {
		resp := types.SetupBgpAttachResponse{}
		if plan, ok := localConverge(ctx, s); ok {
			resp.Nodes = []types.BgpAttachNodePlan{plan}
		}
		summarise(&resp, "converged")
		return respond(resp)
	}

	var req types.SetupBgpAttachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return microTypes.BadRequest(errors.New("failed to decode request body"))
	}
	group, err := bgp.ParseAttachIntent(req)
	if err != nil {
		return microTypes.BadRequest(err)
	}

	// Mutual exclusion with managed `setup bgp`.
	if item, err := config.GetConfig(ctx, s, types.BgpConfigKeyCluster); err != nil {
		return microTypes.InternalError(err)
	} else if item != nil && item.Value == "true" {
		return microTypes.BadRequest(errors.New("managed BGP ('microovn setup bgp') is configured; disable it before using bgp-attach"))
	}

	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	chassis, err := bgp.SwitchMembers(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	// A listed member that is not a chassis (or not a member at all) is a typo
	// in --members, not a transient state: refuse before storing anything.
	for _, m := range group.Members {
		if _, ok := ids[m]; !ok {
			return microTypes.BadRequest(fmt.Errorf("member %s is not a cluster member (see 'microovn cluster list')", m))
		}
		if !chassis[m] {
			return microTypes.BadRequest(fmt.Errorf("member %s does not run the switch service, so it cannot attach", m))
		}
	}

	// Preview the result against the stored groups before writing anything.
	groups, err := bgp.LoadAttachGroups(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	stored := bgp.FindGroup(groups, group.Key())
	preview, err := bgp.MergeGroup(stored, group)
	if err != nil {
		return microTypes.BadRequest(err)
	}
	if err := bgp.CheckGroupConflicts(preview, groups); err != nil {
		return microTypes.BadRequest(err)
	}
	added, removed := bgp.MembershipChange(stored, preview)
	settingsChanged := stored == nil || !bgp.SameSettings(*stored, preview)

	// Find the uplinks that are not recorded yet (new members, or every member
	// when the settings changed, since the pattern may pick another port).
	var discover []string
	for _, m := range preview.Members {
		if _, ok := preview.Ports[m]; !ok || settingsChanged {
			discover = append(discover, m)
		}
	}
	plans := planMembers(ctx, s, preview, discover, ids, chassis)
	found := 0
	for _, p := range plans {
		if p.RouterPort == "" {
			continue
		}
		found++
		if err := bgp.CheckAttachAdvertiseTable(ctx, s, preview, p.RouterPort); err != nil {
			return microTypes.BadRequest(err)
		}
	}
	if len(discover) > 0 && found == 0 {
		return microTypes.BadRequest(fmt.Errorf("--uplink-pattern %q matches no router port on the listed members", group.UplinkPattern))
	}

	if req.DryRun {
		resp := types.SetupBgpAttachResponse{Groups: []types.SetupBgpAttachRequest{preview.Request()}, Added: added, Removed: removed}
		resp.Nodes = planGroup(ctx, s, preview, ids, chassis)
		summariseWarnings(&resp, "dry run: nothing was written")
		return respond(resp)
	}

	// Record each newly found uplink's current values before the first write,
	// so they can be put back when the member leaves.
	newPorts := map[string]bgp.PortRecord{}
	for _, p := range plans {
		if p.RouterPort == "" {
			continue
		}
		if rec, ok := preview.Ports[p.Member]; ok && rec.RouterPort == p.RouterPort {
			continue
		}
		rec, err := bgp.ReadPortPrior(ctx, s, p.RouterPort)
		if err != nil {
			return microTypes.InternalError(err)
		}
		newPorts[p.Member] = rec
	}

	// Store: merge again inside the transaction, on whatever is stored now.
	var final bgp.AttachIntent
	var previous *bgp.AttachIntent
	var toRestore []bgp.PortRecord
	attachWriteMu.Lock()
	err = bgp.UpdateAttachGroups(ctx, s, func(groups []bgp.AttachIntent) (map[string]*bgp.AttachIntent, error) {
		previous = nil
		if g := bgp.FindGroup(groups, group.Key()); g != nil {
			p := *g
			previous = &p
		}
		merged, err := bgp.MergeGroup(previous, group)
		if err != nil {
			return nil, err
		}
		if err := bgp.CheckGroupConflicts(merged, groups); err != nil {
			return nil, err
		}
		restore := bgp.DroppedPorts(previous, &merged)
		for m, rec := range newPorts {
			if !merged.HasMember(m) {
				continue
			}
			if old, ok := merged.Ports[m]; ok {
				if old.RouterPort == rec.RouterPort {
					continue
				}
				// The member's uplink changed: give the old one its values back.
				restore = append(restore, old)
			}
			if merged.Ports == nil {
				merged.Ports = map[string]bgp.PortRecord{}
			}
			merged.Ports[m] = rec
		}
		final, toRestore = merged, restore
		return map[string]*bgp.AttachIntent{group.Key(): &merged}, nil
	})
	attachWriteMu.Unlock()
	if err != nil {
		return microTypes.BadRequest(err)
	}
	added, removed = bgp.MembershipChange(previous, final)
	logger.Infof("bgp-attach group %s stored: networks %q, members %s (added %v, removed %v)",
		final.Key(), final.Request().Networks, strings.Join(final.Members, ","), added, removed)

	// Write every member's router-port keys centrally, in one NB transaction
	// (setting a value it already has changes nothing, and a key removed by
	// hand is put back), and give removed members' uplinks their values back.
	// Then the computes set their own OVS knobs (gate written after the port
	// keys, so attach finds them ready).
	var nbErrors []string
	if err := bgp.WriteGroupPorts(ctx, s, final, final.Members); err != nil {
		nbErrors = append(nbErrors, err.Error())
	}
	if err := bgp.RestorePorts(ctx, s, toRestore); err != nil {
		nbErrors = append(nbErrors, err.Error())
	}

	// Converge the added and removed computes, or every member when the
	// settings changed. Unchanged members are not contacted.
	affected := map[string]bool{}
	for _, m := range append(slices.Clone(added), removed...) {
		affected[m] = true
	}
	if previous == nil || !bgp.SameSettings(*previous, final) {
		for _, m := range final.Members {
			affected[m] = true
		}
	}
	fallback := map[string]types.BgpAttachNodePlan{}
	for _, p := range plans {
		fallback[p.Member] = p
	}

	reported, unreached, err := convergeMembers(ctx, s, affected, false)
	if err != nil {
		return microTypes.SmartError(err)
	}
	resp := types.SetupBgpAttachResponse{Groups: []types.SetupBgpAttachRequest{final.Request()}, Added: added, Removed: removed}
	resp.Nodes = collectReports(affected, reported, unreached, fallback)
	msg := fmt.Sprintf("bgp-attach group %s applied (%d member(s))", final.Key(), len(final.Members))
	if len(added) == 0 && len(removed) == 0 {
		msg += "; no membership change"
	}
	summariseWarnings(&resp, msg)
	addNbErrors(&resp, nbErrors)
	return respond(resp)
}

// addNbErrors appends central NB write errors to a response (the setting is
// stored; rerunning the call writes the keys again).
func addNbErrors(resp *types.SetupBgpAttachResponse, errs []string) {
	if len(errs) == 0 {
		return
	}
	for _, e := range errs {
		resp.Errors = append(resp.Errors, "router ports: "+e)
	}
	resp.Success = false
	resp.Message += "; router-port keys were not fully written, rerun the call"
}

func deleteBgpAttachHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()

	if microTypes.IsNotification(r) {
		resp := types.SetupBgpAttachResponse{}
		if plan, ok := localConverge(ctx, s); ok {
			resp.Nodes = []types.BgpAttachNodePlan{plan}
		}
		summarise(&resp, "converged")
		return respond(resp)
	}

	key := ""
	if peer := strings.TrimSpace(r.URL.Query().Get("peer")); peer != "" {
		var err error
		if key, err = bgp.GroupKeyFromPeer(peer); err != nil {
			return microTypes.BadRequest(err)
		}
	}

	var (
		affected  map[string]bool
		touched   []types.SetupBgpAttachRequest
		toRestore []bgp.PortRecord
		keys      []string
	)
	attachWriteMu.Lock()
	err := bgp.UpdateAttachGroups(ctx, s, func(groups []bgp.AttachIntent) (map[string]*bgp.AttachIntent, error) {
		affected, touched, toRestore, keys = map[string]bool{}, nil, nil, nil
		var targets []bgp.AttachIntent
		if key == "" {
			targets = groups
		} else if g := bgp.FindGroup(groups, key); g != nil {
			targets = []bgp.AttachIntent{*g}
		} else {
			return nil, fmt.Errorf("no bgp-attach group for peer %s", key)
		}
		changes := map[string]*bgp.AttachIntent{}
		for _, g := range targets {
			for _, m := range g.Members {
				affected[m] = true
			}
			touched = append(touched, g.Request())
			toRestore = append(toRestore, bgp.DroppedPorts(&g, nil)...)
			keys = append(keys, g.Key())
			changes[g.Key()] = nil
		}
		return changes, nil
	})
	attachWriteMu.Unlock()
	if err != nil {
		return microTypes.BadRequest(err)
	}
	if len(keys) == 0 {
		return respond(types.SetupBgpAttachResponse{Success: true, Message: "bgp-attach is not configured; nothing to disable"})
	}
	message := fmt.Sprintf("bgp-attach disabled for %s", strings.Join(keys, ", "))
	logger.Infof("bgp-attach: %s", message)

	// Give the computes' uplinks their recorded values back, centrally and in
	// one NB transaction; the computes then clear their own OVS knobs.
	var nbErrors []string
	if err := bgp.RestorePorts(ctx, s, toRestore); err != nil {
		nbErrors = append(nbErrors, err.Error())
	}

	reported, unreached, err := convergeMembers(ctx, s, affected, true)
	if err != nil {
		return microTypes.SmartError(err)
	}
	resp := types.SetupBgpAttachResponse{Groups: touched}
	for m := range affected {
		resp.Removed = append(resp.Removed, m)
	}
	sort.Strings(resp.Removed)
	resp.Nodes = collectReports(affected, reported, unreached, nil)
	summariseWarnings(&resp, message)
	addNbErrors(&resp, nbErrors)
	return respond(resp)
}

func getBgpAttachHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()
	groups, err := bgp.LoadAttachGroups(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	if len(groups) == 0 {
		return respond(types.SetupBgpAttachResponse{Success: true, Message: "bgp-attach is not configured"})
	}

	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	chassis, err := bgp.SwitchMembers(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}

	resp := types.SetupBgpAttachResponse{}
	for _, g := range groups {
		resp.Groups = append(resp.Groups, g.Request())
		resp.Nodes = append(resp.Nodes, planGroup(ctx, s, g, ids, chassis)...)
	}
	for i := range resp.Nodes {
		if resp.Nodes[i].Error == "" && !resp.Nodes[i].Attached {
			resp.Nodes[i].Error = "not attached yet"
		}
	}
	sortNodes(resp.Nodes)
	summariseWarnings(&resp, fmt.Sprintf("bgp-attach is configured for %d group(s)", len(groups)))
	return respond(resp)
}
