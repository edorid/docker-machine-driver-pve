package authorization

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const testRoleUID = "role-uid"

func placementJob() Job {
	return Job{
		Namespace: "fleet-default", Name: "node-machine-provision", UID: "2fcb3180-97a1-45fd-9862-cad297c667a2",
		ServiceAccount: "node-machine-provision", Labels: map[string]string{
			InfraMachineGroup: "proxmox.machine.cattle.io", InfraMachineVersion: "v1", InfraMachineKind: "PveMachine",
			InfraMachineName: "node", InfraJobRemove: "false",
		},
		Arguments: []string{
			"--driver-download-url=https://driver.invalid", "create", "--driver=pve",
			"--pve-placement-ledger-namespace=shared-ledgers", "--pve-placement-cluster-id=proxmox-prod",
			"--pve-placement-ledger-name=proxmox-placement-ledger",
			"node",
		},
	}
}

func TestPlacementCreateJobGetsStableLedgerBinding(t *testing.T) {
	job := placementJob()
	decision, err := Evaluate(job, nil)
	require.NoError(t, err)
	require.True(t, decision.Ensure)
	require.False(t, decision.Revoke)
	require.Equal(t, LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger", ClusterID: "proxmox-prod"}, decision.Ledger)
	require.Equal(t, BindingName("fleet-default", "node-machine-provision"), decision.BindingName)

	job.UID = "f2ed5fb0-d66f-4f84-8ab5-88bd54bed973" // Rancher retry Job, same machine SA
	retry, err := Evaluate(job, nil)
	require.NoError(t, err)
	require.Equal(t, decision.BindingName, retry.BindingName)
}

func TestUnrelatedAndLegacyJobsAreIgnored(t *testing.T) {
	job := placementJob()
	delete(job.Labels, InfraMachineKind)
	_, err := Evaluate(job, nil)
	require.ErrorIs(t, err, ErrNotPlacementJob)

	job = placementJob()
	job.Arguments = []string{"create", "--driver=pve", "node"}
	_, err = Evaluate(job, nil)
	require.ErrorIs(t, err, ErrNotPlacementJob)

	job = placementJob()
	job.Arguments = []string{"create", "--driver=other", "node"}
	_, err = Evaluate(job, nil)
	require.ErrorIs(t, err, ErrNotPlacementJob)
}

func TestCreateRejectsAmbiguousOrMalformedLedgerArgs(t *testing.T) {
	tests := map[string]func(*Job){
		"duplicate namespace": func(job *Job) {
			job.Arguments = append(job.Arguments, "--pve-placement-ledger-namespace=other")
		},
		"missing identity": func(job *Job) {
			job.Arguments = []string{"create", "--driver=pve", "--pve-placement-ledger-namespace=pve-prod"}
		},
		"invalid namespace": func(job *Job) {
			job.Arguments = []string{"create", "--driver=pve", "--pve-placement-ledger-namespace=../bad", "--pve-placement-cluster-id=id", "--pve-placement-ledger-name=ledger"}
		},
		"duplicate driver": func(job *Job) {
			job.Arguments = append(job.Arguments, "--driver=pve")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			job := placementJob()
			mutate(&job)
			_, err := Evaluate(job, nil)
			require.Error(t, err)
		})
	}
}

func TestRemoveReusesBindingAndRevokesOnlyAfterSuccess(t *testing.T) {
	job := placementJob()
	ref := LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger", ClusterID: "proxmox-prod"}
	job.Labels[InfraJobRemove] = trueLabelValue
	job.Arguments = []string{"--secret-namespace=fleet-default", "rm", "-y", "--update-config", "node"}
	decision, err := Evaluate(job, &ref)
	require.NoError(t, err)
	require.True(t, decision.Ensure)
	require.False(t, decision.Revoke)

	job.Succeeded = 1
	decision, err = Evaluate(job, &ref)
	require.NoError(t, err)
	require.True(t, decision.Revoke)
	decision, err = Evaluate(job, nil)
	require.NoError(t, err)
	require.True(t, decision.Revoke) // stale binding cleanup is idempotent
}

