# Placement testing

## Local commands

Use Go 1.24.1 or a compatible toolchain:

```sh
go test ./...
go test -race -count=1 ./...
go vet ./...
go mod verify
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o docker-machine-driver-pve ./cmd/docker-machine-driver-pve
```

## Tests currently present

`internal/placement/scheduler_test.go` tests policy validation and fingerprints,
deterministic host/storage selection, allowed-storage boundaries, control-plane
surge colocation, independent pool counts, eligibility rejection, host-local
storage, full-virtual-size admission, cancellation, migrated ownership and
partially applied allocations without double counting.

`internal/placement/coordinator_test.go` tests replay, conflicting identity and
group policy, bounded CAS contention, unknown schema, record bounds, three/ten
concurrent reservations, one submission winner, process-restart behavior and
lost-write responses. Its memory store models durable CAS semantics for unit
tests; it does not model a real Kubernetes API or prove cross-process behavior.

`internal/placement/rancher_metadata_test.go` verifies CAPI machine matching,
required cluster/pool labels, authorization headers, and fail-closed handling of
missing or ambiguous machine metadata. Driver tests verify NoCloud ISO contents,
upload-before-attachment ordering, machine-incarnation ownership, and tag
normalization validation. They do not prove the machine-provision ServiceAccount
has CAPI Machine list permission in Rancher's live job.

The existing MAC-address parsing tests are retained.

## CI

`.github/workflows/test.yml` adds formatting, module verification, vet, race tests,
Linux amd64 builds for the driver and authorizer, and Helm lint/render checks.
Release builds run dependency verification and race tests before packaging.

`internal/placement/kubernetes_test.go` exercises concurrent reservation over a
local HTTPS mock Kubernetes endpoint, rejects a changed ledger UID, and verifies
that a committed reservation is recovered after its HTTP response is lost. This
is not a real Kubernetes API or validation of admission/persistence semantics.

`internal/authorization/kubernetes_test.go` exercises cluster-wide Job and
RoleBinding reconciliation against an HTTP mock, including paginated Job lists,
projected-token rotation, lost Role/RoleBinding create responses, failed removes,
and lost delete responses. It does not validate Kubernetes RBAC escalation
admission or Rancher's actual Job lifecycle in a live management cluster.

## Still required

- Real API validation of CRD,
  resourceVersion, object UID preconditions and admission behavior.
- Mocked Proxmox discovery, clone, migration and lifecycle tests with partitions,
  unknown clone submission, native cloud-init and ownership mismatch.
- Multi-process/crash/restore testing against a real shared ledger.
- Measured record/byte limits and safe terminal-record compaction tests.
- Rancher 2.15.2 executor credentials/configuration/UI integration.
- Isolated Proxmox 9.2.26 / Rocky Linux 9 x86_64 / pinned Rancher-supported RKE2
  end-to-end validation, including upgrade surge and manual migration.

No destructive production tests or end-to-end provisioning have been run.

## Validation record for the foundation increment

On the development macOS arm64 host, using a checksum-verified temporary Go
1.24.1 toolchain:

- `go test ./...`: baseline passed before implementation.
- `go test ./internal/placement`: new core unit/concurrency tests passed.
- `go test -race ./...`: all existing and new tests passed.
- `go vet ./...`: passed.
- `go mod verify`: passed.
- `golangci-lint` v1.64.6: passed with existing deprecation warnings; selected
  safety-critical validation functions have scoped complexity-lint exceptions.
- Linux amd64 cross-compilation: passed; development artifact saved outside the
  repository, not published as a release or advanced-placement-enabled binary.

These results do not cover the pending integrations listed above.
