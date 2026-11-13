package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/rancher/machine/libmachine/ssh"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
)

func TestPlacementCloudInitNetworkUsesDHCPWithoutStaticDNS(t *testing.T) {
	config := &proxmox.VirtualMachineConfig{Net0: "virtio=BC:24:11:12:34:56,bridge=vmbr0", IPConfig0: "ip=dhcp"}
	network, err := placementCloudInitNetwork(config, "net0")
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(network), &parsed))
	require.NotContains(t, network, "nameserver")
	require.NotContains(t, network, "search")
	require.Contains(t, strings.ToLower(network), "bc:24:11:12:34:56")
	require.Contains(t, network, "dhcp4")
}

func TestPlacementCloudInitNetworkRejectsAmbiguousInterfaces(t *testing.T) {
	for _, config := range []*proxmox.VirtualMachineConfig{
		{Net0: "virtio,bridge=vmbr0", IPConfig0: "ip=dhcp"},
		{Net0: "virtio=BC:24:11:12:34:56,bridge=vmbr0", Net1: "virtio=BC:24:11:12:34:57,bridge=vmbr0", IPConfig0: "ip=dhcp"},
	} {
		_, err := placementCloudInitNetwork(config, "net0")
		require.Error(t, err)
	}
}

func TestPlacementDescriptionPreservesTextAndRejectsConflictingIdentity(t *testing.T) {
	description, err := placementDescription("Owned by operations", "incarnation-a")
	require.NoError(t, err)
	require.Equal(t, "Owned by operations\npve-machine-incarnation=incarnation-a", description)
	replayed, err := placementDescription(description, "incarnation-a")
	require.NoError(t, err)
	require.Equal(t, description, replayed)
	_, err = placementDescription(description, "incarnation-b")
	require.Error(t, err)
}

func TestPlacementOwnershipRequiresExactIdentityOrLegacyMarker(t *testing.T) {
	vm := &proxmox.VirtualMachine{VirtualMachineConfig: &proxmox.VirtualMachineConfig{Description: "human text\npve-machine-incarnation=id-a"}}
	require.True(t, placementOwnershipMatches(vm, "id-a"))
	require.False(t, placementOwnershipMatches(vm, "id-b"))
	vm.VirtualMachineConfig.Tags = pveMachineTag
	require.True(t, placementOwnershipMatches(vm, ""))
	vm.VirtualMachineConfig.Description = "human text"
	require.False(t, placementOwnershipMatches(vm, "id-a"))
}

func TestNewPlacementTagsDropLegacyOwnershipMarker(t *testing.T) {
	driver := &Driver{config: config{Tags: []string{"team"}, RancherClusterName: "Production Cluster", RancherMachinePool: "Workers East Blue"}}
	driver.placementIncarnationID = "incarnation-a"
	require.Equal(t, []string{"team", "rancher-cluster-production-cluster", "rancher-pool-workers-east-blue"}, driver.managedMachineTags("docker-machine;rancher-cluster-production-cluster;rancher-pool-workers-east-blue"))
}

func TestRancherMachineTagsRejectMissingOrTruncatedNames(t *testing.T) {
	require.Error(t, validateRancherMachineTags("cluster-a", ""))
	require.Error(t, validateRancherMachineTags(strings.Repeat("cluster", 9), "pool-a"))
	require.NoError(t, validateRancherMachineTags("cluster-a", "pool-a"))
	require.Empty(t, rancherInfoTag("cluster", strings.Repeat("a", rancherTagValueMax+1)))
}

func TestPlacementMetadataUsesIncarnationIdentity(t *testing.T) {
	metadata, err := placementMetadata("instance-id: old\nhostname: old\n", "worker-1", "incarnation-a")
	require.NoError(t, err)
	var values map[string]string
	require.NoError(t, yaml.Unmarshal([]byte(metadata), &values))
	require.Equal(t, "incarnation-a", values["instance-id"])
	require.Equal(t, "worker-1", values["hostname"])
}

func TestGeneratedPlacementISOUploadsBeforeAttachment(t *testing.T) {
	const upid = "UPID:pve1:00001234:00000042:67300000:qmconfig:106:root@pam:"
	var uploaded, attached bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/106/status/current":
			data = map[string]any{"vmid": 106, "name": "worker-1"}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/106/config":
			data = map[string]any{"net0": "virtio=BC:24:11:12:34:56,bridge=vmbr0", "ipconfig0": "ip=dhcp", "ide2": "local:cloudinit"}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/storage/local/status":
			data = map[string]any{"storage": "local", "enabled": 1, "active": 1, "content": "iso", "total": 1000000}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/storage/local/content":
			data = []any{}
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/storage/local/upload":
			require.NoError(t, r.ParseMultipartForm(16<<20))
			require.Equal(t, "iso", r.FormValue("content"))
			_, file, err := r.FormFile("filename")
			require.NoError(t, err)
			require.Equal(t, "pve-cloud-init-incarnation.iso", file.Filename)
			uploaded = true
			data = upid
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/106/config":
			require.True(t, uploaded, "ISO upload must finish before attachment")
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "local:iso/pve-cloud-init-incarnation.iso,media=cdrom", body["ide2"])
			attached = true
			data = upid
		case strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			data = map[string]any{"upid": upid, "node": "pve1", "status": "stopped", "exitstatus": "OK"}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	d := NewDriver("worker-1", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "secret", true
	d.placementNode, d.placementISOStorage = "pve1", "local"
	d.placementISOName, d.placementIncarnationID = "pve-cloud-init-incarnation.iso", "incarnation"
	d.SSHKeyPath = filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, os.WriteFile(d.GetSSHPublicKeyPath(), []byte("ssh-rsa AAAA test\n"), 0600))
	vm, err := d.getPVEVirtualMachineOnNode(t.Context(), 106, "pve1")
	require.NoError(t, err)
	require.NoError(t, d.setupGeneratedPlacementISO(t.Context(), vm))
	require.True(t, uploaded)
	require.True(t, attached)
}

