package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// memoryStore is a test-only CAS implementation, never a production coordinator.
type memoryStore struct {
	mu       sync.Mutex
	revision int
	ledger   Ledger
	lost     bool
	conflict bool
}

func emptyLedger() Ledger {
	return Ledger{
		Version: 1, ClusterID: "proxmox-cluster-a",
		CapacityPolicy: CapacityPolicy{Configured: true, Hosts: map[string]CapacityReserve{}},
		Groups:         map[string]string{}, Records: map[string]Record{},
	}
}

func cloneLedger(ledger Ledger) Ledger {
	data, err := json.Marshal(ledger)
	if err != nil {
		panic(err)
	}

	var cloned Ledger
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}

	return cloned
}

func (m *memoryStore) Read(ctx context.Context) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, fmt.Errorf("read context: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return Document{UID: "ledger-uid", Revision: strconv.Itoa(m.revision), Ledger: cloneLedger(m.ledger)}, nil
}

func (m *memoryStore) CompareAndSwap(ctx context.Context, previous Document, ledger Ledger) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write context: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conflict || previous.Revision != strconv.Itoa(m.revision) {
		return ErrConflict
	}

	m.ledger = cloneLedger(ledger)
	m.revision++

	if m.lost {
		m.lost = false
		return ErrLedgerUnavailable // write committed, response lost
	}

	return nil
}

func testIntent(index int) Intent {
	return Intent{
		MachineID: fmt.Sprintf("machine-%d", index), OwnerID: fmt.Sprintf("owner-%d", index), IncarnationID: fmt.Sprintf("incarnation-%d", index), ISOName: fmt.Sprintf("pve-cloud-init-incarnation-%d.iso", index),
		RancherClusterName: "cluster-a", RancherPoolName: "workers", VMID: 100 + index,
		Policy: testPolicy(), Resources: testResources(), BootDiskBytes: 32 << 30,
	}
}

func discoverTestInventory(context.Context) (Snapshot, error) { return testInventory(), nil }

func newTestCoordinator() (Coordinator, *memoryStore) {
	store := &memoryStore{ledger: emptyLedger()}
	return Coordinator{Store: store, ClusterID: store.ledger.ClusterID}, store
}

func TestReserveReplayAndExactlyOneSubmission(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	intent := testIntent(1)
	first, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	require.Equal(t, intent.IncarnationID, first.IncarnationID)
	require.Equal(t, intent.ISOName, first.ISOName)
	require.Equal(t, "disk-b", first.ISOStorage)
	replay, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	require.Equal(t, first, replay)

	changedBootDisk := intent
	changedBootDisk.BootDiskBytes++
	_, _, err = coordinator.Reserve(t.Context(), changedBootDisk, discoverTestInventory)
	require.ErrorIs(t, err, ErrIdentityConflict)

	submitting, err := coordinator.BeginSubmission(t.Context(), intent.MachineID, intent.OwnerID)
	require.NoError(t, err)
	require.Equal(t, Submitting, submitting.Phase)
	require.NotEmpty(t, submitting.SubmissionToken)

	_, err = coordinator.BeginSubmission(t.Context(), intent.MachineID, intent.OwnerID)
	require.ErrorIs(t, err, ErrUncertainOperation)
	_, err = coordinator.RecordTask(t.Context(), intent.MachineID, intent.OwnerID, "wrong-token", "UPID:task")
	require.ErrorIs(t, err, ErrIdentityConflict)
	submitted, err := coordinator.RecordTask(t.Context(), intent.MachineID, intent.OwnerID, submitting.SubmissionToken, "UPID:task")
	require.NoError(t, err)
	require.Equal(t, Submitted, submitted.Phase)

	replayed, err := coordinator.RecordTask(t.Context(), intent.MachineID, intent.OwnerID, submitting.SubmissionToken, "UPID:task")
	require.NoError(t, err)
	require.Equal(t, submitted, replayed)

	_, err = coordinator.RecordTask(t.Context(), intent.MachineID, intent.OwnerID, submitting.SubmissionToken, "UPID:different")
	require.ErrorIs(t, err, ErrUncertainOperation)
}

func TestSelectISOStoragePrefersVMStorageWhenISOCapable(t *testing.T) {
	snapshot := testInventory()
	for index := range snapshot.Nodes {
		if snapshot.Nodes[index].Name == "host-a" {
			snapshot.Nodes[index].Storages = []Storage{
				{ID: "z-iso", Ready: true, ISOSupported: true},
				{ID: "disk-a", Ready: true, CloneSupported: true, ISOSupported: true},
				{ID: "a-iso", Ready: true, ISOSupported: true},
			}
		}
	}
	selected, err := SelectISOStorage(snapshot, "host-a", "disk-a")
	require.NoError(t, err)
	require.Equal(t, "disk-a", selected)
}

