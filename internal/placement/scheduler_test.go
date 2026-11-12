package placement

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func testPolicy() Policy {
	return Policy{
		GroupID: "cluster-a/control-plane", AllowedHosts: []string{"host-c", "host-a", "host-b"},
		AllowedStorages: []string{"disk-b", "disk-a"}, Templates: map[string]int{"host-a": 9001, "host-b": 9002, "host-c": 9003},
	}
}

func testInventory() Snapshot {
	policy := testPolicy()
	inventory := Snapshot{Complete: true, CapacityPolicy: CapacityPolicy{Configured: true, Hosts: map[string]CapacityReserve{}}}

	for _, host := range policy.AllowedHosts {
		inventory.Nodes = append(inventory.Nodes, Node{
			Name: host, Online: true, Reachable: true, TemplateVMID: policy.Templates[host], TemplateReady: true, NetworkReady: true,
			CPU: 100, MemoryBytes: 1000, FreeMemoryBytes: 1000,
			Storages: []Storage{
				{ID: "disk-a", Ready: true, CloneSupported: true, TotalBytes: 10000, FreeBytes: 8000},
				{ID: "disk-b", Ready: true, CloneSupported: true, TotalBytes: 10000, FreeBytes: 9000},
				{ID: "forbidden", Ready: true, CloneSupported: true, TotalBytes: 100000, FreeBytes: 100000},
			},
		})
	}

	return inventory
}

func testResources() Resources { return Resources{CPU: 2, MemoryBytes: 100, DiskBytes: 1000} }

func testCommitment(index int, host, group string) Commitment {
	return Commitment{
		MachineID: fmt.Sprintf("machine-%d", index), OwnerID: fmt.Sprintf("owner-%d", index), GroupID: group,
		VMID: 100 + index, Node: host, Storage: "disk-b", Resources: testResources(), BootDiskBytes: 32 << 30,
	}
}

func TestPolicyValidation(t *testing.T) {
	tests := map[string]func(*Policy){
		"empty group":       func(p *Policy) { p.GroupID = "" },
		"control character": func(p *Policy) { p.GroupID = "group\nsecret" },
		"empty hosts":       func(p *Policy) { p.AllowedHosts = nil },
		"duplicate host":    func(p *Policy) { p.AllowedHosts[1] = p.AllowedHosts[0] },
		"invalid hostname":  func(p *Policy) { p.AllowedHosts[0] = "../nodes" },
		"empty storage":     func(p *Policy) { p.AllowedStorages = nil },
		"invalid storage":   func(p *Policy) { p.AllowedStorages[0] = "disk/path" },
		"duplicate storage": func(p *Policy) { p.AllowedStorages[0] = p.AllowedStorages[1] },
		"missing template":  func(p *Policy) { delete(p.Templates, "host-a") },
		"duplicate VMID":    func(p *Policy) { p.Templates["host-a"] = p.Templates["host-b"] },
		"invalid VMID":      func(p *Policy) { p.Templates["host-a"] = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			policy := testPolicy()
			mutate(&policy)
			require.Error(t, policy.Validate())
		})
	}

	policy := testPolicy()
	fingerprint, err := policy.Fingerprint()
	require.NoError(t, err)
	slices.Reverse(policy.AllowedHosts)
	slices.Reverse(policy.AllowedStorages)
	reordered, err := policy.Fingerprint()
	require.NoError(t, err)
	require.Equal(t, fingerprint, reordered)

	policy.Templates["host-a"] = 9900
	changed, err := policy.Fingerprint()
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, changed)
}

func TestSpreadThenMostAvailableStorage(t *testing.T) {
	policy, inventory := testPolicy(), testInventory()
	decision, err := Select(t.Context(), policy, testResources(), inventory, nil)
	require.NoError(t, err)
	require.Equal(t, "host-a", decision.Selected.Node)
	require.Equal(t, "disk-b", decision.Selected.Storage)
	require.Equal(t, uint64(8000), decision.Selected.RemainingDiskBytes)
	require.Len(t, decision.Candidates, 6)

	for _, candidate := range decision.Candidates {
		require.NotEqual(t, "forbidden", candidate.Storage)
	}
	// Determinism is independent of API/list iteration order.
	slices.Reverse(inventory.Nodes)

	for index := range inventory.Nodes {
		slices.Reverse(inventory.Nodes[index].Storages)
	}

	repeated, err := Select(t.Context(), policy, testResources(), inventory, nil)
	require.NoError(t, err)
	require.Equal(t, decision, repeated)
}

