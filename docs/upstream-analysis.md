# Upstream analysis

## Scope and evidence

Audit baseline: local commit `9e561c4` (upstream merge of VM-tag support).
This is source inspection, not validation against a running Rancher or Proxmox cluster.
The agreed first release is machine-pool spread with permitted colocation, not
mandatory control-plane anti-affinity. See [implementation plan](implementation-plan.md).

## Repository and dependencies

- `cmd/docker-machine-driver-pve/main.go`: `main` registers a Rancher Machine
  plugin through `plugin.RegisterDriver`; it is not a Kubernetes controller.
- `cmd/docker-machine-driver-pve/driver`: configuration, lifecycle, Proxmox
  transport, hardware, cloud-init, SSH and IP discovery.
- `pkg/pve-node-driver`: Vue Rancher UI extension with cloud credentials and
  machine configuration. Credential access uses Rancher Secrets; UI discovery
  uses Rancher's `/meta/proxy/` transport.
- `deploy/charts/pve-node-driver`: Helm installation of `NodeDriver` and
  `UIPlugin` resources. It currently installs no coordination resources.
- `deploy/templates`: example template construction, not automatic distribution.
- `go.mod`: Go 1.24.1; `go-proxmox v0.2.3`, Rancher Machine
  `v0.15.0-rancher134`, `x/crypto v0.40.0`, YAML v3, testify.
  The old Docker replacement is existing upstream compatibility debt.
- `Taskfile.yml:tools:rancher-machine` installs rancher126, which differs from the
  library pin. Dependency changes need separate validation, not blanket upgrades.

## Exact lifecycle code paths

All paths below are relative to `cmd/docker-machine-driver-pve/driver/`.

| Concern | Function / file | Verified behavior and limitation |
| --- | --- | --- |
| Configuration | `config.go:GetCreateFlags`, `SetConfigFromFlags` | Existing flags and derived `PVE_*` environment variables; no host/storage scheduling options. Exported embedded configuration fields are serialized with driver state. |
| Template selection | `pve.go:getPVETemplate` | Uses the configured single template VMID. No template equivalence or host mapping. |
| Template host discovery | `pve.go:getPVEVirtualMachine` | Searches current resource-pool members, takes the member's current node, then calls `getPVEVirtualMachineOnNode`. |
| Clone submission | `pve.go:createPVEVirtualMachine` | Calls `template.Clone` with name, pool and full-clone setting only. No explicit destination, storage or VMID. |
| VMID allocation | SDK `VirtualMachine.Clone` | Missing `NewID` causes `/cluster/nextid` lookup, followed by a separate clone request; this is not an ID reservation. |
| Destination | SDK `VirtualMachine.Clone` | Submits `/nodes/{template.Node}/qemu/{template.VMID}/clone`; the driver omits `Target` and `Storage`. |
| VMID persistence | `driver.go:Create` | Sets `PVEMachineID` only after clone task success. A failed or interrupted call can leave a VM whose ID was never persisted. |
| Initialization | `driver.go:initialize` | Tags, configures hardware and cloud-init, starts, waits over SSH, then removes generated cloud-init media. |
| Hardware | `hardware.go:setupHardware` | Optional sockets, cores, memory and balloon settings. No root-disk resize, bridge selection, VLAN customization or multi-storage admission logic. |
| Ownership | `machine.go:getCurrentMachine` | Requires VMID plus generic `docker-machine` tag. Does not uniquely identify an incarnation; VMID reuse by another tagged machine is unsafe. |
| Migration lookup | `pve.go:getPVEVirtualMachine` | Re-fetches pool members and current node. Completed same-cluster migration can be found if VMID/pool/tag remain intact. Migration between lookup and action can race. |
| State | `driver.go:GetState` | Reports stopped if VM is not running; otherwise queries guest agent. Agent unavailability has a specific string-based special case. |
| Address | `driver.go:GetIP` | Gets configured interface MAC, queries guest agent, sorts matching IPs and prefers IPv4. No network eligibility checks. |
| Start/stop | `driver.go:Start`, `Stop`, `Kill`, `Restart` | Direct task calls; no desired-state guards. An unreachable host produces errors, not proof that its VM stopped. |
| Delete | `driver.go:Remove` | Calls `Kill`, then ownership-checked `Delete`. Already absent resources are errors, not explicit idempotent success. |
| Task wait | `pve.go:waitForPVETaskToSucceed` | Polls every three seconds, ten-minute timeout. Timeout is not cancellation of the Proxmox operation. |
| Interrupted create | `driver.go:Create` | Warns about a possibly created VMID, without durable reconciliation. Retries string-matched VMID conflicts with unbounded exponential backoff. |
| Failed initialize | `driver.go:Create` | Attempts `Remove`; cleanup before tagging can fail its own ownership check. No durable operation journal. |
| Repeated create | `driver.go:Create` | Generates SSH keys and starts another clone; no stable operation lookup or exactly-once guarantee. |

