package main

// Replies for Claude Code subagents. Subagents share their session's id and
// its MCP server process, so neither the MCP tools nor the CLI know which
// agent of the session calls them. The session's hooks do: an event inside a
// subagent carries its agent_id and agent_type
// (https://code.claude.com/docs/en/hooks#common-input-fields).
//
//   - PreToolUse of an ask (the agentlink MCP send/discuss tools, or a shell
//     command running agentlink send/discuss) leaves a stamp in the session's
//     hook state: who calls, keyed by the tool call's input. The tool, run
//     right after, takes the stamp of its own input (takeAskStamp) and records
//     the subagent on the message it sends (node.SendRequest.AgentID).
//   - The node marks the replies to such a message (UnreadMessage.ForAgent).
//     While that subagent lives (SubagentStart .. SubagentStop, hookState.Live)
//     only its own PostToolUse delivers them; the main agent's hooks and wakes
//     skip them (the node hears the live ones: SessionRequest.Agents). Once it
//     ended they go to the main agent, marked as a reply for that subagent.
//
// Codex tool hooks carry no agent id: Codex asks stay the session's.

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Subagent tracking limits.
const (
	// liveAgentTTL: a subagent with no hook event for this long counts as
	// ended (its SubagentStop was missed).
	liveAgentTTL = 30 * time.Minute
	// askStampTTL: a stamp no tool took for this long is dropped (the call was
	// denied or failed). A CLI stamp matches loosely, so it lapses sooner.
	askStampTTL    = 10 * time.Minute
	cliAskStampTTL = 2 * time.Minute
	maxAskStamps   = 32
	maxLiveAgents  = 64
	// askStampCmdMax bounds the command line a CLI stamp keeps.
	askStampCmdMax = 4096
)

// liveAgent is a live subagent of the session (hookState.Live).
type liveAgent struct {
	Type string    `json:"type,omitempty"`
	At   time.Time `json:"at"` // its last hook event
}

// askStamp is an ask tool call about to run (hookState.Stamps): Agent ("" for
// the main agent) of type Type calls it; Key is the call's input
// (askKey), Cmd a shell call's command line.
type askStamp struct {
	Agent string    `json:"agent,omitempty"`
	Type  string    `json:"type,omitempty"`
	Key   string    `json:"key"`
	Cmd   string    `json:"cmd,omitempty"`
	At    time.Time `json:"at"`
}

// cliAskKey is the key of every shell ask (askKey).
const cliAskKey = "cli"

// trackAgent keeps hookState.Live for event of subagent id (type typ): a
// SubagentStart or a tool event makes it live, SubagentStop ends it, and
// agents silent for liveAgentTTL are dropped. It reports whether the set of
// live agents changed (the node is told, heartbeat).
func (st *hookState) trackAgent(event, id, typ string, now time.Time) bool {
	before := liveAgentIDs(*st)
	for a, l := range st.Live {
		if now.Sub(l.At) > liveAgentTTL {
			delete(st.Live, a)
		}
	}
	if id != "" && len(id) <= 128 {
		switch event {
		case evSubagentStop:
			delete(st.Live, id)
		case evSubagentStart, evPreTool, evPostTool, evPostToolFail:
			if _, ok := st.Live[id]; ok || len(st.Live) < maxLiveAgents {
				if st.Live == nil {
					st.Live = map[string]liveAgent{}
				}
				if len(typ) > 128 {
					typ = ""
				}
				st.Live[id] = liveAgent{Type: cmp.Or(typ, st.Live[id].Type), At: now}
			}
		}
	}
	return !slices.Equal(before, liveAgentIDs(*st))
}

// liveAgentIDs are the session's live subagents, sorted (SessionRequest.Agents).
func liveAgentIDs(st hookState) []string {
	return slices.Sorted(maps.Keys(st.Live))
}

// agentlinkAsk matches a shell command line that runs agentlink send or discuss.
var agentlinkAsk = regexp.MustCompile(`(?i)agentlink(\.exe|\.cmd)?["']?\s+(send|discuss)\b`)

// askKey is the stamp key of a tool call that asks through agent-link: the
// agentlink MCP server's send or discuss tool (by its input), or a shell
// command running agentlink send/discuss (cliAskKey, with its command line).
func askKey(tool string, input json.RawMessage) (key, cmd string, ok bool) {
	if strings.HasPrefix(tool, "mcp__") {
		i := strings.LastIndex(tool, "__")
		name, server := tool[i+2:], tool[len("mcp__"):i]
		if (name != "send" && name != "discuss") || !strings.HasSuffix(server, mcpServerName) {
			return "", "", false
		}
		h, ok := inputHash(input)
		return "mcp:" + name + ":" + h, "", ok
	}
	switch tool {
	case "Bash", "PowerShell":
	default:
		return "", "", false
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil || !agentlinkAsk.MatchString(in.Command) {
		return "", "", false
	}
	if len(in.Command) > askStampCmdMax {
		in.Command = in.Command[:askStampCmdMax]
	}
	return cliAskKey, in.Command, true
}

// mcpAskKey is askKey of the agentlink MCP tool name called with raw arguments.
func mcpAskKey(name string, args json.RawMessage) (string, bool) {
	h, ok := inputHash(args)
	if !ok {
		return "", false
	}
	return "mcp:" + name + ":" + h, true
}

// inputHash is the hash of a JSON tool input, whatever its key order and
// spacing: the hook's tool_input and the MCP server's arguments are the same
// value.
func inputHash(raw json.RawMessage) (string, bool) {
	var v any
	if len(bytes.TrimSpace(raw)) == 0 {
		v = map[string]any{}
	} else if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	canon, err := json.Marshal(v) // map keys sorted
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:16]), true
}

