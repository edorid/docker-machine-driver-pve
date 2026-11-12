// Package authorization models the Rancher machine-provision Job seam used to
// grant one machine incarnation access to its Proxmox placement ledger.
package authorization

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	InfraMachineGroup   = "rke.cattle.io/infra-machine-group"
	InfraMachineVersion = "rke.cattle.io/infra-machine-version"
	InfraMachineKind    = "rke.cattle.io/infra-machine-kind"
	InfraMachineName    = "rke.cattle.io/infra-machine-name"
	InfraJobRemove      = "rke.cattle.io/infra-remove"
	DefaultLedgerName   = "pve-placement-ledger"
	placementArgCount   = 3
	trueLabelValue      = "true"
)

var (
	ErrNotPlacementJob = errors.New("job is not a Proxmox placement job")
	ErrInvalidJob      = errors.New("invalid Rancher machine-provision job")
	ErrBindingRequired = errors.New("existing machine ledger binding is required")
	dnsName            = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
)

// Job is the limited, relevant subset of Rancher's batch/v1 Job API response.
type Job struct {
	Namespace      string
	Name           string
	UID            string
	Labels         map[string]string
	ServiceAccount string
	Arguments      []string
	Succeeded      int32
}

// LedgerRef is explicit in create args and durably associated with a machine
// ServiceAccount until its Rancher remove Job completes.
type LedgerRef struct {
	Namespace string
	Name      string
	ClusterID string
}

// BindingDecision describes an idempotent role-binding lifecycle transition.
type BindingDecision struct {
	SourceNamespace string
	ServiceAccount  string
	Ledger          LedgerRef
	BindingName     string
	Ensure          bool
	Revoke          bool
}

// Evaluate validates Rancher metadata and driver arguments. The caller supplies
// the previously persisted ledger reference for remove jobs; remove args contain
// no driver fields in Rancher 2.15.2. unrelated jobs are ignored.
//
//nolint:cyclop // Classification combines Rancher create/remove metadata and driver arguments as one policy boundary.
func Evaluate(job Job, existing *LedgerRef) (BindingDecision, error) {
	if job.Labels == nil || job.Labels[InfraMachineGroup] == "" || job.Labels[InfraMachineVersion] == "" ||
		job.Labels[InfraMachineKind] == "" || job.Labels[InfraMachineName] == "" {
		return BindingDecision{}, ErrNotPlacementJob
	}

	if !validDNS(job.Namespace) || !validDNS(job.Name) || !validDNS(job.UID) || !validDNS(job.ServiceAccount) {
		return BindingDecision{}, ErrInvalidJob
	}

	remove := job.Labels[InfraJobRemove] == trueLabelValue

	if value := job.Labels[InfraJobRemove]; value != trueLabelValue && value != "false" {
		return BindingDecision{}, ErrInvalidJob
	}

	if remove {
		if existing == nil && job.Succeeded > 0 {
			return BindingDecision{
				SourceNamespace: job.Namespace, ServiceAccount: job.ServiceAccount,
				BindingName: BindingName(job.Namespace, job.ServiceAccount), Revoke: true,
			}, nil
		}

		if existing == nil || !validLedgerRef(*existing) {
			return BindingDecision{}, ErrBindingRequired
		}

		return BindingDecision{
			SourceNamespace: job.Namespace, ServiceAccount: job.ServiceAccount,
			Ledger: *existing, BindingName: BindingName(job.Namespace, job.ServiceAccount),
			Ensure: true, Revoke: job.Succeeded > 0,
		}, nil
	}

	if countArg(job.Arguments, "create") != 1 || countArg(job.Arguments, "--driver=pve") != 1 {
		return BindingDecision{}, ErrNotPlacementJob
	}

	placementFlagCount := 0

	for _, arg := range job.Arguments {
		for _, prefix := range []string{
			"--pve-placement-ledger-namespace=", "--pve-placement-ledger-name=", "--pve-placement-cluster-id=",
		} {
			if strings.HasPrefix(arg, prefix) {
				placementFlagCount++
			}
		}
	}

	if placementFlagCount == 0 {
		return BindingDecision{}, ErrNotPlacementJob
	}

	ref, err := ledgerFromArgs(job.Arguments)
	if err != nil {
		return BindingDecision{}, err
	}

	if existing != nil && *existing != ref {
		return BindingDecision{}, ErrInvalidJob
	}

	return BindingDecision{
		SourceNamespace: job.Namespace, ServiceAccount: job.ServiceAccount,
		Ledger: ref, BindingName: BindingName(job.Namespace, job.ServiceAccount), Ensure: true,
	}, nil
}

func ledgerFromArgs(args []string) (LedgerRef, error) {
	values := make(map[string]string, placementArgCount)

	for _, arg := range args {
		key, value, found := strings.Cut(arg, "=")
		if !found {
			continue
		}

		switch key {
		case "--pve-placement-ledger-namespace", "--pve-placement-ledger-name", "--pve-placement-cluster-id":
			if _, duplicate := values[key]; duplicate {
				return LedgerRef{}, ErrInvalidJob
			}

			values[key] = value
		}
	}

	ref := LedgerRef{
		Namespace: values["--pve-placement-ledger-namespace"],
		Name:      values["--pve-placement-ledger-name"],
		ClusterID: values["--pve-placement-cluster-id"],
	}
	if !validLedgerRef(ref) {
		return LedgerRef{}, fmt.Errorf("placement ledger namespace and cluster identity are required: %w", ErrInvalidJob)
	}

	return ref, nil
}

func validLedgerRef(ref LedgerRef) bool {
	if !validDNS(ref.Namespace) || !validDNS(ref.Name) || len(ref.ClusterID) == 0 || len(ref.ClusterID) > 128 || strings.TrimSpace(ref.ClusterID) != ref.ClusterID {
		return false
	}

	for _, char := range ref.ClusterID {
		if char < ' ' || char > '~' {
			return false
		}
	}

	return true
}

func countArg(args []string, expected string) int {
	count := 0

	for _, arg := range args {
		if arg == expected {
			count++
		}
	}

	return count
}

// BindingName is stable across Rancher's create and remove Jobs, which reuse
// their machine-specific service account name.
func BindingName(namespace, serviceAccount string) string {
	identity := namespace + "/" + serviceAccount
	digest := sha256.Sum256([]byte(identity))

	return "pve-placement-" + hex.EncodeToString(digest[:12])
}

func validDNS(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dnsName.MatchString(value)
}
