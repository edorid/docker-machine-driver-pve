package authorization

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	managedLabel         = "placement.pve.edorid.github.io/managed"
	clusterIDAnnotation  = "placement.pve.edorid.github.io/cluster-id"
	ledgerNameAnnotation = "placement.pve.edorid.github.io/ledger-name"
	sourceNSAnnotation   = "placement.pve.edorid.github.io/source-namespace"
	sourceSAAnnotation   = "placement.pve.edorid.github.io/source-service-account"
)

var ErrBindingMismatch = errors.New("existing placement role binding does not match machine identity")

// RoleBinding is the Kubernetes RBAC object created for one Rancher machine SA.
type RoleBinding struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   BindingMetadata  `json:"metadata"`
	Subjects   []BindingSubject `json:"subjects"`
	RoleRef    BindingRoleRef   `json:"roleRef"`
}

type BindingMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
}

type BindingSubject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	APIGroup  string `json:"apiGroup,omitempty"`
}

type BindingRoleRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

// NewRoleBinding grants access through a per-ledger Role in the ledger namespace.
func NewRoleBinding(decision BindingDecision) (RoleBinding, error) {
	if !decision.Ensure || decision.Revoke || !validDNS(decision.SourceNamespace) ||
		!validDNS(decision.ServiceAccount) || !validLedgerRef(decision.Ledger) ||
		decision.BindingName != BindingName(decision.SourceNamespace, decision.ServiceAccount) {
		return RoleBinding{}, fmt.Errorf("invalid role-binding decision")
	}
	return RoleBinding{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "RoleBinding",
		Metadata: BindingMetadata{
			Name: decision.BindingName, Namespace: decision.Ledger.Namespace,
			Labels: map[string]string{managedLabel: "true"},
			Annotations: map[string]string{
				clusterIDAnnotation:  decision.Ledger.ClusterID,
				ledgerNameAnnotation: decision.Ledger.Name,
				sourceNSAnnotation:   decision.SourceNamespace,
				sourceSAAnnotation:   decision.ServiceAccount,
			},
		},
		Subjects: []BindingSubject{{Kind: "ServiceAccount", Name: decision.ServiceAccount, Namespace: decision.SourceNamespace}},
		RoleRef:  BindingRoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: LedgerRoleName(decision.Ledger)},
	}, nil
}

// ValidateExisting rejects binding name reuse, cross-ledger changes and mutated
// subjects/roleRefs. It never adopts an arbitrary pre-existing RoleBinding.
func ValidateExisting(existing RoleBinding, wanted RoleBinding) error {
	if existing.Metadata.UID == "" || existing.Metadata.Name != wanted.Metadata.Name ||
		existing.Metadata.Namespace != wanted.Metadata.Namespace ||
		existing.Metadata.Labels[managedLabel] != "true" ||
		existing.Metadata.Annotations[clusterIDAnnotation] != wanted.Metadata.Annotations[clusterIDAnnotation] ||
		existing.Metadata.Annotations[ledgerNameAnnotation] != wanted.Metadata.Annotations[ledgerNameAnnotation] ||
		existing.Metadata.Annotations[sourceNSAnnotation] != wanted.Metadata.Annotations[sourceNSAnnotation] ||
		existing.Metadata.Annotations[sourceSAAnnotation] != wanted.Metadata.Annotations[sourceSAAnnotation] ||
		!sameSubject(existing.Subjects, wanted.Subjects) || existing.RoleRef != wanted.RoleRef {
		return ErrBindingMismatch
	}
	return nil
}

// LedgerRole is an exact, machine-independent role limited to one ledger object.
type LedgerRole struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   BindingMetadata `json:"metadata"`
	Rules      []PolicyRule    `json:"rules"`
}

type PolicyRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
	Verbs         []string `json:"verbs"`
}

// LedgerRoleName creates a stable DNS-safe name for a ledger-specific Role.
func LedgerRoleName(ref LedgerRef) string {
	identity := ref.Namespace + "/" + ref.Name
	digest := sha256.Sum256([]byte(identity))
	return "pve-ledger-" + hex.EncodeToString(digest[:12])
}

func NewLedgerRole(ref LedgerRef) (LedgerRole, error) {
	if !validLedgerRef(ref) {
		return LedgerRole{}, fmt.Errorf("invalid Proxmox ledger reference")
	}
	return LedgerRole{
		APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role",
		Metadata: BindingMetadata{
			Name: LedgerRoleName(ref), Namespace: ref.Namespace,
			Labels:      map[string]string{managedLabel: "true"},
			Annotations: map[string]string{clusterIDAnnotation: ref.ClusterID, ledgerNameAnnotation: ref.Name},
		},
		Rules: []PolicyRule{{
			APIGroups: []string{"placement.pve.edorid.github.io"}, Resources: []string{"proxmoxplacementledgers"},
			ResourceNames: []string{ref.Name}, Verbs: []string{"get", "update"},
		}},
	}, nil
}

func ValidateExistingRole(existing, wanted LedgerRole) error {
	if existing.Metadata.UID == "" || existing.Metadata.Name != wanted.Metadata.Name ||
		existing.Metadata.Namespace != wanted.Metadata.Namespace || existing.Metadata.Labels[managedLabel] != "true" ||
		existing.Metadata.Annotations[clusterIDAnnotation] != wanted.Metadata.Annotations[clusterIDAnnotation] ||
		existing.Metadata.Annotations[ledgerNameAnnotation] != wanted.Metadata.Annotations[ledgerNameAnnotation] ||
		len(existing.Rules) != 1 || !equalRule(existing.Rules[0], wanted.Rules[0]) {
		return ErrBindingMismatch
	}
	return nil
}

func equalRule(left, right PolicyRule) bool {
	return equalStrings(left.APIGroups, right.APIGroups) && equalStrings(left.Resources, right.Resources) &&
		equalStrings(left.ResourceNames, right.ResourceNames) && equalStrings(left.Verbs, right.Verbs)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameSubject(left, right []BindingSubject) bool {
	if len(left) != 1 || len(right) != 1 {
		return false
	}
	return left[0] == right[0]
}

func decodeRoleBinding(data []byte) (RoleBinding, error) {
	var binding RoleBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return RoleBinding{}, ErrBindingMismatch
	}
	return binding, nil
}
