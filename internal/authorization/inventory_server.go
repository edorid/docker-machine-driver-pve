package authorization

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const requestHeaderConfigMapPath = "/api/v1/namespaces/kube-system/configmaps/extension-apiserver-authentication"

type proxyTrust struct {
	roots        *x509.CertPool
	allowedNames map[string]bool
}

func (c *kubeClient) requestHeaderTrust(ctx context.Context) (*proxyTrust, error) {
	data, err := c.request(ctx, http.MethodGet, requestHeaderConfigMapPath, nil)
	if err != nil {
		return nil, errors.New("Kubernetes aggregation proxy trust configuration is unavailable")
	}
	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(data, &configMap) != nil {
		return nil, errors.New("Kubernetes aggregation proxy trust configuration is invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(configMap.Data["requestheader-client-ca-file"])) {
		return nil, errors.New("Kubernetes aggregation proxy CA is invalid")
	}
	var names []string
	if json.Unmarshal([]byte(configMap.Data["requestheader-allowed-names"]), &names) != nil || len(names) == 0 {
		return nil, errors.New("Kubernetes aggregation proxy allowed names are invalid")
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	return &proxyTrust{roots: roots, allowedNames: allowed}, nil
}

func inventoryTLSConfig(serverCert, serverKey string, trust *atomic.Pointer[proxyTrust]) (*tls.Config, error) {
	if serverCert == "" || serverKey == "" {
		return nil, errors.New("inventory API serving certificate is not configured")
	}
	if _, err := tls.LoadX509KeyPair(serverCert, serverKey); err != nil {
		return nil, errors.New("inventory API serving certificate is unavailable")
	}
	loadCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		certificate, err := tls.LoadX509KeyPair(serverCert, serverKey)
		if err != nil {
			return nil, errors.New("inventory API serving certificate reload failed")
		}
		return &certificate, nil
	}
	verifyName := func(state tls.ConnectionState) error {
		current := trust.Load()
		if current == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return errors.New("trusted aggregation proxy certificate is required")
		}
		leaf := state.PeerCertificates[0]
		if !current.allowedNames[leaf.Subject.CommonName] {
			return errors.New("aggregation proxy certificate name is not allowed")
		}
		return nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			current := trust.Load()
			if current == nil {
				return nil, errors.New("Kubernetes aggregation proxy trust is unavailable")
			}
			return &tls.Config{
				MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert,
				ClientCAs: current.roots, GetCertificate: loadCertificate, VerifyConnection: verifyName,
			}, nil
		},
	}, nil
}

func startInventoryAPI(ctx context.Context, client *kubeClient) (*http.Server, error) {
	trustConfig, err := client.requestHeaderTrust(ctx)
	if err != nil {
		return nil, err
	}
	var trust atomic.Pointer[proxyTrust]
	trust.Store(trustConfig)
	address := os.Getenv("INVENTORY_API_ADDRESS")
	if address == "" {
		address = ":8443"
	}
	serverCert, serverKey := os.Getenv("INVENTORY_TLS_CERT_FILE"), os.Getenv("INVENTORY_TLS_KEY_FILE")
	tlsConfig, err := inventoryTLSConfig(serverCert, serverKey, &trust)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(inventoryAPIBase, inventoryAPI{kubernetes: client, discover: discoverProxmoxInventory})
	mux.Handle(inventoryAPIBase+"/", inventoryAPI{kubernetes: client, discover: discoverProxmoxInventory})
	mux.Handle("/apis/"+inventoryAPIGroup, inventoryAPI{kubernetes: client, discover: discoverProxmoxInventory})
	server := &http.Server{
		Addr: address, Handler: mux, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, errors.New("inventory API listener could not be opened")
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				updated, err := client.requestHeaderTrust(ctx)
				if err == nil {
					trust.Store(updated)
				} else {
					fmt.Fprintln(os.Stderr, "inventory API proxy trust refresh failed")
				}
			}
		}
	}()
	go func() {
		if err := server.Serve(tls.NewListener(listener, tlsConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "inventory API server stopped: %s\n", strings.TrimSpace(err.Error()))
		}
	}()
	return server, nil
}
