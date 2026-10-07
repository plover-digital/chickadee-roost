# Experimental fleet broker

Roost's `chickadee-roost` binary owns GitHub scale-set listeners, JIT generation,
customer concurrency and placement. Chickadee's `chickadee-worker` owns trusted
images, physical warm capacity and VM execution. Standalone Chickadee remains
usable without Roost. This is a two-host prototype, not a highly available
scheduler or billing ledger.

Protocol v1 uses Chickadee's public `workerapi` package. Each worker binds an
explicit private/loopback HTTPS address. Require TLS 1.3, a private pinned CA,
serverAuth/clientAuth certificates, worker DNS names and exact URI identities:
`spiffe://chickadee/worker/WORKER_ID` and
`spiffe://chickadee/broker/BROKER_ID`. Restrict the host firewall to the broker;
never expose this API to guests or the public web. Worker certificates grant VM
execution authority. App keys stay exclusively in the broker account. Run the
broker and local worker under separate OS identities.

`chickadee-roost -catalog QUEUES_JSON -fleet FLEET_JSON` reads the established
admission bridge catalog, plus a separate private fleet configuration containing
worker endpoints/TLS files, immutable image digests and aggregate resource limits.
The image digest is SHA256 of the immutable bundle's SHA256SUMS bytes. Workers
verify every listed file, machine and disk shape before serving. Runner labels
resolve to content and resource shape; they never choose a physical host.

The scheduler prefers compatible READY guests, then eligible cold capacity.
Each private worker configuration may set `placement_priority` to an integer
from -100 to 100 (default 0). Higher values prefer that worker among eligible
READY hosts, then among eligible cold hosts when no compatible READY guest exists.
A lower-priority READY guest always wins over higher-priority cold capacity;
equal priorities use worker ID order. Offline, draining or incompatible workers
are skipped. Only trusted broker configuration sets priority; worker inventory
cannot promote itself. Changing priority requires a controlled broker restart.

Workers choose VM IDs and enforce their own CPU/RAM/VM/storage limits. Customer
quotas span labels and hosts; operator scopes may explicitly have larger quotas.
Warm VMs are shared physical capacity, unregistered and credential-free.

Assignments are private, fsynced before reservation and credential intent. JIT is
never stored or replayed. Lost JIT/delivery acknowledgements consume the VM;
local reservation deadlines retire abandoned guests. Running VMs keep their
local job deadline during broker outage. Broker restart reconciles worker
inventory; it does not reap or stop remote jobs. Uncertain hosts/registrations
retain quota. Worker restart conservatively reaps only owned QEMU processes and
confirms exit before removing disks.

SIGHUP changes queue policy and preserves assignments; existing queue identity
and image changes require a controlled broker restart. SIGUSR1 drains central
admission and waits for assignments. SIGTERM stops the broker while worker jobs
continue. Worker SIGTERM drains its local pool while keeping status available until jobs
finish. Worker systemd units must use `KillMode=mixed`: TERM reaches the manager
only, with final group KILL after the bounded stop timeout. Keep stop timeouts
consistent with job deadlines. Roll workers one at a time and wait for
authenticated inventory with READY capacity before stopping the next host;
systemd active alone does not prove that image verification or boot is complete. Do not increment a worker generation with unresolved
assignments; the initial generation is operator-managed, not automatic failover.

The compatibility status/reload/usage files retain existing names for the bridge.
Usage deduplicates central assignment IDs and reports reserved VM time rather
than GitHub job execution time or charges. The website remains a separate unit.

Limitations: one broker, local journals with bounded retained tombstones, no
certificate rotation automation, no host auto-enrollment or remote image builds,
no live VM migration, no sophisticated fairness or multi-broker fencing. Queue
polling uses statistics and bounded per-scope maxima; coordinated fleet-wide
GitHub acquisition credits remain future work, so acquired jobs may wait for
physical capacity. Hostile-tenant cgroups/storage-quota and broad adversarial
acceptance remain required before claiming production-grade isolation.

Live acceptance must prove one real job, exit-before-delete and fresh warm
replacement on each host, broker restart during a running job, missing-host quota
retention, and guest denial of host/LAN/other-guest access. Passing unit tests or
publishing source does not establish these live results.

Worker inventory/status calls have short deadlines so an unavailable host does
not hold every admission pass for the generic HTTP timeout. Cold reserve calls
can time out while a guest continues booting; the broker retries only after a
definitive missing-assignment status and never replays credential delivery.
GitHub JIT calls remain bounded and serialized in this prototype.
