// Command fakerelease stands in for the GitHub releases API in
// scripts/e2e-update.ps1: it serves one release, tag, whose assets are the
// files in dir. A test build points selfupdate's apiBase at it via -ldflags.
package main

import (
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
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/UberMorgott/agent-link/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		entries, err := os.ReadDir(*dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		assets := []asset{}
		for _, e := range entries {
			assets = append(assets, asset{Name: e.Name(), URL: "http://" + *addr + "/download/" + e.Name()})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": *tag, "assets": assets})
	})
	mux.HandleFunc("GET /download/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(*dir, filepath.Base(r.PathValue("name"))))
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
