package main

import (
	"regexp"
	"strings"
	"testing"
)

// These run without a network. They exist because the catalogue is the part
// that keeps being wrong in ways nothing else notices: an alias colliding, a
// `contains` flag claiming libraries a pack does not ship, a banner filed under
// the wrong build. Each of those surfaces far from its cause -- a link failure,
// or worse, a successful build with the wrong compiler.

const catPath = "catalogue.json"

var (
	buildRe = regexp.MustCompile(`^\d+\.\d+\.\d+(\.\d+)?$`)
	shaRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func load(t *testing.T) *Catalogue {
	t.Helper()
	c, err := LoadCatalogue(catPath)
	if err != nil {
		t.Fatalf("loading catalogue: %v", err)
	}
	return c
}

func TestSchema(t *testing.T) {
	if c := load(t); c.Schema != 1 {
		t.Errorf("schema = %d, want 1", c.Schema)
	}
}

func TestToolchainKeysAreBuilds(t *testing.T) {
	for build := range load(t).Toolchains {
		if !buildRe.MatchString(build) {
			t.Errorf("%s: key must be a dotted build number", build)
		}
	}
}

// An alias used twice resolves to whichever entry the map happened to yield
// first, which is a silent wrong-compiler bug of exactly the kind this tool
// exists to prevent.
func TestAliasesAreUniqueAndResolve(t *testing.T) {
	c := load(t)
	seen := map[string]string{}
	for build, tc := range c.Toolchains {
		for _, a := range tc.Aliases {
			low := strings.ToLower(a)
			if prev, dup := seen[low]; dup {
				t.Errorf("alias %q used by both %s and %s", a, prev, build)
			}
			seen[low] = build
			if strings.EqualFold(a, build) {
				t.Errorf("%s: alias %q duplicates its own key", build, a)
			}
		}
	}
	for alias, build := range seen {
		got, _, err := c.Resolve(alias)
		if err != nil || got != build {
			t.Errorf("Resolve(%q) = %q, %v; want %q", alias, got, err, build)
		}
	}
	for build := range c.Toolchains {
		if got, _, err := c.Resolve(build); err != nil || got != build {
			t.Errorf("Resolve(%q) = %q, %v", build, got, err)
		}
	}
}

func TestResolveRejectsUnknown(t *testing.T) {
	if _, _, err := load(t).Resolve("definitely-not-a-compiler"); err == nil {
		t.Error("Resolve accepted an unknown version")
	}
}

// Every route to bytes must be pinned. An unpinned mirror is a mirror that can
// serve anything, which would make the whole self-hosting arrangement unsafe.
func TestEveryMirrorIsPinnedAndHTTPS(t *testing.T) {
	c := load(t)
	check := func(where string, m Mirror) {
		if !strings.HasPrefix(m.URL, "https://") {
			t.Errorf("%s: url must be https, got %q", where, m.URL)
		}
		if !shaRe.MatchString(m.SHA256) {
			t.Errorf("%s: sha256 must be 64 lowercase hex chars", where)
		}
	}
	for build, tc := range c.Toolchains {
		if len(tc.Mirrors) == 0 && len(tc.Components) == 0 {
			t.Errorf("%s: needs mirrors or components", build)
		}
		for i, m := range tc.Mirrors {
			check(build+".mirrors["+itoa(i)+"]", m)
		}
		for name, comp := range tc.Components {
			if !shaRe.MatchString(comp.SHA256) {
				t.Errorf("%s.components[%s]: bad sha256", build, name)
			}
			if comp.Path == "" && len(comp.Mirrors) == 0 {
				t.Errorf("%s.components[%s]: no path and no mirrors", build, name)
			}
		}
	}
	for name, a := range c.Artifacts {
		for i, m := range a.Mirrors {
			check("artifacts."+name+".mirrors["+itoa(i)+"]", m)
		}
	}
}

// A banner filed under the wrong build would make verification assert the wrong
// thing, which is worse than not verifying at all.
func TestBannerAgreesWithItsKey(t *testing.T) {
	for build, tc := range load(t).Toolchains {
		if tc.Banner == "" {
			t.Errorf("%s: no pinned banner, so a wrong compiler would only warn", build)
			continue
		}
		if got := BannerBuild(tc.Banner); got != build {
			t.Errorf("%s: banner says %q", build, got)
		}
	}
}

func TestComponentsMatchLayout(t *testing.T) {
	for build, tc := range load(t).Toolchains {
		for name := range tc.Components {
			if _, ok := tc.Layout[name]; !ok {
				t.Errorf("%s: component %q is not in layout", build, name)
			}
		}
		if len(tc.Components) > 0 && tc.DerivedFrom == nil {
			t.Errorf("%s: repacked components must record derived_from, "+
				"or the derivation cannot be checked", build)
		}
	}
}

// The 2005 pair is why this project exists, so its specific facts are asserted
// rather than left to general rules. SP1 was compiler-only until a complete
// pack turned up; borrowing RTM's CRT would link but never byte-match.
func TestVisualCpp2005Pair(t *testing.T) {
	c := load(t)
	rtm, okR := c.Toolchains["14.00.50727.42"]
	sp1, okS := c.Toolchains["14.00.50727.762"]
	if !okR || !okS {
		t.Fatal("both 2005 RTM and SP1 must be present and distinct")
	}
	if !truthy(sp1.Contains["crt_libs"]) {
		t.Error("2005 SP1 must carry its own CRT libraries: RTM's would not byte-match")
	}
	if sp1.LibsFrom != "" {
		t.Error("2005 SP1 ships libraries now and must not borrow RTM's")
	}
	if !truthy(rtm.Contains["crt_libs"]) {
		t.Error("2005 RTM should carry CRT libraries")
	}
}

func TestLibsFromPointsSomewhereUseful(t *testing.T) {
	c := load(t)
	for build, tc := range c.Toolchains {
		if tc.LibsFrom == "" {
			continue
		}
		donor, ok := c.Toolchains[tc.LibsFrom]
		if !ok {
			t.Errorf("%s: libs_from %q is not in the catalogue", build, tc.LibsFrom)
			continue
		}
		if !truthy(donor.Contains["crt_libs"]) && !truthy(donor.Contains["platform_sdk_libs"]) {
			t.Errorf("%s: donor %s has no libraries to lend", build, tc.LibsFrom)
		}
		if truthy(tc.Contains["crt_libs"]) {
			t.Errorf("%s: has libs_from but also claims crt_libs", build)
		}
	}
}

func TestSelectComponents(t *testing.T) {
	c := load(t)
	_, tc, err := c.Resolve("8.0sp1")
	if err != nil {
		t.Fatal(err)
	}
	all, skipped, err := selectComponents(tc, "")
	if err != nil || len(skipped) != 0 {
		t.Fatalf("empty selection should mean all: %v, skipped %v", err, skipped)
	}
	keep, skipped, err := selectComponents(tc, "bin,include")
	if err != nil {
		t.Fatal(err)
	}
	if len(keep) != 2 || len(skipped) != len(all)-2 {
		t.Errorf("keep=%v skipped=%v", keep, skipped)
	}
	// bin must come first whatever order the caller wrote, because the search
	// paths are assembled in this order.
	if keep[0] != "bin" {
		t.Errorf("component order not canonical: %v", keep)
	}
	if _, _, err := selectComponents(tc, "headers"); err == nil {
		t.Error("an unknown component must be rejected at setup, not discovered later")
	}
}

func TestRunnerWiboIsDocumented(t *testing.T) {
	c := load(t)
	w, ok := c.Runners["wibo"]
	if !ok {
		t.Fatal("runners.wibo missing")
	}
	if len(w.KnownGaps) == 0 {
		t.Error("wibo's gaps must be documented, not discovered by users")
	}
	for plat, m := range w.Mirrors {
		if !shaRe.MatchString(m.SHA256) {
			t.Errorf("runners.wibo.mirrors[%s]: bad sha256", plat)
		}
	}
}

func itoa(i int) string {
	return string(rune('0' + i%10))
}
