#!/usr/bin/env python3
"""Record release provenance and gate source plus exact-binary vulnerability scans."""
from contextlib import contextmanager
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import zipfile

BINARIES = ("chickadee-web", "chickadee-roost")
EVIDENCE = Path("build/release")


def output(*args):
    return subprocess.check_output(args, text=True).strip()


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def metadata():
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    (EVIDENCE / "scan-status.json").unlink(missing_ok=True)
    revision = output("git", "rev-parse", "HEAD")
    dirty = bool(output("git", "status", "--porcelain", "--untracked-files=normal"))
    records = {}
    for name in BINARIES:
        binary = Path("bin") / name
        info = output("go", "version", "-m", str(binary))
        # Reject stale or foreign binaries; make release builds these first.
        for expected in ("go1.26.9", "vcs.revision=" + revision,
                         "vcs.modified=" + str(dirty).lower(), "CGO_ENABLED=0",
                         "-trimpath=true"):
            if expected not in info:
                raise ValueError(f"{name}: missing build metadata {expected}")
        (EVIDENCE / (name + ".buildinfo.txt")).write_text(info + "\n")
        records[name] = {"sha256": digest(binary), "buildinfo": info}
    manifest = {
        "recorded_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "revision": revision, "dirty": dirty, "toolchain": output("go", "version"),
        "binaries": records,
    }
    (EVIDENCE / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (EVIDENCE / "SHA256SUMS").write_text("".join(
        f'{record["sha256"]}  bin/{name}\n' for name, record in records.items()))


def scan_one(scanner, name, args):
    """JSON alone is not a gate: upstream convert mode returns 3 for findings."""
    with (EVIDENCE / (name + ".json")).open("w") as report, \
            (EVIDENCE / (name + ".stderr.txt")).open("w") as errors:
        result = subprocess.run([scanner, "-json", *args], stdout=report, stderr=errors)
    if result.returncode:
        return result.returncode
    with (EVIDENCE / (name + ".json")).open() as report, \
            (EVIDENCE / (name + ".txt")).open("w") as summary:
        result = subprocess.run([scanner, "-mode=convert"], stdin=report,
                                stdout=summary, stderr=subprocess.STDOUT)
    return result.returncode


@contextmanager
def database_snapshot():
    """Use one retained official snapshot for all scans, or replay a saved ZIP."""
    archive = EVIDENCE / "vulndb.zip"
    saved = os.environ.get("VULNDB_ARCHIVE")
    if saved:
        if Path(saved).resolve() != archive.resolve():
            shutil.copyfile(saved, archive)
    else:
        with urllib.request.urlopen("https://vuln.go.dev/vulndb.zip", timeout=60) as response:
            data = response.read(32 * 1024 * 1024 + 1)
        if len(data) > 32 * 1024 * 1024:
            raise ValueError("vulnerability database archive exceeds size limit")
        archive.write_bytes(data)
    with tempfile.TemporaryDirectory(prefix="roost-vulndb-") as directory:
        root = Path(directory)
        with zipfile.ZipFile(archive) as bundle:
            if sum(info.file_size for info in bundle.infolist()) > 128 * 1024 * 1024:
                raise ValueError("expanded vulnerability database exceeds size limit")
            for info in bundle.infolist():
                if not re.fullmatch(r"(?:index/(?:db|modules|vulns)|ID/GO-[0-9]+-[0-9]+)\.json", info.filename):
                    raise ValueError("unexpected vulnerability database archive member")
                target = root / info.filename
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(bundle.read(info))
        record = {"source": "https://vuln.go.dev/vulndb.zip",
                  "sha256": digest(archive), "replay": bool(saved),
                  "database": json.loads((root / "index/db.json").read_text()),
                  "recorded_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat()}
        (EVIDENCE / "database.json").write_text(json.dumps(record, indent=2) + "\n")
        yield root.as_uri()


def scan():
    (EVIDENCE / "scan-status.json").unlink(missing_ok=True)
    scanner = "build/tools/govulncheck"
    (EVIDENCE / "scanner.buildinfo.txt").write_text(
        output("go", "version", "-m", scanner) + "\n")
    # JSON config retains scanner/database versions; the ZIP makes the
    # database content reproducible even after the official service updates.
    with database_snapshot() as database:
        results = {"source": scan_one(scanner, "source", ["-db=" + database, "./..."])}
        for name in BINARIES:
            results[name] = scan_one(scanner, name,
                                     ["-db=" + database, "-mode=binary", "bin/" + name])
    (EVIDENCE / "scan-status.json").write_text(json.dumps(results, indent=2) + "\n")
    for name, status in results.items():
        print(f"{name}: {'PASS' if status == 0 else 'FAIL'} (exit {status})")
    return int(any(results.values()))


if __name__ == "__main__":
    if sys.argv[1:] == ["metadata"]:
        metadata()
    elif sys.argv[1:] == ["scan"]:
        sys.exit(scan())
    else:
        sys.exit("usage: release-check.py metadata|scan")
