package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeIndex(t *testing.T) {
	status, headers, body := (&Server{}).Serve("/index.html", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html", got)
	}
	if len(body) == 0 {
		t.Fatal("body is empty")
	}
	if string(body[:min(len(body), 15)]) == "" {
		t.Fatal("body not a string")
	}
	// Embedded file should be an HTML document.
	if !contains(body, "<!doctype html>") && !contains(body, "<!DOCTYPE html>") {
		t.Fatalf("body does not look like html: %s", string(body[:40]))
	}
}

func TestServeTrailingSlashTrimmed(t *testing.T) {
	status, _, _ := (&Server{}).Serve("/index.html/", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (trailing slash trimmed)", status)
	}
}

func TestServeUnknownPath404(t *testing.T) {
	status, _, _ := (&Server{}).Serve("/assets/app.js", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// An index.html in the data directory replaces the embedded page; removing
// it brings the embedded page back.
func TestServeOverrideFromDataDir(t *testing.T) {
	dir := t.TempDir()
	server := &Server{}
	server.SetDataDir(dir)
	if source, _ := server.Describe(); source != "embedded" {
		t.Fatalf("source without override = %q", source)
	}
	override := filepath.Join(dir, OverrideDir, "index.html")
	if err := os.MkdirAll(filepath.Dir(override), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("<!doctype html><title>newer</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.checkedAt = time.Time{}
	if _, _, body := server.Serve(IndexPath, nil); string(body) != "<!doctype html><title>newer</title>" {
		t.Fatalf("override not served: %.40s", body)
	}
	if source, _ := server.Describe(); source != "override" {
		t.Fatalf("source with override = %q", source)
	}
	if err := os.Remove(override); err != nil {
		t.Fatal(err)
	}
	server.checkedAt = time.Time{}
	if source, _ := server.Describe(); source != "embedded" {
		t.Fatalf("source after removing the override = %q", source)
	}
}

// If-None-Match compares weakly, so a tag a compressing proxy weakened still
// revalidates.
func TestServeRevalidatesWeakTags(t *testing.T) {
	server := &Server{}
	_, headers, _ := server.Serve(IndexPath, nil)
	etag := headers.Get("Etag")
	for _, match := range []string{etag, "W/" + etag, `"other", W/` + etag, "*"} {
		status, headers, body := server.Serve(IndexPath, http.Header{"If-None-Match": {match}})
		if status != http.StatusNotModified || len(body) != 0 || headers.Get("Etag") != etag {
			t.Fatalf("If-None-Match %s = %d with %d bytes, etag %q", match, status, len(body), headers.Get("Etag"))
		}
	}
	if status, _, _ := server.Serve(IndexPath, http.Header{"If-None-Match": {`"other"`}}); status != http.StatusOK {
		t.Fatalf("unrelated tag = %d, want 200", status)
	}
}

// A client that accepts gzip gets the page compressed under its own tag, and
// both variants revalidate.
func TestServeGzipVariant(t *testing.T) {
	server := &Server{}
	_, identity, page := server.Serve(IndexPath, nil)
	if identity.Get("Content-Encoding") != "" || identity.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("identity headers = %v", identity)
	}
	status, headers, body := server.Serve(IndexPath, http.Header{"Accept-Encoding": {"deflate, gzip;q=0.5, br"}})
	etag := headers.Get("Etag")
	if status != http.StatusOK || headers.Get("Content-Encoding") != "gzip" || headers.Get("Vary") != "Accept-Encoding" ||
		!strings.HasSuffix(etag, `-gz"`) || etag == identity.Get("Etag") {
		t.Fatalf("gzip response = %d %v", status, headers)
	}
	if got := gunzip(t, body); !bytes.Equal(got, page) {
		t.Fatalf("gunzipped body is %d bytes, want the %d-byte page", len(got), len(page))
	}
	status, headers, body = server.Serve(IndexPath, http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {etag}})
	if status != http.StatusNotModified || len(body) != 0 || headers.Get("Vary") != "Accept-Encoding" || headers.Get("Content-Encoding") != "" {
		t.Fatalf("gzip revalidation = %d with %d bytes, headers %v", status, len(body), headers)
	}
	// The identity tag names a different representation.
	if status, headers, _ := server.Serve(IndexPath, http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {identity.Get("Etag")}}); status != http.StatusOK || headers.Get("Content-Encoding") != "gzip" {
		t.Fatalf("identity tag with gzip = %d %v", status, headers)
	}
	for _, refused := range []string{"gzip;q=0", "GZIP; q=0.000", "br, identity", "gzip;q=bad"} {
		if _, headers, body := server.Serve(IndexPath, http.Header{"Accept-Encoding": {refused}}); headers.Get("Content-Encoding") != "" || !bytes.Equal(body, page) {
			t.Fatalf("Accept-Encoding %q got %v", refused, headers)
		}
	}
}

// An override page is compressed from its own bytes.
func TestServeGzipOverride(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, OverrideDir, "index.html")
	if err := os.MkdirAll(filepath.Dir(override), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("<!doctype html><title>newer</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	server.SetDataDir(dir)
	_, headers, body := server.Serve(IndexPath, http.Header{"Accept-Encoding": {"gzip"}})
	if got := gunzip(t, body); string(got) != "<!doctype html><title>newer</title>" || headers.Get("Content-Encoding") != "gzip" {
		t.Fatalf("override gzip = %q %v", got, headers)
	}
}

func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func contains(haystack []byte, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, []byte(needle)) >= 0
}

func indexOf(h, n []byte) int {
outer:
	for i := 0; i <= len(h)-len(n); i++ {
		for j := 0; j < len(n); j++ {
			if h[i+j] != n[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
