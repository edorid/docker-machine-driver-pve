package authorization

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/stretchr/testify/require"
)

func trustedInventoryRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	proxyCertificate := &x509.Certificate{Subject: pkix.Name{CommonName: "system:auth-proxy"}}
	request.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{proxyCertificate},
		VerifiedChains:   [][]*x509.Certificate{{proxyCertificate}},
	}
	request.Header.Set("X-Remote-User", "user-123")
	request.Header.Add("X-Remote-Group", "system:authenticated")
	return request
}

func TestValidateProxmoxURLRequiresSafeURL(t *testing.T) {
	for _, raw := range []string{"https://pve-a.example:8006", "https://unlisted.example:8006", "https://192.0.2.10:8006"} {
		_, err := validateProxmoxURL(raw)
		require.NoError(t, err, raw)
	}
	for _, raw := range []string{
		"https://user@pve-a.example:8006", "https://pve-a.example:8006/path",
		"https://pve-a.example:8006/?target=other", "https://pve-a.example:8006#fragment", "ftp://pve-a.example",
		"https://pve-a.example:70000",
	} {
		_, err := validateProxmoxURL(raw)
		require.Error(t, err, raw)
	}
}

func TestProxmoxHostAllowlistUsesExactHostnames(t *testing.T) {
	require.True(t, isProxmoxHostAllowed("pve-a.example", []string{"pve-a.example"}))
	require.True(t, isProxmoxHostAllowed("192.0.2.10", []string{"192.0.2.10"}))
	require.False(t, isProxmoxHostAllowed("pve-a.example", []string{"*.example"}))
	require.False(t, isProxmoxHostAllowed("pve-a.example", []string{"pve-a.example:8006"}))
}

func TestInventoryAPIEndpointDiscoveryHasOnlyReadVerb(t *testing.T) {
	request := trustedInventoryRequest(http.MethodGet, inventoryAPIBase)
	response := httptest.NewRecorder()
	(inventoryAPI{}).ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var resources struct {
		Resources []struct {
			Name       string   `json:"name"`
			Namespaced bool     `json:"namespaced"`
			Verbs      []string `json:"verbs"`
		} `json:"resources"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &resources))
	require.Equal(t, "proxmoxinventories", resources.Resources[0].Name)
	require.True(t, resources.Resources[0].Namespaced)
	require.Equal(t, []string{"get"}, resources.Resources[0].Verbs)
}

func TestAggregatorLegacyDiscoveryWithoutGroups(t *testing.T) {
	request := trustedInventoryRequest(http.MethodGet, inventoryAPIBase)
	request.Header.Set("X-Remote-User", "system:kube-aggregator")
	request.Header.Del("X-Remote-Group")
	response := httptest.NewRecorder()
	(inventoryAPI{}).ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var resources struct {
		GroupVersion string `json:"groupVersion"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &resources))
	require.Equal(t, inventoryAPIGroup+"/"+inventoryAPIVersion, resources.GroupVersion)
}