func TestSpreadAllowsUpgradeSurgeAndIndependentPools(t *testing.T) {
	policy := testPolicy()
	commitments := []Commitment{
		testCommitment(1, "host-a", policy.GroupID), testCommitment(2, "host-b", policy.GroupID), testCommitment(3, "host-c", policy.GroupID),
	}
	decision, err := Select(t.Context(), policy, testResources(), testInventory(), commitments)
	require.NoError(t, err)
	require.Equal(t, uint64(1), decision.Selected.GroupCount)
	require.Equal(t, "host-a", decision.Selected.Node)

	policy.GroupID = "cluster-a/workers"
	decision, err = Select(t.Context(), policy, testResources(), testInventory(), commitments)
	require.NoError(t, err)
	require.Zero(t, decision.Selected.GroupCount)
	require.Equal(t, uint64(96), decision.Selected.RemainingCPU) // pending control-plane CPU still counts
}

func TestEligibilityExclusions(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Node)
		reason string
	}{
		"offline":            {func(n *Node) { n.Online = false }, "host_offline"},
		"unreachable":        {func(n *Node) { n.Reachable = false }, "host_unreachable"},
		"template not ready": {func(n *Node) { n.TemplateReady = false }, "template_incompatible"},
		"wrong template":     {func(n *Node) { n.TemplateVMID = 9999 }, "template_incompatible"},
		"bridge missing":     {func(n *Node) { n.NetworkReady = false }, "network_incompatible"},
		"allocated CPU":      {func(n *Node) { n.AllocatedCPU = 100 }, "insufficient_cpu"},
		"allocated memory":   {func(n *Node) { n.AllocatedMemoryBytes = 950 }, "insufficient_memory"},
		"actual free memory": {func(n *Node) { n.FreeMemoryBytes = 99 }, "insufficient_memory"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			inventory := testInventory()
			for index := range inventory.Nodes {
				test.mutate(&inventory.Nodes[index])
			}

			decision, err := Select(t.Context(), testPolicy(), testResources(), inventory, nil)
			require.ErrorIs(t, err, ErrNoEligibleDestination)
			require.Len(t, decision.Excluded, 3)

			for _, exclusion := range decision.Excluded {
				require.Equal(t, test.reason, exclusion.Reason)
			}
		})
	}
}

func TestStorageCapacityIsHostLocalAndNoThinOvercommit(t *testing.T) {
	policy := testPolicy()

	inventory := testInventory()
	for index := range inventory.Nodes {
		inventory.Nodes[index].Storages[0].CloneSupported = false
		inventory.Nodes[index].Storages[1].AllocatedBytes = 9500
	}

	decision, err := Select(t.Context(), policy, testResources(), inventory, nil)
	require.ErrorIs(t, err, ErrNoEligibleDestination)
	require.Len(t, decision.Excluded, 6)

	inventory = testInventory()
	// Saturate host-a's allowed storage, not every storage with the same ID.
	commitment := testCommitment(1, "host-a", "other-pool")
	commitment.Resources.DiskBytes = math.MaxUint64
	second := testCommitment(2, "host-a", "other-pool")
	second.Storage = "disk-a"
	second.Resources.DiskBytes = math.MaxUint64
	decision, err = Select(t.Context(), policy, testResources(), inventory, []Commitment{commitment, second})
	require.NoError(t, err)
	require.Equal(t, "host-b", decision.Selected.Node)
	require.Equal(t, "disk-b", decision.Selected.Storage)
}

