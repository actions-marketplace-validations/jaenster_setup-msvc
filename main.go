// setup-msvc sets up a historical Microsoft Visual C++ toolchain, keyed on the
// exact cl.exe build, and verifies after install that the compiler really is the
// one that was asked for.
//
// It is one static binary on purpose. It runs inside other people's CI, where a
// language runtime is one more thing to install, to cache and to have break.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type Options struct {
	Version         string
	Components      string
	Extras          string
	ExtrasCatalogue string
	Runner          string
	CacheDir        string
	InstallDir      string
	Catalogue       string
}

func logf(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
}

func die(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if os.Getenv("GITHUB_ACTIONS") != "" {
		fmt.Fprintf(os.Stderr, "::error::%s\n", strings.ReplaceAll(msg, "\n", "%0A"))
	} else {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `setup-msvc - historical MSVC toolchains for CI, pinned and verified

  setup-msvc setup   --version <build|alias> [flags]   install a toolchain
  setup-msvc list    [--catalogue PATH]                show what is available
  setup-msvc verify  --version <build|alias>           check mirrors without installing
  setup-msvc repack  --build <build> --src <dir>       split a pack into components (maintainer)

Run a subcommand with -h for its flags.
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "setup":
		cmdSetup(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "repack":
		cmdRepack(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		die("unknown subcommand %q", os.Args[1])
	}
}

// defaultDirs puts things where CI already knows to cache them, while staying
// overridable so a self-hosted runner can point at persistent disk. That matters
// more than it sounds: GitHub Actions caches are scoped per repository, so a
// self-hosted runner with a shared directory is the only real cross-repository
// cache that exists.
func defaultDirs() (cache, install string) {
	base := os.Getenv("RUNNER_TOOL_CACHE")
	if base == "" {
		if h, err := os.UserCacheDir(); err == nil {
			base = h
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "setup-msvc", "downloads"),
		filepath.Join(base, "setup-msvc", "toolchains")
}

func defaultCataloguePath() string {
	if exe, err := os.Executable(); err == nil {
		c := filepath.Join(filepath.Dir(exe), "catalogue.json")
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "catalogue.json"
}

func addCommonFlags(fs *flag.FlagSet, o *Options) {
	dc, di := defaultDirs()
	fs.StringVar(&o.Version, "version", "", "exact cl.exe build (14.00.50727.762) or alias (8.0sp1)")
	fs.StringVar(&o.Components, "components", "",
		"comma-separated components to install; empty installs all.\n"+
			"Names come from the toolchain's layout, e.g. bin,include,platform_sdk_include,lib,platform_sdk_lib")
	fs.StringVar(&o.Extras, "extras", "", "comma-separated artifacts to install alongside the toolchain")
	fs.StringVar(&o.ExtrasCatalogue, "extras-catalogue", "",
		"path to a catalogue file defining extras, usually in your own repository")
	fs.StringVar(&o.Runner, "runner", "auto", "auto | native | wibo")
	fs.StringVar(&o.CacheDir, "cache-dir", dc, "where downloads are cached")
	fs.StringVar(&o.InstallDir, "install-dir", di, "where toolchains are installed")
	fs.StringVar(&o.Catalogue, "catalogue", defaultCataloguePath(), "catalogue path")
}

func loadCatalogues(o Options) *Catalogue {
	c, err := LoadCatalogue(o.Catalogue)
	if err != nil {
		die("%v", err)
	}
	if o.ExtrasCatalogue != "" {
		extra, err := LoadCatalogue(o.ExtrasCatalogue)
		if err != nil {
			die("reading extras catalogue: %v", err)
		}
		// Only artifacts merge. Letting a third-party file redefine a toolchain
		// would make `version: 8.0sp1` mean different things in different
		// repositories, which is exactly the ambiguity this tool exists to remove.
		c.Merge(extra)
		logf("extras catalogue: %s (%d artifacts)", o.ExtrasCatalogue, len(extra.Artifacts))
	}
	return c
}

func cmdSetup(args []string) {
	var o Options
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	addCommonFlags(fs, &o)
	fs.Parse(args)
	if o.Version == "" {
		die("--version is required")
	}
	if o.Runner == "auto" {
		if runtime.GOOS == "windows" {
			o.Runner = "native"
		} else {
			o.Runner = "wibo"
		}
		logf("runner: %s (auto-detected on %s)", o.Runner, runtime.GOOS)
	}

	c := loadCatalogues(o)
	inst, err := Setup(c, o, logf)
	if err != nil {
		die("%v", err)
	}

	inc := strings.Join(winPaths(inst, inst.Include), ";")
	lib := strings.Join(winPaths(inst, inst.Lib), ";")

	logf("exporting:")
	export("INCLUDE", inc)
	export("LIB", lib)
	export("MSVC_ROOT", inst.Root)
	export("MSVC_BIN", inst.Bin[0])
	export("MSVC_VERSION", inst.Version)
	if inst.Wibo != "" {
		export("WIBO", inst.Wibo)
	}
	for name, dir := range inst.Extras {
		export("MSVC_EXTRA_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_")), dir)
	}
	addPath(inst.Bin...)
	if inst.Wibo != "" {
		addPath(filepath.Dir(inst.Wibo))
	}

	canLink := len(inst.Lib) > 0
	output("version", inst.Version)
	output("root", inst.Root)
	output("bin", inst.Bin[0])
	output("can-link", fmt.Sprintf("%t", canLink))

	// Say plainly what was left out and what that costs, rather than letting it
	// surface later as LNK1104 with nothing pointing back here.
	if len(inst.Skipped) > 0 {
		logf("\nnot installed, because --components did not ask for them: %s",
			strings.Join(inst.Skipped, ", "))
	}
	if !canLink {
		logf("::warning::No lib component is installed, so linking will fail. " +
			"Add lib (and platform_sdk_lib) to --components if you need to link.")
	}
	logf("\nready: %s (%s), linking %s",
		inst.Toolchain.Name, inst.Version,
		map[bool]string{true: "available", false: "NOT available"}[canLink])
}

func cmdList(args []string) {
	var o Options
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	addCommonFlags(fs, &o)
	fs.Parse(args)
	c := loadCatalogues(o)

	var builds []string
	for b := range c.Toolchains {
		builds = append(builds, b)
	}
	sort.Strings(builds)
	for _, b := range builds {
		t := c.Toolchains[b]
		flags := []byte("---")
		if truthy(t.Contains["cl"]) {
			flags[0] = 'C'
		}
		if truthy(t.Contains["crt_libs"]) {
			flags[1] = 'L'
		}
		if truthy(t.Contains["platform_sdk_libs"]) {
			flags[2] = 'S'
		}
		size := ""
		if len(t.Components) > 0 {
			var total int64
			for _, comp := range t.Components {
				total += comp.Bytes
			}
			size = fmt.Sprintf("%6.1f MB", float64(total)/1e6)
		}
		fmt.Printf("%-18s [%s] %9s  %-26s %s\n",
			b, flags, size, t.Name, strings.Join(t.Aliases, ", "))
	}
	fmt.Println("\n[C]ompiler  [L]ibc  platform [S]DK libs   size is all components")

	if len(c.Artifacts) > 0 {
		fmt.Println("\nartifacts:")
		var names []string
		for n := range c.Artifacts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("  %-26s %s\n", n, c.Artifacts[n].Summary)
		}
	}
}

func cmdVerify(args []string) {
	var o Options
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	addCommonFlags(fs, &o)
	fs.Parse(args)
	if o.Version == "" {
		die("--version is required")
	}
	c := loadCatalogues(o)
	build, tc, err := c.Resolve(o.Version)
	if err != nil {
		die("%v", err)
	}
	fmt.Printf("%s  %s\n", build, tc.Name)
	if len(tc.Components) == 0 {
		fmt.Println("  no repacked components; whole-pack download only")
	}
	for _, n := range tc.ComponentNames() {
		if comp, ok := tc.Components[n]; ok {
			fmt.Printf("  %-22s %7.1f MB  %5d files  %s\n",
				n, float64(comp.Bytes)/1e6, comp.Files, comp.SHA256[:16])
		} else {
			fmt.Printf("  %-22s (not repacked)\n", n)
		}
	}
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
