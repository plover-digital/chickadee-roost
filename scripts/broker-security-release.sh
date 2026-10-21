#!/usr/bin/env bash
set -eu
: "${BROKER_SOURCE_REVISION:?set the verified source candidate}"
: "${BROKER_ORIGINAL_SHA256:?set the measured original binary SHA-256}"
: "${RELEASE_DIR:?invoke through the reviewed launcher}"
release_dir="$RELEASE_DIR"
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
export GOTOOLCHAIN=go1.26.10
# Retain the byte-reproduced source anchor; apply only the reviewed API pin.
cp "$release_dir/source/go.mod" "$release_dir/original.go.mod"
cp "$release_dir/source/go.sum" "$release_dir/original.go.sum"
api_version=v0.0.0-20261019192139-83eba5906053
go -C "$release_dir/source" mod edit -require="github.com/plover-digital/chickadee@$api_version"
go -C "$release_dir/source" mod download "github.com/plover-digital/chickadee@$api_version"
# Fail if resolving the API pin changed the module's compatibility directive.
test "$(sed -n '/^go /p' "$release_dir/original.go.mod")" = "$(sed -n '/^go /p' "$release_dir/source/go.mod")"
cp "$release_dir/source/go.mod" "$release_dir/candidate.go.mod"
cp "$release_dir/source/go.sum" "$release_dir/candidate.go.sum"
tar -cf "$release_dir/candidate-source.tar" -C "$release_dir/source" .
CGO_ENABLED=0 go build -C "$release_dir/source" -buildvcs=false -mod=readonly \
  -trimpath -o "$release_dir/chickadee-roost" ./cmd/chickadee-roost
CGO_ENABLED=1 go -C "$release_dir/source" test -race -mod=readonly ./... \
  > "$release_dir/go-tests.txt" 2>&1
(cd "$release_dir/source" && python3 -m unittest discover -s scripts -p 'test_*.py') \
  > "$release_dir/python-tests.txt" 2>&1
CGO_ENABLED=1 go -C "$release_dir/source" test -race -mod=readonly -count=1 \
  github.com/plover-digital/chickadee/workerapi > "$release_dir/workerapi-tests.txt" 2>&1
# Validate the exact candidate dependency's native and legacy wire capabilities.
cat > "$release_dir/compatibility.go" <<'GO'
package main
import (
    "encoding/json"
    "bytes"
    "github.com/plover-digital/chickadee/workerapi"
)
func main() {
    for _, machine := range []string{"q35", "microvm", "apple-vz"} {
        if !workerapi.SupportedMachine(machine) { panic("VM/native API capability lost") }
    }
    data, err := json.Marshal(workerapi.Request{})
    if err != nil || bytes.Contains(data, []byte("work_folder")) { panic("legacy wire shape changed") }
}
GO
CGO_ENABLED=0 go -C "$release_dir/source" run -mod=readonly "$release_dir/compatibility.go"
CGO_ENABLED=0 go -C "$release_dir/source" list -mod=readonly -deps ./cmd/chickadee-roost \
  > "$release_dir/broker-packages.txt"
GOBIN="$release_dir/tools" go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
export CGO_ENABLED=0
python3 - <<'PY'
import base64, datetime, json, os, pathlib, types
root = pathlib.Path(os.environ['RELEASE_DIR'])
# Execute and hash the same immutable buffer supplied by the launcher.
helper = base64.b64decode(os.environ['REVIEWED_HELPER_B64'], validate=True)
check = types.ModuleType('release_check')
exec(compile(helper, 'reviewed-release-check.py', 'exec'), check.__dict__)
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
         'chickadee-roost', 'tools/govulncheck', 'vulndb.zip', 'candidate-source.tar',
         'original.go.mod', 'original.go.sum', 'candidate.go.mod', 'candidate.go.sum',
         'compatibility.go', 'broker-packages.txt']
core_packages = [p for p in (root / 'broker-packages.txt').read_text().splitlines()
                 if p.startswith('github.com/plover-digital/chickadee/')]
if core_packages != ['github.com/plover-digital/chickadee/workerapi']:
    raise SystemExit('unexpected core host-engine import')
metadata = {name: {'sha256': check.digest(root / name)} for name in files}
for name in ['broker-original', 'chickadee-roost', 'tools/govulncheck']:
    metadata[name]['buildinfo'] = check.output('go', 'version', '-m', str(root / name))
manifest = {
    'recorded_at_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'original_source_revision': (root / 'source-revision.txt').read_text().strip(),
    'candidate_change': {'workerapi_module': 'v0.0.0-20261019192139-83eba5906053',
                         'scope': 'compiler and workerapi client cancellation fix; module pin only'},
    'original_sha256': os.environ['BROKER_ORIGINAL_SHA256'],
    'recipe_provenance': json.loads(os.environ['RECIPE_PROVENANCE_JSON']),
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
diff -r --exclude=__pycache__ --exclude=go.mod --exclude=go.sum "$release_dir/source-verify" "$release_dir/source"
cmp "$release_dir/candidate.go.mod" "$release_dir/source/go.mod"
cmp "$release_dir/candidate.go.sum" "$release_dir/source/go.sum"
# Keep caches and the expanded source out of the deployable evidence archive.
tar -czf "$release_dir/release.tar.gz" -C "$release_dir" \
  chickadee-roost broker-original source.tar source-revision.txt manifest.json \
  database.json vulndb.zip scan-status.json source-scan.json source-scan.txt \
  source-scan.stderr.txt broker-scan.json broker-scan.txt broker-scan.stderr.txt \
  go-tests.txt python-tests.txt workerapi-tests.txt recipe \
  candidate-source.tar original.go.mod original.go.sum candidate.go.mod candidate.go.sum \
  compatibility.go broker-packages.txt
sha256sum "$release_dir/release.tar.gz"
