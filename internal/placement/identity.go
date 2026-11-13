package placement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// EndpointIdentity derives a stable, non-secret identity and ledger name from a
// Proxmox API endpoint. The scheme is intentionally excluded so changing HTTP
// to HTTPS does not make the same host and port a different placement domain.
func EndpointIdentity(raw string) (clusterID, ledgerName string, err error) {
	target, err := url.Parse(raw)
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" ||
		(target.Scheme != "https" && target.Scheme != "http") || (target.Path != "" && target.Path != "/") {
		return "", "", errors.New("invalid Proxmox API endpoint")
	}

	host := strings.ToLower(target.Hostname())
	port := target.Port()
	if port == "" {
		port = "8006"
	}
	if net.ParseIP(host) == nil && strings.ContainsAny(host, " /\\\r\n\t") {
		return "", "", errors.New("invalid Proxmox API host")
	}

	canonical := net.JoinHostPort(host, port)
	digest := sha256.Sum256([]byte("pve-placement-endpoint-v1\x00" + canonical))
	sum := hex.EncodeToString(digest[:])

	return "endpoint-sha256-" + sum, "pve-" + sum[:32], nil
}

// LedgerNameForIdentity checks that a generated endpoint identity is paired
// with its deterministic ledger name.
func LedgerNameForIdentity(clusterID string) (string, error) {
	const prefix = "endpoint-sha256-"
	if len(clusterID) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(clusterID, prefix) {
		return "", errors.New("invalid endpoint-derived Proxmox cluster identity")
	}

	sum := strings.TrimPrefix(clusterID, prefix)
	if _, err := hex.DecodeString(sum); err != nil {
		return "", fmt.Errorf("invalid endpoint-derived Proxmox cluster identity: %w", err)
	}

	return "pve-" + sum[:32], nil
}
