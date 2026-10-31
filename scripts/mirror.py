#!/usr/bin/env python3
"""Populate a mirror of every pack the catalogue names, and point the catalogue at it.

The catalogue pins a sha256 for every download and the action tries mirrors in
order, so a mirror is only ever a faster, more reliable route to bytes that are
already pinned. It cannot substitute different content: a mirror that serves
something else fails verification and the action falls through to the next one.

That property is what makes self-mirroring safe to automate. This script:

  fetch    download every pack from its upstream, verify the pinned sha256, and
           lay the files out under ./mirror/ in the URL shape the mirror serves
  verify   re-hash everything already in ./mirror/ against the catalogue
  adopt    insert the mirror as the FIRST mirror of every entry, keeping the
           upstream URLs after it as fallbacks

Layout is content-addressed by the first 16 hex of the sha256, so re-running
after a catalogue change only fetches what actually changed, and two entries that
share an archive share one file:

  mirror/packs/<sha16>/<original-filename>

Usage:
  python3 scripts/mirror.py fetch
  python3 scripts/mirror.py adopt --base https://files.example.com/setup-msvc
  rsync -av mirror/ user@host:/var/www/setup-msvc/
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

import setup_msvc as m  # noqa: E402


def entries(cat: dict):
    """Every (label, mirror-dict) pair in the catalogue, toolchains and runners."""
    for build, e in cat["toolchains"].items():
        for mi in e.get("mirrors", []):
            yield f"{build} ({e['name']})", e, mi
    for rname, r in cat.get("runners", {}).items():
        for plat, mi in r.get("mirrors", {}).items():
            yield f"runner {rname} / {plat}", r, mi


def local_path(mirror_root: Path, mi: dict) -> Path:
    name = Path(urlparse(mi["url"]).path).name or "download"
    return mirror_root / "packs" / mi["sha256"][:16] / name


def rel_url(mi: dict) -> str:
    name = Path(urlparse(mi["url"]).path).name or "download"
    return f"packs/{mi['sha256'][:16]}/{name}"


def cmd_fetch(cat: dict, mirror_root: Path, only: str | None) -> int:
    bad = 0
    seen: set[str] = set()
    for label, _parent, mi in entries(cat):
        if only and only not in label:
            continue
        if mi["sha256"] in seen:
            continue
        seen.add(mi["sha256"])
        dest = local_path(mirror_root, mi)
        if dest.is_file() and m.sha256_file(dest) == mi["sha256"]:
            print(f"  have  {label}: {rel_url(mi)}")
            continue
        dest.parent.mkdir(parents=True, exist_ok=True)
        tmp = dest.with_suffix(dest.suffix + ".part")
        print(f"  fetch {label}\n        {mi['url']}")
        try:
            m.download(mi["url"], tmp)
        except Exception as exc:
            print(f"        FAILED: {exc}")
            tmp.unlink(missing_ok=True)
            bad += 1
            continue
        got = m.sha256_file(tmp)
        if got != mi["sha256"]:
            print(f"        HASH MISMATCH: got {got}, pinned {mi['sha256']}")
            print("        Upstream changed or was replaced. NOT mirroring this.")
            tmp.unlink(missing_ok=True)
            bad += 1
            continue
        tmp.replace(dest)
        size = dest.stat().st_size
        print(f"        ok, {size / 1e6:.1f} MB -> {rel_url(mi)}")
    print(f"\n{len(seen) - bad}/{len(seen)} archives mirrored"
          + ("" if not bad else f", {bad} FAILED"))
    return 1 if bad else 0


def cmd_verify(cat: dict, mirror_root: Path) -> int:
    missing = wrong = ok = 0
    seen: set[str] = set()
    for label, _parent, mi in entries(cat):
        if mi["sha256"] in seen:
            continue
        seen.add(mi["sha256"])
        dest = local_path(mirror_root, mi)
        if not dest.is_file():
            print(f"  MISSING  {label}: {rel_url(mi)}")
            missing += 1
        elif m.sha256_file(dest) != mi["sha256"]:
            print(f"  CORRUPT  {label}: {rel_url(mi)}")
            wrong += 1
        else:
            ok += 1
    print(f"\n{ok} ok, {missing} missing, {wrong} corrupt")
    return 1 if (missing or wrong) else 0


def cmd_adopt(cat_path: Path, cat: dict, base: str) -> int:
    """Insert the mirror first, keeping upstream as fallback. Idempotent."""
    base = base.rstrip("/")
    added = 0

    def adopt_into(mirrors: list[dict]) -> list[dict]:
        nonlocal added
        upstream = [mi for mi in mirrors if not mi["url"].startswith(base + "/")]
        if not upstream:
            return mirrors
        first = upstream[0]
        ours = {"url": f"{base}/{rel_url(first)}", "sha256": first["sha256"]}
        for key in ("strip_prefix", "download_bytes"):
            if key in first:
                ours[key] = first[key]
        ours["$note"] = "Mirror. Same sha256 as upstream, which follows as fallback."
        added += 1
        return [ours] + upstream

    for e in cat["toolchains"].values():
        if e.get("mirrors"):
            e["mirrors"] = adopt_into(e["mirrors"])
    for r in cat.get("runners", {}).values():
        for plat, mi in list(r.get("mirrors", {}).items()):
            if not mi["url"].startswith(base + "/"):
                r["mirrors"][plat] = {
                    "url": f"{base}/{rel_url(mi)}",
                    "sha256": mi["sha256"],
                    "fallback": mi["url"],
                }
                added += 1

    cat_path.write_text(json.dumps(cat, indent=2) + "\n", encoding="utf-8")
    print(f"catalogue updated: {added} entries now try {base} first")
    print("upstream URLs kept as fallbacks, so an unpopulated mirror is harmless")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["fetch", "verify", "adopt"])
    ap.add_argument("--mirror", default=str(ROOT / "mirror"),
                    help="local directory to build the mirror in")
    ap.add_argument("--base", help="adopt: public base URL the mirror is served at")
    ap.add_argument("--only", help="fetch: substring filter on the entry label")
    ap.add_argument("--catalogue", default=str(ROOT / "catalogue.json"))
    args = ap.parse_args()

    cat_path = Path(args.catalogue)
    cat = json.loads(cat_path.read_text(encoding="utf-8"))
    mirror_root = Path(args.mirror)

    if args.command == "fetch":
        return cmd_fetch(cat, mirror_root, args.only)
    if args.command == "verify":
        return cmd_verify(cat, mirror_root)
    if not args.base:
        ap.error("adopt needs --base")
    return cmd_adopt(cat_path, cat, args.base)


if __name__ == "__main__":
    sys.exit(main())