// stamp records an ask about to run (PreToolUse).
func (st *hookState) stamp(agent, typ, key, cmd string, now time.Time) {
	st.pruneStamps(now)
	if len(st.Stamps) >= maxAskStamps {
		st.Stamps = st.Stamps[1:]
	}
	if len(agent) > 128 {
		agent = ""
	}
	if len(typ) > 128 {
		typ = ""
	}
	st.Stamps = append(st.Stamps, askStamp{Agent: agent, Type: typ, Key: key, Cmd: cmd, At: now})
}

// dropStamp removes agent's stamp of key (PostToolUse of that call: a tool
// that did not take it, say with an explicit --session, leaves none behind).
func (st *hookState) dropStamp(agent, key, cmd string) {
	if i := slices.IndexFunc(st.Stamps, func(s askStamp) bool { return s.Agent == agent && s.Key == key && s.Cmd == cmd }); i >= 0 {
		st.Stamps = slices.Delete(st.Stamps, i, i+1)
	}
}

func (st *hookState) pruneStamps(now time.Time) {
	st.Stamps = slices.DeleteFunc(st.Stamps, func(s askStamp) bool {
		ttl := askStampTTL
		if s.Key == cliAskKey {
			ttl = cliAskStampTTL
		}
		return now.Sub(s.At) > ttl
	})
}

// dropAgentStamps removes every stamp of subagent agent (its SubagentStop:
// an ask it never ran must not name it later).
func (st *hookState) dropAgentStamps(agent string) {
	if agent == "" {
		return
	}
	st.Stamps = slices.DeleteFunc(st.Stamps, func(s askStamp) bool { return s.Agent == agent })
}

// takeStamp takes the oldest stamp matching the ask: of key, and for a shell
// ask (cliAskKey) one whose command line holds the whole body. It never
// guesses: no match, or matches of different agents (identical asks run
// concurrently, in an order the stamps need not show), take nothing, and the
// ask stays the session's.
func (st *hookState) takeStamp(key, body string, now time.Time) (askStamp, bool) {
	st.pruneStamps(now)
	body = strings.TrimSpace(body)
	if key == cliAskKey && body == "" {
		return askStamp{}, false // an ask from a file: nothing to match
	}
	match := func(s askStamp) bool {
		return s.Key == key && (key != cliAskKey || strings.Contains(s.Cmd, body))
	}
	i := slices.IndexFunc(st.Stamps, match)
	if i < 0 {
		return askStamp{}, false
	}
	s := st.Stamps[i]
	if slices.ContainsFunc(st.Stamps, func(o askStamp) bool { return match(o) && o.Agent != s.Agent }) {
		return askStamp{}, false
	}
	st.Stamps = slices.Delete(st.Stamps, i, i+1)
	return s, true
}

// hookStateDir is where the hooks keep their state (defaultHookEnv).
func hookStateDir() (string, error) {
	p, err := settings.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "hooks"), nil
}

// takeAskStamp is the subagent of Claude Code session sid that runs the ask
// of key (askKey) right now, from the stamp its PreToolUse left: "" for the
// main agent, or when no hook saw the call (older hooks, a Codex session).
func takeAskStamp(dir, sid, key, body string, now time.Time) (agent, typ string) {
	if dir == "" || sid == "" || key == "" {
		return "", ""
	}
	path := hookStatePath(dir, hookClaude, sid)
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return "", ""
	}
	defer unlock()
	st := loadHookState(path)
	if len(st.Stamps) == 0 {
		return "", ""
	}
	s, ok := st.takeStamp(key, body, now)
	_ = saveHookState(path, st)
	if !ok {
		return "", ""
	}
	return s.Agent, s.Type
}

// askOrigin is the subagent of this process's Claude Code session that runs
// the ask of key (takeAskStamp); none outside a Claude Code session.
func askOrigin(key, body string) (agent, typ string) {
	sid, client := agentSession()
	if client != hookClaude {
		return "", ""
	}
	dir, err := hookStateDir()
	if err != nil {
		return "", ""
	}
	return takeAskStamp(dir, sid, key, body, time.Now())
}

// agentQuery asks the node for this run's messages before it cuts the page
// (node.AgentFilter): a subagent's run only its replies (for_agent), the main
// agent's none of its live subagents' (skip_agents). An older node ignores
// both; forMe filters again.
func (h *hookSession) agentQuery(q url.Values) {
	switch {
	case h.agent != "":
		q.Set("for_agent", h.agent)
	case h.st != nil && len(h.st.Live) > 0:
		q.Set("skip_agents", strings.Join(liveAgentIDs(*h.st), ","))
	}
}

// forMe keeps the messages of page this hook run delivers: a subagent's run
// (h.agent) only the replies for it; the main agent's none for a live
// subagent, and the replies for an ended one marked as such.
func (h *hookSession) forMe(page node.UnreadPage) node.UnreadPage {
	n := len(page.Messages)
	kept := make([]node.UnreadMessage, 0, n)
	for _, m := range page.Messages {
		switch {
		case h.agent != "":
			if m.ForAgent != h.agent {
				continue
			}
		case m.ForAgent != "":
			if h.st != nil && h.isLive(m.ForAgent) {
				continue
			}
			m.Body = "[ответ для завершившегося субагента " + agentName(m.ForAgentType, m.ForAgent) + "]\n" + m.Body
		}
		kept = append(kept, m)
	}
	page.Messages = kept
	page.Total -= n - len(kept)
	return page
}

// isLive reports whether subagent id is live (hookState.Live).
func (h *hookSession) isLive(id string) bool {
	_, ok := h.st.Live[id]
	return ok
}

// agentName is how a subagent is named to the model: type/id.
func agentName(typ, id string) string {
	if typ == "" {
		return id
	}
	return typ + "/" + id
}
