// Command fakerelease stands in for github.com and the GitHub releases API in
// scripts/e2e-update.ps1: it serves one release, tag, through the
// releases/latest redirect, the release-by-tag API and the download URLs,
// whose assets are the files in dir, each with the "digest" GitHub reports.
// The digests are taken once at start, as GitHub fixes them at upload: a file
// changed afterwards no longer matches, which is how the e2e script tampers
// with a release. With -limited the API answers as GitHub does when its rate
// limit is used up. The releases list API has tag alone, with -notes as its
// notes (the changelog); -rate slows the download so its progress shows. A test build points selfupdate's webBase and apiBase at
// it via -ldflags.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7549", "listen address")
	dir := flag.String("dir", ".", "folder whose files are the release assets")
	tag := flag.String("tag", "v0.0.2", "release tag")
	limited := flag.Bool("limited", false, "the API answers 403 rate limit exceeded")
	notes := flag.String("notes", "* Fake release for the update e2e test.", "the release notes of tag")
	rate := flag.Int("rate", 0, "download speed limit in KiB/s (0: none), to watch the progress")
	flag.Parse()

	type asset struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}
	assets := []asset{}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(*dir, e.Name()))
		if err != nil {
			log.Fatal(err)
		}
		sum := sha256.Sum256(data)
		assets = append(assets, asset{Name: e.Name(), Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	mux := http.NewServeMux()
	const repo = "/UberMorgott/agent-link"
	mux.HandleFunc("GET "+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, repo+"/releases/tag/"+*tag, http.StatusFound)
	})
	mux.HandleFunc("GET /repos"+repo+"/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if *limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		if r.PathValue("tag") != *tag {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": *tag, "assets": assets})
	})
	mux.HandleFunc("GET /repos"+repo+"/releases", func(w http.ResponseWriter, _ *http.Request) {
		if *limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": *tag, "name": *tag, "body": *notes, "published_at": time.Now().UTC().Format(time.RFC3339)}})
	})
	mux.HandleFunc("GET "+repo+"/releases/download/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("tag") != *tag {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(*dir, filepath.Base(r.PathValue("name")))
		if *rate <= 0 {
			http.ServeFile(w, r, path)
			return
		}
		data, err := os.ReadFile(path) // #nosec G304 -- a file of the release folder
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		chunk := *rate << 10 / 10 // a tenth of a second's worth
		for len(data) > 0 {
			n := min(chunk, len(data))
			if _, err := w.Write(data[:n]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			data = data[n:]
			time.Sleep(100 * time.Millisecond)
		}
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
