#!/usr/bin/env python3
"""Set up a historical MSVC toolchain and prove it is the one that was asked for.

Resolve a version (an exact cl.exe build, or a friendly alias) against
catalogue.json, fetch each pack it needs from the first mirror that answers,
verify every download against its pinned sha256, compose them into one tree, and
export INCLUDE / LIB / PATH for it.

Two things make this different from a download script:

  COMPOSITION. No single community pack is a complete toolchain. The 2005 SP1
  pack has the exact compiler the byte-matching work needs and no libraries at
  all; the RTM pack has every library and the wrong compiler. A `libs_from` entry
  in the catalogue says "take bin and include from me, lib from there", and this
  script overlays them so link.exe has something to resolve against.

  VERIFICATION. For byte-matching, compiler identity IS the question: 2005 RTM
  and SP1 both stamp 50727 in the Rich header and emit different code. So after
  setup this runs cl.exe and checks the banner against the catalogue. A pack that
  claims to be SP1 and answers .42 fails here rather than six hours later in a
  diff nobody can explain.

This redistributes nothing. It points at upstream community archives and checks
what arrives.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

CHUNK = 1 << 20


def log(msg: str) -> None:
    print(msg, flush=True)


def fail(msg: str) -> "None":
    print(f"::error::{msg}" if os.environ.get("GITHUB_ACTIONS") else f"error: {msg}",
          file=sys.stderr, flush=True)
    sys.exit(1)


# --------------------------------------------------------------------- catalogue

def load_catalogue(path: Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        fail(f"cannot read catalogue {path}: {exc}")


def resolve(cat: dict, wanted: str) -> tuple[str, dict]:
    """Accept an exact build ('14.00.50727.762') or an alias ('8.0sp1')."""
    tools = cat["toolchains"]
    key = wanted.strip()
    if key in tools:
        return key, tools[key]
    low = key.lower()
    for build, entry in tools.items():
        if low in (a.lower() for a in entry.get("aliases", [])):
            return build, entry
    known = []
    for build, entry in tools.items():
        known.append(f"  {build:<18} {', '.join(entry.get('aliases', []))}")
    fail("unknown version %r. Known:\n%s" % (wanted, "\n".join(sorted(known))))


# ---------------------------------------------------------------------- fetching

def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for block in iter(lambda: fh.read(CHUNK), b""):
            h.update(block)
    return h.hexdigest()


def download(url: str, dest: Path) -> None:
    req = urllib.request.Request(url, headers={"User-Agent": "setup-msvc"})
    with urllib.request.urlopen(req, timeout=120) as resp, dest.open("wb") as out:
        shutil.copyfileobj(resp, out, CHUNK)


def fetch_verified(mirrors: list[dict], cache: Path, label: str) -> tuple[Path, dict]:
    """First mirror that answers AND matches its pinned hash wins."""
    problems = []
    for mirror in mirrors:
        url, want = mirror["url"], mirror["sha256"]
        cached = cache / f"{want[:16]}-{Path(url).name}"
        if cached.is_file() and sha256_file(cached) == want:
            log(f"  {label}: cache hit ({cached.name})")
            return cached, mirror
        cache.mkdir(parents=True, exist_ok=True)
        tmp = cached.with_suffix(cached.suffix + ".part")
        try:
            log(f"  {label}: fetching {url}")
            download(url, tmp)
        except (urllib.error.URLError, OSError, TimeoutError) as exc:
            problems.append(f"{url}: {exc}")
            tmp.unlink(missing_ok=True)
            continue
        got = sha256_file(tmp)
        if got != want:
            problems.append(f"{url}: sha256 {got}, expected {want}")
            tmp.unlink(missing_ok=True)
            continue
        tmp.replace(cached)
        log(f"  {label}: verified sha256 {want[:16]}...")
        return cached, mirror
    fail(f"no usable mirror for {label}:\n  " + "\n  ".join(problems))


def extract(archive: Path, into: Path, strip_prefix: str | None) -> Path:
    into.mkdir(parents=True, exist_ok=True)
    name = archive.name.lower()
    if name.endswith((".tar.gz", ".tgz", ".tar")):
        mode = "r:gz" if name.endswith((".tar.gz", ".tgz")) else "r:"
        with tarfile.open(archive, mode) as tf:
            members = tf.getmembers()
            _reject_escapes(m.name for m in members)
            # Python 3.12+ wants an explicit filter; older ones do not accept it.
            try:
                tf.extractall(into, members=members, filter="data")
            except TypeError:
                tf.extractall(into, members=members)
    elif name.endswith(".zip"):
        with zipfile.ZipFile(archive) as zf:
            _reject_escapes(zf.namelist())
            zf.extractall(into)
    else:
        fail(f"don't know how to extract {archive.name}")
    return (into / strip_prefix) if strip_prefix else into


def _reject_escapes(names) -> None:
    for n in names:
        p = Path(n)
        if p.is_absolute() or ".." in p.parts:
            fail(f"archive member escapes its directory: {n}")


# ---------------------------------------------------------------- pack assembly

def pack_root(cat: dict, build: str, cache: Path, work: Path) -> tuple[Path, dict]:
    entry = cat["toolchains"][build]
    archive, mirror = fetch_verified(entry["mirrors"], cache, entry["name"])
    dest = work / build.replace(".", "_")
    if not dest.exists():
        root = extract(archive, dest.with_suffix(".x"), mirror.get("strip_prefix"))
        dest.parent.mkdir(parents=True, exist_ok=True)
        root.rename(dest)
        shutil.rmtree(dest.with_suffix(".x"), ignore_errors=True)
    return dest, entry


def compose(cat: dict, build: str, cache: Path, work: Path) -> dict:
    """Build the search paths, overlaying a donor pack's libraries if needed."""
    root, entry = pack_root(cat, build, cache, work)
    lay = entry.get("layout", {})

    def part(key: str) -> Path | None:
        rel = lay.get(key)
        if not rel:
            return None
        p = root / rel
        return p if p.exists() else None

    bins = [p for p in (part("bin"),) if p]
    incs = [p for p in (part("include"), part("platform_sdk_include")) if p]
    libs = [p for p in (part("lib"), part("platform_sdk_lib")) if p]

    donor_build = entry.get("libs_from")
    donor = None
    if donor_build and not libs:
        log(f"composing: {entry['name']} ships no libraries, taking them from "
            f"{cat['toolchains'][donor_build]['name']}")
        droot, dentry = pack_root(cat, donor_build, cache, work)
        dlay = dentry.get("layout", {})
        for key in ("lib", "platform_sdk_lib"):
            rel = dlay.get(key)
            if rel and (droot / rel).exists():
                libs.append(droot / rel)
        # The donor also supplies the tools the compiler pack omits (lib.exe,
        # dumpbin.exe, undname.exe). Its bin goes LAST so its cl.exe never
        # shadows the one that was actually asked for.
        if dlay.get("bin") and (droot / dlay["bin"]).exists():
            bins.append(droot / dlay["bin"])
        donor = donor_build

    if not libs:
        log("::warning::no libraries in this toolchain; compilation will work but "
            "linking will not. See the catalogue's 'contains' for what is missing.")

    return {"root": root, "entry": entry, "build": build, "donor": donor,
            "bin": bins, "include": incs, "lib": libs}


