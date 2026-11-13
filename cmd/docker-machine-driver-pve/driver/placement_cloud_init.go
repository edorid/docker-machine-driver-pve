//nolint:wsl // Provisioning steps remain visibly ordered rather than whitespace-cuddled.
package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/luthermonson/go-proxmox"
	"github.com/rancher/machine/libmachine/log"
	yaml "gopkg.in/yaml.v3"
)

// placementCloudInitNetwork uses the clone's MAC rather than assuming a guest
// interface name. It refuses to silently discard any other template interfaces.
func placementCloudInitNetwork(config *proxmox.VirtualMachineConfig, interfaceName string) (string, error) {
	if config == nil || len(config.MergeNets()) != 1 {
		return "", errors.New("generated cloud-init networking requires exactly one interface")
	}
	index, err := networkInterfaceNumber(interfaceName)
	if err != nil {
		return "", err
	}
	if err := requireDynamicCloudInitAddress(config, interfaceName, index); err != nil {
		return "", err
	}
	device := config.MergeNets()[interfaceName]
	model, macValue, found := strings.Cut(strings.SplitN(device, ",", 2)[0], "=")
	mac, err := net.ParseMAC(macValue)
	if !found || model == "" || err != nil || len(mac) != 6 {
		return "", errors.New("generated cloud-init networking requires an explicit Ethernet MAC address")
	}
	network := map[string]any{
		"version": 2,
		"ethernets": map[string]any{
			"primary": map[string]any{"match": map[string]string{"macaddress": strings.ToLower(mac.String())}, "dhcp4": true},
		},
	}
	data, err := yaml.Marshal(network)
	if err != nil {
		return "", fmt.Errorf("encode generated cloud-init network: %w", err)
	}
	return string(data), nil
}

const (
	cloudInitAuxiliaryBytes = 4 * mebibyte
	placementTagCapacity    = 2
	rancherTagValueMax      = 48
)

const placementIdentityMarker = "pve-machine-incarnation="

func placementDescription(description, identity string) (string, error) {
	if identity == "" {
		return description, nil
	}
	lines := strings.Split(description, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, placementIdentityMarker) {
			if strings.TrimPrefix(line, placementIdentityMarker) != identity {
				return "", errors.New("VM description contains a conflicting machine-incarnation identity")
			}
			continue
		}
		kept = append(kept, line)
	}
	base := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if base != "" {
		base += "\n"
	}
	return base + placementIdentityMarker + identity, nil
}

//nolint:contextcheck // The driver SSH wait uses its own bounded timeout; other operations use ctx.
func (d *Driver) initializePlaced(ctx context.Context) error {
	if d.PVEMachineID == nil || d.placementNode == "" {
		return errors.New("placed machine identity is unavailable")
	}

	vm, err := d.getPVEVirtualMachineOnNode(ctx, *d.PVEMachineID, d.placementNode)

	if err != nil || vm.VirtualMachineConfig == nil || vm.Name != d.MachineName {
		return errors.New("cloned Proxmox machine identity could not be verified")
	}
	if d.placementIncarnationID != "" {
		if err := d.discoverRancherMachineNames(ctx); err != nil {
			return fmt.Errorf("revalidate Rancher machine metadata: %w", err)
		}
		if err := validateRancherMachineTags(d.RancherClusterName, d.RancherMachinePool); err != nil {
			return err
		}
	}

	d.Tags = d.managedMachineTags(vm.Tags)
	description, err := placementDescription(vm.VirtualMachineConfig.Description, d.placementIncarnationID)
	if err != nil {
		return err
	}

	options := []proxmox.VirtualMachineOption{{Name: "tags", Value: d.Tags}}
	if d.placementIncarnationID != "" {
		options = append(options, proxmox.VirtualMachineOption{Name: "description", Value: description})
	}
	task, err := vm.Config(ctx, options...)
	if err != nil {
		return fmt.Errorf("tag placed VM: %w", err)
	}

	if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
		return fmt.Errorf("wait for placed VM ownership tag: %w", err)
	}

	if err := d.resizePlacedBootDisk(ctx); err != nil {
		return err
	}

	if err := d.setupHardware(ctx); err != nil {
		return err
	}

	if d.placementIncarnationID != "" {
		if err := d.setupGeneratedPlacementISO(ctx, vm); err != nil {
			return err
		}
	} else if err := d.setupNativeCloudInit(ctx); err != nil {
		// Existing placement reservations and VMs retain the old native-drive path.
		return err
	}

	if err := d.Start(); err != nil {
		return err
	}

	if err := d.waitForCloudinit(); err != nil {
		return fmt.Errorf("wait for cloud-init: %w", err)
	}

	log.Info("Placed VM is initialized")

	return nil
}

