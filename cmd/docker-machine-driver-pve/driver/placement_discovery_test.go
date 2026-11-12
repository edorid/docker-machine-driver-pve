//nolint:wsl // Tests prioritize direct table-driven evidence over whitespace groupings.
package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

//nolint:wsl // The mock handler switches over independent Proxmox API endpoints.
func TestDiscoverPlacementBuildsCompleteSingleNodeSnapshot(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload any
		switch request.URL.Path {
		case "/api2/json/nodes":
			payload = []any{map[string]any{"node": "pve1", "status": "online", "maxcpu": 8, "maxmem": 32 << 30}}
		case "/api2/json/nodes/pve1/status":
			payload = map[string]any{"memory": map[string]any{"total": 32 << 30, "free": 24 << 30}}
		case "/api2/json/nodes/pve1/storage":
			payload = []any{map[string]any{"storage": "local-lvm", "enabled": 1, "active": 1, "content": "images,rootdir", "total": 1000, "avail": 800}}
		case "/api2/json/nodes/pve1/qemu":
			payload = []any{map[string]any{"vmid": 900, "name": "rocky-template", "status": "stopped", "cpus": 2, "maxmem": 2 << 30, "tags": "template"}}
		case "/api2/json/nodes/pve1/lxc":
			payload = []any{}
		case "/api2/json/nodes/pve1/network":
			payload = []any{map[string]any{"iface": "vmbr0", "type": "bridge"}}
		case "/api2/json/nodes/pve1/qemu/900/status/current":
			payload = map[string]any{"vmid": 900, "name": "rocky-template", "tags": "template"}
		case "/api2/json/nodes/pve1/qemu/900/config":
			payload = map[string]any{
				"template": 1, "agent": "1", "boot": "order=scsi0;net0", "scsi0": "local-lvm:vm-900-disk-0,size=32G",
				"ide2": "local-lvm:cloudinit", "net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0", "ipconfig0": "ip=dhcp",
			}
		default:
			t.Errorf("unexpected Proxmox API path %s", request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{"data": payload}); err != nil {
			t.Errorf("encode Proxmox response: %v", err)
		}
	}))
	defer server.Close()

	d := NewDriver("worker-one", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pam!test", "test-secret", true
	d.PlacementTemplates = map[string]int{"pve1": 900}
	d.NetworkInterfaceName = "net0"
	snapshot, err := d.discoverPlacement(t.Context(), nil, "worker-one")
	require.NoError(t, err)
	require.True(t, snapshot.Complete)
	require.Len(t, snapshot.Nodes, 1)
	require.True(t, snapshot.Nodes[0].Online)
	require.True(t, snapshot.Nodes[0].Reachable)
	require.Equal(t, uint64(2), snapshot.Nodes[0].AllocatedCPU)
	require.Equal(t, uint64(2<<30), snapshot.Nodes[0].AllocatedMemoryBytes)
	require.True(t, snapshot.Nodes[0].TemplateReady)
	require.True(t, snapshot.Nodes[0].NetworkReady)
	require.Equal(t, uint64(32)*gibibyte+4*mebibyte, snapshot.Nodes[0].TemplateDiskBytes)
	require.Equal(t, uint64(32)*gibibyte, snapshot.Nodes[0].TemplateBootDiskBytes)
	require.Equal(t, []placement.Storage{{ID: "local-lvm", Ready: true, CloneSupported: true, TotalBytes: 1000, FreeBytes: 800, AllocatedBytes: 200}}, snapshot.Nodes[0].Storages)
}

func TestVMStorageLocationRequiresOneUnambiguousStorage(t *testing.T) {
	config := &proxmox.VirtualMachineConfig{SCSI0: "local-lvm:vm-100-disk-0,size=32G", IDE2: "local-lvm:cloudinit"}
	storage, err := vmStorageLocation(config)
	require.NoError(t, err)
	require.Equal(t, "local-lvm", storage)

	ambiguous := &proxmox.VirtualMachineConfig{
		SCSI0: "local-lvm:vm-100-disk-0,size=32G", SATA0: "other:vm-100-disk-1,size=1G",
	}
	_, err = vmStorageLocation(ambiguous)
	require.ErrorIs(t, err, placement.ErrIncompleteInventory)
}

func TestTemplateDiskSizesUsesConfiguredBootOrderAndAllDisks(t *testing.T) {
	config := &proxmox.VirtualMachineConfig{
		Boot: "order=net0;scsi0;virtio0", SCSI0: "local-lvm:vm-100-disk-0,size=20G",
		VirtIO0: "local-lvm:vm-100-disk-1,size=8G", IDE2: "local-lvm:cloudinit",
	}
	total, boot, err := templateDiskSizes(config)
	require.NoError(t, err)
	require.Equal(t, uint64(28)*gibibyte+4*mebibyte, total)
	require.Equal(t, uint64(20)*gibibyte, boot)

	config.Boot = ""
	_, _, err = templateDiskSizes(config)
	require.ErrorIs(t, err, placement.ErrIncompleteInventory)
}

