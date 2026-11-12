// Package placement implements transport-independent machine-pool spread placement.
// It does not authorize Proxmox operations or establish inventory completeness.
package placement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	hostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	storageID = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
)

// Policy is explicit machine-pool placement configuration. It has no credentials.
type Policy struct {
	GroupID         string         `json:"groupId"`
	AllowedHosts    []string       `json:"allowedHosts"`
	AllowedStorages []string       `json:"allowedStorages"`
	Templates       map[string]int `json:"templates"`
}

// Validate rejects ambiguous, incomplete or unsupported placement configuration.
func (p Policy) Validate() error {
	if !validIdentity(p.GroupID) {
		return errors.New("placement group must be a nonempty printable identity of at most 128 bytes")
	}

	if err := validateNames(p.AllowedHosts, validHost, "allowed host"); err != nil {
		return err
	}

	if err := validateNames(p.AllowedStorages, storageID.MatchString, "allowed storage"); err != nil {
		return err
	}

	if len(p.Templates) != len(p.AllowedHosts) {
		return errors.New("exactly one local template mapping is required per allowed host")
	}

	seen := make(map[int]bool)

	for _, host := range p.AllowedHosts {
		vmid := p.Templates[host]
		if vmid < 100 || vmid > 999999999 || seen[vmid] {
			return errors.New("local template VMIDs must be unique and in [100,999999999]")
		}

		seen[vmid] = true
	}

	return nil
}

// Fingerprint identifies the group contract independently of list ordering.
func (p Policy) Fingerprint() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}

	p.AllowedHosts = slices.Sorted(slices.Values(p.AllowedHosts))
	p.AllowedStorages = slices.Sorted(slices.Values(p.AllowedStorages))

	encoded, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode placement policy: %w", err)
	}

	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:]), nil
}

func validIdentity(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}

	for _, char := range value {
		if char < ' ' || char > '~' {
			return false
		}
	}

	return true
}

func validHost(host string) bool {
	const maxHostnameLength = 253
	if len(host) > maxHostnameLength {
		return false
	}

	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 || !hostLabel.MatchString(label) {
			return false
		}
	}

	return true
}

func validateNames(names []string, valid func(string) bool, kind string) error {
	if len(names) == 0 {
		return fmt.Errorf("at least one %s is required", kind)
	}

	seen := make(map[string]bool)
	for _, name := range names {
		if !valid(name) || seen[name] {
			return fmt.Errorf("invalid or duplicate %s", kind)
		}

		seen[name] = true
	}

	return nil
}