// setupGeneratedPlacementISO uploads the ledger-named ISO before attaching it.
// Existing content is never overwritten: any prior/ambiguous upload requires recovery.
func (d *Driver) setupGeneratedPlacementISO(ctx context.Context, vm *proxmox.VirtualMachine) error {
	if d.placementISOName == "" || d.placementISOStorage == "" || d.placementIncarnationID == "" {
		return errors.New("durable placement initialization-media identity is missing")
	}
	config := vm.VirtualMachineConfig
	if config == nil || config.CICustom != "" {
		return errors.New("placement templates with custom cloud-init snippets are unsupported")
	}
	interfaceName, err := placementNetworkInterface(config)
	if err != nil {
		return err
	}
	network, err := placementCloudInitNetwork(config, interfaceName)
	if err != nil {
		return err
	}
	userdata, err := d.generateCloudinitUserdata()
	if err != nil {
		return err
	}
	metadata, err := d.generateCloudinitMetadata()
	if err != nil {
		return err
	}
	metadata, err = placementMetadata(metadata, d.MachineName, d.placementIncarnationID)
	if err != nil {
		return err
	}
	uploadDir, err := os.MkdirTemp(os.TempDir(), "pve-placement-iso-upload-")
	if err != nil {
		return fmt.Errorf("create private initialization ISO directory: %w", err)
	}
	defer os.RemoveAll(uploadDir)
	isoPath, err := buildPlacementISO(uploadDir, userdata, metadata, network)
	if err != nil {
		return fmt.Errorf("build machine initialization ISO: %w", err)
	}
	uploadPath := filepath.Join(uploadDir, d.placementISOName)
	if err := os.Rename(isoPath, uploadPath); err != nil {
		return fmt.Errorf("name initialization ISO for Proxmox upload: %w", err)
	}
	node, err := d.getPVEClient().Node(ctx, d.placementNode)
	if err != nil {
		return fmt.Errorf("read ISO upload node: %w", err)
	}
	storage, err := node.Storage(ctx, d.placementISOStorage)
	if err != nil || storage.Enabled != 1 || storage.Active != 1 || !hasContent(storage.Content, "iso") {
		return errors.New("selected placement storage is not active and ISO-capable")
	}
	contents, err := storage.GetContent(ctx)
	if err != nil {
		return fmt.Errorf("verify initialization ISO absence before upload: %w", err)
	}
	for _, content := range contents {
		if content == nil || content.Volid == d.placementISOStorage+":iso/"+d.placementISOName {
			return errors.New("placement initialization ISO already exists; refusing to overwrite uncertain ownership")
		}
	}
	upload, err := storage.Upload("iso", uploadPath)
	if err != nil {
		return fmt.Errorf("upload placement initialization ISO; retain ledger reservation: %w", err)
	}
	if err := d.waitForPVETaskToSucceed(ctx, upload); err != nil {
		return fmt.Errorf("wait for placement initialization ISO upload: %w", err)
	}
	device := ""
	for name, value := range config.MergeDisks() {
		if strings.Contains(value, "cloudinit") {
			device = name
			break
		}
	}
	if device == "" {
		return errors.New("cloned VM has no cloud-init media slot")
	}
	value := d.placementISOStorage + ":iso/" + d.placementISOName + ",media=cdrom"
	task, err := vm.Config(ctx, proxmox.VirtualMachineOption{Name: device, Value: value})
	if err != nil {
		return fmt.Errorf("attach placement initialization ISO; retain ISO and ledger reservation: %w", err)
	}
	if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
		return fmt.Errorf("wait for placement initialization ISO attachment: %w", err)
	}
	return nil
}

func placementMetadata(existing, machineName, incarnationID string) (string, error) {
	var metadata map[string]any
	if err := yaml.Unmarshal([]byte(existing), &metadata); err != nil || metadata == nil {
		return "", errors.New("generated cloud-init metadata is invalid")
	}
	metadata["instance-id"] = incarnationID
	metadata["hostname"] = machineName
	encoded, err := yaml.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode machine-incarnation metadata: %w", err)
	}
	return string(encoded), nil
}

