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

## Read-only administrator counts

Optionally set `CHICKADEE_WEB_ADMIN_USER_ID` to one positive immutable GitHub
numeric user ID. That authenticated user sees `/dashboard/admin` and a header
link; usernames grant no authority. Other users cannot read global telemetry.
The existing Unix admin socket remains private. An operator collector may POST
`/telemetry` there with UTC `at`, `ready`, `booting`, `reserved`, `running`,
`uncertain`, `workers_online`, and `workers_total`. Counts contain no customer or
host identities. Invalid/out-of-order samples are rejected. The private durable
history retains the latest report per observed minute for at most 24 hours and
1440 samples. Missing observations are never filled with invented zero counts.
Running describes credentialed intent/connected/waiting VMs, not confirmed job
execution. Offline workers contribute no observed warm counts; uncertain central
assignments remain explicit. Samples older than three minutes are marked stale.

User dashboard totals may also include configured operator-owned accounts that
have no website enrollment. The private `/account-usage` endpoint accepts a full
replacement array of verified snapshots: `observed_at`, numeric `installation_id`
and `account_id`, `account_type`, the complete selected `repository_ids`, and
seven bounded daily `usage` values. The collector must verify actual App
installation identity and runner-group selected-repository access; never replace
a broader ACL with an intersection. Omitted scopes disappear, and observations
expire after five minutes. Personal snapshots require the signed-in owner;
organization snapshots require current administration access to every repository
in the verified scope. Scopes with unrestricted/all-repository visibility are
excluded until a separately verified organization-wide authorization exists.
Totals deduplicate enrollment snapshots and include shared organization reserved
VM time. They do not identify which individual GitHub user triggered a job.

Account snapshots may additionally contain `live_at` and `runners`, with supported
`label`, `allocated`, and `credentialed` counts. These are scoped assigned-runner
observations, not tenant-owned warm VM capacity. The user dashboard places this
current activity beneath the account usage graph, followed by repository setup
and service configuration status. Activity older than three minutes is unavailable;
a fresh empty observation may report no assigned runners. Credentialed runners
may be connecting or waiting and do not prove a GitHub job is executing.

### Queue initialization failures

A revoked or temporarily unavailable GitHub scope does not stop other listeners.
Upstream initialization failures retry after 15 seconds with exponential backoff
capped at five minutes; each initialization attempt has a five-second deadline.
Local configuration/key errors remain fatal. Existing journal recovery retains
its recorded scale-set identity without an upstream lookup and never replays
runner credentials.

The compatibility `reload.json` acknowledgement means the queue catalog was
accepted. It does not prove every GitHub listener initialized. New brokers add
`queue_initialized` to each `status.json` queue: true means a listener was
initialized, not that a runner connected or a job is executing. The admission
bridge keeps unavailable queues pending, and treats missing or stale status as
unavailable. Fresh older-broker queue entries without this field retain their
previous interpretation. Deploy the broker first and then the bridge; there is
no worker API or worker deployment change.