func TestNativeCloudInitURLencodesSSHKeyForProxmox(t *testing.T) {
	const upid = "UPID:pve1:00001234:00000042:67300000:qmconfig:106:root@pam:"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/pools/testpool":
			data = map[string]any{"poolid": "testpool", "members": []any{map[string]any{"type": "qemu", "vmid": 106, "node": "pve1"}}}
		case "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case "/api2/json/nodes/pve1/qemu/106/status/current":
			data = map[string]any{"vmid": 106, "name": "test", "tags": "docker-machine"}
		case "/api2/json/nodes/pve1/qemu/106/config":
			if r.Method == http.MethodPost {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				encoded, ok := body["sshkeys"].(string)
				require.True(t, ok)
				decoded, err := url.QueryUnescape(encoded)
				require.NoError(t, err)
				if !strings.Contains(encoded, "%20") || !strings.HasPrefix(decoded, "ssh-rsa ") {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"errors":{"sshkeys":"invalid urlencoded string"}}`))
					return
				}
				require.Equal(t, "ssh-rsa AAAA+BBB/CCC test", decoded)
				data = upid
			} else {
				data = map[string]any{"net0": "virtio,bridge=vmbr0", "ipconfig0": "ip=dhcp", "ide2": "local:cloudinit", "tags": "docker-machine"}
			}
		case "/api2/json/nodes/pve1/tasks/" + upid + "/status":
			data = map[string]any{"upid": upid, "node": "pve1", "status": "stopped", "exitstatus": "OK"}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	d := NewDriver("test", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "test-secret", true
	d.ResourcePoolName, d.NetworkInterfaceName = "testpool", "net0"
	d.SSHKeyPath = filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, os.WriteFile(d.GetSSHPublicKeyPath(), []byte("ssh-rsa AAAA+BBB/CCC test\n"), 0600))
	vmid := 106
	d.PVEMachineID = &vmid
	require.NoError(t, d.setupNativeCloudInit(t.Context()))
}

func TestNativeCloudInitUsesExistingNetworkSnippet(t *testing.T) {
	const upid = "UPID:pve1:00001234:00000042:67300000:qmconfig:106:root@pam:"
	volume := "local-vm1:snippets/dhcp.yaml"
	var configured bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/pools/testpool":
			data = map[string]any{"members": []any{map[string]any{"type": "qemu", "vmid": 106, "node": "pve1"}}}
		case "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case "/api2/json/nodes/pve1/qemu/106/status/current":
			data = map[string]any{"vmid": 106, "name": "test"}
		case "/api2/json/nodes/pve1/qemu/106/config":
			if r.Method == http.MethodPost {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "user=local:snippets/user.yaml,network="+volume, body["cicustom"])
				configured = true
				data = upid
			} else {
				data = map[string]any{"net0": "virtio,bridge=vmbr0", "ipconfig0": "ip=dhcp", "ide2": "local:cloudinit", "tags": "docker-machine", "cicustom": "user=local:snippets/user.yaml"}
			}
		case "/api2/json/nodes/pve1/storage/local-vm1/content":
			data = []any{map[string]any{"volid": volume, "content": "snippets"}}
		case "/api2/json/nodes/pve1/tasks/" + upid + "/status":
			data = map[string]any{"upid": upid, "node": "pve1", "status": "stopped", "exitstatus": "OK"}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	d := NewDriver("test", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "test-secret", true
	d.ResourcePoolName, d.NetworkInterfaceName, d.CloudInitNetworkSnippet = "testpool", "net0", volume
	d.SSHKeyPath = filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, os.WriteFile(d.GetSSHPublicKeyPath(), []byte("ssh-rsa AAAA test\n"), 0600))
	vmid := 106
	d.PVEMachineID = &vmid
	require.NoError(t, d.setupNativeCloudInit(t.Context()))
	require.True(t, configured)
}

func TestNetworkSnippetSafety(t *testing.T) {
	for _, invalid := range []string{"", "local:snippets/../evil.yaml", "local:snippets/foo.yaml,network=evil", "local:iso/foo.yaml", "local:snippets/foo.sh"} {
		_, err := validateNetworkSnippet(invalid)
		require.Error(t, err, invalid)
	}
	storage, err := validateNetworkSnippet("local-vm1:snippets/dhcp.yaml")
	require.NoError(t, err)
	require.Equal(t, "local-vm1", storage)
	_, err = networkSnippetCustom("network=local:snippets/other.yaml", "local-vm1:snippets/dhcp.yaml")
	require.ErrorContains(t, err, "refusing to overwrite")
	custom, err := networkSnippetCustom("meta=local:snippets/meta.yaml,network=local-vm1:snippets/dhcp.yaml", "local-vm1:snippets/dhcp.yaml")
	require.NoError(t, err)
	require.Equal(t, "meta=local:snippets/meta.yaml,network=local-vm1:snippets/dhcp.yaml", custom)
}

func TestVerifyNetworkSnippetRequiresSelectedHostCopy(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api2/json/nodes/pve2/storage/local-vm1/content", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"volid": "local-vm1:snippets/other.yaml", "content": "snippets"}}})
	}))
	defer server.Close()
	d := NewDriver("test", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "test-secret", true
	require.ErrorContains(t, d.verifyNetworkSnippet(t.Context(), "pve2", "local-vm1:snippets/dhcp.yaml"), "not available on node")
}

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
