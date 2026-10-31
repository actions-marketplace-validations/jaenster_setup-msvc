package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Repacking exists because transfer size is the only lever available. GitHub
// Actions caches are scoped per repository, so there is no shared cache across
// consumers: every repository using this action pays a cold fetch, and the very
// first run of all pays it regardless. Upstream packs are whole Visual Studio
// installations, 160-177 MB of IDE, docs, atlmfc and redist, of which bin,
// include and lib are wanted.
//
// Each component becomes its own archive named by the hash of its contents, so
// a caller can ask for the compiler without the Platform SDK, and so components
// that are byte-identical across toolchains resolve to one URL and one cache
// entry. The 2005 SP1 tree is an RTM tree with the SP1 cabinet laid over it, so
// that sharing is real rather than theoretical.
//
// Output is reproducible because a repack is no longer upstream's bytes and
// therefore cannot carry upstream's hash. Without reproducibility the chain of
// custody is only our word; with it, anyone can re-derive the archive from
// upstream and compare. Hence: entries sorted, mtime zeroed, ownership and pax
// headers dropped, modes normalised, compression written without timestamps.

func cmdRepack(args []string) {
	fs_ := flag.NewFlagSet("repack", flag.ExitOnError)
	build := fs_.String("build", "", "catalogue key, e.g. 14.00.50727.762")
	src := fs_.String("src", "", "extracted upstream pack root")
	upURL := fs_.String("upstream-url", "", "where the pack came from")
	upSHA := fs_.String("upstream-sha256", "", "that pack's sha256")
	out := fs_.String("out", "mirror", "local mirror directory to write into")
	cat := fs_.String("catalogue", "catalogue.json", "catalogue path")
	comp := fs_.String("compressor", "zstd", "zstd | gzip")
	write := fs_.Bool("write", false, "update the catalogue; otherwise report only")
	fs_.Parse(args)

	if *build == "" || *src == "" {
		die("--build and --src are required")
	}
	c, err := LoadCatalogue(*cat)
	if err != nil {
		die("%v", err)
	}
	tc, ok := c.Toolchains[*build]
	if !ok {
		die("%s is not in the catalogue", *build)
	}

	ext := "tar.zst"
	if *comp == "gzip" {
		ext = "tar.gz"
	}
	components := map[string]*Component{}
	var total int64

	logf("repacking %s (%s) from %s", *build, tc.Name, *src)
	for _, name := range tc.ComponentNames() {
		rel := tc.Layout[name]
		dir := filepath.Join(*src, filepath.FromSlash(rel))
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			logf("  %-22s ABSENT in this pack (%s)", name, rel)
			continue
		}
		sum, size, count, blob, err := buildComponent(dir, *comp)
		if err != nil {
			die("%s: %v", name, err)
		}
		relPath := fmt.Sprintf("packs/%s/%s-%s.%s", sum[:16], *build, name, ext)
		dest := filepath.Join(*out, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			die("%v", err)
		}
		if err := os.WriteFile(dest, blob, 0o644); err != nil {
			die("%v", err)
		}
		components[name] = &Component{Path: relPath, SHA256: sum, Bytes: size, Files: count}
		total += size
		logf("  %-22s %6d files  %8.1f MB  %s", name, count, float64(size)/1e6, sum[:16])
	}
	logf("  %-22s %6s  %8.1f MB", "TOTAL", "", float64(total)/1e6)

	if !*write {
		logf("\n(report only; pass --write to update the catalogue)")
		return
	}
	tc.Components = components
	tc.ComponentFmt = ext
	if *upURL != "" {
		tc.DerivedFrom = &Mirror{URL: *upURL, SHA256: *upSHA}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		die("%v", err)
	}
	if err := os.WriteFile(*cat, append(b, '\n'), 0o644); err != nil {
		die("%v", err)
	}
	logf("\ncatalogue updated: %s now has %d components", *build, len(components))
}

// buildComponent tars a directory deterministically and compresses it.
func buildComponent(dir, compressor string) (sum string, size int64, count int, blob []byte, err error) {
	var files []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", 0, 0, nil, err
	}
	// Sorted by the path as it will appear in the archive, so the ordering does
	// not depend on how the filesystem happened to enumerate the directory.
	rels := make(map[string]string, len(files))
	for _, p := range files {
		r, _ := filepath.Rel(dir, p)
		rels[p] = filepath.ToSlash(r)
	}
	sort.Slice(files, func(i, j int) bool { return rels[files[i]] < rels[files[j]] })

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			return "", 0, 0, nil, err
		}
		mode := int64(0o644)
		if st.Mode()&0o111 != 0 {
			mode = 0o755
		}
		hdr := &tar.Header{
			Name:     rels[p],
			Mode:     mode,
			Size:     st.Size(),
			Typeflag: tar.TypeReg,
			ModTime:  zeroTime(),
			Format:   tar.FormatGNU,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", 0, 0, nil, err
		}
		f, err := os.Open(p)
		if err != nil {
			return "", 0, 0, nil, err
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return "", 0, 0, nil, err
		}
		f.Close()
	}
	if err := tw.Close(); err != nil {
		return "", 0, 0, nil, err
	}

	var packed bytes.Buffer
	if compressor == "gzip" {
		// Level 9 and an explicit zero mtime, or gzip stamps the current time
		// into the header and the output stops being reproducible.
		gw, _ := gzip.NewWriterLevel(&packed, gzip.BestCompression)
		gw.ModTime = zeroTime()
		if _, err := gw.Write(raw.Bytes()); err != nil {
			return "", 0, 0, nil, err
		}
		gw.Close()
	} else {
		zw, err := zstd.NewWriter(&packed, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			return "", 0, 0, nil, err
		}
		if _, err := zw.Write(raw.Bytes()); err != nil {
			return "", 0, 0, nil, err
		}
		zw.Close()
	}

	blob = packed.Bytes()
	h := sha256.Sum256(blob)
	return hex.EncodeToString(h[:]), int64(len(blob)), len(files), blob, nil
}

// zeroTime is the single fixed timestamp every archive entry carries. Real
// mtimes vary by machine and by when the pack was extracted, which would make
// the same inputs produce different bytes and defeat the whole point.
func zeroTime() time.Time { return time.Unix(0, 0).UTC() }
