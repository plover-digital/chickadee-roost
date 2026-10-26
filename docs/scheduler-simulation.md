# Fleet scheduling experiments

Baseline: Roost `39b9a56680232960de93c5246b3c358583473124`. The engine source inspected alongside it was Chickadee `61842ee`; Roost retains its existing pinned worker API dependency. Investigation: [issue 22](https://github.com/plover-digital/chickadee-roost/issues/22).

Keep the central broker and measure queue delay before changing placement. Unequal host job counts alone do not establish a problem: a faster worker should often execute more jobs. The useful objective is prompt starts on eligible capacity while respecting scope quotas and execution ownership. This study adds test-only experiments and synthetic results; it changes no production scheduling behavior or protocol.

## What the code actually schedules

- GitHub owns the job backlog and chooses a matching connected runner. Roost receives assigned-demand statistics for a scale set, not a portable job object with a known duration. Before reservation, unmet demand can use any enrolled worker with the exact compatible profile. There is no Roost per-host waiting-job queue to steal from.
- Roost rotates scopes and labels for admission. Each queue currently gets at most one new assignment per `Sync`. Within placement, any compatible READY guest wins over cold capacity. Trusted `placement_priority`, then worker ID, order hosts within each of these categories. This is first-fit with an operator preference, not least-loaded scheduling.
- Offline, draining, mismatched or unreconciled workers are excluded. Global and scope counts include uncertain and retiring assignments. Physical workers enforce their own capacity and reclaim only credential-free READY guests of a different profile as documented. Never rebind a durable intent based solely on a timeout or replay credentials to shift an allocation.
- An overloaded worker at its configured limit cannot receive another cold allocation. Unrelated host CPU/I/O pressure below those static allocation limits is not a placement input. Warm preference can still select a busy but eligible worker. An idle worker with the wrong image/platform cannot relieve that queue.
- Chickadee standalone scheduling is local to one host, with a stable waiting order and resource/credential lifecycle checks. It cannot balance across hosts. Its local FIFO can also hold smaller requests behind a request that does not fit; this study does not modify that policy. Hosted placement belongs in Roost, not in the VM engine.

Relevant source: [broker](../internal/fleet/broker.go), [admission order](../internal/fleet/fairness.go), [remote RPC deadlines](../internal/dispatch/worker.go), [one-second main loop](../cmd/chickadee-roost/main.go), [worker reservation behavior](https://github.com/plover-digital/chickadee/blob/83eba5906053/internal/worker/engine.go), and [standalone pool](https://github.com/plover-digital/chickadee/blob/61842ee/internal/pool/controller.go).

## Offline experiment

[The opt-in Go harness](../internal/fleet/simulation_test.go) calls the actual `Broker.Sync`, admission ordering, placement, capacity checks, fsynced journal and asynchronous registration cleanup. Workers and GitHub are fakes. No VMs, Ubuntu guest processes, runner registrations, external requests, infrastructure or customer records are created.

A logical tick is one simulated second. The generic model has two workers with three slots each, 2-vCPU/4-GiB small runners, a six-second cold boot and one shared small warm target per worker. Mixed runs also use 4-vCPU/8-GiB runners and a warm target for that size. Each worker has six allocated vCPU and 12 GiB allocated guest memory, so a medium consumes two small units. These are toy budgets, not an inventory or a replay of a deployed fleet. The scale case uses sixteen workers with two slots each, within current fleet configuration bounds. Every ordinary case has 240 requests; the scale case has 1,000. Default scope concurrency is one; quota4, burst and label experiments explicitly differ. Synthetic desired counts use the exact arrived backlog plus running count rather than emulating the listener’s per-label maximum and statistics lag. A real policy change would also have to respect the separately configured per-label maximum.

Jobs usually take 8–32 logical seconds; the burst and label scenarios use ten seconds. Low-load arrivals span 2,400 seconds, near-capacity/mixed/outage/drain arrivals span 1,200 seconds, overload spans 120 seconds, and scale spans 600 seconds. Burst/label jobs arrive together. The heterogeneous case gives the preferred host 1.4 times the execution speed. Outage hides one worker between seconds 100 and 200 while its executions continue. Drain stops new placement on one worker at second 100 and preserves running jobs.

The alternative policies retain the same broker safety checks:

| Policy | Experiment |
| --- | --- |
| Current | Warm first; static priority, then stable worker ID |
| Spread | Warm first; prefer fewer allocated execution CPUs as a fraction of host budget; static priority breaks ties |
| Round robin | Warm first; prefer fewer historical placements, with priority breaking ties |
| Adaptive warm | Keep current placement; choose credential-free warm profiles using arrived, quota-eligible pending demand; reclaim unused READY shapes |
| Eager fill | Keep current placement; repeat admission immediately while assignments are added, instead of waiting for another tick |

Spread/rotation are test-only dynamic worker priority adapters, recalculated when `Sync` orders workers, not after each admission within that pass. They are not existing fleet configuration options. Adaptive warming is a model extension with knowledge of synthetic pending counts; worker API v1 has no dynamic warm-target operation. Eager fill is an optimistic experiment in repeated reconciliation, not a deployable implementation of a scheduler pump.

The harness checks CPU, memory, VM and scope caps every tick; all work must drain, each request must start once, and the final runner assignment count must equal the generated request count. Fake cleanup RPCs finish before the next logical tick; their results still enter the real broker on the next `Sync`.

### Results

p95 arrival-to-delivery wait, in **simulated seconds**, seed 42. This is not a production latency estimate.

| Scenario | Current | Spread | Rotation | Adaptive warm | Eager fill |
| --- | ---: | ---: | ---: | ---: | ---: |
| low | 32 | 32 | 32 | 32 | 32 |
| low-quota4 | 6 | 6 | 6 | 6 | 6 |
| near-capacity | 196 | 153 | 159 | 177 | 182 |
| overload | 1011 | 1038 | 1028 | 1011 | 1019 |
| burst | 792 | 792 | 792 | 792 | 790 |
| heterogeneous | 63 | 72 | 88 | 63 | 55 |
| outage | 179 | 176 | 179 | 179 | 179 |
| drain | 1123 | 1123 | 1123 | 1123 | 1123 |
| mixed | 866 | 861 | 888 | 727 | 918 |
| label-fairness | 790 | 790 | 790 | 790 | 790 |
| scale | 326 | 328 | 320 | 330 | 314 |

The main matrix completed **17,000 simulated jobs across 55 cases**. Two additional seeds cover low, near-capacity and mixed traffic (7,200 more jobs). Every case drained with one assignment per job and no modeled cap violation. All artifacts contain synthetic identities only. [Recorded result summaries and metadata](scheduler-simulation/results.json).

At low load, current placement delivered 160 jobs on the preferred host and 80 on the other; rotation delivered 121 and 119. Both had p50 **0** and p95 **32**. In the same low-load trace, increasing the synthetic scope cap from one to four reduced p95 to **6**. With cap one, the diagnostic counted **1,559 waiting-job seconds** blocked by the scope cap and **17** by unavailable resource/host capacity. These metrics include broker-retained ownership until cleanup; they are not GitHub execution-time measurements.

| Sensitivity: p95 across seeds 42/43/44 | Current | Spread | Adaptive warm |
| --- | --- | --- | --- |
| low | 32 / 27 / 20 | 32 / 26 / 20 | 32 / 27 / 20 |
| near-capacity | 196 / 158 / 110 | 153 / 154 / 168 | 177 / 158 / 114 |
| mixed | 866 / 846 / 813 | 861 / 850 / 813 | 727 / 639 / 808 |

Mixed traffic illustrates the tradeoff: seed 42 adaptive warming reduced p95 from **866 to 727**, while its median increased from **78 to 163**. In seed 44 its tail improvement was small. Near-capacity spreading helped seeds 42/43 and hurt seed 44. The synthetic faster-host case gave current placement p95 **63**, versus **88** with rotation. These are directional experiments, not a statistical estimate of policy superiority.

### Interpretation

Host rotation has little benefit at low synthetic load. More even counts do not imply lower waits. Raising the synthetic scope quota is a diagnostic counterfactual, not a recommendation to change customer admission policy: a queue already using its scope quota cannot start more jobs simply by moving them to another host.

No alternative consistently wins across arrival seeds. The faster-host case favors retaining a speed preference rather than forcing equal counts. Under overload, changing host ranking does not create capacity. The burst already reaches both hosts; warm replenishment, cold reservations and per-pass admission limit start rate. Eager fill improves some cases but does not eliminate those limits.

Adaptive prewarming is interesting for mixed-size tails, but its median and sensitivity tradeoffs argue against enabling the naive model as a default. A better next experiment would retain a default small warm floor and adapt only spare capacity, with minimum residence times and demand-age hysteresis to avoid reclaim/reboot oscillation. It needs real boot and cleanup measurements and a worker-owned capacity interface, not privileged QEMU commands in Roost.

## Controlled scaling findings

### A worker inventory request delays unrelated admission

`TestSchedulerSlowInventoryCharacterization` blocks one fake inventory RPC with a channel. A healthy READY worker receives no placement until that unrelated call is released, then receives the assignment. This is a deterministic ordering reproduction, not a benchmark based on sleep thresholds.

Inventory calls and assignment status/provisioning remain serialized. The remote adapter bounds inventory/status to three seconds each; bounding each call does not bound a complete reconciliation pass to one second. Many degraded workers can therefore consume multiple tick intervals. The latest-observation mailbox fixes observation backlog, but cannot make progress during a blocked `Sync`.

Next implementation: gather inventories with bounded concurrency, validate identities and freshness, and feed results to one journal owner. Then consider bounded per-worker provisioning operations so a cold boot does not stall other workers. Preserve durable intents, independent deadlines, host/generation reconciliation, quotas, and no-replay behavior. Do not simply run concurrent `Sync` calls.

### Many labels can buy more admission opportunities

`TestSchedulerManyLabelsCharacterization` gives two continuously backlogged scopes identical six-runner caps. One has eight labels; the other has one. Six READY slots finish together each round, removing boot noise. Across ten admission rounds, the first scope gets **51 assignments** and the second **9**. Neither violates its quota; opportunity fairness still differs. Existing single-slot rotation tests do not cover this case. The ordinary finite-trace label scenario need not exhibit the same imbalance; synchronization and available capacity matter.

Next implementation: bounded admission rounds that give each eligible scope one opportunity before returning to its next label, and allow another round when free capacity and demand remain. A single busy queue should be able to fill useful spare capacity without gaining priority over another eligible scope. Test unequal label counts, different sizes, skipped queues, restart cursors, lowered quotas and ambiguous reservations. Later consider resource-weighted fairness, since equal job counts also favor larger runners.

### Retained history and configured bounds matter before scheduler replacement

Current fleet loading permits at most sixteen workers and thirty-two VMs. The broker retains at most 10,000 assignments or an 8-MiB journal, whichever limit arrives first; writes and several scans include retained history. This experiment starts each case with fresh history and reaches only 1,000 assignments. It does not establish indefinite operation or safe journal compaction. Completed history/usage retention and recovery-safe compaction deserve explicit endurance tests before raising fleet limits. Never delete uncertain/live ownership records to reclaim space.

## Options researched through Exa

Four search angles requested 22 results, including duplicate references. The sources below were selected for primary documentation/research, rather than treating search matches as validated recommendations.

| Option | Useful principle | Fit here |
| --- | --- | --- |
| Continue custom central placement | GitHub provides the scale-set client for custom VM provisioning | Best current fit; keep one listener per queue and improve the small broker incrementally |
| Nomad | Separate feasibility filtering from ranking; combine packing, preferences and failure-domain spread | Good design reference; adopting another control plane does not fix GitHub demand/accounting semantics |
| Kubernetes/ARC | Resource-aware scoring can pack or spread; ARC owns runner lifecycle inside Kubernetes | Appropriate when Kubernetes is already the intended substrate; a major architectural change for this standalone VM engine |
| Sparrow-style late binding | Delay choosing a physical worker until it can actually serve work; avoid long worker-local queues | Useful for readiness/capacity offers; does not authorize reassigning an existing credential intent |
| Dominant Resource Fairness | Allocate across users by their dominant resource share, rather than a raw number of jobs | Worth testing once mixed resource classes and larger scope quotas make slot fairness insufficient |

GitHub matches jobs to online idle runners and documents a 60-second pickup requeue for an assigned self-hosted runner. Distinct same-name scale sets in different runner groups receive arbitrary assignment; that is not a configurable host balancing mechanism. Keep host choice inside the central service. [GitHub runner routing](https://docs.github.com/en/actions/reference/runners/self-hosted-runners), [custom scale-set clients](https://github.com/actions/scaleset/tree/v0.4.0), [scale-set distribution](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets#high-availability-and-automatic-failover).

Nomad separates node feasibility from scoring and incorporates packing and preferences. Its spread controls are soft preferences. Kubernetes exposes resource-weighted scoring curves for packing or spreading. These support improving Roost's filter/rank separation without adopting their infrastructure. [Nomad scheduling](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/how-scheduling-works), [Nomad spread](https://developer.hashicorp.com/nomad/docs/job-specification/spread), [Kubernetes resource scoring](https://kubernetes.io/docs/concepts/scheduling-eviction/resource-bin-packing/).

Sparrow studies late binding to avoid the poor relationship between worker queue length and actual wait, especially for short parallel tasks. Its results are not VM CI benchmarks. DRF studies fairness across multiple resource dimensions; it does not address credential ownership or VM migration. [Sparrow paper](https://people.eecs.berkeley.edu/~matei/papers/2013/sosp_sparrow.pdf), [DRF paper and talk](https://www.usenix.org/conference/nsdi11/dominant-resource-fairness-fair-allocation-multiple-resource-types).

## Recommended sequence

1. Capture bounded operational measurements: first-observed unmet-demand age (not an invented GitHub job age), runner READY-to-delivery time, `Sync` duration, per-phase RPC latency, backlog with eligible spare capacity, worker coverage and scope/resource/compatibility reasons for blocking. Report configured allocation separately from actual job execution or host utilization. Existing status counts and an empty snapshot cannot establish a sustained queue-latency distribution.
2. Fix serial RPC coupling and multi-label admission opportunity fairness, with focused safety regressions. Add a long-history endurance/retention test. Keep placement preference unchanged while establishing these baselines.
3. Run alternative placement and warm-pool decisions in shadow mode: calculate suggestions against the same fresh inventory without reserving a second VM. Store bounded aggregates and synthetic/anonymized replay inputs, not credentials or customer job content. Compare p50/p95 wait and resource waste, rather than visual host-count symmetry.
4. Trial adaptive spare warm capacity with floors/hysteresis only if measured mixed-size waits warrant it. Prefer safe changes for new unbound demand; never migrate a running VM or transfer spent credentials.
5. Revisit a general scheduler when the intended fleet exceeds current bounds or needs failure domains, elastic provider provisioning, durable fair queues or multiple fenced brokers. Two eligible idle hosts and one hot host alone do not justify that migration.

## Reproduce and validation

Run the experiment from this Go module. It is skipped by normal tests unless explicitly enabled:

```sh
go test -c -o /tmp/chickadee-scheduler.test ./internal/fleet
CHICKADEE_SCHEDULER_SIM=1 \
CHICKADEE_SCHEDULER_SIM_OUT=/tmp/scheduler-results.json \
/tmp/chickadee-scheduler.test -test.run '^TestScheduler' -test.v -test.timeout=10m
```

Synthetic journals are fsynced. On a host with an appropriate writable memory filesystem, set `TMPDIR=/dev/shm` for the test binary to reduce disk I/O; compile the executable outside that filesystem because it may be mounted noexec. Results then characterize scheduling logic, **not durable disk throughput**. Test temporary directories are private and removed normally. Use a suitable scratch filesystem and available capacity.

`CHICKADEE_SCHEDULER_SIM_SEED` selects the arrival/duration seed (default 42). For a smaller sensitivity run, use `-test.run '^TestSchedulerSimulation/(low|near-capacity|mixed)/'`. Job traces repeat for a given seed; exact outcomes can vary because production uses random assignment IDs to break simultaneous cleanup ties. Output wall time also depends on the environment.

Validation for this addition:

- `make build test`: both binaries, full Go race suites, pinned public worker API race tests and 99 Python checks pass.
- Main opt-in matrix: 55 cases / 17,000 synthetic jobs pass.
- Sensitivity seeds 43/44: 30 cases / 7,200 synthetic jobs pass.
- Controlled serial-inventory and multi-label fairness characterizations pass, including race checks.
- Representative burst, outage, mixed-profile and scale simulations pass separately under the race detector.

Normal required tests use local mock HTTP servers; they need loopback listening permissions. The opt-in experiment itself needs no network access.

No network latency, actual Ubuntu boot, QEMU contention, real image/storage cleanup, upstream statistics lag/cancellation, GitHub API behavior, authentication, certificate failure, external host load, partition timing or multiple brokers are emulated end to end. Simulated cold reservations return immediately and mature later, whereas real calls can consume their RPC deadline. These omissions generally make start latency optimistic; simplistic warm behavior can also create model-specific bottlenecks. Treat seconds as comparative model outputs, not promises for production. Existing lifecycle tests and live acceptance remain separate evidence.

This addition requires no deployment and changes no worker API, fleet/catalog configuration, journal version or deployment order. No production scheduler policy is selected by the test environment variables.
