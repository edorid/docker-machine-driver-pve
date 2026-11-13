//nolint:wsl // Inventory validation is deliberately separated into explicit fail-closed checks.
package driver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/luthermonson/go-proxmox"
)

// discoverPlacement obtains a fresh cluster-wide inventory. Any failed endpoint
// makes the snapshot unusable; it never treats a partial API response as complete.
//
//nolint:wsl // Assemble only complete per-node snapshots.
func (d *Driver) discoverPlacement(ctx context.Context, commitments []placement.Commitment, pendingMachineID string) (placement.Snapshot, error) {
	client := d.getPVEClient()
	statuses, err := client.Nodes(ctx)
	if err != nil || len(statuses) == 0 {
		return placement.Snapshot{}, errors.New("proxmox node inventory is unavailable or empty")
	}
	byVMID, byName, err := indexCommitments(commitments)
	if err != nil {
		return placement.Snapshot{}, err
	}

	snapshot := placement.Snapshot{Complete: true}
	seenNodes := make(map[string]bool)
	seenVMIDs := make(map[int]bool)
	for _, status := range statuses {
		if status == nil || status.Node == "" || seenNodes[status.Node] {
			return placement.Snapshot{}, placement.ErrIncompleteInventory
		}
		seenNodes[status.Node] = true
		if status.Status == "offline" || status.Online == 0 && status.Status == "" {
			snapshot.Nodes = append(snapshot.Nodes, placement.Node{Name: status.Node, Online: false, Reachable: false})
			continue
		}
		observation, err := d.discoverNode(ctx, client, status, byVMID, byName, pendingMachineID, seenVMIDs)
		if err != nil {
			return placement.Snapshot{}, err
		}
		snapshot.Nodes = append(snapshot.Nodes, observation.node)
		snapshot.Machines = append(snapshot.Machines, observation.machines...)
	}

	return snapshot, nil
}

type nodeObservation struct {
	node     placement.Node
	machines []placement.Machine
}

//nolint:wsl // Build both lookup indexes from the same validated commitment set.
func indexCommitments(commitments []placement.Commitment) (map[int]placement.Commitment, map[string]placement.Commitment, error) {
	byVMID := make(map[int]placement.Commitment, len(commitments))
	byName := make(map[string]placement.Commitment, len(commitments))
	for _, commitment := range commitments {
		if _, exists := byVMID[commitment.VMID]; exists || byName[commitment.MachineID].MachineID != "" {
			return nil, nil, placement.ErrIncompleteInventory
		}
		byVMID[commitment.VMID], byName[commitment.MachineID] = commitment, commitment
	}
	return byVMID, byName, nil
}

