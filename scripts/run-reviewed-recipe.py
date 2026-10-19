#!/usr/bin/env python3
"""Launch compiler-only releases using immutable reviewed Git objects."""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

FILES = ('scripts/run-reviewed-recipe.py', 'scripts/compiler-only-release.sh',
         'scripts/release-check.py')


def reviewed_inputs(revision):
    if not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise ValueError('recipe revision must be a full reviewed commit ID')
    resolved = subprocess.check_output(
        ['git', 'rev-parse', '--verify', revision + '^{commit}'], text=True).strip()
    if resolved != revision:
        raise ValueError('recipe revision must identify a commit directly')
    buffers = {}
    records = {}
    for name in FILES:
        # Resolve each path once; use the returned blob ID for the byte read.
        blob = subprocess.check_output(
            ['git', 'rev-parse', revision + ':' + name], text=True).strip()
        data = subprocess.check_output(['git', 'cat-file', 'blob', blob])
        buffers[name] = data
        records[name] = {'git_blob': blob, 'sha256': hashlib.sha256(data).hexdigest()}
    return buffers, {'revision': revision, 'files': records}


def run(revision):
    buffers, provenance = reviewed_inputs(revision)
    Path('build').mkdir(exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix='broker-compiler.', dir='build')).resolve()
    (root / 'recipe').mkdir()
    for name, data in buffers.items():
        (root / 'recipe' / Path(name).name).write_bytes(data)
    env = os.environ.copy()
    env.update(RELEASE_DIR=str(root),
               REVIEWED_HELPER_B64=base64.b64encode(buffers[FILES[2]]).decode('ascii'),
               RECIPE_PROVENANCE_JSON=json.dumps(provenance))
    print('Release output: ' + str(root), flush=True)
    # stdin receives exactly the bytes retained and hashed above; do not re-open
    # a working-tree or extracted script after checking its identity.
    return subprocess.run(['bash', '-s'], input=buffers[FILES[1]], env=env).returncode


if __name__ == '__main__':
    if len(sys.argv) != 2:
        sys.exit('usage: run-reviewed-recipe.py FULL_REVIEWED_COMMIT')
    sys.exit(run(sys.argv[1]))
