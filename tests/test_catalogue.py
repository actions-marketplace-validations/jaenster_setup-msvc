#!/usr/bin/env python3
"""Checks on the catalogue that need no network.

These exist to make the eventual Zig port safe: they pin the behaviour the
reference implementation has, so a rewrite has something to be correct against.
Nothing here downloads anything.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

import setup_msvc as m  # noqa: E402

BUILD_RE = re.compile(r"^\d+\.\d+\.\d+(\.\d+)?$")
SHA_RE = re.compile(r"^[0-9a-f]{64}$")

failures: list[str] = []


def check(cond: bool, msg: str) -> None:
    if not cond:
        failures.append(msg)


def main() -> int:
    cat = json.loads((ROOT / "catalogue.json").read_text(encoding="utf-8"))
    check(cat.get("schema") == 1, "schema must be 1")

    tools = cat["toolchains"]
    check(bool(tools), "catalogue has no toolchains")

    seen_aliases: dict[str, str] = {}
    for build, e in tools.items():
        where = f"toolchains[{build}]"

        check(bool(BUILD_RE.match(build)),
              f"{where}: key must be a dotted build number, got {build!r}")
        check("name" in e, f"{where}: missing name")
        check(e.get("license") in ("proprietary", "free", "open-source"),
              f"{where}: license must be stated explicitly, got {e.get('license')!r}")

        # Aliases must be unique across the whole catalogue, or resolution is
        # ambiguous and silently picks whichever came first.
        for a in e.get("aliases", []):
            low = a.lower()
            check(low not in seen_aliases,
                  f"{where}: alias {a!r} already used by {seen_aliases.get(low)}")
            seen_aliases[low] = build
            check(low != build.lower(),
                  f"{where}: alias {a!r} duplicates its own build key")

        mirrors = e.get("mirrors") or []
        check(bool(mirrors), f"{where}: needs at least one mirror")
        for i, mi in enumerate(mirrors):
            at = f"{where}.mirrors[{i}]"
            check(mi.get("url", "").startswith("https://"), f"{at}: url must be https")
            check(bool(SHA_RE.match(mi.get("sha256", ""))),
                  f"{at}: sha256 must be 64 lowercase hex chars")

        # A banner, when present, must agree with the key it is filed under.
        banner = e.get("banner")
        if banner:
            got = m.BANNER_RE.search(banner)
            check(got is not None, f"{where}: banner is unparseable: {banner!r}")
            if got:
                check(got.group(1) == build,
                      f"{where}: banner says {got.group(1)}, key says {build}")

        # libs_from must point at something real, and only where it is needed.
        donor = e.get("libs_from")
        if donor:
            check(donor in tools, f"{where}: libs_from {donor!r} is not in the catalogue")
            check(not e.get("contains", {}).get("crt_libs", False),
                  f"{where}: has libs_from but also claims crt_libs")
            if donor in tools:
                dc = tools[donor].get("contains", {})
                check(dc.get("crt_libs", False) or dc.get("platform_sdk_libs", False),
                      f"{where}: donor {donor} has no libraries to lend")

        check("layout" in e and "bin" in e["layout"],
              f"{where}: layout must name at least bin")

    # Every alias and every build key must resolve, and to itself.
    for build in tools:
        got, _ = m.resolve(cat, build)
        check(got == build, f"resolve({build!r}) returned {got!r}")
    for alias, build in seen_aliases.items():
        got, _ = m.resolve(cat, alias)
        check(got == build, f"resolve({alias!r}) returned {got!r}, expected {build!r}")

    # The runner entry the action depends on.
    wibo = cat.get("runners", {}).get("wibo")
    check(wibo is not None, "runners.wibo missing")
    if wibo:
        for plat, mi in wibo.get("mirrors", {}).items():
            check(bool(SHA_RE.match(mi.get("sha256", ""))),
                  f"runners.wibo.mirrors[{plat}]: bad sha256")
        check(bool(wibo.get("known_gaps")),
              "runners.wibo: known_gaps must be documented, not discovered by users")

    # The 2005 pair is the reason this project exists; assert it stays correct.
    check("14.00.50727.762" in tools and "14.00.50727.42" in tools,
          "both 2005 RTM and SP1 must be present and distinct")
    sp1 = tools.get("14.00.50727.762", {})
    check(sp1.get("libs_from") == "14.00.50727.42",
          "2005 SP1 must borrow libraries from RTM: it ships none")

    if failures:
        print(f"FAILED ({len(failures)}):")
        for f in failures:
            print("  -", f)
        return 1
    print(f"ok: {len(tools)} toolchains, {len(seen_aliases)} aliases, all resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