func (d *Driver) setupNativeCloudInit(ctx context.Context) error {
	publicKey, err := os.ReadFile(d.GetSSHPublicKeyPath())
	if err != nil {
		return fmt.Errorf("read placement SSH public key: %w", err)
	}
	publicKey = bytes.TrimSpace(publicKey)
	if len(publicKey) == 0 || bytes.ContainsAny(publicKey, "\r\n") {
		return errors.New("placement SSH public key is empty or malformed")
	}
	interfaceNumber, err := networkInterfaceNumber(d.NetworkInterfaceName)
	if err != nil {
		return err
	}
	options := []proxmox.VirtualMachineOption{
		{Name: "ciuser", Value: d.SSHUser},
		{Name: "sshkeys", Value: strings.ReplaceAll(url.QueryEscape(string(publicKey)), "+", "%20")},
	}
	return d.runTaskOnCurrentMachine(ctx, func(ctx context.Context, machine *proxmox.VirtualMachine) (*proxmox.Task, error) {
		if machine.VirtualMachineConfig == nil || !hasCloudInitDisk(machine.VirtualMachineConfig) {
			return nil, errors.New("cloned VM has no Proxmox cloud-init drive")
		}
		if err := requireDynamicCloudInitAddress(machine.VirtualMachineConfig, d.NetworkInterfaceName, interfaceNumber); err != nil {
			return nil, err
		}
		return machine.Config(ctx, options...)
	})
}

func requireDynamicCloudInitAddress(config *proxmox.VirtualMachineConfig, interfaceName string, interfaceNumber int) error {
	if _, found := config.MergeNets()[interfaceName]; !found {
		return errors.New("configured placement network interface is missing from cloned template")
	}
	address := config.MergeIPConfigs()["ipconfig"+strconv.Itoa(interfaceNumber)]
	ipv4Dynamic, ipv6Dynamic := false, true
	for _, option := range strings.Split(address, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(option), "=")
		if !found {
			continue
		}
		switch key {
		case "ip":
			if value != "dhcp" || ipv4Dynamic {
				return errors.New("placement template must configure dynamic addressing with ip=dhcp")
			}
			ipv4Dynamic = true
		case "ip6":
			if value != "dhcp" || !ipv6Dynamic {
				return errors.New("placement template must use ip6=dhcp when IPv6 cloud-init addressing is configured")
			}
			ipv6Dynamic = false
		}
	}
	if ipv4Dynamic {
		return nil
	}
	return errors.New("placement template has no verifiable dynamic cloud-init address")
}

func placementNetworkInterface(config *proxmox.VirtualMachineConfig) (string, error) {
	networks := config.MergeNets()
	name := ""
	if networks["net0"] != "" {
		name = "net0"
	} else if len(networks) == 1 {
		for candidate := range networks {
			name = candidate
		}
	} else {
		return "", errors.New("placement template network interface is missing or ambiguous")
	}
	number, err := networkInterfaceNumber(name)
	if err != nil {
		return "", err
	}
	if err := requireDynamicCloudInitAddress(config, name, number); err != nil {
		return "", err
	}
	return name, nil
}

func (d *Driver) resizePlacedBootDisk(ctx context.Context) error {
	if d.DiskSizeGiB == 0 || d.DiskSizeGiB > ^uint64(0)/gibibyte {
		return errors.New("invalid target boot disk size")
	}
	vm, err := d.getPVEVirtualMachineOnNode(ctx, *d.PVEMachineID, d.placementNode)
	if err != nil || vm.VirtualMachineConfig == nil {
		return errors.New("read cloned VM disks before resize")
	}
	node, err := d.getPVEClient().Node(ctx, d.placementNode)
	if err != nil {
		return errors.New("read cloned VM node before resize")
	}
	device, existingBytes, err := bootDiskWithResolver(vm.VirtualMachineConfig, storageVolumeSizeResolver(ctx, node))
	if err != nil {
		return err
	}
	targetBytes := d.DiskSizeGiB * gibibyte
	if targetBytes < existingBytes {
		return errors.New("target boot disk is smaller than the selected template boot disk")
	}
	if targetBytes == existingBytes {
		return nil
	}
	increment, err := pveResizeIncrement(targetBytes - existingBytes)
	if err != nil {
		return err
	}
	task, err := vm.ResizeDisk(ctx, device, increment)
	if err != nil {
		return fmt.Errorf("resize placed VM boot disk: %w", err)
	}
	if err := d.waitForPVETaskToSucceed(ctx, task); err != nil {
		return fmt.Errorf("wait for placed VM boot disk resize: %w", err)
	}
	return nil
}

