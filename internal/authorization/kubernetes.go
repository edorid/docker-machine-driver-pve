package authorization

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ledgerAPIPath = "/apis/placement.pve.edorid.github.io/v1alpha1"
	requestLimit  = 16 << 20
)

var (
	ErrNotFound = errors.New("Kubernetes object not found")
	ErrConflict = errors.New("Kubernetes resource version conflict")
)

type kubeClient struct {
	base        string
	client      *http.Client
	token       string
	tokenSource func() (string, error)
}

// inClusterClient uses the rotating projected service-account token and the
// Kubernetes service CA. It never accepts an insecure TLS configuration.
func inClusterClient() (*kubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if host == "" || port == "" || strings.ContainsAny(host+port, "/@\r\n") {
		return nil, errors.New("in-cluster Kubernetes service environment is unavailable")
	}
	endpoint := "https://" + host + ":" + port
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, errors.New("in-cluster Kubernetes endpoint is invalid")
	}
	caPEM, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, errors.New("Kubernetes service CA is unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err != nil {
		return nil, errors.New("projected Kubernetes service-account token is unavailable")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &kubeClient{
		base: endpoint,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		tokenSource: func() (string, error) {
			token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
			if err != nil || strings.TrimSpace(string(token)) == "" {
				return "", errors.New("projected Kubernetes service-account token is unavailable")
			}
			return strings.TrimSpace(string(token)), nil
		},
	}, nil
}

func (c *kubeClient) request(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, errors.New("encode Kubernetes request")
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, 0, errors.New("build Kubernetes request")
	}
	token := c.token
	if c.tokenSource != nil {
		token, err = c.tokenSource()
		if err != nil {
			return nil, 0, errors.New("read projected Kubernetes service-account token")
		}
	}
	if token == "" {
		return nil, 0, errors.New("Kubernetes service-account token is unavailable")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, errors.New("Kubernetes API request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, requestLimit+1))
	if err != nil || len(data) > requestLimit {
		return nil, response.StatusCode, errors.New("Kubernetes API response unavailable or oversized")
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
		return data, response.StatusCode, nil
	case http.StatusNotFound:
		return nil, response.StatusCode, ErrNotFound
	case http.StatusConflict:
		return nil, response.StatusCode, ErrConflict
	default:
		return nil, response.StatusCode, fmt.Errorf("Kubernetes API returned HTTP %d", response.StatusCode)
	}
}

type jobList struct {
	Items    []jobWire `json:"items"`
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
}

type jobWire struct {
	Metadata struct {
		Namespace string            `json:"namespace"`
		Name      string            `json:"name"`
		UID       string            `json:"uid"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `json:"serviceAccountName"`
				Containers         []struct {
					Name string   `json:"name"`
					Args []string `json:"args"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Succeeded int32 `json:"succeeded"`
	} `json:"status"`
}

func (c *kubeClient) listJobs(ctx context.Context) ([]Job, error) {
	var jobs []Job
	continueToken := ""
	for page := 0; page < 100; page++ {
		path := "/apis/batch/v1/jobs?limit=500"
		if continueToken != "" {
			path += "&continue=" + url.QueryEscape(continueToken)
		}
		data, _, err := c.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var list jobList
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, errors.New("decode Rancher provisioning Jobs")
		}
		if len(jobs)+len(list.Items) > 50000 {
			return nil, errors.New("Rancher Job inventory exceeds safe controller limit")
		}
		for _, item := range list.Items {
			var args []string
			for _, container := range item.Spec.Template.Spec.Containers {
				if container.Name == "machine" {
					args = container.Args
					break
				}
			}
			jobs = append(jobs, Job{
				Namespace: item.Metadata.Namespace, Name: item.Metadata.Name, UID: item.Metadata.UID,
				Labels: item.Metadata.Labels, ServiceAccount: item.Spec.Template.Spec.ServiceAccountName,
				Arguments: args, Succeeded: item.Status.Succeeded,
			})
		}
		continueToken = list.Metadata.Continue
		if continueToken == "" {
			return jobs, nil
		}
	}
	return nil, errors.New("Rancher Job pagination exceeded safe controller limit")
}

