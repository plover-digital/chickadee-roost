# Broker compiler and client security maintenance release

This route preserves the byte-reproduced broker application source while updating
Go to 1.26.10 and the public worker API client to the reviewed core commit
`83eba5906053ab187839528f6ce05c38b4fc7368` (module version
`v0.0.0-20261019192139-83eba5906053`). It is **not compiler-only** and does not
deploy current-main Roost application code. The original source archive and
module files remain separate from the candidate source archive and module pin.
Do not replace a newer website with this broker-only artifact.

The client fix preserves context cancellation when response reading succeeds at
EOF concurrently with cancellation. It executes in Roost; replacing a worker
server binary alone does not fix the broker client. The API dependency retains
native `apple-vz`, `q35`, `microvm` and resource summaries. Compared with the
historical `c0a0cea` client, it also understands optional `work_folder` and the
`landlock` enum. Existing VM responses omit the optional field; Roost catalog
validation continues to reject Landlock service profiles. Only `workerapi` is
imported from core, not its host engine. API v1, identity/certificates and journals
remain compatible; no worker generation or state migration is required.

Using the reviewed public main API avoids creating a separate maintained core
fork. Do not substitute the older worker backport lineage: it predates native
Mac API support. Validate the dependency diff and the pinned package's tests,
including the deterministic successful-EOF cancellation regression.

## Prove deployed source before rebuilding

Obtain the running executable's SHA-256, embedded Go/module/build settings and
an immutable source candidate from private deployment evidence. A nearby Git
checkout, source timestamp or matching module versions alone is insufficient.
For older releases built without VCS metadata, reproduce the original bytes.
If reproduction fails, stop and investigate source, toolchain and build settings;
do not describe the candidate as the deployed source.

The following recipe applies to a Linux/amd64 broker originally built with
Go 1.26.3, CGO disabled, the default amd64 v1 target, no build tags, and
`-buildvcs=false -mod=readonly -trimpath`. Adjusting those assumptions requires
separate evidence/review. The replacement uses Go 1.26.10, updating only the reviewed core requirement and checksums in `go.mod`/`go.sum`,
while preserving the module's Go directive. Raising that directive can
change Go compatibility defaults and is outside this maintenance route.

Run from a trusted Roost Git repository. Set `RECIPE_REVISION` to the **full
40-character reviewed commit** containing the launcher, recipe and helper;
set `BROKER_SOURCE_REVISION` to the source candidate and
`BROKER_ORIGINAL_SHA256` to the independently measured running-binary hash.
Keep host/access/configuration details private. Assess host capacity first;
Go work is serialized and output stays under ignored `build/`.

Execute the launcher from the pinned Git object, never from a working-tree copy:

```sh
set -euo pipefail
export RECIPE_REVISION=FULL_REVIEWED_COMMIT
export BROKER_SOURCE_REVISION=FULL_SOURCE_COMMIT
export BROKER_ORIGINAL_SHA256=MEASURED_ORIGINAL_SHA256
# Optional: reuse a dedicated isolated compilation cache via GOCACHE.
git show "$RECIPE_REVISION:scripts/run-reviewed-recipe.py" | \
  nice -n 15 python3 - "$RECIPE_REVISION"
```

The launcher rejects symbolic or abbreviated revisions. It reads the launcher,
[shell recipe](../scripts/broker-security-release.sh) and release-check helper
from that immutable commit, retains their exact bytes in `recipe/`, and records
SHA-256 hashes and Git blob IDs in the release manifest. The shell runs from the
same in-memory bytes that were hashed; the helper is passed as an immutable
encoded buffer and compiled from those bytes. Neither execution reads a helper
or recipe from the working tree, nor resolves HEAD after the build. Dirty files
and concurrent branch movement therefore cannot silently change the attributed
recipe. `recipe_provenance` identifies the executed objects; it does not claim
the caller's working tree is clean. The archive retains all three reviewed files.

The recipe reproduces original bytes, applies the reviewed API pin and rebuilds with the patched compiler,
runs Go race/Python and pinned workerapi package tests, checks native/legacy API
compatibility and the absence of host-engine imports, performs snapshot-backed
source/binary scans, verifies
unchanged application source and candidate module files, then packages the broker and evidence. A failure
stops release creation. As with every release process, the launching interpreter,
Git executable and operator environment must be trusted; these checks do not
protect against a malicious operator replacing the launcher command itself.

Retain the original-byte match, source archive, manifest, scanner/database
versions, tests, scan streams and archive hash. Original and candidate source/module hashes are distinct. No application version is
embedded by these historical flags: the manifest and hashes are essential.
A passing scan does not prove absence of vulnerabilities or deployment success.

## Coordinated rollout and rollback

Review the exact source/recipe/artifact and resolve review findings first.
Immediately before rollout, recheck the running and installed hashes: concurrent
work may have changed either executable. A changed hash invalidates the assumed
rollback/provenance baseline and requires investigation. Compare the candidate
against the actual current config/journal reader contract, not only the runbook.

Follow the [broker drain and acceptance sequence](releases.md#compatibility-and-coordinated-deployment).
Retain the exact original executable and compatible config privately. Install
only the broker artifact, preserving worker IDs, generations, certificates,
configuration, journal and API v1. Do not run a second GitHub listener. No worker
or website replacement is implied by this recipe. Verify installed/running
hashes, inventory, reconciliation and an authorized job through cleanup.

If rollback is necessary, stop/drain the replacement and restore the saved
executable/config while retaining the latest journal. Never restore an old
journal snapshot over assignments created after the backup. The rollback
compiler may be vulnerable; record that temporary exposure and fix forward.
