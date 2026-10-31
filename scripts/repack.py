#!/usr/bin/env python3
"""Split an upstream pack into per-component archives, reproducibly.

WHY. The upstream packs are whole Visual Studio installations: 160-177 MB of
IDE, docs, atlmfc and redist, of which the action keeps bin, include and lib.
GitHub Actions caches are scoped per repository, so there is no shared cache to
amortise that - every consumer pays the full transfer on every cold cache, and
on the very first run no cache helps at all. The only lever is to send less.

WHAT. Each component named in the catalogue's `layout` becomes its own archive,
named by the hash of its own contents:

    packs/<sha16>/<name>.tar.zst

Two consequences worth having:

  - A caller asking for `components: bin,include` fetches two archives and not
    a 160 MB installation.
  - Content addressing deduplicates across toolchains. The 2005 SP1 tree is an
    RTM tree with the SP1 cabinet laid over it, so most headers are identical
    between them: identical component, identical hash, same URL, one download
    and one cache entry serving both.

REPRODUCIBLE, because a repack is no longer the upstream's bytes and so cannot
carry the upstream's hash. Anyone must be able to re-derive it and get the same
file, or the chain of custody is just our word. Therefore: entries sorted by
path, mtime fixed at 0, uid/gid/uname/gname cleared, mode normalised to 0644 or
0755, no directory entries, gzip/zstd written without timestamps. The catalogue
records `derived_from` with the upstream URL and ITS hash, so the derivation is
checkable end to end.
"""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
EPOCH = 0


def norm(ti: tarfile.TarInfo) -> tarfile.TarInfo:
    """Strip everything that varies between machines and runs."""
    ti.mtime = EPOCH
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = ""
    if ti.isdir():
        ti.mode = 0o755
    elif ti.mode & 0o111:
        ti.mode = 0o755
    else:
        ti.mode = 0o644
    ti.pax_headers = {}
    return ti


def build_component(src: Path, out: Path, compressor: str) -> tuple[str, int, int]:
    """Tar `src` deterministically. Returns (sha256, bytes, file count)."""
    files = sorted(
        (p for p in src.rglob("*") if p.is_file() and not p.is_symlink()),
        key=lambda p: p.relative_to(src).as_posix(),
    )
    raw = io.BytesIO()
    # format=GNU_FORMAT and no pax headers keeps the output stable across
    # Python versions, which PAX does not.
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.GNU_FORMAT) as tf:
        for p in files:
            ti = tf.gettarinfo(str(p), arcname=p.relative_to(src).as_posix())
            ti = norm(ti)
            with p.open("rb") as fh:
                tf.addfile(ti, fh)
    data = raw.getvalue()

    if compressor == "zstd":
        if shutil.which("zstd") is None:
            print("  zstd not found, falling back to gzip", file=sys.stderr)
            compressor = "gzip"
    if compressor == "zstd":
        proc = subprocess.run(["zstd", "-19", "-T0", "-c", "-"], input=data,
                              capture_output=True, check=True)
        blob = proc.stdout
    else:
        import gzip
        # mtime=0 or gzip stamps the current time into the header.
        buf = io.BytesIO()
        with gzip.GzipFile(fileobj=buf, mode="wb", compresslevel=9, mtime=0) as gz:
            gz.write(data)
        blob = buf.getvalue()

    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_bytes(blob)
    return hashlib.sha256(blob).hexdigest(), len(blob), len(files)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--build", required=True, help="catalogue key, e.g. 14.00.50727.762")
    ap.add_argument("--src", required=True, help="extracted upstream pack root")
    ap.add_argument("--upstream-url", required=True)
    ap.add_argument("--upstream-sha256", required=True)
    ap.add_argument("--out", default=str(ROOT / "mirror"))
    ap.add_argument("--catalogue", default=str(ROOT / "catalogue.json"))
    ap.add_argument("--compressor", default="zstd", choices=["zstd", "gzip"])
    ap.add_argument("--write", action="store_true",
                    help="update the catalogue; without it, only report")
    args = ap.parse_args()

    cat = json.loads(Path(args.catalogue).read_text(encoding="utf-8"))
    entry = cat["toolchains"].get(args.build)
    if entry is None:
        print(f"error: {args.build} is not in the catalogue", file=sys.stderr)
        return 1

    src = Path(args.src)
    layout = entry.get("layout", {})
    ext = "tar.zst" if args.compressor == "zstd" else "tar.gz"
    components: dict[str, dict] = {}
    total = 0

    print(f"repacking {args.build} ({entry['name']}) from {src}")
    for name, rel in layout.items():
        d = src / rel
        if not d.is_dir():
            print(f"  {name:<22} ABSENT in this pack ({rel})")
            continue
        tmp = Path(args.out) / f".{name}.{ext}"
        sha, size, count = build_component(d, tmp, args.compressor)
        final = Path(args.out) / "packs" / sha[:16] / f"{args.build}-{name}.{ext}"
        final.parent.mkdir(parents=True, exist_ok=True)
        tmp.replace(final)
        rel_url = f"packs/{sha[:16]}/{final.name}"
        components[name] = {"path": rel_url, "sha256": sha, "bytes": size, "files": count}
        total += size
        print(f"  {name:<22} {count:>6} files  {size/1e6:>8.1f} MB  {sha[:16]}")

    print(f"  {'TOTAL':<22} {'':>6}        {total/1e6:>8.1f} MB")

    if not args.write:
        print("\n(report only; pass --write to update the catalogue)")
        return 0

    entry["components"] = components
    entry["component_format"] = ext
    entry["derived_from"] = {"url": args.upstream_url, "sha256": args.upstream_sha256}
    Path(args.catalogue).write_text(json.dumps(cat, indent=2) + "\n", encoding="utf-8")
    print(f"\ncatalogue updated: {args.build} now has {len(components)} components")
    return 0


if __name__ == "__main__":
    sys.exit(main())