func TestSelectISOStorageFallsBackToDeterministicISOStorage(t *testing.T) {
	snapshot := testInventory()
	for index := range snapshot.Nodes {
		if snapshot.Nodes[index].Name == "host-a" {
			snapshot.Nodes[index].Storages = []Storage{
				{ID: "z-iso", Ready: true, ISOSupported: true},
				{ID: "images-only", Ready: true, CloneSupported: true},
				{ID: "a-iso", Ready: true, ISOSupported: true},
				{ID: "not-ready", Ready: false, ISOSupported: true},
			}
		}
	}
	selected, err := SelectISOStorage(snapshot, "host-a", "images-only")
	require.NoError(t, err)
	require.Equal(t, "a-iso", selected)
	selected, err = SelectISOStorage(snapshot, "host-a", "not-ready")
	require.NoError(t, err)
	require.Equal(t, "a-iso", selected)
	_, err = SelectISOStorage(snapshot, "unknown-host", "disk-a")
	require.Error(t, err)
}

func TestRancherNameCollisionRejectsDistinctNamesWithSameNormalizedTag(t *testing.T) {
	require.True(t, rancherNameCollision("Production Cluster", "production-cluster"))
	require.False(t, rancherNameCollision("production", "production"))
	require.False(t, rancherNameCollision("production", "staging"))
}

func TestReserveRejectsPersistedRancherTagCollision(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	first := testIntent(1)
	first.RancherClusterName = "Production Cluster"
	_, _, err := coordinator.Reserve(t.Context(), first, discoverTestInventory)
	require.NoError(t, err)
	second := testIntent(2)
	second.RancherClusterName = "production-cluster"
	_, _, err = coordinator.Reserve(t.Context(), second, discoverTestInventory)
	require.ErrorContains(t, err, "collide after Proxmox tag normalization")
}

func TestConcurrentReservationsAndSubmission(t *testing.T) {
	for _, count := range []int{3, 10} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			_, store := newTestCoordinator()
			start := make(chan struct{})
			results := make(chan error, count)

			for index := range count {
				go func() {
					<-start
					// Distinct coordinator instances; only shared CAS provides safety.
					coordinator := Coordinator{Store: store, ClusterID: "proxmox-cluster-a"}
					_, _, err := coordinator.Reserve(t.Context(), testIntent(index), discoverTestInventory)
					results <- err
				}()
			}

			close(start)

			for range count {
				require.NoError(t, <-results)
			}

			document, err := store.Read(t.Context())
			require.NoError(t, err)

			counts := map[string]int{"host-a": 0, "host-b": 0, "host-c": 0}
			for _, record := range document.Ledger.Records {
				counts[record.Node]++
			}

			for _, left := range counts {
				for _, right := range counts {
					require.LessOrEqual(t, left-right, 1)
				}
			}

			var winners int

			for range count {
				go func() {
					coordinator := Coordinator{Store: store, ClusterID: "proxmox-cluster-a"}
					_, err := coordinator.BeginSubmission(t.Context(), "machine-0", "owner-0")
					results <- err
				}()
			}

			for range count {
				err := <-results
				if err == nil {
					winners++
				} else {
					require.ErrorIs(t, err, ErrUncertainOperation)
				}
			}

			require.Equal(t, 1, winners)
		})
	}
}

func TestLostWritesAndAbandonedOperationsNeverExpire(t *testing.T) {
	coordinator, store := newTestCoordinator()
	intent := testIntent(1)
	store.lost = true
	_, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.ErrorIs(t, err, ErrLedgerUnavailable)
	reserved, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	require.Equal(t, Reserved, reserved.Phase)

	store.lost = true
	_, err = coordinator.BeginSubmission(t.Context(), intent.MachineID, intent.OwnerID)
	require.ErrorIs(t, err, ErrLedgerUnavailable)
	// A process crash, arbitrary age, or new coordinator never resets submission.
	store.mu.Lock()
	record := store.ledger.Records[intent.MachineID]
	record.CreatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	store.ledger.Records[intent.MachineID] = record
	store.mu.Unlock()
	restarted := Coordinator{Store: store, ClusterID: coordinator.ClusterID}
	_, err = restarted.BeginSubmission(t.Context(), intent.MachineID, intent.OwnerID)
	require.ErrorIs(t, err, ErrUncertainOperation)
	replayed, _, err := restarted.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	require.Equal(t, Submitting, replayed.Phase)
}

