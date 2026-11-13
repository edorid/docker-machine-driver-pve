package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/stretchr/testify/require"
)

func TestRemovePlacementISOVerifiesAbsenceAfterDeletion(t *testing.T) {
	const (
		upid  = "UPID:pve1:00001234:00000042:67300000:imgcopy:0:root@pam:"
		volid = "local:iso/pve-cloud-init-incarnation.iso"
	)
	var deleted bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes":
			data = []any{map[string]any{"node": "pve1", "status": "online", "online": 1}}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/storage/local/status":
			data = map[string]any{"storage": "local", "enabled": 1, "active": 1, "content": "iso", "total": 1000000}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/storage/local/content":
			if deleted {
				data = []any{}
			} else {
				data = []any{map[string]any{"volid": volid, "content": "iso"}}
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu":
			data = []any{}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/storage/local/content/"+volid:
			data = map[string]any{"volid": volid}
		case r.Method == http.MethodDelete && r.URL.Path == "/api2/json/nodes/pve1/storage/local/content/"+volid:
			deleted = true
			data = upid
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/tasks/"+upid+"/status":
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
	record := placement.Record{Commitment: placement.Commitment{
		Node: "pve1", Storage: "images", ISOStorage: "local", ISOName: "pve-cloud-init-incarnation.iso",
	}}
	require.NoError(t, d.removePlacementISO(t.Context(), record))
	require.True(t, deleted)
}

func TestRemovePlacementISORefusesInUseMedia(t *testing.T) {
	const volid = "local:iso/pve-cloud-init-incarnation.iso"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = []any{map[string]any{"node": "pve1", "status": "online", "online": 1}}
		case "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case "/api2/json/nodes/pve1/storage/local/status":
			data = map[string]any{"storage": "local", "enabled": 1, "active": 1, "content": "iso", "total": 1000000}
		case "/api2/json/nodes/pve1/storage/local/content":
			data = []any{map[string]any{"volid": volid, "content": "iso"}}
		case "/api2/json/nodes/pve1/qemu":
			data = []any{map[string]any{"vmid": 107, "name": "other"}}
		case "/api2/json/nodes/pve1/qemu/107/status/current":
			data = map[string]any{"vmid": 107, "name": "other"}
		case "/api2/json/nodes/pve1/qemu/107/config":
			data = map[string]any{"scsi1": volid + ",media=cdrom"}
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
	record := placement.Record{Commitment: placement.Commitment{
		Node: "pve1", ISOStorage: "local", ISOName: "pve-cloud-init-incarnation.iso",
	}}
	err := d.removePlacementISO(t.Context(), record)
	require.ErrorContains(t, err, "remains in use")
}

func TestRemovePlacementISOProvesAbsenceAndNonUseBeforeSucceeding(t *testing.T) {
	var scannedVMInventory bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = []any{map[string]any{"node": "pve1", "status": "online", "online": 1}}
		case "/api2/json/nodes/pve1/status":
			data = map[string]any{}
		case "/api2/json/nodes/pve1/storage/local/status":
			data = map[string]any{"storage": "local", "enabled": 1, "active": 1, "content": "iso", "total": 1000000}
		case "/api2/json/nodes/pve1/storage/local/content":
			data = []any{}
		case "/api2/json/nodes/pve1/qemu":
			scannedVMInventory = true
			data = []any{}
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
	record := placement.Record{Commitment: placement.Commitment{
		Node: "pve1", ISOStorage: "local", ISOName: "pve-cloud-init-incarnation.iso",
	}}
	require.NoError(t, d.removePlacementISO(t.Context(), record))
	require.True(t, scannedVMInventory)
}
