package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Catalogue is the whole of what this tool knows how to fetch. It is data, not
// code: a third party can supply their own for extras without this binary
// changing. What makes that safe is that every artifact carries a sha256, so a
// wrong definition fetches bytes that fail verification rather than bytes that
// get used.
type Catalogue struct {
	Schema     int                  `json:"schema"`
	// MirrorBase is where repacked components are served from. Components
	// exist only on a mirror -- upstream ships whole packs -- so a component
	// fetch that fails falls back to the whole pack rather than failing.
	MirrorBase string               `json:"mirror_base,omitempty"`
	Toolchains map[string]*Toolchain `json:"toolchains"`
	Artifacts  map[string]*Artifact  `json:"artifacts,omitempty"`
	Runners    map[string]*Runner    `json:"runners,omitempty"`
}

// Toolchain is keyed in the map by its exact cl.exe build, e.g. 14.00.50727.762.
// Not by the marketing version: Visual C++ 2005 RTM and SP1 both stamp 50727 in
// the PE Rich header and emit different code, so for byte-matching work only the
// fourth component tells them apart.
type Toolchain struct {
	Aliases        []string              `json:"aliases,omitempty"`
	Name           string                `json:"name"`
	Product        string                `json:"product,omitempty"`
	Banner         string                `json:"banner,omitempty"`
	License        string                `json:"license,omitempty"`
	RunsUnderWibo  *bool                 `json:"runs_under_wibo,omitempty"`
	Mirrors        []Mirror              `json:"mirrors,omitempty"`
	Layout         map[string]string     `json:"layout,omitempty"`
	Components     map[string]*Component `json:"components,omitempty"`
	ComponentFmt   string                `json:"component_format,omitempty"`
	DerivedFrom    *Mirror               `json:"derived_from,omitempty"`
	Contains       map[string]any        `json:"contains,omitempty"`
	LibsFrom       string                `json:"libs_from,omitempty"`
}

// Component is one directory of a toolchain, repacked on its own and named by
// the hash of its contents. Splitting them is the only lever available for
// transfer size: GitHub Actions caches are scoped per repository, so there is no
// shared cache across consumers and every one of them pays a cold fetch.
type Component struct {
	Path    string   `json:"path"`
	SHA256  string   `json:"sha256"`
	Bytes   int64    `json:"bytes"`
	Files   int      `json:"files"`
	Mirrors []Mirror `json:"mirrors,omitempty"`
}

// Artifact is anything else a build needs that is not a compiler: a DirectX
// header set, a redistributable DLL, a vendor SDK's import stubs. Same
// primitive as a toolchain component, deliberately: fetch, verify, unpack,
// export. No dependency resolution and no version solving; this is not a
// package manager and should not grow into a bad one.
type Artifact struct {
	Name    string            `json:"name,omitempty"`
	Summary string            `json:"summary,omitempty"`
	License string            `json:"license,omitempty"`
	Mirrors []Mirror          `json:"mirrors,omitempty"`
	Unpack  string            `json:"unpack,omitempty"`
	Strip   string            `json:"strip_prefix,omitempty"`
	Export  map[string]string `json:"export,omitempty"`
	Append  map[string]string `json:"append,omitempty"`
}

// Mirror is one route to a set of bytes. It is a list elsewhere because these
// are third-party community archives and any of them can vanish; the hash is
// what is trusted, never the host.
type Mirror struct {
	SHA256        string `json:"sha256"`
	URL           string `json:"url"`
	StripPrefix   string `json:"strip_prefix,omitempty"`
	DownloadBytes int64  `json:"download_bytes,omitempty"`
	IsMirror      bool   `json:"is_mirror,omitempty"`
	Fallback      string `json:"fallback,omitempty"`
	Note          string `json:"$note,omitempty"`
}

type Runner struct {
	Name      string            `json:"name,omitempty"`
	Version   string            `json:"version,omitempty"`
	Mirrors   map[string]Mirror `json:"mirrors,omitempty"`
	KnownGaps []string          `json:"known_gaps,omitempty"`
}

func LoadCatalogue(path string) (*Catalogue, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading catalogue: %w", err)
	}
	var c Catalogue
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Toolchains == nil {
		c.Toolchains = map[string]*Toolchain{}
	}
	return &c, nil
}

// Merge folds another catalogue in, for a consumer's own extras file. Only
// artifacts merge: letting a third-party file redefine a toolchain would make
// `version: 8.0sp1` mean different things in different repositories.
func (c *Catalogue) Merge(other *Catalogue) {
	if other.Artifacts == nil {
		return
	}
	if c.Artifacts == nil {
		c.Artifacts = map[string]*Artifact{}
	}
	for k, v := range other.Artifacts {
		c.Artifacts[k] = v
	}
}

// Resolve accepts an exact build ("14.00.50727.762") or an alias ("8.0sp1").
func (c *Catalogue) Resolve(want string) (string, *Toolchain, error) {
	key := strings.TrimSpace(want)
	if tc, ok := c.Toolchains[key]; ok {
		return key, tc, nil
	}
	low := strings.ToLower(key)
	for build, tc := range c.Toolchains {
		for _, a := range tc.Aliases {
			if strings.EqualFold(a, low) {
				return build, tc, nil
			}
		}
	}
	var known []string
	for build, tc := range c.Toolchains {
		known = append(known, fmt.Sprintf("  %-18s %s", build, strings.Join(tc.Aliases, ", ")))
	}
	sort.Strings(known)
	return "", nil, fmt.Errorf("unknown version %q. Known:\n%s", want, strings.Join(known, "\n"))
}

// ComponentNames returns the components of a toolchain in a stable order, so
// INCLUDE and LIB are assembled the same way on every run. Order matters: the
// compiler searches these paths in sequence.
func (t *Toolchain) ComponentNames() []string {
	order := []string{"bin", "include", "platform_sdk_include", "lib", "platform_sdk_lib"}
	seen := map[string]bool{}
	var out []string
	for _, n := range order {
		if _, ok := t.Layout[n]; ok {
			out = append(out, n)
			seen[n] = true
		}
	}
	var rest []string
	for n := range t.Layout {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

var bannerRe = regexp.MustCompile(`Version\s+(\d+\.\d+\.\d+(?:\.\d+)?)\s+for\s+(\S+)`)

// BannerBuild pulls the build out of a cl.exe banner line.
func BannerBuild(s string) string {
	if m := bannerRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