### Authentication, transport and secrets

`pve.go:getPVEClient` builds `/api2/json` from the configured endpoint and uses
API-token authentication. TLS verification is on unless explicitly disabled.
The HTTP client has no overall timeout; most lifecycle callers use
`context.TODO()`. New paths need bounded contexts and redacted error handling.
No scheduling decision may treat a permission-filtered inventory as complete.

### Cloud-init and Rocky compatibility

`cloud-init.go:setupCloudinit` generates NoCloud metadata and user-data and calls
SDK `VirtualMachine.CloudInit`. The SDK generates an ISO and calls
`Node.StorageISO`, choosing ISO storage independently of VM disks. This is **not**
native Proxmox cloud-init; the SDK explicitly documents the distinction.
`cleanupCloudinit` unmounts the ISO and removes its tag.

User-data creates the SSH user, authorized key and passwordless sudo. Native
Proxmox settings must preserve usable SSH and sudo on Rocky Linux 9, without
silently discarding required customization. Hostname, DHCP, guest-agent operation,
cloud-init module behavior and SSH must be tested with the actual template.
Legacy ISO behavior must not be changed implicitly.

Native-mode investigation found additional constraints in official
[`Cloudinit.pm`](https://github.com/proxmox/qemu-server/blob/49ab5913f068fe3abff3d57f7137714ea203ddf7/src/PVE/QemuServer/Cloudinit.pm):
`ciuser`/`sshkeys` inherit password locking and sudo from the guest image's
default-user configuration; Proxmox does not independently guarantee those
settings. DHCP needs the intended `ipconfigN=ip=dhcp`. Inherited `cicustom` snippets
can replace generated user/network data, and `ciupgrade` can cause package
upgrades. Native-drive detection must parse the volume field, not merely look
for `media=cdrom`. Keep the native drive attached; legacy ISO cleanup must never
detach it. Its newly cloned volume is 4 MiB in the inspected source.

The pinned SDK's `Config` can return `(nil, nil)` when the API finishes the
configuration update synchronously (`background_delay`). `NewTask` returns nil
for an empty UPID, while `waitForPVETaskToSucceed` currently dereferences it.
This is an existing panic risk requiring a regression test and a successful
no-task-result guard before native-mode integration.

## Verified Proxmox clone constraints

Primary API: [QEMU clone endpoint][clone-api]. Server implementation inspected at
[`qemu-server` commit b0965ee][clone-source], `clone_vm` and `clone_disk`.

- `newid`, `full`, `storage`, `target`, `pool` and `name` are actual parameters.
  The pinned Go client exposes them in `VirtualMachineCloneOptions`.
- Remote `target` requires shared source volumes; an explicit remote destination
  storage must also be shared. Identical local storage IDs are **not** shared
  storage. `full=1` does not enable arbitrary remote-local copying.
- Same-node full clone with `storage` selects destination storage for cloned
  attached disks, including EFI/TPM and native cloud-init volumes. ISO references
  are not relocated; unused disks are not cloned.
- Linked clones reject explicit destination storage and retain backing-volume
  dependencies. They cannot implement the agreed free storage selection.
- Selected host-local template + same-node full clone + explicit storage is the
  minimal valid workflow for the agreed local-storage layout. Do not set a remote
  clone `target` for this workflow.
- Clone-then-migrate is a distinct multi-task workflow; it is deferred.
- `/cluster/nextid` only reports availability at check time. External creators
  can still race even when cooperating drivers atomically claim VMIDs.

Sources: [pinned SDK clone/cloud-init][sdk-vm], [pinned SDK types][sdk-types],
[Proxmox cluster API source][cluster-source], [native cloud-init][native-ci].
These are source-verified constraints; the installed Proxmox package build still
needs recording and real-environment testing.

## Rancher lifecycle and persistence boundaries

The pinned Machine [`Client.Create` / `performCreate`][machine-create] calls
`PreCreateCheck`, saves before creation, calls `Driver.Create`, then saves after
successful creation. It does not checkpoint each driver side effect. Its special
save-on-failure path is for vSphere, not this driver. Therefore exported fields
alone cannot close the crash window around clone submission.

The driver interface supplies lifecycle calls, not a verified cluster identity,
machine-pool identity, Kubernetes role, predecessor relationship or fencing proof.
Use explicit placement-group configuration rather than invented environment
variables. Rancher decides scaling, upgrades, bootstrap and replacement;
the driver must not manage etcd membership or independently replace VMs.
The exact Rancher 2.15.2 executor credential injection and retry behavior still
requires release-specific integration validation.

## UI, installation and CI

`pkg/pve-node-driver/machine-config/pve.vue` mirrors existing configuration and
discovers pools/templates/devices. Insecure TLS disables enhanced UI discovery.
`currentValue.tags` is not copied in `validate`, a source-visible persistence bug.
The package declares Rancher >=2.10 and UI Extensions >=3,<4; this does not prove
Rancher 2.15.2 compatibility. `cloud-credential/pve.vue` accepts PVE major 8 or 9.

The chart installs a downloadable Linux node-driver binary through
`management.cattle.io/v3 NodeDriver`, and UI through `catalog.cattle.io/v1 UIPlugin`.
Credentials are categorized as public/private/password fields in the NodeDriver.
Do not add coordinator tokens to public machine-pool fields.

`.github/workflows/lint.yml` runs Go, JS and Markdown linting.
`.github/workflows/release.yml` builds Linux amd64, emits SHA256, builds UI and Helm
packages, generates release notes and publishes artifacts on a tag. Initially it
has no test gate. `util_test.go` only tests MAC parsing; lifecycle, scheduling,
concurrency, HTTP failure and recovery tests are missing.

## Modification plan and compatibility risks

1. Independent placement module with explicit policy validation, deterministic
   host-count spread and host-local storage accounting. No API calls in scoring.
2. Atomic Kubernetes ledger: durable operation/VMID/ownership/reservation data in
   one bounded object; no expiry-based reuse after uncertain side effects.
3. Proxmox discovery adapter must establish complete inventory, allocation,
   storage, template and bridge compatibility. Cluster resources alone is not
   sufficient and is filtered by authorization.
4. Coordinated lifecycle must persist intent before clone, use explicit VMID,
   verify unique ownership, reconcile retries and migration, and fail closed
   where an outcome or replacement safety is ambiguous.
5. Native cloud-init mode and Rancher configuration/UI are opt-in and gated until
   functional compatibility is tested. Existing configurations do not acquire
   a coordinator dependency or change placement.
6. Test-only and isolated integration validation precede release qualification.

The audit is complete as a source-level baseline. Release-specific executor
integration, installed API behavior and end-to-end operation are **not verified**.
No Proxmox HA integration is part of the design or implementation.

[clone-api]: https://pve.proxmox.com/pve-docs/api-viewer/index.html#/nodes/{node}/qemu/{vmid}/clone
[clone-source]: https://github.com/proxmox/qemu-server/blob/b0965eeba61248046f803a4995112f0ab8523843/src/PVE/API2/Qemu.pm
[sdk-vm]: https://github.com/luthermonson/go-proxmox/blob/v0.2.3/virtual_machine.go
[sdk-types]: https://github.com/luthermonson/go-proxmox/blob/v0.2.3/types.go
[cluster-source]: https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Cluster.pm
[native-ci]: https://pve.proxmox.com/wiki/Cloud-Init_Support
[machine-create]: https://github.com/rancher/machine/blob/v0.15.0-rancher134/libmachine/libmachine.go
