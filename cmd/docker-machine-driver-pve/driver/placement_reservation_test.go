package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/stretchr/testify/require"
)

type reservationTestStore struct {
	mu      sync.Mutex
	version int
	ledger  placement.Ledger
}

func (s *reservationTestStore) Read(ctx context.Context) (placement.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.ledger)
	var ledger placement.Ledger
	_ = json.Unmarshal(data, &ledger)
	return placement.Document{UID: "uid", Revision: strconv.Itoa(s.version), Ledger: ledger}, ctx.Err()
}

func (s *reservationTestStore) CompareAndSwap(ctx context.Context, old placement.Document, ledger placement.Ledger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if old.Revision != strconv.Itoa(s.version) {
		return placement.ErrConflict
	}
	s.ledger, s.version = ledger, s.version+1
	return nil
}

func TestPlacementReservationsAvoidAlreadyReservedVMIDs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/cluster/status" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		if r.URL.Path == "/api2/json/cluster/nextid" {
			id := r.URL.Query().Get("vmid")
			if id == "" {
				id = "105"
			}
			if id == "109" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":{"vmid":"VM 109 already exists"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": id})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store := &reservationTestStore{ledger: placement.Ledger{Version: 1, ClusterID: "cluster", CapacityPolicy: placement.CapacityPolicy{Configured: true, Hosts: map[string]placement.CapacityReserve{}}, Groups: map[string]string{}, Records: map[string]placement.Record{}}}
	coordinator := placement.Coordinator{Store: store, ClusterID: "cluster"}
	policy := placement.Policy{GroupID: "group", AllowedHosts: []string{"host-a"}, AllowedStorages: []string{"disk"}, Templates: map[string]int{"host-a": 900}}
	inventory := placement.Snapshot{Complete: true, Nodes: []placement.Node{{Name: "host-a", Online: true, Reachable: true, TemplateVMID: 900, TemplateReady: true, NetworkReady: true, CPU: 100, MemoryBytes: 10000, FreeMemoryBytes: 10000, Storages: []placement.Storage{{ID: "disk", Ready: true, CloneSupported: true, ISOSupported: true, TotalBytes: 100000, FreeBytes: 100000}}}}}
	discover := func(context.Context) (placement.Snapshot, error) { return inventory, nil }
	for i := 0; i < 4; i++ {
		d := NewDriver(fmt.Sprintf("worker-%d", i), t.TempDir())
		d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "secret", true
		intent := placement.Intent{MachineID: d.MachineName, OwnerID: d.MachineName, IncarnationID: fmt.Sprintf("identity-%d", i), ISOName: fmt.Sprintf("pve-cloud-init-identity-%d.iso", i), RancherClusterName: "cluster", RancherPoolName: "pool", Policy: policy, Resources: placement.Resources{CPU: 1, MemoryBytes: 100, DiskBytes: 100}, BootDiskBytes: 100}
		record, err := d.reservePlacement(t.Context(), coordinator, intent, discover)
		require.NoError(t, err)
		require.Equal(t, 105+i, record.VMID)
	}

	type outcome struct {
		vmid int
		err  error
	}
	results := make(chan outcome, 4)
	for i := 4; i < 8; i++ {
		d := NewDriver(fmt.Sprintf("worker-%d", i), t.TempDir())
		d.URL, d.TokenID, d.TokenSecret, d.InsecureTLS = server.URL, "user@pve!test", "secret", true
		intent := placement.Intent{MachineID: d.MachineName, OwnerID: d.MachineName, IncarnationID: fmt.Sprintf("identity-%d", i), ISOName: fmt.Sprintf("pve-cloud-init-identity-%d.iso", i), RancherClusterName: "cluster", RancherPoolName: "pool", Policy: policy, Resources: placement.Resources{CPU: 1, MemoryBytes: 100, DiskBytes: 100}, BootDiskBytes: 100}
		go func() {
			record, err := d.reservePlacement(t.Context(), coordinator, intent, discover)
			results <- outcome{record.VMID, err}
		}()
	}
	ids := make(map[int]bool)
	for range 4 {
		result := <-results
		require.NoError(t, result.err)
		require.False(t, ids[result.vmid], "concurrent reservations must be distinct")
		ids[result.vmid] = true
	}
	for id := 110; id <= 113; id++ {
		require.True(t, ids[id])
	}
}
