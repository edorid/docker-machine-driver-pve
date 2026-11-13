package placement

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type loseFirstPutResponse struct {
	base  http.RoundTripper
	mutex sync.Mutex
	lost  bool
}

func TestReservationPreservesRequiredEmptyHosts(t *testing.T) {
	var putSeen bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"apiVersion":"placement.pve.edorid.github.io/v1alpha1","kind":"ProxmoxPlacementLedger","metadata":{"uid":"ledger-uid","resourceVersion":"1"},"spec":{"version":1,"clusterId":"proxmox-cluster-a","capacityPolicy":{"configured":true,"default":{"cpu":0,"memoryBytes":0},"hosts":{}},"groups":{},"records":{}}}`))
			return
		}
		putSeen = true
		var object map[string]any
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		policy := object["spec"].(map[string]any)["capacityPolicy"].(map[string]any)
		if _, ok := policy["hosts"].(map[string]any); !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		object["metadata"].(map[string]any)["resourceVersion"] = "2"
		_ = json.NewEncoder(w).Encode(object)
	}))
	defer server.Close()
	store, err := NewKubernetesStore(server.URL+"/ledger", "ledger-uid", server.Client(), nil)
	require.NoError(t, err)
	coordinator := Coordinator{Store: store, ClusterID: "proxmox-cluster-a"}
	_, _, err = coordinator.Reserve(t.Context(), testIntent(1), discoverTestInventory)
	require.True(t, putSeen)
	require.NoError(t, err)
}

func TestLedgerRejectsMissingHostOverridesMap(t *testing.T) {
	ledger := emptyLedger()
	ledger.CapacityPolicy.Hosts = nil
	require.ErrorIs(t, ledger.Validate(), ErrLedgerUnavailable)
}

func (transport *loseFirstPutResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err //nolint:wrapcheck // Test transport preserves the underlying error.
	}

	transport.mutex.Lock()
	defer transport.mutex.Unlock()

	if request.Method == http.MethodPut && !transport.lost {
		transport.lost = true
		_ = response.Body.Close()

		return nil, context.DeadlineExceeded
	}

	return response, nil
}

func TestKubernetesCoordinatorConcurrentReservations(t *testing.T) {
	var mutex sync.Mutex

	revision := 1
	ledger := emptyLedger()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()

		if request.Header.Get("Authorization") != "Bearer test-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}

		if request.Method == http.MethodPut {
			var incoming struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
				Spec Ledger `json:"spec"`
			}

			if err := json.NewDecoder(request.Body).Decode(&incoming); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}

			if incoming.Metadata.ResourceVersion != strconv.Itoa(revision) {
				writer.WriteHeader(http.StatusConflict)
				return
			}

			ledger = incoming.Spec
			revision++
		}

		response := map[string]any{
			"apiVersion": LedgerAPIVersion, "kind": LedgerKind,
			"metadata": map[string]string{"uid": "ledger-uid", "resourceVersion": strconv.Itoa(revision)}, "spec": ledger,
		}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	store, err := NewKubernetesStore(server.URL+"/ledger", "ledger-uid", server.Client(), func() (string, error) { return "test-token", nil })
	require.NoError(t, err)

	const count = 10
	results := make(chan error, count)
	start := make(chan struct{})

	for index := range count {
		go func() {
			<-start

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

	counts := map[string]int{}
	for _, record := range document.Ledger.Records {
		counts[record.Node]++
	}

	require.Len(t, document.Ledger.Records, count)
	require.Equal(t, 4, counts["host-a"])
	require.Equal(t, 3, counts["host-b"])
	require.Equal(t, 3, counts["host-c"])
}

func TestKubernetesStoreRejectsRecreatedLedgerAndRedactsErrors(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"apiVersion": LedgerAPIVersion, "kind": LedgerKind,
			"metadata": map[string]string{"uid": "replacement-uid", "resourceVersion": "1"}, "spec": emptyLedger(),
		})
	}))
	defer server.Close()
	store, err := NewKubernetesStore(server.URL+"/ledger", "original-uid", server.Client(), func() (string, error) { return "secret-token", nil })
	require.NoError(t, err)
	_, err = store.Read(t.Context())
	require.ErrorIs(t, err, ErrLedgerUnavailable)
	require.NotContains(t, err.Error(), "secret-token")
}

//nolint:wsl // The server lifecycle and first UID pin are tested together.
func TestKubernetesStorePinsInitialLedgerUID(t *testing.T) {
	uid := "first-ledger-uid"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"apiVersion": LedgerAPIVersion, "kind": LedgerKind,
			"metadata": map[string]string{"uid": uid, "resourceVersion": "1"}, "spec": emptyLedger(),
		})
	}))
	defer server.Close()
	store, err := newKubernetesStore(server.URL+"/ledger", "", false, server.Client(), nil)
	require.NoError(t, err)
	first, err := store.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, uid, first.UID)
	uid = "replacement-ledger-uid"
	_, err = store.Read(t.Context())
	require.ErrorIs(t, err, ErrLedgerUnavailable)
}

func TestKubernetesLostReservationResponseIsRecovered(t *testing.T) {
	var mutex sync.Mutex

	revision := 1
	ledger := emptyLedger()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()

		if request.Method == http.MethodPut {
			var incoming struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
				Spec Ledger `json:"spec"`
			}

			if err := json.NewDecoder(request.Body).Decode(&incoming); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}

			if incoming.Metadata.ResourceVersion != strconv.Itoa(revision) {
				writer.WriteHeader(http.StatusConflict)
				return
			}

			ledger = incoming.Spec
			revision++
		}

		if err := json.NewEncoder(writer).Encode(map[string]any{
			"apiVersion": LedgerAPIVersion, "kind": LedgerKind,
			"metadata": map[string]string{"uid": "ledger-uid", "resourceVersion": strconv.Itoa(revision)}, "spec": ledger,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = &loseFirstPutResponse{base: client.Transport}
	store, err := NewKubernetesStore(server.URL+"/ledger", "ledger-uid", client, nil)
	require.NoError(t, err)

	coordinator := Coordinator{Store: store, ClusterID: "proxmox-cluster-a"}
	intent := testIntent(1)
	_, _, err = coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.Error(t, err)
	recovered, _, err := coordinator.Reserve(t.Context(), intent, discoverTestInventory)
	require.NoError(t, err)
	require.Equal(t, Reserved, recovered.Phase)
	document, err := store.Read(t.Context())
	require.NoError(t, err)
	require.Len(t, document.Ledger.Records, 1)
}
