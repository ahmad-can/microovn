package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

func TestAttachTableRows(t *testing.T) {
	rows := attachTableRows([]types.BgpAttachNodePlan{
		{Member: "compute-0", MemberID: 4, Address: "192.0.2.18/28", Peer: "65101@192.0.2.17", Attached: true, RouterPort: "lrp-c0"},
		{Member: "compute-1"},
	})
	want := [][]string{
		{"65101@192.0.2.17", "compute-0", "4", "192.0.2.18/28", "true", "lrp-c0"},
		{"-", "compute-1", "-", "-", "false", "-"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i := range want {
		if !slices.Equal(rows[i], want[i]) {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
}

func TestPrintAttachResponseExitCode(t *testing.T) {
	if err := printAttachResponse(types.SetupBgpAttachResponse{Success: true, Message: "bgp-attach is not configured"}); err != nil {
		t.Fatalf("success must not return an error: %v", err)
	}
	err := printAttachResponse(types.SetupBgpAttachResponse{
		Success: false,
		Message: "bgp-attach applied, with 1 node error(s)",
		Nodes:   []types.BgpAttachNodePlan{{Member: "compute-1", Error: "no router port"}},
		Errors:  []string{"compute-1: no router port"},
	})
	if err == nil {
		t.Fatal("a response with node errors must return an error (non-zero exit)")
	}
}

func TestSplitMembers(t *testing.T) {
	if got := splitMembers(" c0, ,c1,"); !slices.Equal(got, []string{"c0", "c1"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAttachGroupLine(t *testing.T) {
	line := attachGroupLine(types.SetupBgpAttachRequest{Members: []string{"c0", "c1"}, Peer: "65101@fe80::1", AdvertiseTable: 42, UplinkPattern: "*br-bgp"})
	for _, want := range []string{"peer: 65101@fe80::1", "networks: (unnumbered)", "members: c0,c1"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
}

func TestPrintAttachResponseWarningsExitZero(t *testing.T) {
	err := printAttachResponse(types.SetupBgpAttachResponse{
		Success:  true,
		Message:  "bgp-attach group 65101@192.0.2.17 applied (2 member(s)), with 1 warning(s)",
		Nodes:    []types.BgpAttachNodePlan{{Member: "c1", Error: "not reached (x)"}},
		Warnings: []string{"c1: not reached (x)"},
	})
	if err != nil {
		t.Fatalf("warnings must keep exit 0, got %v", err)
	}
}