func TestParsePVEVolumeSizeValidatesUnitsAndOverflow(t *testing.T) {
	for _, test := range []struct {
		value string
		want  uint64
	}{
		{value: "local-lvm:vm-1-disk-0,size=32G", want: 32 * gibibyte},
		{value: "local-lvm:vm-1-disk-0,size=512M", want: 512 * mebibyte},
		{value: "local-lvm:vm-1-disk-0,size=2048K", want: 2 * mebibyte},
		{value: "local-lvm:vm-1-disk-0,size=4096", want: 4096},
	} {
		got, err := parsePVEVolumeSize(test.value)
		require.NoError(t, err, test.value)
		require.Equal(t, test.want, got, test.value)
	}
	for _, invalid := range []string{
		"local-lvm:disk,size=0G", "local-lvm:disk,size=2X", "local-lvm:disk,size=18446744073709551615T",
		"local-lvm:disk", "local-lvm:disk,size=1.5G",
	} {
		_, err := parsePVEVolumeSize(invalid)
		require.Error(t, err, invalid)
	}
}

func TestBootDiskHonorsFirstBootableConfiguredDisk(t *testing.T) {
	config := &proxmox.VirtualMachineConfig{
		Boot: "order=ide2;scsi1;scsi0", IDE2: "local-lvm:cloudinit",
		SCSI1: "local-lvm:installer,media=cdrom", SCSI0: "local-lvm:vm-1-disk-0,size=64G",
	}
	disk, size, err := bootDisk(config)
	require.NoError(t, err)
	require.Equal(t, "scsi0", disk)
	require.Equal(t, uint64(64)*gibibyte, size)

	config.Boot = "order=net0"
	_, _, err = bootDisk(config)
	require.Error(t, err)
}

func TestPlacementCloudInitInputValidation(t *testing.T) {
	for _, name := range []string{"net0", "net1"} {
		index, err := networkInterfaceNumber(name)
		require.NoError(t, err)
		require.Equal(t, int(name[3]-'0'), index)
	}
	for _, name := range []string{"", "eth0", "net", "net-1", "net32"} {
		_, err := networkInterfaceNumber(name)
		require.Error(t, err, name)
	}
	require.Equal(t, []string{"custom", "existing", pveMachineTag}, nonEmptyTags([]string{"custom", "custom"}, "existing;custom", pveMachineTag))
}

func TestManagedMachineTagsIncludeNormalizedRancherMetadata(t *testing.T) {
	driver := &Driver{
		config: config{
			Tags: []string{"owner:team"}, RancherClusterName: "Production Cluster",
			RancherMachinePool: "workers-east/blue",
		},
	}
	require.Equal(t, []string{
		"owner:team", "existing-tag", pveMachineTag, "rancher-cluster-production-cluster", "rancher-pool-workers-east-blue",
	}, driver.managedMachineTags("existing-tag"))
	require.Empty(t, rancherInfoTag("cluster", "---"))
	require.LessOrEqual(t, len(rancherInfoTag("cluster", strings.Repeat("long-name", 30))), 64)
}

func TestPlacementDiskRequirementReservesLargestEligibleTemplate(t *testing.T) {
	snapshot := placement.Snapshot{Nodes: []placement.Node{
		{Name: "pve-a", Online: true, Reachable: true, TemplateReady: true, TemplateDiskBytes: 44 * gibibyte, TemplateBootDiskBytes: 32 * gibibyte},
		{Name: "pve-b", Online: true, Reachable: true, TemplateReady: true, TemplateDiskBytes: 28 * gibibyte, TemplateBootDiskBytes: 20 * gibibyte},
		{Name: "pve-c", Online: false, TemplateDiskBytes: 128 * gibibyte, TemplateBootDiskBytes: 128 * gibibyte},
	}}
	required, err := placementDiskRequirement(snapshot, []string{"pve-a", "pve-b", "pve-c"}, 64*gibibyte, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(76)*gibibyte, required)
	_, err = placementDiskRequirement(snapshot, []string{"pve-a"}, 64*gibibyte, 70*gibibyte)
	require.Error(t, err)
}

//nolint:wsl // Test handler asserts the expected denied endpoint.
func TestDiscoverPlacementRejectsIncompleteProxmoxInventory(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api2/json/nodes" {
			_, _ = writer.Write([]byte(`{"data":[{"node":"pve1","status":"online","maxcpu":8,"maxmem":1000}]}`))
			return
		}
		http.Error(writer, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	d := NewDriver("worker-one", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pam!test", "test-secret", true
	_, err := d.discoverPlacement(t.Context(), nil, "worker-one")
	require.Error(t, err)
	require.ErrorContains(t, err, "node \"pve1\" status")
}
