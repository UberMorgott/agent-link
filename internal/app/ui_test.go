package app

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// --- the page dictionary ---

var (
	dataTKey   = regexp.MustCompile(`data-t="([^"]+)"`)
	titleKey   = regexp.MustCompile(`content="(page\.[^"]+)"`)
	scriptKey  = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)
	dynPrefix  = regexp.MustCompile(`"([a-z][a-z0-9_]*\.)"\s*\+`)
	cyrillic   = regexp.MustCompile(`\p{Cyrillic}`)
	englishOut = []string{
		"Settings", "Inbox", "Save", "Send", "Reply", "Replying", "Generate", "Copy", "Show",
		"This computer", "The other person", "Answering requests", "Shared secret", "Handler agent",
		"Working folder", "My name", "Their name", "My address", "Their address", "Message",
		"not configured", "offline", "is not running", "Queued", "Not sent", "Not saved", "Saved",
		"Could not load", "None", "Cancel",
	}
)

// webFiles returns every embedded page and script by name.
func webFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".css") {
			return err
		}
		data, err := webFS.ReadFile(p)
		files[p] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestUIStringsCoverPages checks that the pages and the dictionary agree: no
// key used by a page is missing, and no dictionary entry is dead.
func TestUIStringsCoverPages(t *testing.T) {
	used := map[string]bool{}
	prefixes := map[string]bool{}
	for name, body := range webFiles(t) {
		for _, re := range []*regexp.Regexp{dataTKey, titleKey, scriptKey} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				if _, ok := uiStrings[m[1]]; !ok {
					t.Errorf("%s uses key %q, missing from uiStrings", name, m[1])
				}
				used[m[1]] = true
			}
		}
		for _, m := range dynPrefix.FindAllStringSubmatch(body, -1) {
			prefixes[m[1]] = true
		}
	}
	// The server fills some text itself: status problems and error sentences.
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range goFiles {
		if strings.HasSuffix(name, "_test.go") || name == "strings.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range scriptKey.FindAllStringSubmatch(string(data), -1) {
			used[m[1]] = true
		}
		for _, m := range dynPrefix.FindAllStringSubmatch(string(data), -1) {
			prefixes[m[1]] = true
		}
	}
	for key := range uiStrings {
		if used[key] {
			continue
		}
		covered := false
		for p := range prefixes {
			if strings.HasPrefix(key, p) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("uiStrings key %q is never used by a page", key)
		}
	}
}

// TestUIStringsAreRussian checks the values themselves: Cyrillic text, no
// leftover English label.
func TestUIStringsAreRussian(t *testing.T) {
	// Brand names and a pure "{from} → {to}" template carry no words to translate.
	brand := map[string]bool{"settings.handler.claude": true, "settings.handler.codex": true, "inbox.route": true}
	for key, val := range uiStrings {
		if val == "" {
			t.Errorf("uiStrings[%q] is empty", key)
		}
		if !brand[key] && !cyrillic.MatchString(val) {
			t.Errorf("uiStrings[%q] = %q has no Russian text", key, val)
		}
	}
}

// TestPagesServeRussianText renders both pages and the scripts and checks that
// they carry the dictionary and no English user-visible text.
func TestPagesServeRussianText(t *testing.T) {
	h := newHarness(t)
	paths := []string{"/ui/settings", "/ui/inbox", "/ui/static/common.js", "/ui/static/inbox.js", "/ui/static/settings.js"}
	for _, p := range paths {
		code, body := h.do(t, http.MethodGet, p, "", nil)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", p, code)
		}
		for _, word := range englishOut {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(body) {
				t.Errorf("%s still shows English text %q", p, word)
			}
		}
	}
	for _, page := range []string{"/ui/settings", "/ui/inbox"} {
		_, body := h.do(t, http.MethodGet, page, "", nil)
		if strings.Contains(body, "{{STRINGS}}") {
			t.Fatalf("%s: dictionary not substituted", page)
		}
		if !strings.Contains(body, "&#34;nav.settings&#34;:&#34;Настройки&#34;") {
			t.Fatalf("%s: dictionary missing from the page", page)
		}
	}
}

// --- request/reply pairing ---

