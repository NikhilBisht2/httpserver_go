package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// setupStatic creates:
//
//	<tmp>/secret.txt            (OUTSIDE the served directory)
//	<tmp>/public-secrets/x.txt  (sibling whose name starts with "public")
//	<tmp>/public/...            (the served directory)
func setupStatic(t *testing.T) HandlerFunc {
	t.Helper()
	root := t.TempDir() // automatically deleted when the test ends
	pub := filepath.Join(root, "public")

	mustWrite := func(path string, data []byte) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(root, "secret.txt"), []byte("TOP SECRET"))
	mustWrite(filepath.Join(root, "public-secrets", "x.txt"), []byte("sibling secret"))
	mustWrite(filepath.Join(pub, "hello.txt"), []byte("hello"))
	mustWrite(filepath.Join(pub, "style.css"), []byte("body{}"))
	mustWrite(filepath.Join(pub, "page.html"), []byte("<p>hi</p>"))
	mustWrite(filepath.Join(pub, ".env"), []byte("PASSWORD=1"))
	mustWrite(filepath.Join(pub, "sub", "index.html"), []byte("sub index"))
	mustWrite(filepath.Join(pub, "weird.unknownext"), []byte("?"))

	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	mustWrite(filepath.Join(pub, "binary.png"), all)

	return StaticHandler("/static/", pub)
}

func TestStaticServesFiles(t *testing.T) {
	h := setupStatic(t)
	tests := []struct {
		path, wantType, wantBody string
	}{
		{"/static/hello.txt", "text/plain; charset=utf-8", "hello"},
		{"/static/style.css", "text/css; charset=utf-8", "body{}"},
		{"/static/page.html", "text/html; charset=utf-8", "<p>hi</p>"},
		{"/static/sub/", "text/html; charset=utf-8", "sub index"}, // directory -> index.html
		{"/static/sub", "text/html; charset=utf-8", "sub index"},
		{"/static/weird.unknownext", "application/octet-stream", "?"},
	}
	for _, tc := range tests {
		resp := h(get(tc.path))
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d", tc.path, resp.StatusCode)
			continue
		}
		if got := resp.Header["Content-Type"]; got != tc.wantType {
			t.Errorf("%s: Content-Type %q, want %q", tc.path, got, tc.wantType)
		}
		if string(resp.Body) != tc.wantBody {
			t.Errorf("%s: body %q, want %q", tc.path, resp.Body, tc.wantBody)
		}
	}
}

func TestStaticBinarySurvivesIntact(t *testing.T) {
	h := setupStatic(t)
	resp := h(get("/static/binary.png"))
	want := make([]byte, 256)
	for i := range want {
		want[i] = byte(i)
	}
	if !bytes.Equal(resp.Body, want) {
		t.Error("binary body was corrupted")
	}
}

func TestStaticNotFound(t *testing.T) {
	h := setupStatic(t)
	if resp := h(get("/static/nope.txt")); resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if resp := h(get("/static/hello.txt/extra")); resp.StatusCode != 404 {
		t.Errorf("file-as-directory: status = %d, want 404", resp.StatusCode)
	}
}

func TestStaticPathTraversal(t *testing.T) {
	h := setupStatic(t)
	attacks := []string{
		"/static/../secret.txt",
		"/static/../../secret.txt",
		"/static/sub/../../secret.txt",
		"/static/../public-secrets/x.txt", // sibling directory sharing the prefix "public"
		"/static/..",
	}
	for _, path := range attacks {
		resp := h(get(path))
		if resp.StatusCode != 403 {
			t.Errorf("%s: status %d, want 403", path, resp.StatusCode)
		}
		if bytes.Contains(resp.Body, []byte("TOP SECRET")) || bytes.Contains(resp.Body, []byte("sibling secret")) {
			t.Errorf("%s: LEAKED secret content!", path)
		}
	}
}

func TestStaticBlocksDotfilesAndNullBytes(t *testing.T) {
	h := setupStatic(t)
	if resp := h(get("/static/.env")); resp.StatusCode != 404 {
		t.Errorf(".env: status %d, want 404", resp.StatusCode)
	}
	if resp := h(get("/static/hello.txt\x00.png")); resp.StatusCode != 400 {
		t.Errorf("null byte: status %d, want 400", resp.StatusCode)
	}
}

func TestStaticSetsNosniff(t *testing.T) {
	h := setupStatic(t)
	if resp := h(get("/static/hello.txt")); resp.Header["X-Content-Type-Options"] != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}
