//nolint:wsl // Safety-critical lifecycle code uses explicit, separated validation steps.
package driver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/luthermonson/go-proxmox"
)

const (
	mebibyte = uint64(1024 * 1024)
	kibibyte = uint64(1 << 10)
	tebibyte = uint64(1 << 40)
)

//nolint:wsl // Validate the configuration before constructing its store.
func (d *Driver) newPlacementCoordinator() (placement.Coordinator, error) {
	if d.PlacementGroup == "" {
		return placement.Coordinator{}, errors.New("placement mode is not configured")
	}
	store, err := placement.NewInClusterKubernetesStore(d.PlacementLedgerNamespace, d.PlacementLedgerName)
	if err != nil {
		return placement.Coordinator{}, fmt.Errorf("configure management-cluster placement ledger: %w", err)
	}
	return placement.Coordinator{Store: store, ClusterID: d.PlacementClusterID}, nil
}

//nolint:wsl // Keep CPU and memory validation adjacent to their conversions.
func (d *Driver) placementIntent() (placement.Intent, error) {
	if d.ProcessorSockets == nil || d.ProcessorCores == nil || d.Memory == nil ||
		*d.ProcessorSockets < 1 || *d.ProcessorCores < 1 || *d.Memory < 1 || d.DiskSizeGiB == 0 {
		return placement.Intent{}, errors.New("placement resource request is incomplete")
	}
	if *d.ProcessorSockets > int(^uint(0)>>1) / *d.ProcessorCores || positiveIntToUint64(*d.Memory) > ^uint64(0)/mebibyte {
		return placement.Intent{}, errors.New("placement CPU or memory request overflows")
	}
	cpu := positiveIntToUint64(*d.ProcessorSockets * *d.ProcessorCores)
	memoryBytes := positiveIntToUint64(*d.Memory) * mebibyte
	if d.DiskSizeGiB > ^uint64(0)/gibibyte {
		return placement.Intent{}, errors.New("placement resource request overflows")
	}
	var identityBytes [16]byte
	if _, err := rand.Read(identityBytes[:]); err != nil {
		return placement.Intent{}, fmt.Errorf("generate machine-incarnation identity: %w", err)
	}
	return placement.Intent{
		MachineID:          d.MachineName,
		OwnerID:            d.MachineName,
		IncarnationID:      hex.EncodeToString(identityBytes[:]),
		ISOName:            "pve-cloud-init-" + hex.EncodeToString(identityBytes[:]) + ".iso",
		RancherClusterName: d.RancherClusterName,
		RancherPoolName:    d.RancherMachinePool,
		Policy: placement.Policy{
			GroupID: d.PlacementGroup, AllowedHosts: d.PlacementHosts,
			AllowedStorages: d.PlacementStorages, Templates: d.PlacementTemplates,
		},
		Resources:     placement.Resources{CPU: cpu, MemoryBytes: memoryBytes, DiskBytes: d.DiskSizeGiB * gibibyte},
		BootDiskBytes: d.DiskSizeGiB * gibibyte,
	}, nil
}

//nolint:wsl // Each callback invocation reads fresh state before discovery.
func (d *Driver) placementDiscover(coordinator placement.Coordinator, pendingMachineID string, reservedDiskBytes uint64) placement.Discover {
	return func(ctx context.Context) (placement.Snapshot, error) {
		document, err := coordinator.Store.Read(ctx)
		if err != nil {
			return placement.Snapshot{}, fmt.Errorf("read placement commitments: %w", err)
		}
		snapshot, err := d.discoverPlacement(ctx, ledgerCommitments(document), pendingMachineID)
		if err != nil {
			return placement.Snapshot{}, err
		}
		if _, err := placementDiskRequirement(snapshot, d.PlacementHosts, d.DiskSizeGiB*gibibyte, reservedDiskBytes); err != nil {
			return placement.Snapshot{}, err
		}
		snapshot.CapacityPolicy = document.Ledger.CapacityPolicy
		return snapshot, nil
	}
}

