# Machine-pool spread

## Agreed policy

The first policy is count-based spread with colocation permitted. Required
control-plane anti-affinity was deliberately removed so a three-host pool can
admit a temporary fourth control-plane VM during a Rancher-managed upgrade.

For example, three eligible hosts can have counts `1,1,1`, then `2,1,1` after a
surge admission. Six workers on four eligible hosts can have counts `2,2,1,1`.
No automatic migration or rebalance is performed after deletion or recovery.

## Selection order

1. Validate explicit pool group, host/storage allowlists and template mapping.
2. Require complete, internally consistent inventory evidence.
3. Exclude absent/offline/unreachable hosts, incompatible templates/networks,
   insufficient CPU or memory, and unavailable/incompatible/full storage.
4. Count inventory plus pending reservations for the same pool only. Resource
   admission still includes other pools and unrelated allocations.
5. Prefer the eligible host with lowest pool VM count; tie by lexical host name.
6. On that host, prefer greatest remaining allowed storage bytes after the
   request and pending commitments; tie by lexical storage ID.

The scheduler implements these rules in `internal/placement`; placement
configuration is available in the Rancher machine-pool UI, while the cluster-wide
CPU overcommit ratio is configured in the Helm chart.

## Capacity semantics

CPU is allocated vCPU count against the host's logical-CPU capacity after its
reserve, multiplied by the shared CPU overcommit ratio (default `2.0`). For
example, 48 logical CPUs with a reserve of 2 and a ratio of 1.5 admit up to 69
allocated vCPUs before pending request capacity is exhausted. Configure the
ratio with `placement.capacityPolicy.cpuOvercommitRatio`; it must be at least
`1.0`. Existing allocations and pending reservations still count against this
limit. Memory uses configured maximum allocation, not a reduced balloon target,
and also checks actual free memory. No spare-capacity assumption is derived from
low real-time utilization.

For each `(host, storageID)`, admission uses the smaller of reported physical
free bytes and total bytes minus existing **full virtual allocations**, then
subtracts pending bytes. This prevents treating a mostly empty thin disk as
uncommitted capacity. Adapter-provided totals must account for all relevant
volumes, not merely a VM's root disk or actual written bytes.

Arithmetic saturates on overcommit/overflow instead of wrapping into apparently
available capacity. Missing evidence fails closed. No host or storage fallback
outside the allowlists is allowed.

## Deferred policies

Strict anti-affinity, rack/failure-domain scheduling, least allocated, weighted
most-available/balanced scoring and durable round robin are not implemented.
