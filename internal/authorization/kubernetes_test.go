package authorization

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testJobPath = jobCollectionPath

//nolint:cyclop // The mock exercises the full reconciliation protocol.
func TestReconcileCreateRecoversLostRoleAndBindingResponses(t *testing.T) {
	job := placementJob()
	jobWire, err := json.Marshal(map[string]any{"items": []any{
		map[string]any{
			"metadata": map[string]any{"namespace": job.Namespace, "name": job.Name, "uid": job.UID, "labels": job.Labels},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"serviceAccountName": job.ServiceAccount,
				"containers":         []any{map[string]any{"name": "machine", "args": job.Arguments}},
			}}},
		},
	}})
	require.NoError(t, err)

	roleCreated, bindingCreated := false, false
	metadataRoleCreated, metadataBindingCreated := false, false
	loseRoleResponse, loseBindingResponse := true, true

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == testJobPath:
			_, _ = writer.Write(jobWire)
		case request.Method == http.MethodGet && request.URL.Path == roleBindingCollectionPath:
			_, _ = writer.Write([]byte(`{"items":[]}`))
		case request.Method == http.MethodGet && request.URL.Path == "/apis/placement.pve.edorid.github.io/v1alpha1/namespaces/shared-ledgers/proxmoxplacementledgers/proxmox-placement-ledger":
			_, _ = writer.Write([]byte(`{"apiVersion":"placement.pve.edorid.github.io/v1alpha1","kind":"ProxmoxPlacementLedger","spec":{"clusterId":"proxmox-prod"}}`))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/roles/"+machineMetadataRole):
			if metadataRoleCreated {
				role, _ := NewMachineMetadataRole(job.Namespace)
				role.Metadata.UID = "machine-metadata-role-uid"
				_ = json.NewEncoder(writer).Encode(role)
				return
			}
			http.NotFound(writer, request)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/roles/"+LedgerRoleName(LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger"})):
			if roleCreated {
				_ = json.NewEncoder(writer).Encode(serverRole(t))
				return
			}

			http.NotFound(writer, request)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/roles"):
			var role LedgerRole

			if err := json.NewDecoder(request.Body).Decode(&role); err != nil {
				t.Errorf("decode posted Role: %v", err)
				return
			}

			if role.Metadata.Name == machineMetadataRole {
				role.Metadata.UID = "machine-metadata-role-uid"
				metadataRoleCreated = true
			} else {
				role.Metadata.UID = testRoleUID
				roleCreated = true
			}

			if loseRoleResponse {
				loseRoleResponse = false

				http.Error(writer, "response lost", http.StatusInternalServerError)

				return
			}

			writer.WriteHeader(http.StatusCreated)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/rolebindings/"+BindingName(job.Namespace, job.ServiceAccount)+"-capi-read"):
			if metadataBindingCreated {
				binding, _ := NewMachineMetadataRoleBinding(job.Namespace, job.ServiceAccount)
				binding.Metadata.UID = "machine-metadata-binding-uid"
				_ = json.NewEncoder(writer).Encode(binding)
				return
			}
			http.NotFound(writer, request)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/rolebindings/"+BindingName(job.Namespace, job.ServiceAccount)):
			if bindingCreated {
				_ = json.NewEncoder(writer).Encode(serverBinding(t, job))
				return
			}

			http.NotFound(writer, request)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/rolebindings"):
			var binding RoleBinding

			if err := json.NewDecoder(request.Body).Decode(&binding); err != nil {
				t.Errorf("decode posted RoleBinding: %v", err)
				return
			}

			if binding.Metadata.Name == BindingName(job.Namespace, job.ServiceAccount)+"-capi-read" {
				metadataBindingCreated = true
				binding.Metadata.UID = "machine-metadata-binding-uid"
			} else {
				bindingCreated = true
			}

			if loseBindingResponse {
				loseBindingResponse = false

				http.Error(writer, "response lost", http.StatusInternalServerError)

				return
			}

			writer.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected Kubernetes request: %s %s", request.Method, request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := testKubeClient(server)
	require.Error(t, client.reconcile(t.Context())) // Role create succeeded but response was lost.
	require.True(t, roleCreated)
	require.Error(t, client.reconcile(t.Context())) // Ledger binding create succeeded but response was lost.
	require.True(t, bindingCreated)
	require.NoError(t, client.reconcile(t.Context()))
	require.True(t, metadataRoleCreated)
	require.True(t, metadataBindingCreated)
}