//nolint:wsl // Ledger snapshot and its matching Proxmox evidence are processed together.
func (d *Driver) preparePlacementIntent(ctx context.Context, coordinator placement.Coordinator, intent placement.Intent) (placement.Intent, error) {
	document, err := coordinator.Store.Read(ctx)
	if err != nil {
		return placement.Intent{}, fmt.Errorf("read placement ledger before discovery: %w", err)
	}
	if record, found := document.Ledger.Records[intent.MachineID]; found {
		if record.OwnerID != intent.OwnerID || record.Resources.CPU != intent.Resources.CPU ||
			record.Resources.MemoryBytes != intent.Resources.MemoryBytes || record.BootDiskBytes != intent.BootDiskBytes ||
			(record.RancherClusterName != "" && record.RancherClusterName != intent.RancherClusterName) ||
			(record.RancherPoolName != "" && record.RancherPoolName != intent.RancherPoolName) {
			return placement.Intent{}, placement.ErrIdentityConflict
		}
		intent.Resources.DiskBytes = record.Resources.DiskBytes
		intent.IncarnationID = record.IncarnationID
		intent.ISOName = record.ISOName
		if record.RancherClusterName != "" {
			intent.RancherClusterName = record.RancherClusterName
		}
		if record.RancherPoolName != "" {
			intent.RancherPoolName = record.RancherPoolName
		}
		return intent, nil
	}
	snapshot, err := d.discoverPlacement(ctx, ledgerCommitments(document), intent.MachineID)
	if err != nil {
		return placement.Intent{}, err
	}
	intent.Resources.DiskBytes, err = placementDiskRequirement(snapshot, d.PlacementHosts, d.DiskSizeGiB*gibibyte, 0)
	if err != nil {
		return placement.Intent{}, err
	}
	return intent, nil
}

//nolint:wsl // Flatten the latest ledger records for a fresh Proxmox scan.
func ledgerCommitments(document placement.Document) []placement.Commitment {
	commitments := make([]placement.Commitment, 0, len(document.Ledger.Records))
	for _, record := range document.Ledger.Records {
		commitments = append(commitments, record.Commitment)
	}
	return commitments
}

func (d *Driver) discoverRancherMachineNames(ctx context.Context) error {
	names, err := placement.DiscoverInClusterRancherMachineNames(ctx, d.MachineName)
	if err != nil {
		return err
	}
	if d.RancherClusterName != "" && d.RancherClusterName != names.Cluster {
		return errors.New("configured Rancher cluster name conflicts with CAPI metadata")
	}
	if d.RancherMachinePool != "" && d.RancherMachinePool != names.Pool {
		return errors.New("configured Rancher machine-pool name conflicts with CAPI metadata")
	}
	d.RancherClusterName, d.RancherMachinePool = names.Cluster, names.Pool
	return nil
}

//nolint:cyclop,wsl // Compute a conservative maximum across every currently eligible mapped template.
func placementDiskRequirement(snapshot placement.Snapshot, allowedHosts []string, requestedBootBytes, reservedBytes uint64) (uint64, error) {
	var maximum uint64
	for _, hostname := range allowedHosts {
		var node *placement.Node
		for index := range snapshot.Nodes {
			if snapshot.Nodes[index].Name == hostname {
				node = &snapshot.Nodes[index]
				break
			}
		}
		if node == nil || !node.Online || !node.Reachable || !node.TemplateReady {
			continue
		}
		if node.TemplateBootDiskBytes == 0 || node.TemplateDiskBytes < node.TemplateBootDiskBytes {
			return 0, placement.ErrIncompleteInventory
		}
		if requestedBootBytes < node.TemplateBootDiskBytes {
			return 0, errors.New("requested boot disk is smaller than an allowed host template boot disk")
		}
		final := node.TemplateDiskBytes - node.TemplateBootDiskBytes
		if ^uint64(0)-final < requestedBootBytes {
			return 0, placement.ErrIncompleteInventory
		}
		final += requestedBootBytes
		if reservedBytes > 0 && final > reservedBytes {
			return 0, errors.New("template disk allocation grew beyond the durable reservation")
		}
		if final > maximum {
			maximum = final
		}
	}
	if maximum == 0 {
		return 0, placement.ErrIncompleteInventory
	}
	return maximum, nil
}

