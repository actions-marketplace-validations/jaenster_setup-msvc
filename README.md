# setup-msvc

Set up a historical Microsoft Visual C++ toolchain (6.0 through 2010) in CI,
keyed on the **exact `cl.exe` build**, and verify the compiler really is the one
you asked for.

For byte-matching decompilation and recompilation: the work where the compiler's
identity is the question, not a detail.

```yaml
- uses: jaenster/setup-msvc@v1
  with:
    version: '14.00.50727.762'   # or '8.0sp1'
- run: cl.exe /c /O2 /MT /Z7 mysource.cpp
```

## Why the exact build number

Visual C++ 2005 RTM and SP1 both stamp `50727` in the PE Rich header. A binary
cannot tell you which one built it, and they emit different code. Only the fourth
component separates them:

| | |
|-|-|
| `14.00.50727.42` | 2005 RTM |
| `14.00.50727.762` | 2005 SP1 |

So this action takes the full build as the key, and after setup it runs `cl.exe`
and **fails** if the banner disagrees with the catalogue. A pack that claims to be
SP1 and answers `.42` fails at setup rather than hours later in a diff nobody can
explain. Friendly aliases (`8.0sp1`, `2005sp1`, `vc8`) are accepted, but prefer
the build number when it matters.

## What is available

```
12.00.8168       [C--]  Visual C++ 6.0            6.0, 98, vc6
13.00.9466       [C--]  Visual C++ .NET 2002      7.0, 2002, vc70
13.10.3077       [C--]  Visual C++ .NET 2003      7.1, 2003, vc71
14.00.50727.42   [CLS]  Visual C++ 2005 RTM       8.0, 2005, vc8
14.00.50727.762  [C--]  Visual C++ 2005 SP1       8.0sp1, 2005sp1, vc8sp1
15.00.21022.08   [C--]  Visual C++ 2008 RTM       9.0, 2008, vc9
15.00.30729.01   [C--]  Visual C++ 2008 SP1       9.0sp1, 2008sp1
16.00.30319.01   [C--]  Visual C++ 2010 RTM       10.0, 2010, vc10
16.00.40219.01   [C--]  Visual C++ 2010 SP1       10.0sp1, 2010sp1

[C]ompiler   [L]ibc   platform [S]DK libs
```

`setup-msvc --list` prints this. The flags are **measured, not assumed**: these
community packs differ wildly in completeness, and a missing `lib/` is not
discoverable until a link fails confusingly.

### Packs are composed, because none is complete

The 2005 SP1 pack has the exact compiler the byte-matching work needs and **no
libraries at all** (its 42 `.lib` files are CRT-source build inputs, not
`libcmt.lib`). The RTM pack has every library and the wrong compiler. So the
catalogue has a `libs_from` edge, and asking for SP1 fetches both and overlays
them, with the donor's `bin` last so its `cl.exe` never shadows the one you asked
for.

`outputs.can-link` tells you whether the result can link or is compile-only.

## Where to run it

| runner | compile | link |
|-|-|-|
| `windows-latest`, `runner: native` | yes | **yes** |
| `ubuntu`/`macos`, `runner: wibo` | yes | no |

`native` runs `cl.exe` and `link.exe` directly under WOW64. This is the only way
to link today and the recommended path.

`wibo` ([decompals/wibo](https://github.com/decompals/wibo)) runs the 32-bit
compiler on Linux and macOS with no wine, no prefix and no drive letters. It
genuinely compiles, including `windows.h` and the STL, and `ml.exe` works. But
`link.exe` on wibo 1.2.0 aborts on a missing `kernel32` import, `lstrcmpiW`:

> `wibo: call reached missing import lstrcmpiW from kernel32`

Also missing: `GetTempPathW`, `CopyFileW`, `ExpandEnvironmentStringsW`,
`FormatMessageW`. The first two are implemented in wibo `main` but not in 1.2.0,
so this is a small upstream fix away from working.

## Known gaps

- **No DirectX.** Neither 2005 pack ships `ddraw.h`, `d3d9.h` or `dsound.h`, or
  their import libs. A tree that imports them will not link.
- **No SP1 CRT libraries anywhere.** `libcmt.lib` and `libcpmt.lib` as shipped by
  SP1 are in neither pack. The RTM ones will link, but will not byte-match a
  binary that linked SP1's. The only source is the SP1 patch installer.
- **VC6 service packs are not yet separate entries.** SP5 and SP6 change codegen,
  so they need their own entries once their mirrors are pinned; `archaic-msvc`
  hosts `msvc600`, `msvc600_sp5`, `msvc600_sp5_vcpp` and `msvc600_sp6`.

## This action redistributes nothing

Every proprietary toolchain is fetched from a third-party community archive and
checked against a pinned SHA-256. Nothing is hosted here. `mirrors` is a list
because any one of those archives can disappear; the action tries each in turn and
verifies the same hash whichever answers. You are responsible for being licensed
to use what you fetch.

## Credits

The pack URLs and hashes were assembled from the decomp community's own work,
particularly [decompdecomp](https://github.com/n-delic/decompdecomp)'s toolchain
manifest, and the packs themselves are hosted by
[widberg](https://github.com/widberg/msvc8.0),
[archaic-msvc](https://github.com/archaic-msvc),
[Drahsid](https://github.com/Drahsid/ffxiv_msvc8.0sp1),
[OmniBlade](https://github.com/OmniBlade), [roblabla](https://github.com/roblabla)
and [Marvisak](https://github.com/Marvisak). `wibo` is
[decompals/wibo](https://github.com/decompals/wibo).
