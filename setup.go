package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Installed is the result of setting a toolchain up: where everything landed
// and what the compiler said about itself when asked.
type Installed struct {
	Build     string
	Toolchain *Toolchain
	Root      string
	Bin       []string
	Include   []string
	Lib       []string
	Version   string
	Runner    string
	Wibo      string
	Extras    map[string]string
	Requested []string
	Skipped   []string
}

// selectComponents turns the --components flag into a list. Empty means all of
// them. There are deliberately no presets: "compile" and "link" would be our
// guess at the caller's intent, and when the guess is wrong it surfaces as a
// missing header three steps later with nothing pointing back here. Explicit
// names, and a diagnostic when something is absent because it was not asked for.
func selectComponents(t *Toolchain, want string) (keep []string, skipped []string, err error) {
	all := t.ComponentNames()
	if strings.TrimSpace(want) == "" {
		return all, nil, nil
	}
	valid := map[string]bool{}
	for _, n := range all {
		valid[n] = true
	}
	asked := map[string]bool{}
	for _, raw := range strings.Split(want, ",") {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if !valid[n] {
			return nil, nil, fmt.Errorf(
				"unknown component %q for %s.\nThis toolchain has: %s",
				n, t.Name, strings.Join(all, ", "))
		}
		asked[n] = true
	}
	for _, n := range all {
		if asked[n] {
			keep = append(keep, n)
		} else {
			skipped = append(skipped, n)
		}
	}
	if len(keep) == 0 {
		return nil, nil, fmt.Errorf("components named none of: %s", strings.Join(all, ", "))
	}
	return keep, skipped, nil
}

