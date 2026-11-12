package authorization

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcileCreateRecoversLostRoleAndBindingResponses(t *testing.T) {
	job := placementJob()
	jobWire, err := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"namespace": job.Namespace, "name": job.Name, "uid": job.UID, "labels": job.Labels},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"serviceAccountName": job.ServiceAccount,
			"containers":         []any{map[string]any{"name": "machine", "args": job.Arguments}},
		}}}},
	}})
	require.NoError(t, err)
	roleCreated, bindingCreated := false, false
	loseRoleResponse, loseBindingResponse := true, true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/batch/v1/jobs":
			_, _ = w.Write(jobWire)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/rbac.authorization.k8s.io/v1/rolebindings":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/apis/placement.pve.edorid.github.io/v1alpha1/namespaces/shared-ledgers/proxmoxplacementledgers/proxmox-placement-ledger":
			_, _ = w.Write([]byte(`{"apiVersion":"placement.pve.edorid.github.io/v1alpha1","kind":"ProxmoxPlacementLedger","spec":{"clusterId":"proxmox-prod"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/roles/"+LedgerRoleName(LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger"})):
			if roleCreated {
				_ = json.NewEncoder(w).Encode(serverRole(t, r))
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/roles"):
			var role LedgerRole
			require.NoError(t, json.NewDecoder(r.Body).Decode(&role))
			role.Metadata.UID = "role-uid"
			roleCreated = true
			if loseRoleResponse {
				loseRoleResponse = false
				http.Error(w, "response lost", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rolebindings/"+BindingName(job.Namespace, job.ServiceAccount)):
			if bindingCreated {
				_ = json.NewEncoder(w).Encode(serverBinding(t, job))
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rolebindings"):
			var binding RoleBinding
			require.NoError(t, json.NewDecoder(r.Body).Decode(&binding))
			bindingCreated = true
			if loseBindingResponse {
				loseBindingResponse = false
				http.Error(w, "response lost", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testKubeClient(server)
	require.Error(t, client.reconcile(context.Background())) // Role create succeeded but response was lost.
	require.True(t, roleCreated)
	require.Error(t, client.reconcile(context.Background())) // Binding create succeeded but response was lost.
	require.True(t, bindingCreated)
	require.NoError(t, client.reconcile(context.Background()))
}

func TestReconcileRemoveKeepsBindingUntilSuccessAndHandlesLostDelete(t *testing.T) {
	job := placementJob()
	job.Labels[InfraJobRemove] = "true"
	job.Arguments = []string{"rm", "-y", "--update-config", "node"}
	jobJSON, err := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"namespace": job.Namespace, "name": job.Name, "uid": job.UID, "labels": job.Labels},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"serviceAccountName": job.ServiceAccount, "containers": []any{map[string]any{"name": "machine", "args": job.Arguments}},
		}}},
		"status": map[string]any{"succeeded": 0},
	}}})
	require.NoError(t, err)
	binding := serverBinding(t, placementJob())
	bindingJSON, err := json.Marshal(map[string]any{"items": []RoleBinding{binding}})
	require.NoError(t, err)
	deleteCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/batch/v1/jobs":
			_, _ = w.Write(jobJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/rbac.authorization.k8s.io/v1/rolebindings":
			_, _ = w.Write(bindingJSON)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rolebindings/"+binding.Metadata.Name):
			_ = json.NewEncoder(w).Encode(binding)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/proxmoxplacementledgers/"):
			_, _ = w.Write([]byte(`{"apiVersion":"placement.pve.edorid.github.io/v1alpha1","kind":"ProxmoxPlacementLedger","spec":{"clusterId":"proxmox-prod"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/roles/"):
			_ = json.NewEncoder(w).Encode(serverRole(t, r))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/rolebindings/"+binding.Metadata.Name):
			deleteCount++
			var options map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&options))
			require.Equal(t, binding.Metadata.UID, options["preconditions"].(map[string]any)["uid"])
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/proxmoxplacementledgers/"):
			t.Error("successful remove must not need the ledger")
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testKubeClient(server)
	require.NoError(t, client.reconcile(context.Background()))
	require.Zero(t, deleteCount, "failed remove must retain access")

	jobJSON = replaceSucceeded(t, jobJSON)
	// Simulate a delete whose server-side result is a success even if its response is lost.
	loseResponse := true
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apis/batch/v1/jobs" {
			_, _ = w.Write(jobJSON)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/apis/rbac.authorization.k8s.io/v1/rolebindings" {
			if deleteCount > 0 {
				_, _ = w.Write([]byte(`{"items":[]}`))
			} else {
				_, _ = w.Write(bindingJSON)
			}
			return
		}
		if r.Method == http.MethodDelete {
			deleteCount++
			if loseResponse {
				loseResponse = false
				http.Error(w, "response lost", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	require.Error(t, client.reconcile(context.Background()))
	jobJSON = replaceSucceeded(t, jobJSON)
	require.NoError(t, client.reconcile(context.Background()))
	require.Equal(t, 1, deleteCount)
}

func TestJobListPaginatesAndUsesCurrentProjectedToken(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.Header.Get("Authorization") {
		case "Bearer token-1", "Bearer token-2":
		default:
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		if calls == 1 {
			require.Equal(t, "500", r.URL.Query().Get("limit"))
			_, _ = w.Write([]byte(`{"items":[],"metadata":{"continue":"page-2"}}`))
			return
		}
		require.Equal(t, "page-2", r.URL.Query().Get("continue"))
		_, _ = w.Write([]byte(`{"items":[],"metadata":{"continue":""}}`))
	}))
	defer server.Close()
	tokenRead := 0
	client := &kubeClient{
		base: server.URL, client: server.Client(),
		tokenSource: func() (string, error) {
			tokenRead++
			return fmt.Sprintf("token-%d", tokenRead), nil
		},
	}
	jobs, err := client.listJobs(context.Background())
	require.NoError(t, err)
	require.Empty(t, jobs)
	require.Equal(t, 2, calls)
	require.Equal(t, 2, tokenRead)
}

func testKubeClient(server *httptest.Server) *kubeClient {
	return &kubeClient{base: server.URL, client: server.Client(), token: "test"}
}

func serverBinding(t *testing.T, job Job) RoleBinding {
	t.Helper()
	decision, err := Evaluate(job, nil)
	require.NoError(t, err)
	binding, err := NewRoleBinding(decision)
	require.NoError(t, err)
	binding.Metadata.UID = "binding-uid"
	return binding
}

func serverRole(t *testing.T, request *http.Request) LedgerRole {
	t.Helper()
	ref := LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger", ClusterID: "proxmox-prod"}
	role, err := NewLedgerRole(ref)
	require.NoError(t, err)
	role.Metadata.UID = "role-uid"
	return role
}

func replaceSucceeded(t *testing.T, input []byte) []byte {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal(input, &object))
	object["items"].([]any)[0].(map[string]any)["status"] = map[string]any{"succeeded": 1}
	data, err := json.Marshal(object)
	require.NoError(t, err)
	return data
}
