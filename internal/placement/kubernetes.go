package placement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	LedgerAPIVersion = "placement.pve.edorid.github.io/v1alpha1"
	LedgerKind       = "ProxmoxPlacementLedger"
	requestTimeout   = 10 * time.Second
	maxObjectBytes   = 1024 * 1024
)

// KubernetesStore updates an administrator-created ledger using Kubernetes
// resourceVersion optimistic concurrency. It never creates or recreates a missing
// ledger, follows redirects, or logs response bodies/credentials.
type KubernetesStore struct {
	objectURL   string
	expectedUID string
	client      *http.Client
	token       func() (string, error)
}

// NewKubernetesStore accepts the exact HTTPS URL of one namespaced custom
// resource. The client must validate the management cluster's TLS certificate;
// callers configure its trust roots or mTLS. The token callback permits rotation.
func NewKubernetesStore(objectURL, expectedUID string, client *http.Client, token func() (string, error)) (*KubernetesStore, error) {
	endpoint, err := url.Parse(objectURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path == "" || client == nil || expectedUID == "" {
		return nil, errors.New("an exact HTTPS ledger object URL and TLS-verifying HTTP client are required")
	}

	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	return &KubernetesStore{objectURL: endpoint.String(), expectedUID: expectedUID, client: &copyClient, token: token}, nil
}

func (s *KubernetesStore) Read(ctx context.Context) (Document, error) {
	data, err := s.request(ctx, http.MethodGet, nil)
	if err != nil {
		return Document{}, err
	}

	document, err := decodeDocument(data)
	if err != nil {
		return Document{}, err
	}

	if document.UID != s.expectedUID {
		return Document{}, ErrLedgerUnavailable
	}

	return document, nil
}

//nolint:cyclop // Each check protects the atomic update boundary.
func (s *KubernetesStore) CompareAndSwap(ctx context.Context, previous Document, next Ledger) error {
	if previous.UID != s.expectedUID || previous.Revision == "" || len(previous.envelope) == 0 || next.ClusterID != previous.Ledger.ClusterID {
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