//nolint:cyclop // The mock covers multiple removal outcomes.
func TestReconcileRemoveKeepsBindingUntilSuccessAndHandlesLostDelete(t *testing.T) {
	job := placementJob()
	job.Labels[InfraJobRemove] = trueLabelValue
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
	metadataBinding, err := NewMachineMetadataRoleBinding(job.Namespace, job.ServiceAccount)
	require.NoError(t, err)
	metadataBinding.Metadata.UID = "machine-metadata-binding-uid"

	deleteCount := 0
	metadataBindingDeleted := false

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == testJobPath:
			_, _ = writer.Write(jobJSON)
		case request.Method == http.MethodGet && request.URL.Path == roleBindingCollectionPath:
			_, _ = writer.Write(bindingJSON)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/rolebindings/"+metadataBinding.Metadata.Name):
			_ = json.NewEncoder(writer).Encode(metadataBinding)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/rolebindings/"+binding.Metadata.Name):
			_ = json.NewEncoder(writer).Encode(binding)
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/proxmoxplacementledgers/"):
			_, _ = writer.Write([]byte(`{"apiVersion":"placement.pve.edorid.github.io/v1alpha1","kind":"ProxmoxPlacementLedger","spec":{"clusterId":"proxmox-prod"}}`))
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/roles/"):
			_ = json.NewEncoder(writer).Encode(serverRole(t))
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/rolebindings/"+binding.Metadata.Name):
			deleteCount++

			var options struct {
				Preconditions struct {
					UID string `json:"uid"`
				} `json:"preconditions"`
			}

			if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
				t.Errorf("decode DeleteOptions: %v", err)
				return
			}

			if options.Preconditions.UID != binding.Metadata.UID {
				t.Errorf("delete UID precondition = %q, want %q", options.Preconditions.UID, binding.Metadata.UID)
			}

			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/rolebindings/"+metadataBinding.Metadata.Name):
			metadataBindingDeleted = true
			writer.WriteHeader(http.StatusOK)
		case strings.Contains(request.URL.Path, "/proxmoxplacementledgers/"):
			t.Error("successful remove must not need the ledger")
			http.NotFound(writer, request)
		default:
			t.Errorf("unexpected Kubernetes request: %s %s", request.Method, request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := testKubeClient(server)
	require.NoError(t, client.reconcile(t.Context()))
	require.Zero(t, deleteCount, "failed remove must retain access")

	jobJSON = replaceSucceeded(t, jobJSON)
	// Simulate a delete whose server-side result is a success even if its response is lost.
	loseResponse := true
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == testJobPath {
			_, _ = writer.Write(jobJSON)
			return
		}

		if request.Method == http.MethodGet && request.URL.Path == roleBindingCollectionPath {
			if deleteCount > 0 {
				_, _ = writer.Write([]byte(`{"items":[]}`))
			} else {
				_, _ = writer.Write(bindingJSON)
			}

			return
		}

		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/rolebindings/"+metadataBinding.Metadata.Name) {
			if metadataBindingDeleted {
				http.NotFound(writer, request)
			} else {
				_ = json.NewEncoder(writer).Encode(metadataBinding)
			}
			return
		}

		if request.Method == http.MethodDelete {
			if strings.HasSuffix(request.URL.Path, "/rolebindings/"+metadataBinding.Metadata.Name) {
				metadataBindingDeleted = true
				writer.WriteHeader(http.StatusOK)
				return
			}
			deleteCount++

			if loseResponse {
				loseResponse = false

				http.Error(writer, "response lost", http.StatusInternalServerError)

				return
			}

			writer.WriteHeader(http.StatusOK)

			return
		}

		http.NotFound(writer, request)
	})

	require.Error(t, client.reconcile(t.Context()))
	jobJSON = replaceSucceeded(t, jobJSON)
	require.NoError(t, client.reconcile(t.Context()))
	require.Equal(t, 1, deleteCount)
	require.True(t, metadataBindingDeleted)
}

//nolint:varnamelen // The HTTP handler parameters follow net/http's conventional names.
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
			if limit := r.URL.Query().Get("limit"); limit != "500" {
				t.Errorf("first page limit = %q, want 500", limit)
			}

			_, _ = w.Write([]byte(`{"items":[],"metadata":{"continue":"page-2"}}`))

			return
		}

		if continueToken := r.URL.Query().Get("continue"); continueToken != "page-2" {
			t.Errorf("next-page token = %q, want page-2", continueToken)
		}

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
	jobs, err := client.listJobs(t.Context())
	require.NoError(t, err)
	require.Empty(t, jobs)
	require.Equal(t, 2, calls)
	require.Equal(t, 2, tokenRead)
}

func testKubeClient(server *httptest.Server) *kubeClient {
	return &kubeClient{base: server.URL, client: server.Client(), token: "test"}
}

