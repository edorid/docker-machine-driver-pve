//nolint:wsl // Provisioning steps remain visibly ordered rather than whitespace-cuddled.
package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/luthermonson/go-proxmox"
	"github.com/rancher/machine/libmachine/log"
)

const (
	cloudInitAuxiliaryBytes = 4 * mebibyte
	placementTagCapacity    = 2
	rancherTagValueMax      = 48
)

//nolint:contextcheck // The driver SSH wait uses its own bounded timeout; other operations use ctx.
func (d *Driver) initializePlaced(ctx context.Context) error {
	if d.PVEMachineID == nil || d.placementNode == "" {
		return errors.New("placed machine identity is unavailable")
	}

	vm, err := d.getPVEVirtualMachineOnNode(ctx, *d.PVEMachineID, d.placementNode)

	if err != nil || vm.VirtualMachineConfig == nil || vm.Name != d.MachineName {
		return errors.New("cloned Proxmox machine identity could not be verified")
	}

	d.Tags = d.managedMachineTags(vm.Tags)

	task, err := vm.Config(ctx, proxmox.VirtualMachineOption{Name: "tags", Value: d.Tags})
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

	if err := d.setupNativeCloudInit(ctx); err != nil {
		return err
	}

	if err := d.Start(); err != nil {
		return err
	}

	if err := d.waitForCloudinit(); err != nil {
		return fmt.Errorf("wait for native cloud-init: %w", err)
	}

	log.Info("Placed VM is initialized")

	return nil
}

func (d *Driver) setupNativeCloudInit(ctx context.Context) error {
	publicKey, err := os.ReadFile(d.GetSSHPublicKeyPath())
	if err != nil {
		return fmt.Errorf("read placement SSH public key: %w", err)
	}
	if len(bytes.TrimSpace(publicKey)) == 0 || bytes.ContainsAny(publicKey, "\r\n") {
		return errors.New("placement SSH public key is empty or malformed")
	}
	interfaceNumber, err := networkInterfaceNumber(d.NetworkInterfaceName)
	if err != nil {
		return err
	}
	options := []proxmox.VirtualMachineOption{
		{Name: "ciuser", Value: d.SSHUser},
		{Name: "sshkeys", Value: strings.TrimSpace(string(publicKey))},
		{Name: "ipconfig" + strconv.Itoa(interfaceNumber), Value: "ip=dhcp"},
	}
	return d.runTaskOnCurrentMachine(ctx, func(ctx context.Context, machine *proxmox.VirtualMachine) (*proxmox.Task, error) {
		if machine.VirtualMachineConfig == nil || !hasCloudInitDisk(machine.VirtualMachineConfig) {
			return nil, errors.New("cloned VM has no Proxmox cloud-init drive")
		}
		return machine.Config(ctx, options...)
	})
}

func (d *Driver) resizePlacedBootDisk(ctx context.Context) error {
	if d.DiskSizeGiB == 0 || d.DiskSizeGiB > ^uint64(0)/gibibyte {
		return errors.New("invalid target boot disk size")
	}
	vm, err := d.getPVEVirtualMachineOnNode(ctx, *d.PVEMachineID, d.placementNode)
	if err != nil || vm.VirtualMachineConfig == nil {
		return errors.New("read cloned VM disks before resize")
	}
	device, existingBytes, err := bootDisk(vm.VirtualMachineConfig)
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
	tags := nonEmptyTags(d.Tags, existing, pveMachineTag)
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
		if normalized.Len() >= rancherTagValueMax {
			break
		}
	}
	value = strings.Trim(normalized.String(), "-_")
	if value == "" {
		return ""
	}
	return "rancher-" + kind + "-" + value
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
		size, err := parsePVEVolumeSize(value)
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