//nolint:wsl // Return zero for invalid input, otherwise convert the guarded value.
func positiveIntToUint64(value int) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

//nolint:wsl // Return zero for negative input, otherwise convert the guarded value.
func nonNegativeIntToUint64(value int) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}

//nolint:wsl // Validate the API response before returning an allocated identifier.
func (d *Driver) allocatePlacementVMID(ctx context.Context) (int, error) {
	cluster, err := d.getPVEClient().Cluster(ctx)
	if err != nil {
		return 0, fmt.Errorf("read Proxmox cluster identity: %w", err)
	}
	vmid, err := cluster.NextID(ctx)
	if err != nil || vmid < 100 || vmid > 999999999 {
		return 0, errors.New("proxmox did not provide a valid free VMID")
	}
	return vmid, nil
}

//nolint:wsl // Retry only safe identity conflicts and keep each reservation attempt ordered.
func (d *Driver) reservePlacement(ctx context.Context, coordinator placement.Coordinator, intent placement.Intent, discover placement.Discover) (placement.Record, error) {
	for range 3 {
		document, err := coordinator.Store.Read(ctx)
		if err != nil {
			return placement.Record{}, fmt.Errorf("read placement ledger before reservation: %w", err)
		}
		if existing, found := document.Ledger.Records[intent.MachineID]; found {
			if existing.OwnerID != intent.OwnerID || existing.BootDiskBytes != intent.BootDiskBytes ||
				existing.Resources.CPU != intent.Resources.CPU || existing.Resources.MemoryBytes != intent.Resources.MemoryBytes {
				return placement.Record{}, placement.ErrIdentityConflict
			}
			intent.VMID = existing.VMID
		} else {
			intent.VMID, err = d.allocatePlacementVMID(ctx)
			if err != nil {
				return placement.Record{}, err
			}
		}
		record, _, reserveErr := coordinator.Reserve(ctx, intent, discover)
		if reserveErr == nil {
			return record, nil
		}
		if !errors.Is(reserveErr, placement.ErrIdentityConflict) {
			return placement.Record{}, fmt.Errorf("reserve placement: %w", reserveErr)
		}
	}
	return placement.Record{}, fmt.Errorf("reserve placement after VMID collisions: %w", placement.ErrIdentityConflict)
}