func TestIntentAndPolicyConflicts(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	intent := testIntent(1)
	_, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)

	tests := map[string]func(*Intent){
		"owner":     func(i *Intent) { i.OwnerID = "different-owner" },
		"VMID":      func(i *Intent) { i.VMID++ },
		"resources": func(i *Intent) { i.Resources.CPU++ },
		"reused ID": func(i *Intent) { i.MachineID = "different-machine" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := testIntent(1)
			mutate(&changed)
			_, _, err := coordinator.Reserve(t.Context(), changed, discoverTestInventory)
			require.ErrorIs(t, err, ErrIdentityConflict)
		})
	}

	changed := testIntent(2)
	changed.Policy.AllowedStorages = []string{"disk-a"}
	_, _, err = coordinator.Reserve(t.Context(), changed, discoverTestInventory)
	require.ErrorIs(t, err, ErrPolicyConflict)
	// A different machine pool may define its own policy.
	changed.Policy.GroupID = "independent-pool"
	_, _, err = coordinator.Reserve(t.Context(), changed, discoverTestInventory)
	require.NoError(t, err)
}

func TestRemovalTransitionReleasesOnlyVerifiedReservation(t *testing.T) {
	coordinator, store := newTestCoordinator()
	intent := testIntent(1)
	_, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	_, err = coordinator.BeginRemoval(t.Context(), intent.MachineID, intent.OwnerID)
	require.NoError(t, err)
	err = coordinator.CompleteRemoval(t.Context(), intent.MachineID, intent.OwnerID)
	require.NoError(t, err)
	document, err := store.Read(t.Context())
	require.NoError(t, err)
	require.Empty(t, document.Ledger.Records)
}

func TestAmbiguousCloneCannotEnterRemoval(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	intent := testIntent(1)
	_, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	_, err = coordinator.BeginSubmission(t.Context(), intent.MachineID, intent.OwnerID)
	require.NoError(t, err)
	_, err = coordinator.BeginRemoval(t.Context(), intent.MachineID, intent.OwnerID)
	require.ErrorIs(t, err, ErrUncertainOperation)
}

func TestReservationFailsClosed(t *testing.T) {
	coordinator, store := newTestCoordinator()
	store.conflict = true
	_, _, err := coordinator.Reserve(t.Context(), testIntent(1), discoverTestInventory)
	require.ErrorIs(t, err, ErrConflict)

	store.conflict = false
	store.ledger.Version = 2
	_, _, err = coordinator.Reserve(t.Context(), testIntent(1), discoverTestInventory)
	require.ErrorIs(t, err, ErrLedgerUnavailable)

	store.ledger = emptyLedger()
	coordinator.ClusterID = "different-cluster"
	_, _, err = coordinator.Reserve(t.Context(), testIntent(1), discoverTestInventory)
	require.ErrorIs(t, err, ErrLedgerUnavailable)

	coordinator.ClusterID = store.ledger.ClusterID
	_, _, err = coordinator.Reserve(t.Context(), testIntent(1), func(context.Context) (Snapshot, error) {
		return Snapshot{}, errors.New("discovery unavailable")
	})
	require.ErrorContains(t, err, "discovery unavailable")
	require.Empty(t, store.ledger.Records)
}

func TestLedgerRejectsCorruptionAndOverflow(t *testing.T) {
	coordinator, store := newTestCoordinator()
	_, _, err := coordinator.Reserve(t.Context(), testIntent(1), discoverTestInventory)
	require.NoError(t, err)

	ledger := cloneLedger(store.ledger)
	record := ledger.Records["machine-1"]
	record.Phase = "unknown"
	ledger.Records["machine-1"] = record
	require.ErrorIs(t, ledger.Validate(), ErrLedgerUnavailable)
	ledger = cloneLedger(store.ledger)

	record = ledger.Records["machine-1"]
	for index := range MaxLedgerRecords + 1 {
		record.MachineID = fmt.Sprintf("machine-%d", index)
		record.OwnerID = fmt.Sprintf("owner-%d", index)
		record.VMID = 100 + index
		ledger.Records[record.MachineID] = record
	}

	require.ErrorIs(t, ledger.Validate(), ErrLedgerFull)
}
