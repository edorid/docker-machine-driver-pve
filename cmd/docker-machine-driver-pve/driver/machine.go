package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/luthermonson/go-proxmox"
	machine_ssh "github.com/rancher/machine/libmachine/ssh"
	"golang.org/x/crypto/ssh"
)

type TaskCallback = func(context.Context, *proxmox.VirtualMachine) (*proxmox.Task, error)

const (
	// Tag for machines managed by the driver.
	pveMachineTag = "docker-machine"
)

var ErrNonZeroExitCode = errors.New("command finished with non-zero exit code")

// Returns the current machine.
func (d *Driver) getCurrentMachine(ctx context.Context) (*proxmox.VirtualMachine, error) {
	if d.PVEMachineID == nil {
		return nil, errors.New("failed to retrieve current Proxmox VE virtual machine: no ID set")
	}
	if d.PlacementGroup != "" && d.placementIncarnationID == "" {
		coordinator, err := d.newPlacementCoordinator()
		if err != nil {
			return nil, err
		}
		document, err := coordinator.Store.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read placement ownership before VM operation: %w", err)
		}
		record, found := document.Ledger.Records[d.MachineName]
		if !found || record.VMID != *d.PVEMachineID {
			return nil, errors.New("placement ledger does not prove ownership of the current VM")
		}
		d.placementIncarnationID = record.IncarnationID
		d.placementISOName = record.ISOName
		d.placementISOStorage = record.ISOStorage
	}

	vm, err := d.getPVEVirtualMachine(ctx, *d.PVEMachineID)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve current Proxmox VE virtual machine ID='%d': %w", *d.PVEMachineID, err)
	}

	if !placementOwnershipMatches(vm, d.placementIncarnationID) {
		return nil, fmt.Errorf("current Proxmox VE virtual machine ID='%d' does not have expected tag '%s', it could have been replaced or modified outside the driver", *d.PVEMachineID, pveMachineTag)
	}

	return vm, nil
}

// Runs a task on the current machine.
func (d *Driver) runTaskOnCurrentMachine(ctx context.Context, callback TaskCallback) error {
	machine, err := d.getCurrentMachine(ctx)
	if err != nil {
		return err
	}

	task, err := callback(ctx, machine)
	if err != nil {
		return fmt.Errorf("failed to create a task: %w", err)
	}

	return d.waitForPVETaskToSucceed(ctx, task)
}

// Runs command on the current machine.
func (d *Driver) runCommandOnCurrentMachine(command string) error {
	hostname, err := d.GetSSHHostname()
	if err != nil {
		return fmt.Errorf("failed to get machine SSH hostname: %w", err)
	}

	port, err := d.GetSSHPort()
	if err != nil {
		return fmt.Errorf("failed to get machine SSH port: %w", err)
	}

	sshConfig, err := machine_ssh.NewNativeConfig(d.GetSSHUsername(), &machine_ssh.Auth{
		Keys: []string{
			d.GetSSHKeyPath(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH config: %w", err)
	}

	connection, err := ssh.Dial("tcp", net.JoinHostPort(hostname, strconv.Itoa(port)), &sshConfig)
	if err != nil {
		return fmt.Errorf("failed to dial SSH: %w", err)
	}
	defer connection.Close()

	session, err := connection.NewSession()
	if err != nil {
		return fmt.Errorf("failed to open SSH session: %w", err)
	}
	defer session.Close()

	if err := session.Run(command); err != nil {
		//nolint:errorlint // not applicable
		if _, ok := err.(*ssh.ExitError); ok {
			return fmt.Errorf("%w: %w", ErrNonZeroExitCode, err)
		}

		return fmt.Errorf("failed to execute command: %w", err)
	}

	return nil
}
