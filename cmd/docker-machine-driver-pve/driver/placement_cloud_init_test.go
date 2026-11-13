package driver

import (
	"path/filepath"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/rancher/machine/libmachine/ssh"
	"github.com/stretchr/testify/require"
)

func TestNativeCloudInitAcceptsGeneratedPublicKey(t *testing.T) {
	d := NewDriver("test", t.TempDir())
	d.SSHKeyPath = filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, ssh.GenerateSSHKey(d.SSHKeyPath))
	// A missing network interface fails immediately after key validation, without contacting Proxmox.
	err := d.setupNativeCloudInit(t.Context())
	require.ErrorContains(t, err, "network interface")
}

func TestRequireDynamicCloudInitAddress(t *testing.T) {
	tests := []struct {
		name    string
		config  *proxmox.VirtualMachineConfig
		wantErr bool
	}{
		{
			name:   "dynamic address on selected interface",
			config: &proxmox.VirtualMachineConfig{Net0: "virtio,bridge=vmbr0", IPConfig0: "ip=dhcp,ip6=dhcp"},
		},
		{
			name:    "no address evidence",
			config:  &proxmox.VirtualMachineConfig{Net0: "virtio,bridge=vmbr0"},
			wantErr: true,
		},
		{
			name:    "static address",
			config:  &proxmox.VirtualMachineConfig{Net0: "virtio,bridge=vmbr0", IPConfig0: "ip=192.0.2.4/24,gw=192.0.2.1"},
			wantErr: true,
		},
		{
			name:    "address on another interface",
			config:  &proxmox.VirtualMachineConfig{Net0: "virtio,bridge=vmbr0", IPConfig1: "ip=dhcp"},
			wantErr: true,
		},
		{
			name:    "selected interface absent",
			config:  &proxmox.VirtualMachineConfig{IPConfig0: "ip=dhcp"},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := requireDynamicCloudInitAddress(test.config, "net0", 0)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestPlacementNetworkInterfaceInference(t *testing.T) {
	for _, test := range []struct {
		name      string
		config    *proxmox.VirtualMachineConfig
		want      string
		wantError bool
	}{
		{
			name: "prefer net0 when multiple interfaces exist",
			config: &proxmox.VirtualMachineConfig{
				Net0: "virtio,bridge=vmbr0", Net1: "virtio,bridge=vmbr1", IPConfig0: "ip=dhcp",
			},
			want: "net0",
		},
		{
			name: "use sole nonzero interface",
			config: &proxmox.VirtualMachineConfig{
				Net2: "virtio,bridge=vmbr2", IPConfig2: "ip=dhcp",
			},
			want: "net2",
		},
		{
			name: "reject ambiguity without net0",
			config: &proxmox.VirtualMachineConfig{
				Net1: "virtio,bridge=vmbr1", Net2: "virtio,bridge=vmbr2", IPConfig1: "ip=dhcp",
			},
			wantError: true,
		},
		{
			name: "reject static address",
			config: &proxmox.VirtualMachineConfig{
				Net0: "virtio,bridge=vmbr0", IPConfig0: "ip=192.0.2.4/24",
			},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := placementNetworkInterface(test.config)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