//nolint:cyclop,nestif,wsl // Persist reservation, claim submission, and recover a recorded task in strict order.
func (d *Driver) createPlacedVirtualMachine(ctx context.Context) (int, error) {
	if err := d.discoverRancherMachineNames(ctx); err != nil {
		return 0, fmt.Errorf("discover Rancher machine tags before reserving or cloning: %w", err)
	}
	if err := validateRancherMachineTags(d.RancherClusterName, d.RancherMachinePool); err != nil {
		return 0, err
	}
	coordinator, err := d.newPlacementCoordinator()
	if err != nil {
		return 0, err
	}
	intent, err := d.placementIntent()
	if err != nil {
		return 0, err
	}
	intent, err = d.preparePlacementIntent(ctx, coordinator, intent)
	if err != nil {
		return 0, err
	}
	discover := d.placementDiscover(coordinator, intent.MachineID, intent.Resources.DiskBytes)
	record, err := d.reservePlacement(ctx, coordinator, intent, discover)
	if err != nil {
		return 0, err
	}
	if record.Phase == placement.Submitting {
		return 0, fmt.Errorf("clone submission state is ambiguous and requires operator recovery: %w", placement.ErrUncertainOperation)
	}
	if record.Phase == placement.Reserved {
		if err := d.revalidateReservedPlacement(ctx, coordinator, intent, record); err != nil {
			return 0, err
		}
		record, err = coordinator.BeginSubmission(ctx, intent.MachineID, intent.OwnerID)
		if err != nil {
			return 0, fmt.Errorf("claim clone submission: %w", err)
		}
		vmid, task, err := d.submitPlacedClone(ctx, record)
		if err != nil {
			return 0, err
		}
		taskID, err := cloneTaskIdentity(task, record.Node)
		if err != nil {
			return 0, err
		}
		record, err = coordinator.RecordTask(ctx, intent.MachineID, intent.OwnerID, record.SubmissionToken, taskID)
		if err != nil {
			return 0, fmt.Errorf("persist clone task identity; do not resubmit: %w", err)
		}
		if vmid != record.VMID {
			return 0, placement.ErrIdentityConflict
		}
	}
	if record.Phase != placement.Submitted || record.TaskID == "" {
		return 0, placement.ErrUncertainOperation
	}
	d.placementIncarnationID = record.IncarnationID
	d.placementISOName = record.ISOName
	d.placementISOStorage = record.ISOStorage
	if d.placementISOStorage == "" && record.IncarnationID != "" {
		return 0, errors.New("placement ledger lacks the ISO storage identity; operator recovery required")
	}
	task, err := recordedCloneTask(record.TaskID, record.Node, d.getPVEClient())
	if err != nil {
		return 0, err
	}
	if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
		return 0, fmt.Errorf("wait for recorded clone task: %w", err)
	}
	vm, err := d.getPVEVirtualMachineOnNode(ctx, record.VMID, record.Node)
	if err != nil || vm.Name != record.MachineID {
		return 0, placement.ErrIdentityConflict
	}
	d.PVEMachineID = &record.VMID
	d.placementNode = record.Node
	vm, err = d.getPVEVirtualMachineOnNode(ctx, record.VMID, record.Node)
	if err != nil || vm.VirtualMachineConfig == nil {
		return 0, errors.New("read cloned VM network configuration")
	}
	d.NetworkInterfaceName, err = placementNetworkInterface(vm.VirtualMachineConfig)
	if err != nil {
		return 0, fmt.Errorf("revalidate cloned template networking: %w", err)
	}
	return record.VMID, nil
}

func (d *Driver) revalidateReservedPlacement(ctx context.Context, coordinator placement.Coordinator, intent placement.Intent, record placement.Record) error {
	document, err := coordinator.Store.Read(ctx)
	if err != nil {
		return fmt.Errorf("read ledger before clone submission: %w", err)
	}
	snapshot, err := d.discoverPlacement(ctx, ledgerCommitments(document), intent.MachineID)
	if err != nil {
		return fmt.Errorf("revalidate placement before clone submission: %w", err)
	}
	for _, machine := range snapshot.Machines {
		if machine.MachineID == intent.MachineID || machine.VMID == record.VMID {
			return placement.ErrIdentityConflict
		}
	}

	snapshot.CapacityPolicy = document.Ledger.CapacityPolicy
	commitments := make([]placement.Commitment, 0, len(document.Ledger.Records))

	for machineID, existing := range document.Ledger.Records {
		if machineID != intent.MachineID {
			commitments = append(commitments, existing.Commitment)
		}
	}
	decision, err := placement.Select(ctx, intent.Policy, intent.Resources, snapshot, commitments)
	if err != nil {
		return fmt.Errorf("no longer safe to submit reserved clone: %w", err)
	}
	if record.ISOStorage != "" {
		isoStorage, err := placement.SelectISOStorage(snapshot, record.Node, record.Storage)
		if err != nil || isoStorage != record.ISOStorage {
			return errors.New("reserved initialization ISO storage is unavailable on selected host")
		}
	}
	for _, candidate := range decision.Candidates {
		if candidate.Node == record.Node && candidate.Storage == record.Storage && candidate.TemplateVMID == intent.Policy.Templates[record.Node] {
			return nil
		}
	}

	return errors.New("reserved Proxmox destination is no longer eligible; operator recovery required")
}

