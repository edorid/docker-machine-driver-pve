package placement

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	RancherClusterLabel      = "cluster.x-k8s.io/cluster-name"
	RancherPoolLabel         = "rke.cattle.io/rke-machine-pool-name"
	maxMachineListBytes      = 4 << 20
	rancherServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	rancherMetadataTimeout   = 10 * time.Second
)

type RancherMachineNames struct {
	Cluster string
	Pool    string
}

func DiscoverInClusterRancherMachineNames(ctx context.Context, infrastructureName string) (RancherMachineNames, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	portNumber, err := strconv.Atoi(port)
	if host == "" || err != nil || portNumber < 1 || portNumber > 65535 || strings.ContainsAny(host, "/@\r\n") {
		return RancherMachineNames{}, errors.New("in-cluster Kubernetes API endpoint is unavailable")
	}
	namespaceBytes, err := os.ReadFile(filepath.Join(rancherServiceAccountDir, "namespace"))
	if err != nil {
		return RancherMachineNames{}, errors.New("provisioning pod namespace is unavailable")
	}
	namespace := strings.TrimSpace(string(namespaceBytes))
	if namespace == "" || strings.ContainsAny(namespace, "/\r\n") {
		return RancherMachineNames{}, errors.New("provisioning pod namespace is invalid")
	}
	caPEM, err := os.ReadFile(filepath.Join(rancherServiceAccountDir, "ca.crt"))
	if err != nil {
		return RancherMachineNames{}, errors.New("Kubernetes service CA is unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return RancherMachineNames{}, errors.New("Kubernetes service CA is invalid")
	}
	tokenBytes, err := os.ReadFile(filepath.Join(rancherServiceAccountDir, "token"))
	if err != nil {
		return RancherMachineNames{}, errors.New("provisioning service-account token is unavailable")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return RancherMachineNames{}, errors.New("provisioning service-account token is invalid")
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return RancherMachineNames{}, errors.New("default HTTP transport is incompatible")
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	client := &http.Client{Transport: transport, Timeout: rancherMetadataTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	apiBase := "https://" + net.JoinHostPort(host, port)
	return DiscoverRancherMachineNames(ctx, client, apiBase, namespace, infrastructureName, token)
}

type capiMachineList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			InfrastructureRef struct {
				Name string `json:"name"`
			} `json:"infrastructureRef"`
		} `json:"spec"`
	} `json:"items"`
}

// DiscoverRancherMachineNames derives names only from the CAPI Machine whose
// infrastructureRef identifies this provider machine. It rejects incomplete,
// ambiguous, and paginated-partial results rather than guessing.
func DiscoverRancherMachineNames(ctx context.Context, client *http.Client, apiBase, namespace, infrastructureName, token string) (RancherMachineNames, error) {
	if client == nil || !validKubernetesName(namespace) || !validKubernetesName(infrastructureName) || token == "" {
		return RancherMachineNames{}, errors.New("CAPI machine discovery requires a client, namespace, machine name, and service-account token")
	}
	base, err := url.Parse(apiBase)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return RancherMachineNames{}, errors.New("invalid in-cluster Kubernetes API endpoint")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/apis/cluster.x-k8s.io/v1beta1/namespaces/" + url.PathEscape(namespace) + "/machines"
	var match *RancherMachineNames
	continueToken := ""
	for pages := 0; pages < 20; pages++ {
		endpoint := *base
		query := endpoint.Query()
		query.Set("limit", "200")
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return RancherMachineNames{}, errors.New("construct CAPI machine discovery request")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return RancherMachineNames{}, fmt.Errorf("read CAPI machine metadata: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxMachineListBytes+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || len(body) > maxMachineListBytes {
			return RancherMachineNames{}, errors.New("CAPI machine metadata response is incomplete or too large")
		}
		if response.StatusCode != http.StatusOK {
			return RancherMachineNames{}, fmt.Errorf("CAPI machine metadata API returned HTTP %d", response.StatusCode)
		}
		var list capiMachineList
		if err := json.Unmarshal(body, &list); err != nil {
			return RancherMachineNames{}, errors.New("CAPI machine metadata response is invalid")
		}
		for _, machine := range list.Items {
			if machine.Spec.InfrastructureRef.Name != infrastructureName {
				continue
			}
			found := RancherMachineNames{
				Cluster: strings.TrimSpace(machine.Metadata.Labels[RancherClusterLabel]),
				Pool:    strings.TrimSpace(machine.Metadata.Labels[RancherPoolLabel]),
			}
			if found.Cluster == "" || found.Pool == "" || match != nil {
				return RancherMachineNames{}, errors.New("CAPI machine metadata is missing or ambiguous for this provider machine")
			}
			match = &found
		}
		continueToken = list.Metadata.Continue
		if continueToken == "" {
			if match == nil {
				return RancherMachineNames{}, errors.New("no CAPI machine references this provider machine")
			}
			return *match, nil
		}
	}
	return RancherMachineNames{}, errors.New("CAPI machine metadata pagination exceeded the safe discovery limit")
}