# -------------------------------------------------------------------- the runner

def install_wibo(cat: dict, cache: Path, work: Path) -> Path:
    rn = cat["runners"]["wibo"]
    system = platform.system()
    key = "macos" if system == "Darwin" else "linux-x86_64"
    if key not in rn["mirrors"]:
        fail(f"no wibo build for {system}")
    archive, _ = fetch_verified([rn["mirrors"][key]], cache, f"wibo {rn['version']}")
    dest = work / "bin" / "wibo"
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(archive, dest)
    dest.chmod(0o755)
    if system == "Darwin":
        subprocess.run(["xattr", "-d", "com.apple.quarantine", str(dest)],
                       capture_output=True, check=False)
    return dest


def win_path(runner: str, wibo: Path | None, p: Path) -> str:
    """Host path -> the path form the Windows tool will understand."""
    if runner == "native":
        return str(p)
    out = subprocess.run([str(wibo), "path", "-w", str(p)],
                          capture_output=True, text=True, check=False)
    if out.returncode == 0 and out.stdout.strip():
        return out.stdout.strip()
    # wibo maps Z: to the host root, so this is the same answer without the call.
    return "Z:" + str(p).replace("/", "\\")


# ------------------------------------------------------------------ verification

BANNER_RE = re.compile(r"Version\s+(\d+\.\d+\.\d+(?:\.\d+)?)\s+for\s+(\S+)")


def verify_compiler(tc: dict, runner: str, wibo: Path | None, env: dict) -> str:
    cl = None
    for b in tc["bin"]:
        cand = b / "cl.exe"
        if cand.is_file():
            cl = cand
            break
    if cl is None:
        fail("no cl.exe found in the composed toolchain")

    cmd = ([str(cl)] if runner == "native" else [str(wibo), str(cl)])
    out = subprocess.run(cmd, capture_output=True, text=True, env=env, check=False)
    blob = (out.stdout or "") + (out.stderr or "")
    m = BANNER_RE.search(blob)
    if not m:
        fail("cl.exe did not print a recognisable banner. Output was:\n" + blob.strip()[:800])
    got = m.group(1)
    log(f"  cl.exe reports: {m.group(0)}")

    expected_banner = tc["entry"].get("banner")
    if expected_banner:
        want = BANNER_RE.search(expected_banner)
        want_ver = want.group(1) if want else None
        if want_ver and got != want_ver:
            fail(f"wrong compiler. Asked for {tc['build']} ({tc['entry']['name']}), "
                 f"but cl.exe is {got}. For byte-matching these are different "
                 f"compilers, so this is a hard failure, not a warning.")
    elif got != tc["build"]:
        log(f"::warning::catalogue has no pinned banner for {tc['build']}; "
            f"cl.exe reports {got}. Consider pinning it.")
    return got