func entry(dir, id, from, to, body string, opts ...func(*node.Entry)) node.Entry {
	e := node.Entry{Direction: dir, Peer: to}
	if dir == "in" {
		e.Peer = from
	}
	e.Message = node.Message{ID: id, From: from, To: to, Body: body, CreatedAt: time.Unix(0, 0).UTC()}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func at(sec int64) func(*node.Entry) {
	return func(e *node.Entry) { e.CreatedAt = time.Unix(sec, 0).UTC() }
}
func replyTo(id string) func(*node.Entry) { return func(e *node.Entry) { e.ReplyTo = id } }
func job(s string) func(*node.Entry)      { return func(e *node.Entry) { e.JobStatus = s } }
func state(s string) func(*node.Entry)    { return func(e *node.Entry) { e.Status = s } }

func TestThreadsPairQuestionAndAnswer(t *testing.T) {
	req := strings.Repeat("a", 32)
	other := strings.Repeat("b", 32)
	cases := []struct {
		name    string
		entries []node.Entry
		want    []Thread
	}{{
		name: "outbound question with the peer's answer",
		entries: []node.Entry{
			entry("in", other, "nikita", "morgott", "готово", at(20), replyTo(req), job(node.JobCompleted)),
			entry("out", req, "morgott", "nikita", "что в файле?", at(10), state("sent"), job(node.JobCompleted)),
		},
		want: []Thread{{
			ID: req, Direction: "out", From: "morgott", To: "nikita", Body: "что в файле?",
			Status: node.JobCompleted, Answer: "готово", Answered: true,
		}},
	}, {
		name: "inbound question answered from this machine",
		entries: []node.Entry{
			entry("out", other, "morgott", "nikita", "мой ответ", at(30), replyTo(req), state("queued")),
			entry("in", req, "nikita", "morgott", "вопрос", at(10), state("delivered")),
		},
		want: []Thread{{
			ID: req, Direction: "in", From: "nikita", To: "morgott", Body: "вопрос",
			Status: statusAnswered, Answer: "мой ответ", Answered: true,
		}},
	}, {
		name: "unanswered inbound question can be replied to",
		entries: []node.Entry{
			entry("in", req, "nikita", "morgott", "вопрос", at(10), state("pending")),
		},
		want: []Thread{{
			ID: req, Direction: "in", From: "nikita", To: "morgott", Body: "вопрос",
			Status: "pending", Replyable: true,
		}},
	}, {
		name: "status updates never make a row",
		entries: []node.Entry{
			func() node.Entry {
				e := entry("in", other, "nikita", "morgott", "", at(15), replyTo(req), job(node.JobRunning))
				e.Kind = node.KindStatus
				return e
			}(),
			entry("out", req, "morgott", "nikita", "вопрос", at(10), state("sent"), job(node.JobRunning)),
		},
		want: []Thread{{
			ID: req, Direction: "out", From: "morgott", To: "nikita", Body: "вопрос",
			Status: node.JobRunning,
		}},
	}, {
		name: "a reply whose question is not listed keeps its own row",
		entries: []node.Entry{
			entry("in", other, "nikita", "morgott", "ответ", at(20), replyTo(req), job(node.JobFailed)),
		},
		want: []Thread{{
			ID: other, Direction: "in", From: "nikita", To: "morgott", Body: "ответ",
			Status: node.JobFailed,
		}},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := threads(c.entries)
			if len(got) != len(c.want) {
				t.Fatalf("got %d threads, want %d: %+v", len(got), len(c.want), got)
			}
			for i, w := range c.want {
				g := got[i]
				g.CreatedAt, g.AnswerAt = time.Time{}, nil
				if g != w {
					t.Errorf("thread %d:\n got %+v\nwant %+v", i, g, w)
				}
			}
		})
	}
}

func TestThreadsKeepTheNewestAnswerAndOrder(t *testing.T) {
	first, second, older := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	got := threads([]node.Entry{
		entry("out", first, "morgott", "nikita", "новый вопрос", at(100), state("queued")),
		entry("out", older, "morgott", "nikita", "старый вопрос", at(10), state("sent")),
		entry("in", second, "nikita", "morgott", "первый ответ", at(20), replyTo(older), job(node.JobRunning)),
		entry("in", strings.Repeat("d", 32), "nikita", "morgott", "второй ответ", at(30), replyTo(older), job(node.JobCompleted)),
	})
	if len(got) != 2 || got[0].ID != first || got[1].ID != older {
		t.Fatalf("order: %+v", got)
	}
	if got[1].Answer != "второй ответ" || got[1].Status != node.JobCompleted {
		t.Fatalf("newest answer not kept: %+v", got[1])
	}
	if got[1].AnswerAt == nil || !got[1].AnswerAt.Equal(time.Unix(30, 0).UTC()) {
		t.Fatalf("answer_at: %+v", got[1].AnswerAt)
	}
}

// A running question shows the peer agent's activity and the silence mark;
// an answered one shows neither.
func TestThreadsCarryActivityAndSilence(t *testing.T) {
	running, answered := strings.Repeat("a", 32), strings.Repeat("b", 32)
	live := func(e *node.Entry) { e.Activity, e.NoNewsMin = "Read docs/index.md", 7 }
	got := threads([]node.Entry{
		entry("out", running, "morgott", "nikita", "вопрос", at(100), job(node.JobRunning), live),
		entry("out", answered, "morgott", "nikita", "другой", at(50), job(node.JobCompleted), live, func(e *node.Entry) { e.Answer = "ok" }),
	})
	if got[0].Activity != "Read docs/index.md" || got[0].NoNewsMin != 7 {
		t.Fatalf("running thread: %+v", got[0])
	}
	if got[1].Activity != "" || got[1].NoNewsMin != 0 {
		t.Fatalf("answered thread: %+v", got[1])
	}
}

func TestThreadsEndpointServesPairs(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(t, http.MethodGet, "/ui/api/threads", "", h.tokenHdr()); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("threads before setup: %d %s", code, body)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/send", `{"to":"bob","body":"вопрос"}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	code, body := h.do(t, http.MethodGet, "/ui/api/threads", "", h.tokenHdr())
	var got []Thread
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != http.StatusOK {
		t.Fatalf("threads: %d %s err=%v", code, body, err)
	}
	if len(got) != 1 || got[0].Direction != "out" || got[0].From != "alice" || got[0].To != "bob" ||
		got[0].Body != "вопрос" || got[0].Answered {
		t.Fatalf("thread %+v", got)
	}
}
