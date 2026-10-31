package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Fetcher downloads, verifies and unpacks. Every route to bytes goes through
// here so there is exactly one place where the hash is checked.
type Fetcher struct {
	CacheDir string
	Log      func(string, ...any)
	Client   *http.Client
}

func NewFetcher(cacheDir string, log func(string, ...any)) *Fetcher {
	return &Fetcher{
		CacheDir: cacheDir,
		Log:      log,
		Client:   &http.Client{Timeout: 30 * time.Minute},
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Get returns a local path holding bytes whose sha256 is `want`, trying each
// mirror in turn. A mirror serving something else is not an error that
// propagates: it is skipped and the next is tried, which is what makes
// self-mirroring safe. Only running out of mirrors is fatal.
func (f *Fetcher) Get(mirrors []Mirror, want, label string) (string, error) {
	if want == "" {
		return "", fmt.Errorf("%s: no sha256 pinned; refusing to fetch unverifiable bytes", label)
	}
	cached := filepath.Join(f.CacheDir, want[:16]+"-"+urlBase(mirrors))
	if got, err := sha256File(cached); err == nil && got == want {
		f.Log("  %s: cache hit", label)
		return cached, nil
	}
	if err := os.MkdirAll(f.CacheDir, 0o755); err != nil {
		return "", err
	}

	var problems []string
	for _, m := range mirrors {
		url := m.URL
		if url == "" {
			continue
		}
		f.Log("  %s: fetching %s", label, url)
		tmp := cached + ".part"
		if err := f.download(url, tmp); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", url, err))
			os.Remove(tmp)
			continue
		}
		got, err := sha256File(tmp)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", url, err))
			os.Remove(tmp)
			continue
		}
		if got != want {
			problems = append(problems,
				fmt.Sprintf("%s: sha256 %s, expected %s", url, got[:16], want[:16]))
			os.Remove(tmp)
			continue
		}
		if err := os.Rename(tmp, cached); err != nil {
			return "", err
		}
		f.Log("  %s: verified sha256 %s...", label, want[:16])
		return cached, nil
	}
	return "", fmt.Errorf("no usable mirror for %s:\n  %s", label, strings.Join(problems, "\n  "))
}

func urlBase(mirrors []Mirror) string {
	for _, m := range mirrors {
		if m.URL != "" {
			if i := strings.LastIndex(m.URL, "/"); i >= 0 && i+1 < len(m.URL) {
				return m.URL[i+1:]
			}
		}
	}
	return "download"
}

func (f *Fetcher) download(url, dest string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "setup-msvc")
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// Extract unpacks a .tar.gz or .tar.zst into dir, optionally dropping a leading
// path element. Members that would escape the destination are refused outright
// rather than sanitised: an archive that contains one is not an archive we
// should be unpacking at all.
func Extract(archive, dir, stripPrefix string) error {
	fh, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer fh.Close()

	var r io.Reader = fh
	switch {
	case strings.HasSuffix(archive, ".zst"), strings.HasSuffix(archive, ".zstd"):
		zr, err := zstd.NewReader(fh)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	case strings.HasSuffix(archive, ".gz"), strings.HasSuffix(archive, ".tgz"):
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		if stripPrefix != "" {
			trimmed := strings.TrimPrefix(name, stripPrefix)
			if trimmed == name {
				continue // not under the prefix we were told to keep
			}
			name = strings.TrimPrefix(trimmed, "/")
		}
		if name == "" {
			continue
		}
		target := filepath.Join(abs, filepath.FromSlash(name))
		rel, err := filepath.Rel(abs, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive member escapes its directory: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			// Bounded copy: a tar header claiming a huge size should not be
			// able to fill the disk through us.
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink, tar.TypeLink:
			// Skipped on purpose. None of these toolchains need links, and
			// honouring them is how an archive reaches outside its directory.
			continue
		}
	}
	return nil
}
