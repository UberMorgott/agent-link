// Command fakerelease stands in for the GitHub releases API in
// scripts/e2e-update.ps1: it serves one release, tag, whose assets are the
// files in dir, each with the "digest" GitHub reports. The digests are taken
// once at start, as GitHub fixes them at upload: a file changed afterwards
// no longer matches, which is how the e2e script tampers with a release. A
// test build points selfupdate's apiBase at it via -ldflags.
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
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7549", "listen address")
	dir := flag.String("dir", ".", "folder whose files are the release assets")
	tag := flag.String("tag", "v0.0.2", "release tag")
	flag.Parse()

	type asset struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
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
		assets = append(assets, asset{Name: e.Name(), URL: "http://" + *addr + "/download/" + e.Name(), Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/UberMorgott/agent-link/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": *tag, "assets": assets})
	})
	mux.HandleFunc("GET /download/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(*dir, filepath.Base(r.PathValue("name"))))
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