# -------------------------------------------------------------------------- main

def export(name: str, value: str) -> None:
    log(f"  {name}={value}")
    gh_env = os.environ.get("GITHUB_ENV")
    if gh_env:
        with open(gh_env, "a", encoding="utf-8") as fh:
            fh.write(f"{name}={value}\n")


def export_output(name: str, value: str) -> None:
    gh_out = os.environ.get("GITHUB_OUTPUT")
    if gh_out:
        with open(gh_out, "a", encoding="utf-8") as fh:
            fh.write(f"{name}={value}\n")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--version",
                    help="exact cl.exe build (14.00.50727.762) or alias (8.0sp1)")
    ap.add_argument("--runner", default="auto", choices=["auto", "native", "wibo"],
                    help="auto (default): native on Windows, wibo elsewhere. "
                         "native runs cl.exe directly under WOW64 and is the only "
                         "way to link today, since wibo 1.2.0's link.exe aborts on "
                         "a missing kernel32 import.")
    ap.add_argument("--dest", default=None, help="where to install (default: a cache dir)")
    ap.add_argument("--cache", default=None, help="download cache directory")
    ap.add_argument("--catalogue", default=None)
    ap.add_argument("--list", action="store_true", help="print the catalogue and exit")
    args = ap.parse_args()

    here = Path(__file__).resolve().parent.parent
    cat = load_catalogue(Path(args.catalogue) if args.catalogue else here / "catalogue.json")

    if args.list:
        for build, e in sorted(cat["toolchains"].items()):
            c = e.get("contains", {})
            flags = "".join([
                "C" if c.get("cl") else "-",
                "L" if c.get("crt_libs") else "-",
                "S" if c.get("platform_sdk_libs") else "-",
            ])
            log(f"{build:<18} [{flags}] {e['name']:<28} {', '.join(e.get('aliases', []))}")
        log("\n[C]ompiler  [L]ibc  platform [S]DK libs")
        return 0

    if not args.version:
        ap.error("--version is required (or use --list)")

    home = Path(os.environ.get("RUNNER_TOOL_CACHE") or Path.home() / ".cache")
    cache = Path(args.cache) if args.cache else home / "setup-msvc" / "downloads"
    work = Path(args.dest) if args.dest else home / "setup-msvc" / "toolchains"

    if args.runner == "auto":
        args.runner = "native" if platform.system() == "Windows" else "wibo"
        log(f"runner: {args.runner} (auto-detected on {platform.system()})")

    log(f"setup-msvc: resolving {args.version!r}")
    build, entry = resolve(cat, args.version)
    log(f"  -> {build}  {entry['name']}")
    if entry.get("license") == "proprietary":
        log("::notice::This is a proprietary Microsoft toolchain fetched from a "
            "community archive. You are responsible for being licensed to use it. "
            "This action hosts nothing.")
    if args.runner == "wibo" and entry.get("runs_under_wibo") is False:
        fail(f"{entry['name']} is known not to run under wibo; use runner: native")

    tc = compose(cat, build, cache, work)

    wibo = install_wibo(cat, cache, work) if args.runner == "wibo" else None

    sep = ";"
    inc = sep.join(win_path(args.runner, wibo, p) for p in tc["include"])
    lib = sep.join(win_path(args.runner, wibo, p) for p in tc["lib"])

    env = dict(os.environ)
    env["INCLUDE"] = inc
    env["LIB"] = lib
    # wibo 1.2.0 dies on a PATH entry it cannot stat (a root-only /usr/local/bin
    # is enough), so keep PATH to the toolchain plus the basics.
    env["PATH"] = os.pathsep.join([str(b) for b in tc["bin"]] + ["/usr/bin", "/bin"])

    log("verifying the compiler is the one that was asked for:")
    got = verify_compiler(tc, args.runner, wibo, env)

    log("exporting:")
    export("INCLUDE", inc)
    export("LIB", lib)
    export("MSVC_ROOT", str(tc["root"]))
    export("MSVC_BIN", str(tc["bin"][0]))
    export("MSVC_VERSION", got)
    if wibo:
        export("WIBO", str(wibo))
    gh_path = os.environ.get("GITHUB_PATH")
    if gh_path:
        with open(gh_path, "a", encoding="utf-8") as fh:
            for b in tc["bin"]:
                fh.write(str(b) + "\n")
            if wibo:
                fh.write(str(wibo.parent) + "\n")

    export_output("version", got)
    export_output("root", str(tc["root"]))
    export_output("bin", str(tc["bin"][0]))
    export_output("can-link", "true" if tc["lib"] else "false")

    c = tc["entry"].get("contains", {})
    if tc["donor"]:
        log(f"\nlibraries composed from {tc['donor']}")
    if not c.get("directx_headers", False):
        log("::warning::no DirectX headers in this toolchain. A tree that imports "
            "ddraw/d3d9/dsound will not link without them.")
    log(f"\nready: {tc['entry']['name']} ({got}), "
        f"linking {'available' if tc['lib'] else 'NOT available'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