func TestGrouplessAggregatorCannotAccessInventoryOrBypassTLS(t *testing.T) {
	api := inventoryAPI{}
	for _, path := range []string{inventoryAPIBase, inventoryAPIBase + "/namespaces/cattle-global-data/proxmoxinventories/cc-example"} {
		request := trustedInventoryRequest(http.MethodGet, path)
		request.Header.Set("X-Remote-User", "system:kube-aggregator")
		request.Header.Del("X-Remote-Group")
		if path == inventoryAPIBase {
			request.TLS = nil
		}
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}
	request := trustedInventoryRequest(http.MethodGet, inventoryAPIBase)
	request.Header.Set("X-Remote-User", "other-user")
	request.Header.Del("X-Remote-Group")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestInferTemplateNetworkPrefersNet0AndRequiresDHCPAndHostBridge(t *testing.T) {
	network, bridge, dynamic := inferTemplateNetwork(
		map[string]string{"net0": "virtio,bridge=vmbr0", "net1": "virtio,bridge=vmbr1"},
		map[string]string{"ipconfig0": "ip=dhcp"}, map[string]bool{"vmbr0": true},
	)
	require.Equal(t, "net0", network)
	require.Equal(t, "vmbr0", bridge)
	require.True(t, dynamic)

	network, bridge, dynamic = inferTemplateNetwork(
		map[string]string{"net2": "virtio,bridge=vmbr2"},
		map[string]string{"ipconfig2": "ip=192.0.2.5/24"}, map[string]bool{"vmbr2": true},
	)
	require.Equal(t, "net2", network, "the sole interface is used when net0 is absent")
	require.Equal(t, "vmbr2", bridge)
	require.False(t, dynamic)

	network, bridge, dynamic = inferTemplateNetwork(
		map[string]string{"net1": "virtio,bridge=missing", "net2": "virtio,bridge=vmbr2"},
		map[string]string{"ipconfig1": "ip=dhcp"}, map[string]bool{"vmbr2": true},
	)
	require.Empty(t, network, "multiple interfaces without net0 are ambiguous")
	require.Empty(t, bridge)
	require.False(t, dynamic)
}

func TestFindBootDiskHonorsBootOrder(t *testing.T) {
	disks := map[string]string{
		"ide2": "local:cloudinit", "scsi0": "local-lvm:vm-101-disk-0,size=20G", "virtio0": "other:vm-101-disk-1,size=5G",
	}
	require.Equal(t, "virtio0", findBootDisk("order=ide2;virtio0;scsi0", disks))
	require.Empty(t, findBootDisk("order=net0", disks))
	storage, ok := templateStorageID(disks["scsi0"])
	require.True(t, ok)
	require.Equal(t, "local-lvm", storage)
}

func TestInventoryAPIRequiresTrustedAggregatorIdentity(t *testing.T) {
	api := inventoryAPI{discover: func(_ context.Context, _ cloudCredential) (any, error) {
		t.Fatal("discovery must not run for an untrusted request")
		return nil, nil
	}}
	request := httptest.NewRequest(http.MethodGet, inventoryAPIBase, nil)
	request.Header.Set("X-Remote-User", "forged")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestInventoryAPIAuthorizesSecretBeforeDiscovery(t *testing.T) {
	t.Setenv("PLACEMENT_LEDGER_DEFAULT_CPU", "2")
	t.Setenv("PLACEMENT_LEDGER_DEFAULT_MEMORY_BYTES", "1073741824")
	var secretReads, discoveries int
	ledgerCreated := false
	clusterID, ledgerName, err := placement.EndpointIdentity("https://proxmox.example:8006")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case subjectAccessReviewPath:
			_, _ = writer.Write([]byte(`{"status":{"allowed":true}}`))
		case "/api/v1/namespaces/cattle-global-data/secrets/cc-example":
			secretReads++
			_, _ = writer.Write([]byte(`{"metadata":{"name":"cc-example","annotations":{"provisioning.cattle.io/driver":"pve"}},"data":{"pvecredentialConfig-url":"aHR0cHM6Ly9wcm94bW94LmV4YW1wbGU6ODAwNg==","pvecredentialConfig-tokenId":"aWQ=","pvecredentialConfig-tokenSecret":"c2VjcmV0"}}`))
		case "/apis/placement.pve.edorid.github.io/v1alpha1/namespaces/fleet-default/proxmoxplacementledgers/" + ledgerName:
			if !ledgerCreated {
				http.NotFound(writer, request)
				return
			}
			_, _ = writer.Write([]byte(`{"apiVersion":"placement.pve.edorid.github.io/v1alpha1","kind":"ProxmoxPlacementLedger","spec":{"clusterId":"` + clusterID + `"}}`))
		case "/apis/placement.pve.edorid.github.io/v1alpha1/namespaces/fleet-default/proxmoxplacementledgers":
			require.Equal(t, http.MethodPost, request.Method)
			ledgerCreated = true
			writer.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected API request %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	api := inventoryAPI{
		kubernetes: testKubeClient(server),
		allowedHosts: func(context.Context) ([]string, error) {
			return []string{"proxmox.example"}, nil
		},
		discover: func(_ context.Context, credential cloudCredential) (any, error) {
			discoveries++
			require.Equal(t, "secret", credential.TokenSecret)
			return map[string]any{"nodes": []string{"pve-a"}}, nil
		},
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, trustedInventoryRequest(http.MethodGet,
		inventoryAPIBase+"/namespaces/cattle-global-data/proxmoxinventories/cc-example"))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, 1, secretReads)
	require.Equal(t, 1, discoveries)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "ProxmoxInventory", body["kind"])
	require.Equal(t, map[string]any{
		"clusterId": clusterID, "ledgerName": ledgerName, "ledgerNamespace": "fleet-default",
	}, body["placement"])
	require.NotContains(t, response.Body.String(), "secret")
}

