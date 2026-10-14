// static.go — serving files from a directory, safely.
package main

import (
	"errors"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// Our own table is checked first because on Windows the standard `mime` package
// reads the registry, which can return empty or odd values for .js or .svg.
var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/plain; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".ico":  "image/x-icon",
	".pdf":  "application/pdf",
}

// contentType picks a Content-Type from the file extension.
func contentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ct, ok := mimeTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	// "unknown binary data": browsers download it instead of displaying it.
	return "application/octet-stream"
}

// StaticHandler returns a handler that serves files from dir for URLs under
// urlPrefix, e.g. StaticHandler("/static/", "public").
//
// It returns a function (a CLOSURE) so the handler can remember urlPrefix and
// absDir even though HandlerFunc has no parameter for them.
func StaticHandler(urlPrefix, dir string) HandlerFunc {
	// Runs ONCE at startup. Resolving to an absolute path now means the handler
	// keeps working even if the working directory changes later.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("static: cannot resolve directory %q: %v", dir, err)
	}

	// Runs on EVERY request.
	return func(req *Request) *Response {
		// "/static/css/a.css" -> "css/a.css"
		rel := strings.TrimPrefix(req.Path, urlPrefix)

		// A null byte can truncate paths in some systems. Reject it.
		if strings.ContainsRune(rel, 0) {
			return Text(400, "bad path")
		}

		// Join also CLEANS the path: "public" + "../main.go" becomes "main.go",
		// which is why we must check the result below.
		full := filepath.Join(absDir, filepath.FromSlash(rel))

		// PATH TRAVERSAL PROTECTION. We check AFTER joining, on the final
		// resolved path, so every trick (.., mixed separators, encoded dots
		// that were decoded by the parser) is caught.
		// The trailing separator matters: without it, "/site/public-secrets"
		// would wrongly count as being inside "/site/public".
		if full != absDir && !strings.HasPrefix(full, absDir+string(filepath.Separator)) {
			return Text(403, "forbidden")
		}

		// Hide dotfiles (.env, .git, ...) anywhere in the path.
		relClean, _ := filepath.Rel(absDir, full)
		for _, seg := range strings.Split(filepath.ToSlash(relClean), "/") {
			if strings.HasPrefix(seg, ".") && seg != "." {
				return Text(404, "not found")
			}
		}

		info, err := os.Stat(full)
		if err == nil && info.IsDir() {
			// A directory request serves its index.html if it has one.
			full = filepath.Join(full, "index.html")
			info, err = os.Stat(full)
		}
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return Text(403, "forbidden")
			}
			// Not found (or anything else we can't serve): 404.
			return Text(404, "not found")
		}
		if info.IsDir() {
			return Text(404, "not found")
		}

		// NOTE: ReadFile loads the whole file into memory. Fine for small
		// assets; large files would need streaming (see README limitations).
		data, err := os.ReadFile(full)
		if err != nil {
			log.Printf("static: read error for %s: %v", full, err)
			return Text(500, "Internal Server Error")
		}

		resp := Bytes(200, contentType(full), data)
		// Stops browsers from guessing a different type than we declared.
		resp.Header["X-Content-Type-Options"] = "nosniff"
		return resp
	}
}
