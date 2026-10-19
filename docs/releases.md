# Release security checks and rollout

The reference toolchain and minimum module version are Go 1.26.9. The Makefile
pins `GOTOOLCHAIN=go1.26.9`; CI installs that exact release. Update these together
with `go.mod`, the metadata validator and README when reviewing a newer baseline.
The pinned scanner is `golang.org/x/vuln/cmd/govulncheck@v1.9.0`.

On 2026-10-19, the [official release history](https://go.dev/doc/devel/release)
listed 1.26.9 and 1.27.2 as patched supported releases.
[GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617) includes the HTTP/2 server
HPACK race fix in those releases. Keeping the 1.26 line limits upgrade scope.
Always recheck release notes and the vulnerability database before deployment.

## Build and verify

Use a dedicated clean checkout of the reviewed commit, Go, Git and Python 3:

```sh
make build test
make vulncheck
sha256sum -c build/release/SHA256SUMS
```

`make release` rebuilds both binaries and writes provenance without scanning.
`make vulncheck` rebuilds, records provenance, installs the pinned scanner and
scans `./...` plus the exact `bin/chickadee-web` and `bin/chickadee-roost` files.
It fails for scanner errors or findings and attempts all three scans. JSON mode
alone always exits zero on findings, so each JSON stream is converted through
upstream text mode to enforce the vulnerability gate. A failed run is not a
release, even if artifact files exist. Rerun after every source/binary change.

`build/release/` contains a UTC manifest, source revision/dirty flag, binary
SHA-256 hashes, `go version -m` for each binary and scanner, full JSON scan streams,
text summaries, stderr and per-scan exit codes. Each scan's JSON config records
the scanner version, database URL and modification time, Go version and mode;
advisories are retained with findings. Builds retain embedded VCS information
and symbols for binary analysis. Release only a clean (`dirty: false`) manifest
whose revision matches the reviewed commit. Do not rebuild after approval and
then deploy different bytes without repeating verification.

The default scan downloads the official [bulk database ZIP](https://go.dev/security/vuln/database#api)
once, retains it as `build/release/vulndb.zip` and scans all three targets against
that same snapshot. `database.json` records its SHA-256, modification time, source
and UTC retrieval time. To reproduce a previous scan, set `VULNDB_ARCHIVE` to
that retained ZIP when running `make vulncheck`. Tool versions and database
bytes are then fixed. Run again without that override against fresh advisories
before deployment. Source reachability does not prove exploitability, and binary
scans may report unreachable symbols. Neither scan proves absence of all bugs.

Outputs stay in ignored `bin/` and `build/`; do not commit binaries, scan output,
private configuration or runtime state. Keep release evidence with private
rollout records. Before publishing logs, inspect them for local paths or other
operator information. CI executes the same checks and retains release evidence for 30 days, but does
not deploy.

On a shared host, assess capacity first. Limit compilation/test concurrency
using `GOMAXPROCS=1 GOFLAGS=-p=1`, lower process priority, and set `GOCACHE` and
`TMPDIR` to dedicated ignored directories. Tests use synthetic data and temporary
state. Do not launch a second broker with live configuration to smoke-test it.

For a compiler-and-client-fix maintenance release anchored to byte-verified
deployed broker source, use the [separate recipe](broker-security-release.md).
It preserves the old module Go directive and does not replace a newer website.

## Compatibility and coordinated deployment

This security-baseline change updates the Chickadee public client dependency
to include the cancellation fix while preserving public worker API v1, fleet/catalog JSON, private bridge contracts, journals,
executable names, environment names, callback routes and cookies. It needs no
worker upgrade, image rebuild or configuration migration. Existing feature
migrations still apply when upgrading from older deployed source; read
[fleet compatibility](fleet.md), particularly readers-before-writers for
resource summaries and broker-before-bridge for queue initialization status.
The toolchain change does not override those requirements.

1. Retain the current executable hashes, binaries and compatible private config
   for rollback. Record the reviewed source revision, candidate hashes, scan
   database date and passing tests. Identify whether any intervening source
   migration affects the actual deployed revision.
2. Coordinate exclusive rollout with other host work. For the broker, signal
   `SIGUSR1` to drain central admission and wait until assignments are resolved.
   Preserve uncertain-assignment quota; do not force an empty journal. Verify
   drain using private status and worker inventory, not just a service state.
   Draining exits the broker when assignments finish; coordinate the service
   manager so an automatic restart cannot reopen admission before replacement.
3. Stop the sole broker with `SIGTERM` if still running, install the verified candidate atomically
   and start it with the existing configuration and journal. Never run old and
   new listeners together. Worker jobs survive broker exit; do not restart
   workers, change worker generations or delete their state as part of this update.
4. Verify authenticated inventory, journal reconciliation, queue initialization,
   requested/applied status, and one authorized synthetic job through completion
   and cleanup. Confirm uncertain/missing-host quota remains conservative.
5. Replace/restart the website independently using the verified candidate.
   Existing in-memory sessions expire at restart; validate HTTPS, a fresh browser
   sign-in, dashboard and native form handling, and that private admin endpoints
   remain unavailable publicly. Use authorized test accounts only.
6. Record installed hashes/build metadata and actual acceptance in private
   operations tracking. Source tests are not production acceptance evidence.

If acceptance fails, drain/stop the broker before atomically restoring the saved
compatible binary/config; retain the current journal and reconcile inventory on
restart. Never restore a stale journal snapshot over active assignments. Restore
the website binary independently and recheck login/status. An old vulnerable
binary is emergency rollback only; record the exposure and fix forward promptly.
