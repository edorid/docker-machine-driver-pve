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
)

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