func TestAuthorizeSecretReadScopesReviewToCredential(t *testing.T) {
	identity := requestIdentity{Username: "user-123", UID: "uid-123", Groups: []string{"team-a"}, Extra: map[string][]string{"scope": {"read"}}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, subjectAccessReviewPath, request.URL.Path)
		var body struct {
			Spec struct {
				User   string              `json:"user"`
				UID    string              `json:"uid"`
				Groups []string            `json:"groups"`
				Extra  map[string][]string `json:"extra"`
				Attrs  map[string]string   `json:"resourceAttributes"`
			} `json:"spec"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, identity.Username, body.Spec.User)
		require.Equal(t, identity.UID, body.Spec.UID)
		require.Equal(t, identity.Groups, body.Spec.Groups)
		require.Equal(t, identity.Extra, body.Spec.Extra)
		require.Equal(t, map[string]string{
			"namespace": "cattle-global-data", "verb": "get", "group": "", "resource": "secrets", "name": "cc-example",
		}, body.Spec.Attrs)
		_, _ = writer.Write([]byte(`{"status":{"allowed":true}}`))
	}))
	defer server.Close()
	client := testKubeClient(server)
	require.NoError(t, client.authorizeSecretRead(t.Context(), identity, "cattle-global-data", "cc-example"))
}

func TestIdentityFromAggregatedRequestRejectsUnverifiedOrIncompleteIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Remote-User", "forged-user")
	request.Header.Set("X-Remote-Group", "system:authenticated")
	_, err := identityFromAggregatedRequest(request)
	require.Error(t, err, "ordinary HTTP headers must not establish identity")

	proxyCertificate := &x509.Certificate{Subject: pkix.Name{CommonName: "front-proxy-client"}}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{proxyCertificate}, VerifiedChains: [][]*x509.Certificate{{proxyCertificate}}}
	identity, err := identityFromAggregatedRequest(request)
	require.NoError(t, err)
	require.Equal(t, "forged-user", identity.Username, "a certificate name configured by the Kubernetes trust bundle is accepted")
}

func TestAuthorizeSecretReadFailsClosed(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = writer.Write([]byte(`{"status":{"allowed":true,"denied":true}}`))
	}))
	defer server.Close()
	client := testKubeClient(server)
	identity := requestIdentity{Username: "user-123", Groups: []string{"team-a"}}
	require.Error(t, client.authorizeSecretRead(t.Context(), requestIdentity{}, "cattle-global-data", "cc-example"))
	require.Error(t, client.authorizeSecretRead(t.Context(), identity, "other-namespace", "cc-example"))
	require.Error(t, client.authorizeSecretRead(t.Context(), identity, "cattle-global-data", "cc-example"))
	require.Equal(t, 1, calls)
}

func TestGetCloudCredentialAuthorizesBeforeReadingAndReturnsOnlyExpectedFields(t *testing.T) {
	var sequence []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sequence = append(sequence, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case subjectAccessReviewPath:
			_, _ = writer.Write([]byte(`{"status":{"allowed":true}}`))
		case "/api/v1/namespaces/cattle-global-data/secrets/cc-example":
			_, _ = writer.Write([]byte(`{"metadata":{"name":"cc-example","annotations":{"provisioning.cattle.io/driver":"pve"}},"data":{"pvecredentialConfig-url":"aHR0cHM6Ly9wcm94bW94LmV4YW1wbGU6ODAwNg==","pvecredentialConfig-insecureTls":"ZmFsc2U=","pvecredentialConfig-tokenId":"dXNlckBwYW0hYXBp","pvecredentialConfig-tokenSecret":"c2VjcmV0"}}`))
		default:
			t.Errorf("unexpected Kubernetes request %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	credential, err := testKubeClient(server).getCloudCredential(t.Context(), requestIdentity{Username: "u", Groups: []string{"g"}}, "cc-example")
	require.NoError(t, err)
	require.Equal(t, cloudCredential{URL: "https://proxmox.example:8006", TokenID: "user@pam!api", TokenSecret: "secret"}, credential)
	require.Equal(t, []string{
		"POST " + subjectAccessReviewPath,
		"GET /api/v1/namespaces/cattle-global-data/secrets/cc-example",
	}, sequence)
}

func TestGetCloudCredentialNeverReadsWhenAuthorizationIsDenied(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		require.Equal(t, subjectAccessReviewPath, request.URL.Path)
		_, _ = writer.Write([]byte(`{"status":{"allowed":false,"denied":true}}`))
	}))
	defer server.Close()
	_, err := testKubeClient(server).getCloudCredential(t.Context(), requestIdentity{Username: "u", Groups: []string{"g"}}, "cc-example")
	require.Error(t, err)
	require.Equal(t, 1, calls, "denied authorization must prevent the Secret GET")
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

func serverRole(t *testing.T) LedgerRole {
	t.Helper()

	ref := LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger", ClusterID: "proxmox-prod"}
	role, err := NewLedgerRole(ref)
	require.NoError(t, err)

	role.Metadata.UID = testRoleUID

	return role
}

func replaceSucceeded(t *testing.T, input []byte) []byte {
	t.Helper()

	var object map[string]any

	require.NoError(t, json.Unmarshal(input, &object))

	items, ok := object["items"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, items)
	firstItem, ok := items[0].(map[string]any)
	require.True(t, ok)

	firstItem["status"] = map[string]any{"succeeded": 1}
	data, err := json.Marshal(object)
	require.NoError(t, err)

	return data
}