//nolint:goconst // Literal host names make the migration scenario easy to read.
func TestVisibleMachineNotDoubleCountedAndMigrationFollowed(t *testing.T) {
	policy, inventory := testPolicy(), testInventory()
	commitment := testCommitment(1, "host-a", policy.GroupID)

	inventory.Machines = []Machine{{
		MachineID: commitment.MachineID, OwnerID: commitment.OwnerID, GroupID: commitment.GroupID,
		VMID: commitment.VMID, Node: "host-b", Storage: "disk-b", Resources: commitment.Resources,
	}}
	for index := range inventory.Nodes {
		if inventory.Nodes[index].Name == "host-b" {
			inventory.Nodes[index].AllocatedCPU = commitment.Resources.CPU
		}
	}

	decision, err := Select(t.Context(), policy, testResources(), inventory, []Commitment{commitment})
	require.NoError(t, err)
	require.Equal(t, "host-a", decision.Selected.Node)

	for _, candidate := range decision.Candidates {
		if candidate.Node == "host-b" {
			require.Equal(t, uint64(1), candidate.GroupCount)
			// Observed resources are already in the adapter's node totals.
			require.Equal(t, uint64(96), candidate.RemainingCPU)
		}
	}

	inventory.Machines[0].Resources.CPU = 1 // hardware not fully configured yet
	for index := range inventory.Nodes {
		if inventory.Nodes[index].Name == "host-b" {
			inventory.Nodes[index].AllocatedCPU = 1
		}
	}

	decision, err = Select(t.Context(), policy, testResources(), inventory, []Commitment{commitment})
	require.NoError(t, err)

	for _, candidate := range decision.Candidates {
		if candidate.Node == "host-b" {
			require.Equal(t, uint64(96), candidate.RemainingCPU)
		}
	}

	inventory.Machines[0].OwnerID = "unrelated-owner"
	_, err = Select(t.Context(), policy, testResources(), inventory, []Commitment{commitment})
	require.ErrorIs(t, err, ErrIncompleteInventory)
}

func TestInvalidOrIncompleteEvidenceAndCancellation(t *testing.T) {
	inventory := testInventory()
	inventory.Complete = false
	_, err := Select(t.Context(), testPolicy(), testResources(), inventory, nil)
	require.ErrorIs(t, err, ErrIncompleteInventory)

	inventory = testInventory()
	inventory.Nodes = append(inventory.Nodes, inventory.Nodes[0])
	_, err = Select(t.Context(), testPolicy(), testResources(), inventory, nil)
	require.ErrorIs(t, err, ErrIncompleteInventory)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = Select(ctx, testPolicy(), testResources(), testInventory(), nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = Select(t.Context(), testPolicy(), Resources{}, testInventory(), nil)
	require.Error(t, err)
}

func TestSaturatingResourceArithmetic(t *testing.T) {
	require.Zero(t, remaining(10, math.MaxUint64))
	require.Zero(t, remaining(10, 6, 6))
	require.Equal(t, uint64(math.MaxUint64), add(math.MaxUint64, 1))
	require.Equal(t, uint64(10), add(4, 6))
}

func TestCentralHostReservesApplyAcrossPoolsAndUnsetBlocks(t *testing.T) {
	policy, inventory := testPolicy(), testInventory()
	inventory.CapacityPolicy.Default = CapacityReserve{CPU: 90, MemoryBytes: 800}
	decision, err := Select(t.Context(), policy, testResources(), inventory, nil)
	require.NoError(t, err)
	require.Equal(t, "host-a", decision.Selected.Node)
	require.Equal(t, uint64(8), decision.Selected.RemainingCPU)
	require.Equal(t, uint64(100), decision.Selected.RemainingMemory)

	for index := range inventory.Nodes {
		inventory.Nodes[index].CPU = 100
	}

	inventory.CapacityPolicy.Hosts["host-a"] = CapacityReserve{CPU: 100, MemoryBytes: 1000}
	inventory.CapacityPolicy.Hosts["host-b"] = CapacityReserve{}
	decision, err = Select(t.Context(), policy, testResources(), inventory, nil)
	require.NoError(t, err)
	require.Equal(t, "host-b", decision.Selected.Node)

	inventory.CapacityPolicy.Configured = false
	_, err = Select(t.Context(), policy, testResources(), inventory, nil)
	require.ErrorContains(t, err, "reserve policy is not configured")
}
