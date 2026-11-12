//nolint:wsl // Kubernetes CAS checks are separated for clarity at the trust boundary.
package placement

import (
	"bytes"
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
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	LedgerAPIVersion  = "placement.pve.edorid.github.io/v1alpha1"
	LedgerKind        = "ProxmoxPlacementLedger"
	requestTimeout    = 10 * time.Second
	maxObjectBytes    = 1024 * 1024
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

var dnsSubdomain = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

// KubernetesStore updates an administrator-created ledger using Kubernetes
// resourceVersion optimistic concurrency. It never creates or recreates a missing
// ledger, follows redirects, or logs response bodies/credentials.
type KubernetesStore struct {
	objectURL   string
	expectedUID string
	client      *http.Client
	token       func() (string, error)
	uidMutex    sync.Mutex
	uidPinned   bool
}

// NewInClusterKubernetesStore uses the provisioner's projected ServiceAccount
// token and the Kubernetes service CA. The initial GET pins the ledger UID.
//
//nolint:wsl // Validate endpoint, trust roots, and projected credentials at one fail-closed boundary.
func NewInClusterKubernetesStore(namespace, name string) (*KubernetesStore, error) {
	if !validKubernetesName(namespace) || !validKubernetesName(name) {
		return nil, errors.New("invalid placement ledger namespace or name")
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	portNumber, portErr := strconv.Atoi(port)
	if host == "" || portErr != nil || portNumber < 1 || portNumber > 65535 || strings.ContainsAny(host, "/@\r\n") {
		return nil, errors.New("in-cluster kubernetes endpoint is unavailable")
	}
	caPath := path.Join(serviceAccountDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, errors.New("kubernetes service CA is unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kubernetes service CA is invalid")
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport is incompatible")
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	client := &http.Client{Transport: transport}
	objectURL := "https://" + net.JoinHostPort(host, port) + "/apis/" + LedgerAPIVersion + "/namespaces/" + url.PathEscape(namespace) + "/proxmoxplacementledgers/" + url.PathEscape(name)
	tokenPath := path.Join(serviceAccountDir, "token")
	return newKubernetesStore(objectURL, "", false, client, func() (string, error) {
		data, err := os.ReadFile(tokenPath)
		if err != nil {
			return "", ErrLedgerUnavailable
		}
		token := strings.TrimSpace(string(data))
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return "", ErrLedgerUnavailable
		}
		return token, nil
	})
}

func validKubernetesName(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dnsSubdomain.MatchString(value)
}

// NewKubernetesStore accepts the exact HTTPS URL of one namespaced custom
// resource. The client must validate the management cluster's TLS certificate;
// callers configure its trust roots or mTLS. The token callback permits rotation.
func NewKubernetesStore(objectURL, expectedUID string, client *http.Client, token func() (string, error)) (*KubernetesStore, error) {
	return newKubernetesStore(objectURL, expectedUID, true, client, token)
}

func newKubernetesStore(objectURL, expectedUID string, uidPinned bool, client *http.Client, token func() (string, error)) (*KubernetesStore, error) {
	endpoint, err := url.Parse(objectURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path == "" || client == nil || (uidPinned && expectedUID == "") {
		return nil, errors.New("an exact HTTPS ledger object URL and TLS-verifying HTTP client are required")
	}

	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	return &KubernetesStore{objectURL: endpoint.String(), expectedUID: expectedUID, uidPinned: uidPinned, client: &copyClient, token: token}, nil
}

//nolint:wsl // UID pinning and validation remain one atomic in-memory operation.
func (s *KubernetesStore) Read(ctx context.Context) (Document, error) {
	data, err := s.request(ctx, http.MethodGet, nil)
	if err != nil {
		return Document{}, err
	}

	document, err := decodeDocument(data)
	if err != nil {
		return Document{}, err
	}

	s.uidMutex.Lock()
	defer s.uidMutex.Unlock()
	if !s.uidPinned {
		s.expectedUID = document.UID
		s.uidPinned = true
	}
	if document.UID != s.expectedUID {
		return Document{}, ErrLedgerUnavailable
	}

	return document, nil
}

//nolint:cyclop // Each check protects the atomic update boundary.
func (s *KubernetesStore) CompareAndSwap(ctx context.Context, previous Document, next Ledger) error {
	s.uidMutex.Lock()
	uidMatches := s.uidPinned && previous.UID == s.expectedUID
	s.uidMutex.Unlock()

	if !uidMatches || previous.Revision == "" || len(previous.envelope) == 0 || next.ClusterID != previous.Ledger.ClusterID {
		return ErrLedgerUnavailable
	}

	if err := next.Validate(); err != nil {
		return err
	}

	envelope := make(map[string]json.RawMessage, len(previous.envelope))
	for key, value := range previous.envelope {
		envelope[key] = value
	}

	var metadata struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	}

	if err := json.Unmarshal(envelope["metadata"], &metadata); err != nil ||
		metadata.UID != previous.UID || metadata.ResourceVersion != previous.Revision {
		return ErrLedgerUnavailable
	}

	var err error

	envelope["spec"], err = json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode ledger update: %w", err)
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode ledger object: %w", err)
	}

	if len(payload) > maxObjectBytes {
		return ErrLedgerFull
	}

	data, err := s.request(ctx, http.MethodPut, payload)
	if err != nil {
		// Only an explicit 409 is safe to retry. A timeout, 5xx or lost response
		// can follow a successful write; callers must not infer rollback.
		return err
	}

	updated, err := decodeDocument(data)
	if err != nil {
		return err
	}

	if updated.UID != previous.UID || updated.Revision == previous.Revision || updated.Ledger.ClusterID != next.ClusterID {
		return ErrLedgerUnavailable
	}
	// Admission webhooks must not silently remove or alter reserved intent.
	want, err := json.Marshal(next)
	if err != nil {
		return ErrLedgerUnavailable
	}

	got, err := json.Marshal(updated.Ledger)
	if err != nil {
		return ErrLedgerUnavailable
	}

	if !bytes.Equal(want, got) {
		return ErrLedgerUnavailable
	}

	return nil
}

func (s *KubernetesStore) request(ctx context.Context, method string, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, method, s.objectURL, bytes.NewReader(payload))
	if err != nil {
		return nil, ErrLedgerUnavailable
	}

	request.Header.Set("Accept", "application/json")

	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}

	if s.token != nil {
		token, err := s.token()
		if err != nil || strings.ContainsAny(token, "\r\n") || strings.TrimSpace(token) != token || token == "" {
			return nil, ErrLedgerUnavailable
		}

		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("management Kubernetes request: %w", ctx.Err())
		}
		// net/http errors can contain endpoint or transport credential details.
		return nil, ErrLedgerUnavailable
	}

	defer response.Body.Close()

	if response.StatusCode == http.StatusConflict {
		return nil, ErrConflict
	}

	if response.StatusCode == http.StatusRequestEntityTooLarge {
		return nil, ErrLedgerFull
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management Kubernetes API HTTP %d: %w", response.StatusCode, ErrLedgerUnavailable)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxObjectBytes+1))
	if err != nil {
		return nil, ErrLedgerUnavailable
	}

	if len(data) > maxObjectBytes {
		return nil, ErrLedgerFull
	}

	return data, nil
}

func decodeDocument(data []byte) (Document, error) {
	var wire struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			UID               string  `json:"uid"`
			ResourceVersion   string  `json:"resourceVersion"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec json.RawMessage `json:"spec"`
	}

	if err := json.Unmarshal(data, &wire); err != nil || wire.APIVersion != LedgerAPIVersion || wire.Kind != LedgerKind ||
		wire.Metadata.UID == "" || wire.Metadata.ResourceVersion == "" || wire.Metadata.DeletionTimestamp != nil {
		return Document{}, ErrLedgerUnavailable
	}

	var ledger Ledger

	decoder := json.NewDecoder(bytes.NewReader(wire.Spec))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&ledger); err != nil {
		return Document{}, ErrLedgerUnavailable
	}

	if err := ledger.Validate(); err != nil {
		return Document{}, err
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Document{}, ErrLedgerUnavailable
	}

	return Document{UID: wire.Metadata.UID, Revision: wire.Metadata.ResourceVersion, Ledger: ledger, envelope: envelope}, nil
}
