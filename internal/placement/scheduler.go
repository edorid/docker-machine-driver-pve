package placement

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
)

var (
	ErrNoEligibleDestination = errors.New("no eligible host and storage")
	ErrIncompleteInventory   = errors.New("placement inventory is incomplete or inconsistent")
)

// Resources are commitments, not current utilization. DiskBytes includes every
// cloned disk, planned expansion and auxiliary/cloud-init volumes.
type Resources struct {
	CPU         uint64 `json:"cpu"`
	MemoryBytes uint64 `json:"memoryBytes"`
	DiskBytes   uint64 `json:"diskBytes"`
}

// Storage contains host-local capacity evidence. AllocatedBytes counts full
// virtual sizes of existing volumes, including stopped machines and templates.
type Storage struct {
	ID             string
	Ready          bool
	CloneSupported bool
	ISOSupported   bool
	TotalBytes     uint64
	FreeBytes      uint64
	AllocatedBytes uint64
}

// CapacityReserve is the amount kept for the Proxmox host rather than guests.
type CapacityReserve struct {
	CPU         uint64 `json:"cpu"`
	MemoryBytes uint64 `json:"memoryBytes"`
}

// CapacityPolicy is centrally configured once per Proxmox cluster. Configured
// distinguishes an intentional zero reserve from the installed unset policy.
type CapacityPolicy struct {
	Configured         bool                       `json:"configured"`
	CPUOvercommitRatio float64                    `json:"cpuOvercommitRatio,omitempty"`
	Default            CapacityReserve            `json:"default"`
	Hosts              map[string]CapacityReserve `json:"hosts"`
}

func (p CapacityPolicy) Validate() error {
	if !p.Configured {
		return errors.New("host capacity reserve policy is not configured")
	}
	if p.CPUOvercommitRatio < 0 || math.IsNaN(p.CPUOvercommitRatio) || math.IsInf(p.CPUOvercommitRatio, 0) || (p.CPUOvercommitRatio > 0 && p.CPUOvercommitRatio < 1) {
		return errors.New("CPU overcommit ratio must be finite and at least 1")
	}

	for host := range p.Hosts {
		if !validHost(host) {
			return errors.New("host capacity reserve has an invalid host override")
		}
	}

	return nil
}

func (p CapacityPolicy) ForHost(host string) CapacityReserve {
	if reserve, found := p.Hosts[host]; found {
		return reserve
	}

	return p.Default
}

// Node is normalized evidence supplied by a discovery adapter, not Proxmox JSON.
// Allocated CPU/memory include ALL VMs and containers, including stopped guests.
// Readiness fields must be established by actual template/bridge/storage checks.
type Node struct {
	Name                  string
	Online                bool
	Reachable             bool
	TemplateVMID          int
	TemplateReady         bool
	TemplateDiskBytes     uint64
	TemplateBootDiskBytes uint64
	NetworkReady          bool
	CPU                   uint64
	AllocatedCPU          uint64
	MemoryBytes           uint64
	FreeMemoryBytes       uint64
	AllocatedMemoryBytes  uint64
	Storages              []Storage
}

// Machine is a uniquely ownership-verified inventory observation. Resources are
// the configured allocation already represented in Node and Storage totals.
type Machine struct {
	MachineID string
	OwnerID   string
	GroupID   string
	VMID      int
	Node      string
	Storage   string
	Resources Resources
}

// Snapshot must include the entire placement domain. A transport adapter is
// responsible for proving permission coverage and obtaining fresh evidence.
type Snapshot struct {
	Complete       bool
	CapacityPolicy CapacityPolicy
	Nodes          []Node
	Machines       []Machine
}