func (c *kubeClient) getLedger(ctx context.Context, ref LedgerRef) error {
	path := fmt.Sprintf("%s/namespaces/%s/proxmoxplacementledgers/%s", ledgerAPIPath, ref.Namespace, ref.Name)
	data, _, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	var object struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			ClusterID string `json:"clusterId"`
		} `json:"spec"`
	}
		if json.Unmarshal(data, &object) != nil || object.APIVersion != "placement.pve.edorid.github.io/v1alpha1" ||
		object.Kind != "ProxmoxPlacementLedger" || object.Spec.ClusterID != ref.ClusterID {
		return errors.New("placement ledger identity does not match requested Proxmox cluster")
	}
	return nil
}

type roleBindingList struct {
	Items    []RoleBinding `json:"items"`
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
}

func (c *kubeClient) listManagedBindings(ctx context.Context) (map[string]RoleBinding, error) {
	bindings := make(map[string]RoleBinding)
	continueToken := ""
	for page := 0; page < 100; page++ {
		path := "/apis/rbac.authorization.k8s.io/v1/rolebindings?limit=500&labelSelector=" + url.QueryEscape(managedLabel+"=true")
		if continueToken != "" {
			path += "&continue=" + url.QueryEscape(continueToken)
		}
		data, _, err := c.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var list roleBindingList
		if json.Unmarshal(data, &list) != nil {
			return nil, errors.New("decode managed placement RoleBindings")
		}
		if len(bindings)+len(list.Items) > 50000 {
			return nil, errors.New("managed RoleBinding inventory exceeds safe controller limit")
		}
		for _, binding := range list.Items {
			if binding.Metadata.Labels[managedLabel] != "true" {
				continue
			}
			namespace := binding.Metadata.Annotations[sourceNSAnnotation]
			serviceAccount := binding.Metadata.Annotations[sourceSAAnnotation]
			ledgerName := binding.Metadata.Annotations[ledgerNameAnnotation]
			if !validDNS(namespace) || !validDNS(serviceAccount) || !validDNS(ledgerName) || binding.Metadata.Name != BindingName(namespace, serviceAccount) {
				return nil, ErrBindingMismatch
			}
			key := namespace + "/" + serviceAccount
			if _, exists := bindings[key]; exists {
				return nil, ErrBindingMismatch
			}
			bindings[key] = binding
		}
		continueToken = list.Metadata.Continue
		if continueToken == "" {
			return bindings, nil
		}
	}
	return nil, errors.New("managed RoleBinding pagination exceeded safe controller limit")
}

func bindingKey(namespace, serviceAccount string) string { return namespace + "/" + serviceAccount }