//nolint:gocognit,cyclop,nestif,wsl // Removal retains the durable intent across each external side effect.
func (d *Driver) removePlaced(ctx context.Context) error {
	coordinator, err := d.newPlacementCoordinator()
	if err != nil {
		return err
	}
	document, err := coordinator.Store.Read(ctx)
	if err != nil {
		return fmt.Errorf("read placement ownership before removal: %w", err)
	}
	record, found := document.Ledger.Records[d.MachineName]
	if !found {
		vm, _, err := d.findPlacedVM(ctx, 0, d.MachineName, "")
		if err != nil {
			return err
		}
		if vm != nil {
			return placement.ErrIdentityConflict
		}
		return nil
	}
	if record.OwnerID != d.MachineName {
		return placement.ErrIdentityConflict
	}
	if (record.Phase == placement.Submitted || record.Phase == placement.Removing) && record.TaskID != "" {
		task, err := recordedCloneTask(record.TaskID, record.Node, d.getPVEClient())
		if err != nil {
			return err
		}
		waitErr := task.Wait(ctx, pveTaskPollingInterval, pveTaskPollingTimeout)
		if waitErr != nil && !task.IsCompleted {
			return fmt.Errorf("clone task is not terminal; retain removal reservation: %w", waitErr)
		}
	}
	vm, nodeName, err := d.findPlacedVM(ctx, record.VMID, record.MachineID, record.IncarnationID)
	if err != nil {
		return err
	}
	if record.Phase == placement.Reserved && vm != nil {
		return placement.ErrIdentityConflict
	}
	if vm != nil && record.ISOName != "" && nodeName != record.Node {
		return errors.New("placement VM moved from its recorded initialization-media host; retain reservation for operator recovery")
	}
	record, err = coordinator.BeginRemoval(ctx, record.MachineID, record.OwnerID)
	if err != nil {
		return fmt.Errorf("persist placement removal intent: %w", err)
	}
	if vm != nil {
		node, err := d.getPVEClient().Node(ctx, nodeName)
		if err != nil {
			return fmt.Errorf("revalidate VM host before removal: %w", err)
		}
		current, err := node.VirtualMachine(ctx, record.VMID)
		if err != nil || current.Name != record.MachineID || !placementOwnershipMatches(current, record.IncarnationID) {
			return placement.ErrIdentityConflict
		}
		if current.IsRunning() {
			task, err := current.Stop(ctx)
			if err != nil {
				return fmt.Errorf("stop recorded placement VM: %w", err)
			}
			if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
				return fmt.Errorf("wait for recorded VM stop: %w", err)
			}
			current, err = node.VirtualMachine(ctx, record.VMID)
			if err != nil || current.Name != record.MachineID || !placementOwnershipMatches(current, record.IncarnationID) {
				return placement.ErrIdentityConflict
			}
		}
		task, err := current.Delete(ctx)
		if err != nil {
			return fmt.Errorf("delete recorded placement VM: %w", err)
		}
		if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
			return fmt.Errorf("wait for recorded VM deletion: %w", err)
		}
	}
	remaining, _, err := d.findPlacedVM(ctx, record.VMID, record.MachineID, record.IncarnationID)
	if err != nil {
		return err
	}
	if remaining != nil {
		return errors.New("recorded VM remains visible after deletion; retaining placement reservation")
	}
	if err := d.removePlacementISO(ctx, record); err != nil {
		return fmt.Errorf("remove owned placement initialization ISO: %w", err)
	}
	if err := coordinator.CompleteRemoval(ctx, record.MachineID, record.OwnerID); err != nil {
		return fmt.Errorf("release placement reservation after verified deletion: %w", err)
	}
	return nil
}