// Commitment remains durable until safe reconciliation, not merely a timeout.
type Commitment struct {
	MachineID          string    `json:"machineId"`
	OwnerID            string    `json:"ownerId"`
	IncarnationID      string    `json:"incarnationId,omitempty"`
	ISOName            string    `json:"isoName,omitempty"`
	ISOStorage         string    `json:"isoStorage,omitempty"`
	RancherClusterName string    `json:"rancherClusterName,omitempty"`
	RancherPoolName    string    `json:"rancherPoolName,omitempty"`
	GroupID            string    `json:"groupId"`
	VMID               int       `json:"vmid"`
	Node               string    `json:"node"`
	Storage            string    `json:"storage"`
	Resources          Resources `json:"resources"`
	BootDiskBytes      uint64    `json:"bootDiskBytes"`
}

type Candidate struct {
	Node               string `json:"node"`
	Storage            string `json:"storage"`
	TemplateVMID       int    `json:"templateVmid"`
	GroupCount         uint64 `json:"groupCount"`
	RemainingCPU       uint64 `json:"remainingCpu"`
	RemainingMemory    uint64 `json:"remainingMemoryBytes"`
	RemainingDiskBytes uint64 `json:"remainingDiskBytes"`
}

type Exclusion struct {
	Node    string `json:"node"`
	Storage string `json:"storage,omitempty"`
	Reason  string `json:"reason"`
}

// Decision preserves eligibility diagnostics even when Select returns no host.
type Decision struct {
	Selected   Candidate   `json:"selected"`
	Candidates []Candidate `json:"candidates"`
	Excluded   []Exclusion `json:"excluded"`
}

type hostUsage struct {
	cpu    uint64
	memory uint64
	count  uint64
	disks  map[string]uint64
}

// Select balances a single group's counts first, then chooses the most available
// allowed storage on that host. It performs no side effects; a caller MUST commit
// its decision using shared compare-and-swap before authorizing a clone.
//
//nolint:cyclop // Eligibility diagnostics are evaluated together for each candidate.
func Select(ctx context.Context, policy Policy, request Resources, inventory Snapshot, commitments []Commitment) (Decision, error) {
	var decision Decision
	if err := ctx.Err(); err != nil {
		return decision, fmt.Errorf("placement context: %w", err)
	}

	if err := policy.Validate(); err != nil {
		return decision, err
	}

	if request.CPU == 0 || request.MemoryBytes == 0 || request.DiskBytes == 0 {
		return decision, errors.New("requested CPU, memory and all-disk capacity must be positive")
	}

	if !inventory.Complete || !consistent(inventory, commitments) {
		return decision, ErrIncompleteInventory
	}

	if err := inventory.CapacityPolicy.Validate(); err != nil {
		return decision, fmt.Errorf("validate host capacity reserves: %w", err)
	}

	usage := account(policy.GroupID, inventory, commitments)
	hosts := slices.Sorted(slices.Values(policy.AllowedHosts))
	storages := slices.Sorted(slices.Values(policy.AllowedStorages))

	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return Decision{}, fmt.Errorf("placement context: %w", err)
		}

		node, found := findNode(inventory.Nodes, host)

		pending := usage[host]
		if pending == nil {
			pending = &hostUsage{disks: make(map[string]uint64)}
		}

		if reason := hostExclusion(node, found, policy.Templates[host], inventory.CapacityPolicy.ForHost(host), inventory.CapacityPolicy.CPUOvercommitRatio, request, pending); reason != "" {
			decision.Excluded = append(decision.Excluded, Exclusion{Node: host, Reason: reason})
			continue
		}

		for _, storageID := range storages {
			storage, found := findStorage(node.Storages, storageID)

			reason := ""

			switch {
			case !found || !storage.Ready:
				reason = "storage_unavailable"
			case !storage.CloneSupported:
				reason = "storage_incompatible"
			case diskAvailable(storage, pending.disks[storageID]) < request.DiskBytes:
				reason = "insufficient_storage"
			}

			if reason != "" {
				decision.Excluded = append(decision.Excluded, Exclusion{Node: host, Storage: storageID, Reason: reason})
				continue
			}

			reserve := inventory.CapacityPolicy.ForHost(host)

			decision.Candidates = append(decision.Candidates, Candidate{
				Node: host, Storage: storageID, TemplateVMID: node.TemplateVMID, GroupCount: pending.count,
				RemainingCPU:       remaining(cpuCapacity(node.CPU, reserve.CPU, inventory.CapacityPolicy.CPUOvercommitRatio), node.AllocatedCPU, pending.cpu, request.CPU),
				RemainingMemory:    remaining(node.MemoryBytes, reserve.MemoryBytes, node.AllocatedMemoryBytes, pending.memory, request.MemoryBytes),
				RemainingDiskBytes: diskAvailable(storage, pending.disks[storageID]) - request.DiskBytes,
			})
		}
	}

	if len(decision.Candidates) == 0 {
		return decision, ErrNoEligibleDestination
	}

	slices.SortFunc(decision.Candidates, compareCandidates)
	decision.Selected = decision.Candidates[0]

	return decision, nil
}

