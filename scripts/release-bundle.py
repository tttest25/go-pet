#!/usr/bin/env python3
"""Pack or verify the release payload against a Git commit, never its checkout."""

import argparse
import gzip
import io
import re
import subprocess
import tarfile


FILES = {
    "scripts/spool-runner.sh": "100755",
    "postgres/bootstrap.sh": "100755",
    "postgres/postgresql.conf": "100644",
}
MIGRATIONS = "postgres/migrations"
MIGRATION = re.compile(r"([0-9]{6})_([a-z0-9_]+)\.(up|down)\.sql")


def git(*args):
    return subprocess.check_output(["git", *args])


def payload(sha):
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or git("cat-file", "-t", sha) != b"commit\n":
        raise ValueError("expected a full Git commit SHA")
    files = {}
    versions = {}
    rows = git("ls-tree", "-rz", sha, "--", *FILES, MIGRATIONS).split(b"\0")
    for row in filter(None, rows):
        metadata, path = row.decode("utf-8").split("\t", 1)
        mode, kind, blob = metadata.split(" ")
        if path in FILES:
            expected = FILES[path]
        elif path.startswith(MIGRATIONS + "/"):
            match = MIGRATION.fullmatch(path[len(MIGRATIONS) + 1 :])
            if not match:
                raise ValueError(f"unexpected migration: {path}")
            version, name, direction = match.groups()
            pair = versions.setdefault(version, {})
            if direction in pair:
                raise ValueError(f"duplicate migration direction: {path}")
            pair[direction] = name
            expected = "100644"
        else:
            raise ValueError(f"unexpected release file: {path}")
        if kind != "blob" or mode != expected:
            raise ValueError(f"wrong file type or mode: {path} ({kind} {mode}, expected {expected})")
        files[path] = (int(mode[-3:], 8), git("cat-file", "blob", blob))
    if missing := FILES.keys() - files.keys():
        raise ValueError(f"missing release files: {', '.join(sorted(missing))}")
    if not versions or any("up" not in p or p["up"] != p.get("down") for p in versions.values()):
        raise ValueError("missing or unpaired migrations")
    return files


def pack(path, files):
    with open(path, "xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(mode="w", fileobj=compressed, format=tarfile.USTAR_FORMAT) as archive:
                for name, (mode, data) in sorted(files.items()):
                    member = tarfile.TarInfo(name)
                    member.mode = mode
                    member.size = len(data)
                    archive.addfile(member, io.BytesIO(data))


def verify(path, files):
    remaining = dict(files)
    with tarfile.open(path, mode="r:gz") as archive:
        for member in archive:
            if member.name not in remaining or member.type != tarfile.REGTYPE:
                raise ValueError(f"unexpected archive member: {member.name}")
            mode, data = remaining.pop(member.name)
            if member.mode != mode or member.size != len(data):
                raise ValueError(f"archive mode or size mismatch: {member.name}")
            with archive.extractfile(member) as f:
                if f.read() != data:
                    raise ValueError(f"archive content mismatch: {member.name}")
    if remaining:
        raise ValueError(f"missing archive members: {', '.join(sorted(remaining))}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["pack", "verify"])
    parser.add_argument("sha")
    parser.add_argument("archive")
    args = parser.parse_args()
    try:
        files = payload(args.sha)
        (pack if args.action == "pack" else verify)(args.archive, files)
    except (OSError, ValueError, tarfile.TarError, subprocess.CalledProcessError) as err:
        parser.exit(1, f"release bundle: {err}\n")


if __name__ == "__main__":
    main()
