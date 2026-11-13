package placement

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverRancherMachineNamesMatchesInfrastructureReference(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/apis/cluster.x-k8s.io/v1beta1/namespaces/fleet-default/machines", r.URL.Path)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"labels": map[string]string{}}, "spec": map[string]any{"infrastructureRef": map[string]string{"name": "other"}}},
			map[string]any{
				"metadata": map[string]any{"labels": map[string]string{RancherClusterLabel: "c-m-abc12345", RancherPoolLabel: "workers-east"}},
				"spec":     map[string]any{"infrastructureRef": map[string]string{"name": "pve-worker-1"}},
			},
		}})
	}))
	defer server.Close()

	names, err := DiscoverRancherMachineNames(context.Background(), server.Client(), server.URL, "fleet-default", "pve-worker-1", "test-token")
	require.NoError(t, err)
	require.Equal(t, RancherMachineNames{Cluster: "c-m-abc12345", Pool: "workers-east"}, names)
}

func TestDiscoverRancherMachineNamesFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		items []any
	}{
		{name: "missing machine", items: []any{}},
		{name: "missing pool", items: []any{map[string]any{"metadata": map[string]any{"labels": map[string]string{RancherClusterLabel: "cluster-a"}}, "spec": map[string]any{"infrastructureRef": map[string]string{"name": "pve-worker-1"}}}}},
		{name: "ambiguous machine", items: []any{
			map[string]any{"metadata": map[string]any{"labels": map[string]string{RancherClusterLabel: "cluster-a", RancherPoolLabel: "pool-a"}}, "spec": map[string]any{"infrastructureRef": map[string]string{"name": "pve-worker-1"}}},
			map[string]any{"metadata": map[string]any{"labels": map[string]string{RancherClusterLabel: "cluster-a", RancherPoolLabel: "pool-a"}}, "spec": map[string]any{"infrastructureRef": map[string]string{"name": "pve-worker-1"}}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": test.items})
			}))
			defer server.Close()
			_, err := DiscoverRancherMachineNames(context.Background(), server.Client(), server.URL, "fleet-default", "pve-worker-1", "test-token")
			require.Error(t, err)
		})
	}
}

func TestDiscoverRancherMachineNamesRequiresHTTPS(t *testing.T) {
	_, err := DiscoverRancherMachineNames(context.Background(), http.DefaultClient, "http://kubernetes.default.svc", "fleet-default", "worker", "token")
	require.Error(t, err)
}