func compareCandidates(left, right Candidate) int {
	if left.GroupCount != right.GroupCount {
		if left.GroupCount < right.GroupCount {
			return -1
		}

		return 1
	}

	if left.Node != right.Node {
		if left.Node < right.Node {
			return -1
		}

		return 1
	}

	if left.RemainingDiskBytes != right.RemainingDiskBytes {
		if left.RemainingDiskBytes > right.RemainingDiskBytes {
			return -1
		}

		return 1
	}

	if left.Storage < right.Storage {
		return -1
	}

	if left.Storage > right.Storage {
		return 1
	}

	return 0
}

func hostExclusion(node Node, found bool, template int, reserve CapacityReserve, cpuOvercommitRatio float64, request Resources, pending *hostUsage) string {
	guestCPUCapacity := cpuCapacity(node.CPU, reserve.CPU, cpuOvercommitRatio)
	guestMemoryCapacity := remaining(node.MemoryBytes, reserve.MemoryBytes)

	switch {
	case !found:
		return "host_missing"
	case !node.Online:
		return "host_offline"
	case !node.Reachable:
		return "host_unreachable"
	case !node.TemplateReady || node.TemplateVMID != template:
		return "template_incompatible"
	case !node.NetworkReady:
		return "network_incompatible"
	case remaining(guestCPUCapacity, node.AllocatedCPU, pending.cpu) < request.CPU:
		return "insufficient_cpu"
	case remaining(guestMemoryCapacity, node.AllocatedMemoryBytes, pending.memory) < request.MemoryBytes ||
		remaining(node.FreeMemoryBytes, pending.memory, reserve.MemoryBytes) < request.MemoryBytes:
		return "insufficient_memory"
	}

	return ""
}

func cpuCapacity(logicalCPUs, reservedCPUs uint64, ratio float64) uint64 {
	if ratio == 0 {
		ratio = 1 // Preserve the no-overcommit behavior of ledgers created before this field existed.
	}
	capacity := float64(remaining(logicalCPUs, reservedCPUs)) * ratio
	if capacity >= float64(math.MaxUint64) {
		return math.MaxUint64
	}
	return uint64(math.Floor(capacity))
}

func diskAvailable(storage Storage, pending uint64) uint64 {
	return remaining(min(storage.FreeBytes, remaining(storage.TotalBytes, storage.AllocatedBytes)), pending)
}

// remaining saturates rather than wrapping on overcommitted/hostile input.
func remaining(total uint64, used ...uint64) uint64 {
	for _, amount := range used {
		if amount >= total {
			return 0
		}

		total -= amount
	}

	return total
}

func add(left, right uint64) uint64 {
	if right > math.MaxUint64-left {
		return math.MaxUint64
	}

	return left + right
}