func TestInvalidJobIdentityAndLabelsFailClosed(t *testing.T) {
	job := placementJob()
	job.ServiceAccount = ""
	_, err := Evaluate(job, nil)
	require.ErrorIs(t, err, ErrInvalidJob)

	job = placementJob()
	job.Labels[InfraJobRemove] = "maybe"
	_, err = Evaluate(job, nil)
	require.ErrorIs(t, err, ErrInvalidJob)

	job = placementJob()
	job.Labels[InfraJobRemove] = trueLabelValue
	ref := LedgerRef{Namespace: "shared-ledgers", Name: "proxmox-placement-ledger", ClusterID: "proxmox-prod"}
	_, err = Evaluate(job, &ref)
	require.NoError(t, err)
}

func TestRoleBindingIsLimitedToOneLedgerAndOneServiceAccount(t *testing.T) {
	decision, err := Evaluate(placementJob(), nil)
	require.NoError(t, err)
	binding, err := NewRoleBinding(decision)
	require.NoError(t, err)
	require.Equal(t, "shared-ledgers", binding.Metadata.Namespace)
	require.Equal(t, "Role", binding.RoleRef.Kind)
	require.Equal(t, LedgerRoleName(decision.Ledger), binding.RoleRef.Name)
	require.Equal(t, []BindingSubject{{Kind: "ServiceAccount", Name: "node-machine-provision", Namespace: "fleet-default"}}, binding.Subjects)
	wanted, err := json.Marshal(binding)
	require.NoError(t, err)
	existing, err := decodeRoleBinding(wanted)
	require.NoError(t, err)

	existing.Metadata.UID = "server-uid"
	require.NoError(t, ValidateExisting(existing, binding))
	existing.Metadata.Annotations[clusterIDAnnotation] = "other-cluster"
	require.ErrorIs(t, ValidateExisting(existing, binding), ErrBindingMismatch)
	existing.Metadata.Annotations[clusterIDAnnotation] = decision.Ledger.ClusterID
	existing.Subjects[0].Name = "other-service-account"
	require.ErrorIs(t, ValidateExisting(existing, binding), ErrBindingMismatch)

	role, err := NewLedgerRole(decision.Ledger)
	require.NoError(t, err)
	require.Equal(t, []string{"proxmox-placement-ledger"}, role.Rules[0].ResourceNames)
	require.Equal(t, []string{"get", "update"}, role.Rules[0].Verbs)
	roleData, err := json.Marshal(role)
	require.NoError(t, err)

	var savedRole LedgerRole

	require.NoError(t, json.Unmarshal(roleData, &savedRole))
	savedRole.Metadata.UID = testRoleUID
	require.NoError(t, ValidateExistingRole(savedRole, role))
	savedRole.Rules[0].ResourceNames = []string{"*"}
	require.ErrorIs(t, ValidateExistingRole(savedRole, role), ErrBindingMismatch)
}

func TestMachineMetadataGrantIsNamespacedAndReadOnly(t *testing.T) {
	role, err := NewMachineMetadataRole("fleet-default")
	require.NoError(t, err)
	require.Equal(t, "fleet-default", role.Metadata.Namespace)
	require.Equal(t, []PolicyRule{{APIGroups: []string{"cluster.x-k8s.io"}, Resources: []string{"machines"}, Verbs: []string{"get", "list"}}}, role.Rules)
	role.Metadata.UID = "role-uid"
	wantedRole, err := NewMachineMetadataRole("fleet-default")
	require.NoError(t, err)
	require.NoError(t, ValidateMachineMetadataRole(role, wantedRole))
	role.Rules[0].Verbs = []string{"*"}
	require.ErrorIs(t, ValidateMachineMetadataRole(role, wantedRole), ErrBindingMismatch)

	binding, err := NewMachineMetadataRoleBinding("fleet-default", "machine-provision-sa")
	require.NoError(t, err)
	require.Equal(t, "fleet-default", binding.Metadata.Namespace)
	require.Equal(t, "pve-capi-machine-metadata-reader", binding.RoleRef.Name)
	require.Equal(t, []BindingSubject{{Kind: "ServiceAccount", Name: "machine-provision-sa", Namespace: "fleet-default"}}, binding.Subjects)
	binding.Metadata.UID = "binding-uid"
	wantedBinding, err := NewMachineMetadataRoleBinding("fleet-default", "machine-provision-sa")
	require.NoError(t, err)
	require.NoError(t, ValidateMachineMetadataRoleBinding(binding, wantedBinding))
	binding.Subjects[0].Name = "other-service-account"
	require.ErrorIs(t, ValidateMachineMetadataRoleBinding(binding, wantedBinding), ErrBindingMismatch)
}