func (d *Driver) removePlacementISO(ctx context.Context, record placement.Record) error {
	if record.ISOName == "" {
		// Records made before generated initialization media remain removable using
		// the legacy cloud-init drive and ownership-tag proof.
		return nil
	}
	node, err := d.getPVEClient().Node(ctx, record.Node)
	if err != nil {
		return fmt.Errorf("read recorded ISO host %q: %w", record.Node, err)
	}
	if record.ISOStorage == "" {
		return errors.New("placement ledger lacks ISO storage ownership; retain placement reservation")
	}
	storage, err := node.Storage(ctx, record.ISOStorage)
	if err != nil || storage.Enabled != 1 || storage.Active != 1 || !hasContent(storage.Content, "iso") {
		return errors.New("recorded ISO storage is unavailable; retain placement reservation")
	}
	contents, err := storage.GetContent(ctx)
	if err != nil {
		return fmt.Errorf("inspect recorded ISO storage: %w", err)
	}
	volid := record.ISOStorage + ":iso/" + record.ISOName
	found := false
	for _, item := range contents {
		if item == nil || item.Volid == "" {
			return placement.ErrIncompleteInventory
		}
		if item.Volid == volid {
			found = true
		}
	}
	statuses, err := d.getPVEClient().Nodes(ctx)
	if err != nil || len(statuses) == 0 {
		return errors.New("cannot verify placement ISO is unused cluster-wide")
	}
	seen := make(map[string]bool)
	for _, status := range statuses {
		if status == nil || status.Node == "" || seen[status.Node] {
			return placement.ErrIncompleteInventory
		}
		seen[status.Node] = true
		node, err := d.getPVEClient().Node(ctx, status.Node)
		if err != nil {
			return err
		}
		vms, err := node.VirtualMachines(ctx)
		if err != nil {
			return err
		}
		for _, vm := range vms {
			if vm == nil || safeVMID(vm.VMID) == 0 {
				return placement.ErrIncompleteInventory
			}
			full, err := node.VirtualMachine(ctx, safeVMID(vm.VMID))
			if err != nil || full.VirtualMachineConfig == nil {
				return errors.New("cannot verify placement ISO is unused cluster-wide")
			}
			for _, disk := range full.VirtualMachineConfig.MergeDisks() {
				if strings.HasPrefix(disk, volid+",") || disk == volid {
					return errors.New("recorded placement ISO remains in use; retain placement reservation")
				}
			}
		}
	}
	if !found {
		return nil
	}
	iso, err := storage.ISO(ctx, record.ISOName)
	if err != nil {
		return fmt.Errorf("read owned placement ISO before deletion: %w", err)
	}
	task, err := iso.Delete(ctx)
	if err != nil {
		return fmt.Errorf("delete owned placement ISO: %w", err)
	}
	if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
		return fmt.Errorf("wait for placement ISO deletion: %w", err)
	}
	contents, err = storage.GetContent(ctx)
	if err != nil {
		return fmt.Errorf("verify placement ISO deletion: %w", err)
	}
	for _, item := range contents {
		if item == nil || item.Volid == "" {
			return placement.ErrIncompleteInventory
		}
		if item.Volid == volid {
			return errors.New("placement ISO remains visible after deletion; retain reservation")
		}
	}
	return nil
}

// A task's ID is its resource identifier, not its unique Proxmox UPID.
func cloneTaskIdentity(task *proxmox.Task, node string) (string, error) {
	if task == nil {
		return "", fmt.Errorf("clone task identity is missing; operator recovery required: %w", placement.ErrUncertainOperation)
	}
	if _, err := recordedCloneTask(string(task.UPID), node, nil); err != nil {
		return "", err
	}
	return string(task.UPID), nil
}

func recordedCloneTask(id, node string, client *proxmox.Client) (*proxmox.Task, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 9 || parts[0] != "UPID" || parts[1] != node || node == "" ||
		parts[2] == "" || parts[3] == "" || parts[4] == "" || parts[5] != "qmclone" || parts[6] == "" || parts[7] == "" || parts[8] != "" {
		return nil, fmt.Errorf("recorded clone task identity is invalid; operator recovery required: %w", placement.ErrUncertainOperation)
	}
	return proxmox.NewTask(proxmox.UPID(id), client), nil
}

