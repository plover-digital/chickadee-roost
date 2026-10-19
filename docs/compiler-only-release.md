# Compiler-only broker security release

Use this route when an immediate compiler fix must preserve the deployed
broker's application code, dependency versions, Go compatibility defaults,
worker identities and journals. It is separate from a release of current main.
Do not deploy a current-main artifact and label it a compiler-only backport.
The website is a separate artifact: inspect its actual running version first,
and preserve any newer website changes.

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
separate evidence/review. The replacement uses Go 1.26.9, preserving `go.mod`
and `go.sum`, including the module's Go directive. Raising that directive can
change Go compatibility defaults and is outside this compiler-only route.

Run from a trusted Roost checkout containing this PR's release-check helper.
Set `BROKER_SOURCE_REVISION` to the reviewed full source commit and
`BROKER_ORIGINAL_SHA256` to the independently measured running-binary hash.
Neither value is a credential; keep host/access/configuration details private.
Assess host capacity first; the commands serialize Go work and keep outputs
under ignored `build/`. Run the shell at reduced priority on a shared host.

```sh
set -eu
: "${BROKER_SOURCE_REVISION:?set the verified source candidate}"
: "${BROKER_ORIGINAL_SHA256:?set the measured original binary SHA-256}"
recipe_root="$PWD"
mkdir -p build
release_dir=$(mktemp -d "$PWD/build/broker-compiler.XXXXXX")
source_revision=$(git rev-parse --verify "${BROKER_SOURCE_REVISION}^{commit}")
printf '%s\n' "$source_revision" > "$release_dir/source-revision.txt"
git archive --format=tar "$source_revision" > "$release_dir/source.tar"
mkdir "$release_dir/source" "$release_dir/tmp" "$release_dir/cache" "$release_dir/tools"
tar -xf "$release_dir/source.tar" -C "$release_dir/source"
export BROKER_ORIGINAL_SHA256
export GOMAXPROCS=1 GOFLAGS=-p=1 GOOS=linux GOARCH=amd64 GOAMD64=v1
export GOWORK=off GOENV=off GO111MODULE=on
export GOCACHE="${GOCACHE:-$release_dir/cache}" TMPDIR="$release_dir/tmp"
unset GODEBUG GOEXPERIMENT
export GOTOOLCHAIN=go1.26.3
CGO_ENABLED=0 go build -C "$release_dir/source" -buildvcs=false -mod=readonly \
  -trimpath -o "$release_dir/broker-original" ./cmd/chickadee-roost
printf '%s  %s\n' "$BROKER_ORIGINAL_SHA256" "$release_dir/broker-original" | sha256sum -c -
export GOTOOLCHAIN=go1.26.9
CGO_ENABLED=0 go build -C "$release_dir/source" -buildvcs=false -mod=readonly \
  -trimpath -o "$release_dir/chickadee-roost" ./cmd/chickadee-roost
CGO_ENABLED=1 go -C "$release_dir/source" test -race -mod=readonly ./... \
  > "$release_dir/go-tests.txt" 2>&1
(cd "$release_dir/source" && python3 -m unittest discover -s scripts -p 'test_*.py') \
  > "$release_dir/python-tests.txt" 2>&1
GOBIN="$release_dir/tools" go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
export RECIPE_ROOT="$recipe_root" RELEASE_DIR="$release_dir" CGO_ENABLED=0
python3 - <<'PY'
import datetime, hashlib, importlib.util, json, os, pathlib, subprocess
root = pathlib.Path(os.environ['RELEASE_DIR'])
spec = importlib.util.spec_from_file_location('release_check',
    pathlib.Path(os.environ['RECIPE_ROOT']) / 'scripts/release-check.py')
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)
check.EVIDENCE = root
scanner = str(root / 'tools/govulncheck')
os.chdir(root / 'source')
with check.database_snapshot() as database:
    results = {
        'source': check.scan_one(scanner, 'source-scan', ['-db=' + database, './...']),
        'broker': check.scan_one(scanner, 'broker-scan',
            ['-db=' + database, '-mode=binary', str(root / 'chickadee-roost')]),
    }
(root / 'scan-status.json').write_text(json.dumps(results, indent=2) + '\n')
files = ['source.tar', 'source/go.mod', 'source/go.sum', 'broker-original',
         'chickadee-roost', 'tools/govulncheck', 'vulndb.zip']
metadata = {name: {'sha256': check.digest(root / name)} for name in files}
for name in ['broker-original', 'chickadee-roost', 'tools/govulncheck']:
    metadata[name]['buildinfo'] = check.output('go', 'version', '-m', str(root / name))
manifest = {
    'recorded_at_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'source_revision': (root / 'source-revision.txt').read_text().strip(),
    'original_sha256': os.environ['BROKER_ORIGINAL_SHA256'],
    'recipe_revision': check.output('git', '-C', os.environ['RECIPE_ROOT'], 'rev-parse', 'HEAD'),
    'files': metadata, 'scan_exit_codes': results,
}
(root / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
assert metadata['broker-original']['sha256'] == manifest['original_sha256']
if any(results.values()):
    raise SystemExit('Vulnerability check failed; do not release')
PY
# Prove the tests/build did not change tracked source or module files.
mkdir "$release_dir/source-verify"
tar -xf "$release_dir/source.tar" -C "$release_dir/source-verify"
diff -r --exclude=__pycache__ "$release_dir/source-verify" "$release_dir/source"
# Keep caches and the expanded source out of the deployable evidence archive.
tar -czf "$release_dir/release.tar.gz" -C "$release_dir" \
  chickadee-roost broker-original source.tar source-revision.txt manifest.json \
  database.json vulndb.zip scan-status.json source-scan.json source-scan.txt \
  source-scan.stderr.txt broker-scan.json broker-scan.txt broker-scan.stderr.txt \
  go-tests.txt python-tests.txt
sha256sum "$release_dir/release.tar.gz"
```

Retain the original-byte match, source archive, manifest, scanner/database
versions, tests, scan streams and archive hash. No application version is
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
