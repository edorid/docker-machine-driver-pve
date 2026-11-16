package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoveredAddressIsSavedInMachineConfig(t *testing.T) {
	addresses := []any{map[string]any{"ip-address": "172.29.32.144", "ip-address-type": "ipv4"}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/pools/test":
			data = map[string]any{"poolid": "test", "members": []any{map[string]any{"vmid": 105, "node": "pve1", "type": "qemu"}}}
		case "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case "/api2/json/nodes/pve1/qemu/105/status/current":
			data = map[string]any{"vmid": 105, "name": "worker-1", "status": "running", "tags": "docker-machine"}
		case "/api2/json/nodes/pve1/qemu/105/config":
			data = map[string]any{"net0": "virtio=BC:24:11:12:34:56,bridge=vmbr0", "tags": "docker-machine"}
		case "/api2/json/nodes/pve1/qemu/105/agent/network-get-interfaces":
			data = map[string]any{"result": []any{map[string]any{"hardware-address": "bc:24:11:12:34:56", "ip-addresses": addresses}}}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	d := NewDriver("worker-1", t.TempDir())
	d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "secret", true
	d.ResourcePoolName, d.NetworkInterfaceName = "test", "net0"
	vmid := 105
	d.PVEMachineID = &vmid
	ip, err := d.GetIP()
	require.NoError(t, err)
	require.Equal(t, "172.29.32.144", ip)
	encoded, err := json.Marshal(d)
	require.NoError(t, err)
	var saved struct{ IPAddress, IPv6Address string }
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.Equal(t, "172.29.32.144", saved.IPAddress)

	addresses = []any{map[string]any{"ip-address": "2001:db8::42", "ip-address-type": "ipv6"}}
	ip, err = d.GetIP()
	require.NoError(t, err)
	require.Equal(t, "2001:db8::42", ip)
	encoded, err = json.Marshal(d)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.Empty(t, saved.IPAddress)
	require.Equal(t, "2001:db8::42", saved.IPv6Address)
}