//nolint:cyclop,wsl // The node inventory boundary keeps every required endpoint fail-closed.
func (d *Driver) discoverNode(
	ctx context.Context,
	client *proxmox.Client,
	status *proxmox.NodeStatus,
	byVMID map[int]placement.Commitment,
	byName map[string]placement.Commitment,
	pendingMachineID string,
	seenVMIDs map[int]bool,
) (nodeObservation, error) {
	var result nodeObservation
	if status.MaxCPU < 0 {
		return result, placement.ErrIncompleteInventory
	}
	node, err := client.Node(ctx, status.Node)
	if err != nil {
		return result, fmt.Errorf("read Proxmox node %q status: %w", status.Node, err)
	}
	storages, err := node.Storages(ctx)
	if err != nil {
		return result, fmt.Errorf("read Proxmox node %q storages: %w", status.Node, err)
	}
	vms, err := node.VirtualMachines(ctx)
	if err != nil {
		return result, fmt.Errorf("read Proxmox node %q VMs: %w", status.Node, err)
	}
	containers, err := node.Containers(ctx)
	if err != nil {
		return result, fmt.Errorf("read Proxmox node %q containers: %w", status.Node, err)
	}
	networks, err := node.Networks(ctx)
	if err != nil {
		return result, fmt.Errorf("read Proxmox node %q networks: %w", status.Node, err)
	}
	storageRecords, err := normalizedStorages(storages)
	if err != nil {
		return result, err
	}
	bridges := make(map[string]bool)
	for _, network := range networks {
		if network != nil && network.Type == "bridge" && network.Iface != "" {
			bridges[network.Iface] = true
		}
	}
	templateID, templateReady, networkReady, templateDiskBytes, templateBootDiskBytes, err := d.inspectPlacementTemplate(ctx, node, status.Node, bridges)
	if err != nil {
		return result, fmt.Errorf("inspect host-local template on Proxmox node %q: %w", status.Node, err)
	}
	result.node = placement.Node{
		Name: status.Node, Online: status.Online == 1 || status.Status == "online", Reachable: true,
		TemplateVMID: templateID, TemplateReady: templateReady, NetworkReady: networkReady,
		TemplateDiskBytes: templateDiskBytes, TemplateBootDiskBytes: templateBootDiskBytes,
		CPU: nonNegativeIntToUint64(status.MaxCPU), MemoryBytes: status.MaxMem, FreeMemoryBytes: node.Memory.Free,
		Storages: storageRecords,
	}
	for _, vm := range vms {
		machine, tracked, err := inspectVM(ctx, node, vm, byVMID, byName, pendingMachineID, seenVMIDs)
		if err != nil {
			return nodeObservation{}, err
		}
		result.node.AllocatedCPU += nonNegativeIntToUint64(vm.CPUs)
		result.node.AllocatedMemoryBytes += vm.MaxMem
		if tracked {
			result.machines = append(result.machines, machine)
		}
	}
	for _, container := range containers {
		if container == nil || container.CPUs < 0 {
			return nodeObservation{}, placement.ErrIncompleteInventory
		}
		vmid := safeVMID(container.VMID)
		if vmid == 0 {
			return nodeObservation{}, placement.ErrIncompleteInventory
		}
		if pendingMachineID != "" && container.Name == pendingMachineID {
			return nodeObservation{}, placement.ErrIdentityConflict
		}
		if seenVMIDs[vmid] {
			return nodeObservation{}, placement.ErrIncompleteInventory
		}
		seenVMIDs[vmid] = true
		result.node.AllocatedCPU += nonNegativeIntToUint64(container.CPUs)
		result.node.AllocatedMemoryBytes += container.MaxMem
	}
	return result, nil
}

//nolint:wsl // Convert every storage row or reject the whole inventory.
func normalizedStorages(storages proxmox.Storages) ([]placement.Storage, error) {
	result := make([]placement.Storage, 0, len(storages))
	for _, storage := range storages {
		if storage == nil || storage.Name == "" || storage.Avail > storage.Total {
			return nil, placement.ErrIncompleteInventory
		}
		result = append(result, placement.Storage{
			ID: storage.Name, Ready: storage.Enabled == 1 && storage.Active == 1 && storage.Total > 0,
			CloneSupported: hasContent(storage.Content, "images"), ISOSupported: hasContent(storage.Content, "iso"), TotalBytes: storage.Total,
			FreeBytes: storage.Avail, AllocatedBytes: storage.Total - storage.Avail,
		})
	}
	return result, nil
}

//nolint:cyclop,wsl // A tracked VM must pass every ownership check before entering inventory.
func inspectVM(
	ctx context.Context,
	node *proxmox.Node,
	vm *proxmox.VirtualMachine,
	byVMID map[int]placement.Commitment,
	byName map[string]placement.Commitment,
	pendingMachineID string,
	seenVMIDs map[int]bool,
) (placement.Machine, bool, error) {
	if vm == nil || vm.VMID == 0 || vm.VMID > 999999999 || vm.CPUs < 0 {
		return placement.Machine{}, false, placement.ErrIncompleteInventory
	}
	vmid := safeVMID(vm.VMID)
	if vmid == 0 {
		return placement.Machine{}, false, placement.ErrIncompleteInventory
	}
	if pendingMachineID != "" && vm.Name == pendingMachineID && byName[pendingMachineID].MachineID == "" {
		return placement.Machine{}, false, placement.ErrIdentityConflict
	}
	if commitment, found := byName[vm.Name]; found && commitment.VMID != vmid {
		return placement.Machine{}, false, placement.ErrIdentityConflict
	}
	if seenVMIDs[vmid] {
		return placement.Machine{}, false, placement.ErrIncompleteInventory
	}
	seenVMIDs[vmid] = true
	commitment, found := byVMID[vmid]
	if !found {
		return placement.Machine{}, false, nil
	}
	if vm.Name != commitment.MachineID || (commitment.IncarnationID == "" && !tagPresent(vm.Tags, pveMachineTag)) {
		return placement.Machine{}, false, placement.ErrIdentityConflict
	}
	observedVM, err := node.VirtualMachine(ctx, vmid)
	if err != nil || observedVM.Name != commitment.MachineID || !placementOwnershipMatches(observedVM, commitment.IncarnationID) || observedVM.VirtualMachineConfig == nil {
		return placement.Machine{}, false, placement.ErrIdentityConflict
	}
	storage, err := vmStorageLocation(observedVM.VirtualMachineConfig)
	if err != nil {
		return placement.Machine{}, false, err
	}
	return placement.Machine{
		MachineID: commitment.MachineID, OwnerID: commitment.OwnerID, GroupID: commitment.GroupID,
		VMID: vmid, Node: vm.Node, Storage: storage, Resources: commitment.Resources,
	}, true, nil
}