func (c *kubeClient) ensureBinding(ctx context.Context, wanted RoleBinding) error {
	path := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + wanted.Metadata.Namespace + "/rolebindings/" + wanted.Metadata.Name
	data, _, err := c.request(ctx, http.MethodGet, path, nil)
	if err == nil {
		existing, decodeErr := decodeRoleBinding(data)
		if decodeErr != nil {
			return decodeErr
		}
		return ValidateExisting(existing, wanted)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	collection := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + wanted.Metadata.Namespace + "/rolebindings"
	if _, _, err := c.request(ctx, http.MethodPost, collection, wanted); err != nil {
		if !errors.Is(err, ErrConflict) {
			return err
		}
		data, _, readErr := c.request(ctx, http.MethodGet, path, nil)
		if readErr != nil {
			return readErr
		}
		existing, decodeErr := decodeRoleBinding(data)
		if decodeErr != nil {
			return decodeErr
		}
		return ValidateExisting(existing, wanted)
	}
	return nil
}

func decodeRole(data []byte) (LedgerRole, error) {
	var role LedgerRole
	if err := json.Unmarshal(data, &role); err != nil {
		return LedgerRole{}, ErrBindingMismatch
	}
	return role, nil
}

func (c *kubeClient) ensureLedgerRole(ctx context.Context, wanted LedgerRole) error {
	path := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + wanted.Metadata.Namespace + "/roles/" + wanted.Metadata.Name
	data, _, err := c.request(ctx, http.MethodGet, path, nil)
	if err == nil {
		existing, decodeErr := decodeRole(data)
		if decodeErr != nil {
			return decodeErr
		}
		return ValidateExistingRole(existing, wanted)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	collection := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + wanted.Metadata.Namespace + "/roles"
	if _, _, err := c.request(ctx, http.MethodPost, collection, wanted); err != nil {
		if !errors.Is(err, ErrConflict) {
			return err
		}
		data, _, readErr := c.request(ctx, http.MethodGet, path, nil)
		if readErr != nil {
			return readErr
		}
		existing, decodeErr := decodeRole(data)
		if decodeErr != nil {
			return decodeErr
		}
		return ValidateExistingRole(existing, wanted)
	}
	return nil
}

func (c *kubeClient) deleteBinding(ctx context.Context, existing RoleBinding) error {
	if existing.Metadata.UID == "" {
		return ErrBindingMismatch
	}
	path := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + existing.Metadata.Namespace + "/rolebindings/" + existing.Metadata.Name
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": existing.Metadata.UID}}
	_, _, err := c.request(ctx, http.MethodDelete, path, body)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *kubeClient) reconcile(ctx context.Context) error {
	jobs, err := c.listJobs(ctx)
	if err != nil {
		return err
	}
	bindings, err := c.listManagedBindings(ctx)
	if err != nil {
		return err
	}
	var reconciliationErr error
	for _, job := range jobs {
		key := bindingKey(job.Namespace, job.ServiceAccount)
		existingBinding, found := bindings[key]
		var existingRef *LedgerRef
		if found {
			ref := LedgerRef{
				Namespace: existingBinding.Metadata.Namespace,
				Name:      existingBinding.Metadata.Annotations[ledgerNameAnnotation],
				ClusterID: existingBinding.Metadata.Annotations[clusterIDAnnotation],
			}
			existingRef = &ref
		}
		decision, err := Evaluate(job, existingRef)
		if errors.Is(err, ErrNotPlacementJob) {
			continue
		}
		if err != nil {
			if reconciliationErr == nil {
				reconciliationErr = fmt.Errorf("evaluate provisioning Job %s/%s: %w", job.Namespace, job.Name, err)
			}
			continue
		}
		if decision.Revoke {
			if found {
				if err := c.deleteBinding(ctx, existingBinding); err != nil {
					return fmt.Errorf("revoke completed machine ledger binding: %w", err)
				}
				delete(bindings, key)
			}
			continue
		}
		if err := c.getLedger(ctx, decision.Ledger); err != nil {
			if reconciliationErr == nil {
				reconciliationErr = fmt.Errorf("verify ledger for Job %s/%s: %w", job.Namespace, job.Name, err)
			}
			continue
		}
		wanted, err := NewRoleBinding(decision)
		if err != nil {
			return err
		}
		role, err := NewLedgerRole(decision.Ledger)
		if err != nil {
			return err
		}
		if err := c.ensureLedgerRole(ctx, role); err != nil {
			if reconciliationErr == nil {
				reconciliationErr = fmt.Errorf("ensure least-privilege ledger Role: %w", err)
			}
			continue
		}
		if err := c.ensureBinding(ctx, wanted); err != nil {
			if reconciliationErr == nil {
				reconciliationErr = fmt.Errorf("ensure machine ledger binding for Job %s/%s: %w", job.Namespace, job.Name, err)
			}
			continue
		}
		bindings[key] = wanted
	}
	return reconciliationErr
}

// Run starts the authorization reconciler. It retries transient API failures and
// exits only on context cancellation or invalid in-cluster configuration.
func Run(ctx context.Context) error {
	client, err := inClusterClient()
	if err != nil {
		return err
	}
	interval := 2 * time.Second
	if configured := os.Getenv("RECONCILE_INTERVAL_SECONDS"); configured != "" {
		seconds, err := strconv.Atoi(configured)
		if err != nil || seconds < 1 || seconds > 60 {
			return errors.New("RECONCILE_INTERVAL_SECONDS must be between 1 and 60")
		}
		interval = time.Duration(seconds) * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := client.reconcile(ctx); err != nil {
				// A later full list/reconcile retries safely. Avoid logging API bodies
				// that might contain data unrelated to authorization.
				fmt.Fprintf(os.Stderr, "placement authorization reconciliation failed: %v\n", err)
			}
		}
	}
}