// Setup fetches and lays out a toolchain. It prefers per-component archives
// when the catalogue has them, because then "I only need the compiler" costs
// 8 MB rather than 69, and falls back to the whole upstream pack otherwise.
func Setup(c *Catalogue, opts Options, log func(string, ...any)) (*Installed, error) {
	build, tc, err := c.Resolve(opts.Version)
	if err != nil {
		return nil, err
	}
	log("setup-msvc: %s -> %s  %s", opts.Version, build, tc.Name)
	if tc.License == "proprietary" {
		log("::notice::%s is proprietary Microsoft software fetched from a "+
			"community archive. You are responsible for being licensed to use it. "+
			"This tool hosts nothing.", tc.Name)
	}

	keep, skipped, err := selectComponents(tc, opts.Components)
	if err != nil {
		return nil, err
	}
	inst := &Installed{
		Build: build, Toolchain: tc, Runner: opts.Runner,
		Requested: keep, Skipped: skipped, Extras: map[string]string{},
	}

	f := NewFetcher(opts.CacheDir, log)
	root := filepath.Join(opts.InstallDir, strings.ReplaceAll(build, ".", "_"))
	inst.Root = root

	if len(tc.Components) > 0 {
		if err := setupFromComponents(f, c, tc, keep, root, log); err != nil {
			// Components live only on a mirror. If it cannot serve them, the
			// upstream whole pack still can, so this is a slow path and not a
			// failure. Transfer size is the only thing lost.
			log("::warning::component fetch failed (%v); falling back to the "+
				"whole upstream pack", err)
			if err := setupFromWholePack(f, tc, root, log); err != nil {
				return nil, err
			}
		}
	} else {
		if err := setupFromWholePack(f, tc, root, log); err != nil {
			return nil, err
		}
	}

	// Build the search paths from what is actually on disk, so a component that
	// was skipped simply is not there rather than producing a path that lies.
	for _, name := range keep {
		dir := filepath.Join(root, name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		switch {
		case name == "bin":
			inst.Bin = append(inst.Bin, dir)
		case strings.HasSuffix(name, "include"):
			inst.Include = append(inst.Include, dir)
		case strings.HasSuffix(name, "lib"):
			inst.Lib = append(inst.Lib, dir)
		}
	}
	if len(inst.Bin) == 0 {
		return nil, fmt.Errorf("no bin component installed; cl.exe would not be found")
	}

	if err := setupExtras(c, f, opts, root, inst, log); err != nil {
		return nil, err
	}

	if opts.Runner == "wibo" {
		w, err := installWibo(c, f, opts.InstallDir)
		if err != nil {
			return nil, err
		}
		inst.Wibo = w
	}

	log("verifying the compiler is the one that was asked for:")
	ver, err := verifyCompiler(inst, log)
	if err != nil {
		return nil, err
	}
	inst.Version = ver
	return inst, nil
}

func setupFromComponents(f *Fetcher, c *Catalogue, tc *Toolchain, keep []string, root string, log func(string, ...any)) error {
	log("  fetching %d of %d components", len(keep), len(tc.ComponentNames()))
	for _, name := range keep {
		comp, ok := tc.Components[name]
		if !ok {
			log("::warning::%s has no repacked component; skipping", name)
			continue
		}
		dest := filepath.Join(root, name)
		if _, err := os.Stat(dest); err == nil {
			log("  %s: already installed", name)
			continue
		}
		mirrors := comp.Mirrors
		if len(mirrors) == 0 && c.MirrorBase != "" && comp.Path != "" {
			mirrors = []Mirror{{
				URL:    strings.TrimSuffix(c.MirrorBase, "/") + "/" + comp.Path,
				SHA256: comp.SHA256,
			}}
		}
		if len(mirrors) == 0 {
			return fmt.Errorf("component %s has no mirrors and the catalogue "+
				"sets no mirror_base", name)
		}
		path, err := f.Get(mirrors, comp.SHA256, name)
		if err != nil {
			return err
		}
		if err := Extract(path, dest, ""); err != nil {
			return fmt.Errorf("extracting %s: %w", name, err)
		}
	}
	return nil
}

func setupFromWholePack(f *Fetcher, tc *Toolchain, root string, log func(string, ...any)) error {
	if len(tc.Mirrors) == 0 {
		return fmt.Errorf("%s has neither components nor mirrors", tc.Name)
	}
	path, err := f.Get(tc.Mirrors, tc.Mirrors[0].SHA256, tc.Name)
	if err != nil {
		return err
	}
	staging := root + ".whole"
	if _, err := os.Stat(staging); err != nil {
		if err := Extract(path, staging, tc.Mirrors[0].StripPrefix); err != nil {
			return err
		}
	}
	// Normalise the upstream layout into the component shape the rest of the
	// tool expects, so there is one directory convention and not two.
	for name, rel := range tc.Layout {
		src := filepath.Join(staging, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(root, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func setupExtras(c *Catalogue, f *Fetcher, opts Options, root string, inst *Installed, log func(string, ...any)) error {
	if strings.TrimSpace(opts.Extras) == "" {
		return nil
	}
	var names []string
	for _, raw := range strings.Split(opts.Extras, ",") {
		if n := strings.TrimSpace(raw); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	log("extras:")
	for _, n := range names {
		a, ok := c.Artifacts[n]
		if !ok {
			var known []string
			for k := range c.Artifacts {
				known = append(known, k)
			}
			sort.Strings(known)
			if len(known) == 0 {
				return fmt.Errorf("extra %q is not defined. No artifacts are in the "+
					"catalogue; point --extras-catalogue at a file in your repository "+
					"that defines it", n)
			}
			return fmt.Errorf("extra %q is not defined. Known: %s", n, strings.Join(known, ", "))
		}
		if len(a.Mirrors) == 0 {
			return fmt.Errorf("extra %q has no mirrors", n)
		}
		dest := filepath.Join(opts.InstallDir, "extras", n)
		if _, err := os.Stat(dest); err != nil {
			path, err := f.Get(a.Mirrors, a.Mirrors[0].SHA256, n)
			if err != nil {
				return err
			}
			if err := Extract(path, dest, a.Strip); err != nil {
				return fmt.Errorf("extracting %s: %w", n, err)
			}
		} else {
			log("  %s: already installed", n)
		}
		inst.Extras[n] = dest
		for k, v := range a.Export {
			os.Setenv(k, strings.ReplaceAll(v, "{dir}", dest))
		}
		// `append` adds to the compiler's own search paths, which is what a
		// header-only artifact such as a DirectX SDK actually needs.
		for k, v := range a.Append {
			val := strings.ReplaceAll(v, "{dir}", dest)
			switch strings.ToUpper(k) {
			case "INCLUDE":
				inst.Include = append(inst.Include, val)
			case "LIB":
				inst.Lib = append(inst.Lib, val)
			case "PATH":
				inst.Bin = append(inst.Bin, val)
			}
		}
	}
	return nil
}

func installWibo(c *Catalogue, f *Fetcher, installDir string) (string, error) {
	r, ok := c.Runners["wibo"]
	if !ok {
		return "", fmt.Errorf("the catalogue defines no wibo runner")
	}
	key := "linux-x86_64"
	if runtime.GOOS == "darwin" {
		key = "macos"
	}
	m, ok := r.Mirrors[key]
	if !ok {
		return "", fmt.Errorf("no wibo build for %s", runtime.GOOS)
	}
	mirrors := []Mirror{m}
	if m.Fallback != "" {
		mirrors = append(mirrors, Mirror{URL: m.Fallback, SHA256: m.SHA256})
	}
	path, err := f.Get(mirrors, m.SHA256, "wibo "+r.Version)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(installDir, "bin", "wibo")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		exec.Command("xattr", "-d", "com.apple.quarantine", dest).Run()
	}
	return dest, nil
}

// verifyCompiler runs cl.exe and checks the banner against the catalogue. For
// byte-matching the compiler's identity is the entire question, so a pack that
// claims to be SP1 and answers .42 fails here rather than hours later in a diff
// nobody can explain.
func verifyCompiler(inst *Installed, log func(string, ...any)) (string, error) {
	var cl string
	for _, b := range inst.Bin {
		cand := filepath.Join(b, "cl.exe")
		if _, err := os.Stat(cand); err == nil {
			cl = cand
			break
		}
	}
	if cl == "" {
		return "", fmt.Errorf("no cl.exe in the installed bin directories")
	}

	var cmd *exec.Cmd
	if inst.Runner == "wibo" {
		cmd = exec.Command(inst.Wibo, cl)
	} else {
		cmd = exec.Command(cl)
	}
	cmd.Env = append(os.Environ(),
		"INCLUDE="+strings.Join(winPaths(inst, inst.Include), ";"),
		"LIB="+strings.Join(winPaths(inst, inst.Lib), ";"),
	)
	out, _ := cmd.CombinedOutput()
	got := BannerBuild(string(out))
	if got == "" {
		return "", fmt.Errorf("cl.exe printed no recognisable banner. Output was:\n%s",
			strings.TrimSpace(firstN(string(out), 800)))
	}
	log("  cl.exe reports: %s", strings.TrimSpace(firstLineWith(string(out), "Version")))

	if want := BannerBuild(inst.Toolchain.Banner); want != "" && want != got {
		return "", fmt.Errorf(
			"wrong compiler. Asked for %s (%s) but cl.exe is %s.\n"+
				"These emit different code, so this is a hard failure rather than a warning.",
			inst.Build, inst.Toolchain.Name, got)
	}
	return got, nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func firstLineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return s
}