//nolint:wsl // Template lookup and its eligibility evidence are one fail-closed operation.
func (d *Driver) inspectPlacementTemplate(ctx context.Context, node *proxmox.Node, nodeName string, bridges map[string]bool) (int, bool, bool, uint64, uint64, error) {
	vmid, configured := d.PlacementTemplates[nodeName]
	if !configured {
		return 0, false, false, 0, 0, nil
	}
	template, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		return vmid, false, false, 0, 0, fmt.Errorf("read template VMID %d: %w", vmid, err)
	}
	if template.VirtualMachineConfig == nil || template.VirtualMachineConfig.Template != 1 {
		return vmid, false, false, 0, 0, nil
	}
	if !hasCloudInitDisk(template.VirtualMachineConfig) || template.VirtualMachineConfig.CICustom != "" ||
		(template.VirtualMachineConfig.Agent != "1" && !strings.Contains(template.VirtualMachineConfig.Agent, "enabled=1")) {
		return vmid, false, false, 0, 0, nil
	}
	diskBytes, bootDiskBytes, err := templateDiskSizesWithResolver(template.VirtualMachineConfig, storageVolumeSizeResolver(ctx, node))
	if err != nil {
		return vmid, false, false, 0, 0, fmt.Errorf("read template disk evidence: %w", err)
	}
	interfaceName, err := placementNetworkInterface(template.VirtualMachineConfig)
	if err != nil {
		return vmid, true, false, diskBytes, bootDiskBytes, nil
	}
	device := template.VirtualMachineConfig.MergeNets()[interfaceName]
	bridge := configValue(device, "bridge")
	return vmid, true, bridge != "" && bridges[bridge], diskBytes, bootDiskBytes, nil
}

func hasContent(content, expected string) bool {
	for _, item := range strings.Split(content, ",") {
		if strings.TrimSpace(item) == expected {
			return true
		}
	}
	return false
}

func tagPresent(tags, expected string) bool {
	for _, tag := range strings.Split(tags, ";") {
		if tag == expected {
			return true
		}
	}
	return false
}

func configValue(device, key string) string {
	for _, item := range strings.Split(device, ",") {
		name, value, found := strings.Cut(item, "=")
		if found && name == key {
			return value
		}
	}
	return ""
}

//nolint:wsl // Validate every attached disk before treating storage location as known.
func vmStorageLocation(config *proxmox.VirtualMachineConfig) (string, error) {
	storage := ""
	devices := config.MergeDisks()
	if config.EFIDisk0 != "" {
		devices["efidisk0"] = config.EFIDisk0
	}
	if config.TPMState0 != "" {
		devices["tpmstate0"] = config.TPMState0
	}
	for _, device := range devices {
		if device == "none" || strings.Contains(device, "media=cdrom") {
			continue
		}
		volume, _, _ := strings.Cut(device, ",")
		if volume == "" {
			volume = device
		}
		storageID, _, foundColon := strings.Cut(volume, ":")
		if !foundColon || storageID == "" {
			return "", placement.ErrIncompleteInventory
		}
		if storage != "" && storage != storageID {
			return "", placement.ErrIncompleteInventory
		}
		storage = storageID
	}
	if storage == "" {
		return "", placement.ErrIncompleteInventory
	}
	return storage, nil
}

//nolint:wsl // Disk sizes are accumulated before validating configured boot order.
func templateDiskSizes(config *proxmox.VirtualMachineConfig) (uint64, uint64, error) {
	return templateDiskSizesWithResolver(config, nil)
}

