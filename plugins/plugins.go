// Package plugins carries facts about the agent-link plugin (plugins/agent-link)
// into the executable built from the same commit. This file sits outside the
// plugin folder, so it is neither shipped with the plugin nor part of its
// content hash.
package plugins

import (
	_ "embed"
	"encoding/json"
)

//go:embed agent-link/.codex-plugin/plugin.json
var codexManifest []byte

// Version is the plugin's version this executable was built with: the Codex
// manifest's "version", whose build part is the hash of the plugin's content
// (see agenthook's TestCodexPluginVersionTracksContent). An installed plugin
// with another version is not the one this executable expects.
func Version() string {
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(codexManifest, &m) != nil {
		return ""
	}
	return m.Version
}