func networkInterfaceNumber(name string) (int, error) {
	if !strings.HasPrefix(name, "net") {
		return 0, errors.New("placement native cloud-init requires a netN network interface")
	}
	index, err := strconv.Atoi(strings.TrimPrefix(name, "net"))
	if err != nil || index < 0 || index > 31 {
		return 0, errors.New("placement network interface number is invalid")
	}
	return index, nil
}

func nonEmptyTags(configured []string, existing, managed string) []string {
	result := make([]string, 0, len(configured)+placementTagCapacity)
	seen := make(map[string]bool)
	for _, tag := range append(configured, append(strings.Split(existing, ";"), managed)...) {
		tag = strings.TrimSpace(tag)
		if tag != "" && !seen[tag] {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}

func (d *Driver) managedMachineTags(existing string) []string {
	managed := pveMachineTag
	if d.placementIncarnationID != "" {
		managed = ""
	}
	tags := nonEmptyTags(d.Tags, existing, managed)
	if d.placementIncarnationID != "" {
		filtered := tags[:0]
		for _, tag := range tags {
			if tag != pveMachineTag {
				filtered = append(filtered, tag)
			}
		}
		tags = filtered
	}
	for _, item := range []struct {
		kind  string
		value string
	}{
		{kind: "cluster", value: d.RancherClusterName},
		{kind: "pool", value: d.RancherMachinePool},
	} {
		if tag := rancherInfoTag(item.kind, item.value); tag != "" {
			tags = nonEmptyTags(tags, "", tag)
		}
	}
	return tags
}

func rancherInfoTag(kind, value string) string {
	var normalized strings.Builder
	separator := false
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-'
		if valid {
			if separator && normalized.Len() > 0 {
				normalized.WriteByte('-')
			}
			separator = false
			normalized.WriteRune(character)
		} else {
			separator = true
		}
	}
	value = strings.Trim(normalized.String(), "-_")
	if value == "" || len(value) > rancherTagValueMax {
		return ""
	}
	return "rancher-" + kind + "-" + value
}

func validateRancherMachineTags(clusterName, poolName string) error {
	if strings.TrimSpace(clusterName) == "" || strings.TrimSpace(poolName) == "" {
		return errors.New("Rancher cluster and machine-pool names are required for placement tags")
	}
	if rancherInfoTag("cluster", clusterName) == "" || rancherInfoTag("pool", poolName) == "" {
		return errors.New("Rancher cluster or machine-pool name cannot be represented as a Proxmox tag")
	}
	return nil
}

func hasCloudInitDisk(config *proxmox.VirtualMachineConfig) bool {
	for _, device := range config.MergeDisks() {
		if strings.Contains(device, "cloudinit") {
			return true
		}
	}

	return false
}

func bootDisk(config *proxmox.VirtualMachineConfig) (string, uint64, error) {
	return bootDiskWithResolver(config, nil)
}

func bootDiskWithResolver(config *proxmox.VirtualMachineConfig, resolve func(string) (uint64, error)) (string, uint64, error) {
	disks := config.MergeDisks()
	_, order, found := strings.Cut(config.Boot, "order=")
	if !found {
		return "", 0, errors.New("template boot order is missing or ambiguous")
	}
	order, _, _ = strings.Cut(order, ",")
	for _, name := range strings.Split(order, ";") {
		value, exists := disks[name]
		if !exists || strings.Contains(value, "media=cdrom") || strings.Contains(value, "cloudinit") {
			continue
		}
		size, err := volumeSize(value, resolve)
		if err != nil {
			return "", 0, err
		}
		return name, size, nil
	}
	return "", 0, errors.New("boot order does not identify a supported disk")
}

func pveResizeIncrement(bytes uint64) (string, error) {
	switch {
	case bytes%gibibyte == 0:
		return fmt.Sprintf("+%dG", bytes/gibibyte), nil
	case bytes%mebibyte == 0:
		return fmt.Sprintf("+%dM", bytes/mebibyte), nil
	default:
		return "", errors.New("boot disk resize increment is not MiB aligned")
	}
}