func templateDiskSizesWithResolver(config *proxmox.VirtualMachineConfig, resolve func(string) (uint64, error)) (uint64, uint64, error) {
	devices := config.MergeDisks()
	if config.EFIDisk0 != "" {
		devices["efidisk0"] = config.EFIDisk0
	}
	if config.TPMState0 != "" {
		devices["tpmstate0"] = config.TPMState0
	}
	sizes := make(map[string]uint64)
	var total uint64
	for name, value := range devices {
		if value == "none" || strings.Contains(value, "media=cdrom") {
			continue
		}
		if strings.Contains(value, "cloudinit") || name == "tpmstate0" {
			if ^uint64(0)-total < cloudInitAuxiliaryBytes {
				return 0, 0, placement.ErrIncompleteInventory
			}
			total += cloudInitAuxiliaryBytes
			continue
		}
		size, err := volumeSize(value, resolve)
		if err != nil || ^uint64(0)-total < size {
			return 0, 0, placement.ErrIncompleteInventory
		}
		sizes[name] = size
		total += size
	}
	_, bootEntries, found := strings.Cut(config.Boot, "order=")
	if !found {
		return 0, 0, placement.ErrIncompleteInventory
	}
	order, _, _ := strings.Cut(bootEntries, ",")
	bootDisk := uint64(0)
	for _, name := range strings.Split(order, ";") {
		if size, found := sizes[name]; found {
			bootDisk = size
			break
		}
	}
	if bootDisk == 0 {
		return 0, 0, placement.ErrIncompleteInventory
	}
	return total, bootDisk, nil
}

// volumeSize only consults storage for an omitted size, not an invalid size.
func volumeSize(value string, resolve func(string) (uint64, error)) (uint64, error) {
	for _, option := range strings.Split(value, ",")[1:] {
		if strings.HasPrefix(option, "size=") {
			return parsePVEVolumeSize(value)
		}
	}
	volume, _, _ := strings.Cut(value, ",")
	if resolve == nil || volume == "" {
		return 0, errors.New("proxmox volume size is unavailable")
	}
	return resolve(volume)
}

// storageVolumeSizeResolver requires one exact, nonzero volume match on the VM's node.
func storageVolumeSizeResolver(ctx context.Context, node *proxmox.Node) func(string) (uint64, error) {
	return func(volume string) (uint64, error) {
		storageID, name, found := strings.Cut(volume, ":")
		if !found || storageID == "" || name == "" {
			return 0, errors.New("proxmox volume identifier is invalid")
		}
		storages, err := node.Storages(ctx)
		if err != nil {
			return 0, errors.New("proxmox storage inventory is unavailable")
		}
		var selected *proxmox.Storage
		for _, storage := range storages {
			if storage != nil && storage.Name == storageID {
				if selected != nil {
					return 0, errors.New("proxmox storage identity is ambiguous")
				}
				selected = storage
			}
		}
		if selected == nil {
			return 0, errors.New("proxmox volume storage is unavailable")
		}
		contents, err := selected.GetContent(ctx)
		if err != nil {
			return 0, errors.New("proxmox storage content is unavailable")
		}
		var size uint64
		foundVolume := false
		for _, content := range contents {
			if content != nil && content.Volid == volume {
				if foundVolume || content.Size == 0 {
					return 0, errors.New("proxmox volume size is ambiguous or invalid")
				}
				foundVolume, size = true, content.Size
			}
		}
		if !foundVolume {
			return 0, errors.New("proxmox volume is unavailable in storage content")
		}
		return size, nil
	}
}

//nolint:wsl // Parse each size option and its unit as one validation branch.
func parsePVEVolumeSize(value string) (uint64, error) {
	for _, option := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(option, "=")
		if !found || key != "size" {
			continue
		}
		if raw == "" {
			break
		}
		unit := raw[len(raw)-1]
		multiplier := uint64(1)
		if unit < '0' || unit > '9' {
			raw = raw[:len(raw)-1]
			switch unit {
			case 'K':
				multiplier = kibibyte
			case 'M':
				multiplier = mebibyte
			case 'G':
				multiplier = gibibyte
			case 'T':
				multiplier = tebibyte
			default:
				return 0, errors.New("unsupported Proxmox volume size unit")
			}
		}
		amount, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || amount == 0 || amount > ^uint64(0)/multiplier {
			return 0, errors.New("invalid Proxmox volume size")
		}
		return amount * multiplier, nil
	}
	return 0, errors.New("proxmox volume size is unavailable")
}