func TestInventoryAPIExplainsUnapprovedHostWithoutDiscovering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case subjectAccessReviewPath:
			_, _ = writer.Write([]byte(`{"status":{"allowed":true}}`))
		case "/api/v1/namespaces/cattle-global-data/secrets/cc-example":
			_, _ = writer.Write([]byte(`{"metadata":{"name":"cc-example","annotations":{"provisioning.cattle.io/driver":"pve"}},"data":{"pvecredentialConfig-url":"aHR0cHM6Ly9wcm94bW94LmV4YW1wbGU6ODAwNg==","pvecredentialConfig-tokenId":"aWQ=","pvecredentialConfig-tokenSecret":"c2VjcmV0"}}`))
		default:
			t.Errorf("unexpected Kubernetes request %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	api := inventoryAPI{
		kubernetes:   testKubeClient(server),
		allowedHosts: func(context.Context) ([]string, error) { return nil, nil },
		discover: func(context.Context, cloudCredential) (any, error) {
			t.Fatal("must not connect to an unapproved Proxmox host")
			return nil, nil
		},
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, trustedInventoryRequest(http.MethodGet,
		inventoryAPIBase+"/namespaces/cattle-global-data/proxmoxinventories/cc-example"))
	require.Equal(t, http.StatusForbidden, response.Code)
	require.JSONEq(t, `{"error":"proxmox_host_not_allowed","host":"proxmox.example"}`, response.Body.String())
}

func TestProxmoxAllowedHostsComeFromNodeDriverAndOperatorAllowlist(t *testing.T) {
	t.Setenv("PROXMOX_ALLOWED_HOSTS", "operator-pve.example, pve-a.example")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/apis/management.cattle.io/v3/nodedrivers/pve", request.URL.Path)
		_, _ = writer.Write([]byte(`{"spec":{"whitelistDomains":["pve-a.example","pve-b.example"]}}`))
	}))
	defer server.Close()
	hosts, err := testKubeClient(server).proxmoxAllowedHosts(t.Context())
	require.NoError(t, err)
	require.True(t, isProxmoxHostAllowed("pve-a.example", hosts))
	require.True(t, isProxmoxHostAllowed("pve-b.example", hosts))
	require.True(t, isProxmoxHostAllowed("operator-pve.example", hosts))
}

func TestInventoryAPIDoesNotDiscoverWhenSecretAuthorizationIsDenied(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		require.Equal(t, subjectAccessReviewPath, request.URL.Path)
		_, _ = writer.Write([]byte(`{"status":{"allowed":false,"denied":true}}`))
	}))
	defer server.Close()
	api := inventoryAPI{
		kubernetes: testKubeClient(server),
		discover: func(context.Context, cloudCredential) (any, error) {
			t.Fatal("discovery must not run for a denied credential")
			return nil, nil
		},
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, trustedInventoryRequest(http.MethodGet,
		inventoryAPIBase+"/namespaces/cattle-global-data/proxmoxinventories/cc-example"))
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Equal(t, 1, calls)
}