func account(group string, inventory Snapshot, commitments []Commitment) map[string]*hostUsage {
	usage := make(map[string]*hostUsage)
	get := func(host string) *hostUsage {
		if usage[host] == nil {
			usage[host] = &hostUsage{disks: make(map[string]uint64)}
		}

		return usage[host]
	}

	observed := make(map[string]Machine)
	for _, machine := range inventory.Machines {
		observed[machine.MachineID] = machine

		if machine.GroupID == group {
			get(machine.Node).count++
		}
	}

	for _, commitment := range commitments {
		host, storage := commitment.Node, commitment.Storage
		resources := commitment.Resources

		if machine, found := observed[commitment.MachineID]; found {
			// Follow completed migrations and retain only the not-yet-observed
			// portion of hardware allocation. Never double-count a visible VM.
			host, storage = machine.Node, machine.Storage
			resources.CPU = remaining(resources.CPU, machine.Resources.CPU)
			resources.MemoryBytes = remaining(resources.MemoryBytes, machine.Resources.MemoryBytes)
			resources.DiskBytes = remaining(resources.DiskBytes, machine.Resources.DiskBytes)
		} else if commitment.GroupID == group {
			get(host).count++
		}

		pending := get(host)
		pending.cpu = add(pending.cpu, resources.CPU)
		pending.memory = add(pending.memory, resources.MemoryBytes)
		pending.disks[storage] = add(pending.disks[storage], resources.DiskBytes)
	}

	return usage
}

//nolint:gocyclo,cyclop // Reject each inconsistent inventory and ownership case explicitly.
func consistent(inventory Snapshot, commitments []Commitment) bool {
	nodes := make(map[string]bool)
	for _, node := range inventory.Nodes {
		if !validHost(node.Name) || nodes[node.Name] || node.FreeMemoryBytes > node.MemoryBytes {
			return false
		}

		nodes[node.Name] = true

		storages := make(map[string]bool)
		for _, storage := range node.Storages {
			if !storageID.MatchString(storage.ID) || storages[storage.ID] || storage.FreeBytes > storage.TotalBytes {
				return false
			}

			storages[storage.ID] = true
		}
	}

	observed := make(map[string]Machine)
	vmids := make(map[int]string)

	for _, machine := range inventory.Machines {
		if !validIdentity(machine.MachineID) || !validIdentity(machine.OwnerID) || !validIdentity(machine.GroupID) || !nodes[machine.Node] {
			return false
		}

		if _, exists := observed[machine.MachineID]; exists || machine.VMID < 100 || vmids[machine.VMID] != "" {
			return false
		}

		observed[machine.MachineID] = machine
		vmids[machine.VMID] = machine.MachineID
	}

	seen := make(map[string]bool)
	claimed := make(map[int]bool)

	for _, commitment := range commitments {
		if !validIdentity(commitment.MachineID) || !validIdentity(commitment.OwnerID) || !validIdentity(commitment.GroupID) ||
			!validHost(commitment.Node) || !storageID.MatchString(commitment.Storage) || commitment.VMID < 100 ||
			commitment.BootDiskBytes == 0 || seen[commitment.MachineID] || claimed[commitment.VMID] {
			return false
		}

		seen[commitment.MachineID], claimed[commitment.VMID] = true, true

		if machine, found := observed[commitment.MachineID]; found {
			if machine.OwnerID != commitment.OwnerID || machine.VMID != commitment.VMID || machine.GroupID != commitment.GroupID {
				return false
			}
		}

		if owner := vmids[commitment.VMID]; owner != "" && owner != commitment.MachineID {
			return false
		}
	}

	return true
}

func findNode(nodes []Node, name string) (Node, bool) {
	for _, node := range nodes {
		if node.Name == name {
			return node, true
		}
	}

	return Node{}, false
}

func findStorage(storages []Storage, id string) (Storage, bool) {
	for _, storage := range storages {
		if storage.ID == id {
			return storage, true
		}
	}

	return Storage{}, false
}
