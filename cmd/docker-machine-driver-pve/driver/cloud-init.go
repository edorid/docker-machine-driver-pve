package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/rancher/machine/libmachine/log"
	yaml "gopkg.in/yaml.v3"
)

// Configures cloud-init for the current machine.
func (d *Driver) setupCloudinit(ctx context.Context) error {
	machine, err := d.getCurrentMachine(ctx)
	if err != nil {
		return err
	}

	cloudinitMetadata, err := d.generateCloudinitMetadata()
	if err != nil {
		return fmt.Errorf("failed to generate cloud-init metadata: %w", err)
	}

	cloudinitUserdata, err := d.generateCloudinitUserdata()
	if err != nil {
		return fmt.Errorf("failed to generate cloud-init userdata: %w", err)
	}

	if err := machine.CloudInit(ctx, d.ISODeviceName, cloudinitUserdata, cloudinitMetadata, "", ""); err != nil {
		return fmt.Errorf("failed to configure cloud-init for Proxmox VE virtual machine ID='%d': %w", machine.VMID, err)
	}

	return nil
}

// Blocks until cloud-init finishes setup on the current machine.
func (d *Driver) waitForCloudinit() error {
	ctx, cancel := context.WithTimeout(context.TODO(), pveTaskPollingTimeout)
	defer cancel()

	for {
		err := d.runCommandOnCurrentMachine("sudo cloud-init status --wait")
		if err == nil {
			return nil
		}

		if errors.Is(err, ErrNonZeroExitCode) {
			return fmt.Errorf("cloud-init finished with non-zero exit code: %w", err)
		}

		log.Warn("failed to execute 'sudo cloud-init status --wait' over SSH, will retry:", err.Error())

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for cloud-init to finish: %w", context.DeadlineExceeded)
		case <-time.After(pveTaskPollingInterval):
			continue
		}
	}
}

// Removes cloud-init configuration from the current machine.
func (d *Driver) cleanupCloudinit(ctx context.Context) error {
	machine, err := d.getCurrentMachine(ctx)
	if err != nil {
		return err
	}

	if err := machine.UnmountCloudInitISO(ctx, d.ISODeviceName); err != nil {
		return fmt.Errorf("failed to remove cloud-init ISO: %w", err)
	}

	err = d.runTaskOnCurrentMachine(ctx, func(ctx context.Context, vm *proxmox.VirtualMachine) (*proxmox.Task, error) {
		return vm.RemoveTag(ctx, proxmox.MakeTag(proxmox.TagCloudInit))
	})
	if err != nil {
		return fmt.Errorf("failed to remove cloud-init tag: %w", err)
	}

	return nil
}

// Generates cloud-init metadatadata for the current machine.
func (d *Driver) generateCloudinitMetadata() (string, error) {
	metadata := map[string]interface{}{
		"instance-id": d.MachineName,
		"hostname":    d.MachineName,
	}

	metadataYAML, err := yaml.Marshal(&metadata)
	if err != nil {
		return "", fmt.Errorf("failed to marshal cloud-init metadata: %w", err)
	}

	return string(metadataYAML), nil
}

// Generates cloud-init userdata for the current machine.
func (d *Driver) generateCloudinitUserdata() (string, error) {
	sshPublicKey, err := os.ReadFile(d.GetSSHPublicKeyPath())
	if err != nil {
		return "", fmt.Errorf("failed to read machine's SSH public key: %w", err)
	}
	keys := []string{strings.TrimSpace(string(sshPublicKey))}
	if d.SharedSSHPublicKey != "" {
		keys = append(keys, d.SharedSSHPublicKey)
	}

	userdata := map[string]interface{}{
		"hostname":             d.MachineName,
		"preserve_hostname":    false,
		"create_hostname_file": true,
		"users": []map[string]interface{}{
			{
				"name":                d.SSHUser,
				"lock_passwd":         true,
				"sudo":                "ALL=(ALL) NOPASSWD:ALL",
				"ssh_authorized_keys": keys,
			},
		},
	}

	userdataYAML, err := yaml.Marshal(&userdata)
	if err != nil {
		return "", fmt.Errorf("failed to marshal cloud-init userdata: %w", err)
	}

	return fmt.Sprintf("#cloud-config\n%s", userdataYAML), nil
}
