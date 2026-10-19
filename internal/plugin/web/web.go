// Package web embeds the built management UI (a single inlined index.html)
// and serves it as a CPA plugin resource under
// /v0/resource/plugins/cpa-key-policy/index.html.
//
// dist/index.html is a build artifact produced by `npm run build` in ../../web.
// A placeholder is committed so the Go build never fails when the frontend has
// not been built yet; the real UI replaces it after a frontend build.
//
// An operator can ship a newer UI without replacing the plugin library: an
// index.html placed in OverrideDir next to the state file is served instead
// of the embedded page. The panel reloads the page on every menu click, so
// responses carry an ETag and a revalidation answers 304 without a body.
// CPA does not compress responses, so a client that accepts gzip gets a copy
// compressed once per page, under its own ETag.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed dist/index.html
var indexHTML []byte

const contentType = "text/html; charset=utf-8"

// IndexPath is the resource path (relative to the plugin resource base) the UI
// is served at.
const IndexPath = "/index.html"

// OverrideDir is the directory, next to the state file, whose index.html
// replaces the embedded page.
const OverrideDir = "cpa-key-policy-ui"

var embeddedTag = digest(indexHTML)

var (
	embeddedGzipOnce sync.Once
	embeddedGzip     []byte
)

// Server serves the UI page, preferring an override file when one exists.
// It checks the override at most once a second.
type Server struct {
	mu        sync.Mutex
	dir       string
	checkedAt time.Time
	modTime   time.Time
	size      int64
	page      []byte
	gz        []byte
	tag       string
}

// SetDataDir points the override lookup at the plugin's data directory.
func (s *Server) SetDataDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != dir {
		s.dir, s.page, s.gz, s.tag, s.checkedAt = dir, nil, nil, "", time.Time{}
	}
}

// Serve answers a plugin resource GET.
func (s *Server) Serve(path string, requestHeaders http.Header) (status int, headers http.Header, body []byte) {
	if strings.TrimRight(path, "/") != IndexPath {
		return http.StatusNotFound, http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}}, []byte("not found")
	}
	page, gz, tag, _ := s.current()
	etag := `"` + tag + `"`
	gzipped := gz != nil && acceptsGzip(requestHeaders)
	if gzipped {
		page, etag = gz, `"`+tag+`-gz"`
	}
	headers = http.Header{"Etag": []string{etag}, "Cache-Control": []string{"no-cache"}, "Vary": []string{"Accept-Encoding"}}
	for _, candidate := range requestHeaders.Values("If-None-Match") {
		for value := range strings.SplitSeq(candidate, ",") {
			// If-None-Match uses the weak comparison (RFC 9110 13.1.2): a
			// proxy that compresses the identity page weakens its tag.
			if value = strings.TrimSpace(value); value == "*" || strings.TrimPrefix(value, "W/") == etag {
				return http.StatusNotModified, headers, nil
			}
		}
	}
	headers.Set("Content-Type", contentType)
	if gzipped {
		headers.Set("Content-Encoding", "gzip")
	}
	return http.StatusOK, headers, page
}

// Describe reports which page is served ("embedded" or "override") and its
// SHA-256.
func (s *Server) Describe() (source, sha string) {
	_, _, tag, override := s.current()
	if override {
		return "override", tag
	}
	return "embedded", tag
}

// current returns the page, its gzip copy (nil when compression failed) and
// its SHA-256.
func (s *Server) current() (page, gz []byte, tag string, override bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" && time.Since(s.checkedAt) >= time.Second {
		s.checkedAt = time.Now()
		path := filepath.Join(s.dir, OverrideDir, "index.html")
		info, err := os.Stat(path)
		switch {
		case err != nil:
			s.page, s.gz, s.tag = nil, nil, ""
		case s.page == nil || !info.ModTime().Equal(s.modTime) || info.Size() != s.size:
			if raw, readErr := os.ReadFile(path); readErr == nil && len(raw) > 0 {
				s.page, s.gz, s.tag, s.modTime, s.size = raw, compress(raw), digest(raw), info.ModTime(), info.Size()
			} else {
				s.page, s.gz, s.tag = nil, nil, ""
			}
		}
	}
	if s.page != nil {
		return s.page, s.gz, s.tag, true
	}
	embeddedGzipOnce.Do(func() { embeddedGzip = compress(indexHTML) })
	return indexHTML, embeddedGzip, embeddedTag, false
}

// acceptsGzip reports whether Accept-Encoding lists gzip with a nonzero
// weight.
func acceptsGzip(requestHeaders http.Header) bool {
	for _, candidate := range requestHeaders.Values("Accept-Encoding") {
		for value := range strings.SplitSeq(candidate, ",") {
			coding, params, _ := strings.Cut(value, ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			for param := range strings.SplitSeq(params, ";") {
				name, weight, _ := strings.Cut(strings.TrimSpace(param), "=")
				if strings.EqualFold(strings.TrimSpace(name), "q") {
					q, err := strconv.ParseFloat(strings.TrimSpace(weight), 64)
					return err == nil && q > 0
				}
			}
			return true
		}
	}
	return false
}

func compress(raw []byte) []byte {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		return nil
	}
	if err := writer.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