//nolint:gocognit,gocyclo,cyclop,wsl // Complete cluster scan proves absence before releasing capacity.
func (d *Driver) findPlacedVM(ctx context.Context, vmid int, name, incarnationID string) (*proxmox.VirtualMachine, string, error) {
	statuses, err := d.getPVEClient().Nodes(ctx)
	if err != nil || len(statuses) == 0 {
		return nil, "", errors.New("cannot prove placement VM absence from complete Proxmox node inventory")
	}
	var foundVM *proxmox.VirtualMachine
	foundNode := ""
	seenNodes := make(map[string]bool)
	for _, status := range statuses {
		if status == nil || status.Node == "" || seenNodes[status.Node] {
			return nil, "", placement.ErrIncompleteInventory
		}
		seenNodes[status.Node] = true
		node, err := d.getPVEClient().Node(ctx, status.Node)
		if err != nil {
			return nil, "", fmt.Errorf("inspect Proxmox node %q for removal: %w", status.Node, err)
		}
		vms, err := node.VirtualMachines(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("inspect VMs on Proxmox node %q: %w", status.Node, err)
		}
		for _, candidate := range vms {
			if candidate == nil {
				return nil, "", placement.ErrIncompleteInventory
			}
			candidateID := safeVMID(candidate.VMID)
			if candidateID == 0 {
				return nil, "", placement.ErrIncompleteInventory
			}
			if candidate.Name == name && vmid != 0 && candidateID != vmid {
				return nil, "", placement.ErrIdentityConflict
			}
			if (vmid != 0 && candidateID == vmid) || (vmid == 0 && candidate.Name == name) {
				if candidate.Name != name || foundVM != nil {
					return nil, "", placement.ErrIdentityConflict
				}
				full, err := node.VirtualMachine(ctx, candidateID)
				if err != nil || full.Name != name || !placementOwnershipMatches(full, incarnationID) {
					return nil, "", placement.ErrIdentityConflict
				}
				foundVM, foundNode = full, status.Node
			}
		}
		containers, err := node.Containers(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("inspect containers on Proxmox node %q: %w", status.Node, err)
		}
		for _, container := range containers {
			if container == nil || container.VMID < 100 || container.VMID > 999999999 {
				return nil, "", placement.ErrIncompleteInventory
			}
			if container.Name == name || (vmid != 0 && container.VMID == proxmox.StringOrUint64(vmid)) {
				return nil, "", placement.ErrIdentityConflict
			}
		}
	}
	return foundVM, foundNode, nil
}

func placementOwnershipMatches(vm *proxmox.VirtualMachine, incarnationID string) bool {
	if vm == nil || vm.VirtualMachineConfig == nil {
		return false
	}
	if incarnationID == "" {
		return vm.HasTag(pveMachineTag)
	}
	for _, line := range strings.Split(vm.VirtualMachineConfig.Description, "\n") {
		if strings.HasPrefix(line, placementIdentityMarker) {
			return strings.TrimPrefix(line, placementIdentityMarker) == incarnationID
		}
	}
	return false
}

func safeVMID(value proxmox.StringOrUint64) int {
	if value < 100 || value > 999999999 {
		return 0
	}
	//nolint:gosec // The Proxmox VMID protocol range is within every supported Go int.
	return int(value)
}

//nolint:wsl // Keep selected-template validation adjacent to the clone request.
func (d *Driver) submitPlacedClone(ctx context.Context, record placement.Record) (int, *proxmox.Task, error) {
	templateID := d.PlacementTemplates[record.Node]
	template, err := d.getPVEVirtualMachineOnNode(ctx, templateID, record.Node)
	if err != nil || template.VirtualMachineConfig == nil || template.VirtualMachineConfig.Template != 1 {
		return 0, nil, errors.New("selected host-local template is unavailable or invalid")
	}
	vmid, task, err := template.Clone(ctx, &proxmox.VirtualMachineCloneOptions{
		NewID: record.VMID, Name: record.MachineID, Pool: d.ResourcePoolName,
		Full: 1, Storage: record.Storage,
	})
	if err != nil {
		return vmid, task, fmt.Errorf("submit selected-host Proxmox clone; do not retry automatically: %w", err)
	}
	if task == nil {
		return vmid, nil, errors.New("proxmox accepted clone without returning a task; do not retry automatically")
	}
	return vmid, task, nil
}
