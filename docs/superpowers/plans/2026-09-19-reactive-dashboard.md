# Reactive Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the current settings/inbox pages with a polished, fully reactive dashboard for overview, conversations, remote participants, settings, notifications, and tray-driven tab reuse.

**Architecture:** Keep the Go server, embedded assets, durable message store, and vanilla JavaScript. Add application-layer view models that aggregate existing messages and membership data, then serve one reactive HTML shell for all UI routes. One authenticated SSE stream publishes mutation topics from the Go process; small JavaScript modules refresh only affected slices and render individual views without a build step or scheduled polling.

**Tech Stack:** Go 1.27+, `net/http`, embedded HTML/CSS/vanilla JavaScript, PowerShell 7 end-to-end scripts, Windows tray app.

**Spec:** `docs/superpowers/specs/2026-09-19-dashboard-redesign-design.md`

## Global Constraints

- Do not add a frontend framework, build step, database, third-party service, or wire-protocol field.
- Preserve the loopback-only control API, token middleware, DNS-rebinding protection, CSP, and read-only agent limits.
- Render user-controlled data with `textContent`; never inject message, participant, address, activity, or error text as HTML.
- Continue sourcing every user-visible string from `internal/app/strings.go`.
- Preserve full local message history and existing request/reply/status semantics; status frames do not count as messages.
- Keep ordinary UI workflows reactive: no document reload except restart/update, expired token, or unrecoverable shell-version mismatch.
- Preserve drafts, focus, selected conversation, and scroll position across event-driven refreshes.
- UI data must not be refreshed on a timer. SSE keepalives may hold the connection open but must not trigger API reads.
- Use the browser-tab launcher as same-profile best effort; never add browser automation, an extension, or WebView2.
- Run `qgate` after file changes. If the gate is provably incorrect, do not bypass it: capture `qgate where` and open an issue in `UberMorgott/quality-gate`.
- Commit only intended paths using Conventional Commits; never push unless explicitly requested.

## Review Focus

- Peer names containing spaces, Cyrillic, `&`, `?`, or `#` must survive query routing via `URLSearchParams` and render as text; Task 6 adds a browser verification and Task 2 pins server filtering.
- A peer with more than 200 stored entries must expose complete history and correct counts; Tasks 1 and 2 use fixtures above that boundary.
- Push events during a multiline draft or while reading older history must not reset the composer, focus, selected peer, or scroll position; Task 4 adds the event/store contract and Task 6 verifies it live.
- Stale launcher heartbeats, blocked popups, and an absent named tab must leave the user in one usable dashboard tab; Task 9 tests the launcher decision function and verifies browser fallback.
- Node stop/restart and token rotation must retain the last good view, show one compact error, and reload only on 403; Tasks 4 and 10 cover these transitions.

---

## File Structure

- `internal/app/dashboard.go`: pure aggregation of status, participants, message counts, active work, and conversation summaries.
- `internal/app/dashboard_test.go`: table-driven aggregation and boundary tests.
- `internal/app/threads.go`: request/reply view model plus peer filtering helpers.
- `internal/app/web.go`: UI routes, JSON handlers, and the authenticated SSE endpoint; it delegates aggregation and rendering.
- `internal/app/events.go`: non-blocking coalescing UI event broadcaster owned by `App`.
- `internal/app/web/app.html`: the one application shell and semantic containers for all views.
- `internal/app/web/open.html`: tiny same-profile tab-reuse launcher.
- `internal/app/web/static/common.js`: token-aware API client, translations, formatting, safe DOM helpers, and URL routing.
- `internal/app/web/static/app.js`: central state store, SSE lifecycle, view lifecycle, toast dispatch, and startup.
- `internal/app/web/static/overview.js`: overview projection and DOM renderer.
- `internal/app/web/static/inbox.js`: conversation list, message timeline, composer, and reply actions.
- `internal/app/web/static/participants.js`: participant list, statistics, add/remove actions, and open-chat actions.
- `internal/app/web/static/settings.js`: reactive settings, agent/folder pickers, updates, and save actions; no participant management.
- `internal/app/web/static/open.js`: launcher decision and named-window activation.
- `internal/app/web/static/style.css`: complete responsive visual system for the shared shell and all views.
- `internal/app/strings.go`: Russian copy for navigation, dashboard, conversations, participants, toasts, and errors.
- `internal/app/ui_test.go`: shell/routes/string contracts and conversation filtering.
- `internal/app/app_test.go`: API token/route contracts.
- `internal/worker/command.go`: explicit remote-computer handler context.
- `internal/worker/command_test.go`: prompt contract and unchanged read-only resume flags.
- `cmd/agentlink/app.go`: tray targets the launcher route.
- `cmd/agentlink/main_test.go`: tray URL/debounce contract.
- `scripts/e2e-tray.ps1`: reactive dashboard smoke flow on two local nodes.

### Task 1: Aggregate Dashboard and Participant Data

**Files:**

- Create: `internal/app/dashboard.go`
- Create: `internal/app/dashboard_test.go`
- Modify: `internal/app/threads.go`

**Interfaces:**

- Consumes: `Status`, `node.MemberInfo`, `node.Entry`, and `threads([]node.Entry) []Thread`.
- Produces: `buildDashboard(Status, []node.Entry) DashboardSummary`, `buildParticipants(Status, []node.Entry) []ParticipantView`, `threadPeer(Thread, local string) string`, and `filterThreads([]Thread, peer, local string) []Thread`.

- [ ] **Step 1: Write failing aggregation tests**

Add fixtures that cover two peers, inbound/outbound requests and replies, one status frame, an offline member, a running request, and 205 entries for one peer. Assert literal counts and ordering:

```go
func TestBuildParticipantsCountsRealMessagesOnly(t *testing.T) {
    status := Status{Node: "alice", Members: []node.MemberInfo{
        {Name: "alice", Self: true, Online: true},
        {Name: "bob", Online: true},
        {Name: "карл", Online: false, Seen: time.Unix(40, 0).UTC()},
    }}
    entries := []node.Entry{
        entry("out", strings.Repeat("a", 32), "alice", "bob", "q", at(10)),
        entry("in", strings.Repeat("b", 32), "bob", "alice", "a", at(20), replyTo(strings.Repeat("a", 32))),
        func() node.Entry {
            e := entry("in", strings.Repeat("c", 32), "bob", "alice", "", at(15))
            e.Kind = node.KindStatus
            return e
        }(),
    }
    got := buildParticipants(status, entries)
    if len(got) != 2 || got[0].Name != "bob" || got[0].Sent != 1 || got[0].Received != 1 || got[0].Total != 2 {
        t.Fatalf("participants: %+v", got)
    }
}

func TestBuildDashboardKeepsAllHistoryBeyondRecentPage(t *testing.T) {
    entries := make([]node.Entry, 205)
    for i := range entries {
        entries[i] = entry("out", fmt.Sprintf("%032x", i+1), "alice", "bob", fmt.Sprintf("m%d", i), at(int64(i)))
    }
    got := buildDashboard(Status{Node: "alice"}, entries)
    if got.TotalMessages != 205 || got.SentMessages != 205 || len(got.Recent) != 1 {
        t.Fatalf("summary: %+v", got)
    }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./internal/app -run 'TestBuild(Participants|Dashboard)'`

Expected: compilation fails because `buildParticipants`, `buildDashboard`, `ParticipantView`, and `DashboardSummary` do not exist.

- [ ] **Step 3: Implement the pure view models**

Define stable JSON types and derive them in one pass over entries:

```go
type ConversationSummary struct {
    Peer      string    `json:"peer"`
    Preview   string    `json:"preview"`
    LatestAt  time.Time `json:"latest_at"`
    Direction string    `json:"direction"`
    Status    string    `json:"status,omitempty"`
}

type ParticipantView struct {
    node.MemberInfo
    Sent          int       `json:"sent"`
    Received      int       `json:"received"`
    Total         int       `json:"total"`
    LatestAt      time.Time `json:"latest_at,omitzero"`
    LatestPreview string    `json:"latest_preview,omitempty"`
}

type DashboardSummary struct {
    Status           Status                `json:"status"`
    SentMessages     int                   `json:"sent_messages"`
    ReceivedMessages int                   `json:"received_messages"`
    TotalMessages    int                   `json:"total_messages"`
    ActiveRequests   int                   `json:"active_requests"`
    Recent           []ConversationSummary `json:"recent"`
}
```

Exclude `node.KindStatus`; use `Entry.Peer` as the durable remote identity; cap `DashboardSummary.Recent` at five peers after sorting by latest activity; exclude `MemberInfo.Self` from participants.

- [ ] **Step 4: Add peer-filter tests and implementation**

Test `threadPeer` for inbound, outbound, reply-only, and area threads. Test `filterThreads` with peer `карл & sons` and an empty peer. Implement exact equality filtering without lowercasing or URL decoding inside the domain helper.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `go test ./internal/app -run 'Test(Build|ThreadPeer|FilterThreads)'`

Expected: PASS.

- [ ] **Step 6: Commit the aggregation slice**

```powershell
git add internal/app/dashboard.go internal/app/dashboard_test.go internal/app/threads.go
git commit -m "feat(app): aggregate dashboard activity"
```

### Task 2: Serve Complete Conversations and Dashboard APIs

**Files:**

- Modify: `internal/app/web.go`
- Modify: `internal/app/ui_test.go`
- Modify: `internal/app/app_test.go`

**Interfaces:**

- Consumes: Task 1's `buildDashboard`, `buildParticipants`, and `filterThreads`.
- Produces: `(*App).recent(limit int) ([]node.Entry, error)`, `GET /ui/api/dashboard`, `GET /ui/api/participants`, and peer-aware `GET /ui/api/threads?peer=<name>`.

- [ ] **Step 1: Write failing endpoint tests**

Extend the existing harness tests to assert:

```go
func TestDashboardAPIsRequireToken(t *testing.T) {
    h := newHarness(t)
    for _, path := range []string{"/ui/api/dashboard", "/ui/api/participants", "/ui/api/threads?peer=bob"} {
        if code, _ := h.do(t, http.MethodGet, path, "", nil); code != http.StatusForbidden {
            t.Errorf("%s: %d, want 403", path, code)
        }
    }
}

func TestThreadsEndpointFiltersDecodedPeerAndUsesCompleteHistory(t *testing.T) {
    entries := make([]node.Entry, 0, 206)
    for i := range 205 {
        entries = append(entries, entry("out", fmt.Sprintf("%032x", i+1), "alice", "карл & sons", fmt.Sprintf("m%d", i), at(int64(i))))
    }
    entries = append(entries, entry("out", strings.Repeat("f", 32), "alice", "bob", "other", at(999)))
    got := filterThreads(threads(entries), "карл & sons", "alice")
    if len(got) != 205 {
        t.Fatalf("got %d filtered threads, want 205", len(got))
    }
}
```

The pure 205-entry boundary belongs in this test. Add a second HTTP harness
assertion using `url.QueryEscape("карл & sons")` and a real sent message to
prove query decoding reaches the same filter without mocking `node.Node`.

- [ ] **Step 2: Run endpoint tests and verify RED**

Run: `go test ./internal/app -run 'Test(DashboardAPIs|ThreadsEndpointFilters)'`

Expected: `/dashboard` and `/participants` return 404, and `/threads` ignores `peer` or truncates at 200.

- [ ] **Step 3: Implement shared history loading and handlers**

Add:

```go
func (a *App) recent(limit int) ([]node.Entry, error) {
    n := a.node()
    if n == nil {
        return []node.Entry{}, nil
    }
    entries, err := n.Recent(limit)
    if entries == nil {
        entries = []node.Entry{}
    }
    return entries, err
}
```

Register dashboard and participant routes under the existing token-protected mux. For a non-empty `peer`, call `recent(0)` then `filterThreads`; for the unfiltered compatibility feed keep `recent(200)`. Log storage failures once and return the existing localized internal error.

- [ ] **Step 4: Pin stopped-node and malformed-query behavior**

Add tests proving unconfigured apps return empty arrays/zero summaries with HTTP 200, an empty `peer=` behaves like the unfiltered feed, and `peer=%26%3F%23` reaches the exact decoded name without a server error.

- [ ] **Step 5: Run the app suite and verify GREEN**

Run: `go test ./internal/app`

Expected: PASS.

- [ ] **Step 6: Commit the API slice**

```powershell
git add internal/app/web.go internal/app/ui_test.go internal/app/app_test.go
git commit -m "feat(app): expose dashboard conversation APIs"
```

### Task 3: Replace Multiple Pages with One Application Shell

**Files:**

- Create: `internal/app/web/app.html`
- Create: `internal/app/web/static/app.js`
- Create: `internal/app/web/static/overview.js`
- Create: `internal/app/web/static/participants.js`
- Modify: `internal/app/web/static/common.js`
- Modify: `internal/app/web.go`
- Modify: `internal/app/strings.go`
- Modify: `internal/app/ui_test.go`
- Delete: `internal/app/web/inbox.html`
- Delete: `internal/app/web/settings.html`

**Interfaces:**

- Consumes: existing token/string embedding from `(*App).page` and Task 2 APIs.
- Produces: one shell served at `/ui/dashboard`, `/ui/inbox`, `/ui/participants`, and `/ui/settings`; `routeFromPath(pathname)`, `navigate(route, query)`, and `renderRoute(state)`.

- [ ] **Step 1: Write failing shell-route tests**

Replace the two-page assertions with a four-route table. Each page must return the same shell marker and embedded token, while `/` redirects to `/ui/dashboard`:

```go
for _, path := range []string{"/ui/dashboard", "/ui/inbox", "/ui/participants", "/ui/settings"} {
    code, body := h.do(t, http.MethodGet, path, "", nil)
    if code != http.StatusOK || !strings.Contains(body, `id="app-shell"`) || !strings.Contains(body, h.app.token) {
        t.Fatalf("%s: %d", path, code)
    }
}
```

Extend string coverage to every new embedded JavaScript file and require that obsolete `settings.members.*` keys disappear unless still used by participant rendering.

- [ ] **Step 2: Run shell tests and verify RED**

Run: `go test ./internal/app -run 'Test(Page|Pages|UIStrings)'`

Expected: `/ui/dashboard` and `/ui/participants` return 404 and no app shell exists.

- [ ] **Step 3: Build the semantic shell and routing**

Create one HTML document with:

```html
<body>
  <div id="app-shell" class="app-shell">
    <aside id="sidebar" aria-label="Основная навигация">...</aside>
    <section class="workspace">
      <header id="topbar">...</header>
      <main id="view" tabindex="-1"></main>
    </section>
    <div id="toast-region" role="status" aria-live="polite"></div>
  </div>
  <script src="/ui/static/common.js"></script>
  <script src="/ui/static/overview.js"></script>
  <script src="/ui/static/inbox.js"></script>
  <script src="/ui/static/participants.js"></script>
  <script src="/ui/static/settings.js"></script>
  <script src="/ui/static/app.js"></script>
</body>
```

Use real links as progressive fallback. Intercept same-origin UI links, call `history.pushState`, and handle `popstate`. Never use `innerHTML` for dynamic remote data; static view scaffolding may be created from `<template>` elements already embedded in `app.html`.

- [ ] **Step 4: Serve the shell from all routes**

Point all four GET routes at `a.page("web/app.html")`; redirect `/` to `/ui/dashboard`; update the handler comment and page title key. Remove the obsolete HTML files only after the route test passes against the shell.

- [ ] **Step 5: Run route and localization tests**

Run: `go test ./internal/app -run 'Test(Page|Pages|UIStrings)'`

Expected: PASS with no dead or untranslated keys.

- [ ] **Step 6: Commit the shell slice**

```powershell
git add internal/app/web.go internal/app/web/app.html internal/app/web/static/common.js internal/app/web/static/app.js internal/app/web/static/overview.js internal/app/web/static/participants.js internal/app/strings.go internal/app/ui_test.go
git rm internal/app/web/inbox.html internal/app/web/settings.html
git commit -m "feat(ui): add reactive dashboard shell"
```

### Task 4: Add Server-Push Reactivity, the Store, and Overview

**Files:**

- Modify: `internal/app/web/static/app.js`
- Modify: `internal/app/web/static/common.js`
- Modify: `internal/app/web/static/inbox.js`
- Modify: `internal/app/web/static/settings.js`
- Modify: `internal/app/web/static/overview.js`
- Modify: `internal/app/web/app.html`
- Modify: `internal/app/strings.go`
- Modify: `internal/app/ui_test.go`
- Create: `internal/app/events.go`
- Create: `internal/app/events_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/app/members.go`
- Modify: `internal/app/autostart.go`
- Modify: `internal/app/update.go`
- Modify: `internal/app/web.go`
- Modify: `internal/node/node.go`
- Modify: `internal/node/node_test.go`
- Modify: `internal/node/conn.go`
- Modify: `internal/node/members.go`
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`

**Interfaces:**

- Consumes: node/member/message/worker/settings/update mutation callbacks, `GET status`, `GET dashboard`, `GET participants`, `GET update`, and route helpers from Task 3.
- Produces: authenticated `GET /ui/api/events`, a coalescing revision/topic broadcaster, `createStore(initial)`, `store.get()`, `store.patch(slice, value)`, `store.subscribe(slice, listener)`, event-driven slice refresh, and `renderOverview(summary)`.

- [ ] **Step 1: Add failing static contracts for reactive state**

Extend `ui_test.go` to parse the rendered shell and assert one shell, one
`aria-live` region, and real links for all four routes. Do not grep JavaScript
implementation text; pin changing data through the HTTP integration behavior
below and verify browser state in Step 5.

Add HTTP integration tests proving `/ui/api/events` rejects missing/wrong tokens, immediately emits an initial event, emits a higher revision after representative mutations, unsubscribes on cancellation, and coalesces for a slow subscriber. Add an executable JavaScript contract proving there is no periodic data refresh and an SSE event refreshes only its named slices.

- [ ] **Step 2: Run the focused tests and verify RED**

Run: `go test ./internal/app -run 'Test(Reactive|DashboardRefresh|UIShell)'`

Expected: missing broadcaster/SSE/store contract.

- [ ] **Step 3: Implement the event broadcaster and mutation hooks**

Own one broadcaster on `App`. Subscribers receive a buffered, coalesced revision
and topic set; publishing must never block node, worker, or HTTP paths. Invoke
callbacks only after durable mutations and outside node/worker/app locks.

Publish topics for peer connect/disconnect, member-table changes, inbound and
outbound message persistence/delivery, worker request/activity transitions,
successful settings/member mutations, and update state changes. Do not change
the network wire protocol.

Serve `GET /ui/api/events` behind the existing token middleware. Stream an
initial `event: change` record, later revision/topic records, and comment-only
keepalives about every 15 seconds. Do not put the control token in the URL.

- [ ] **Step 4: Implement store and stream lifecycle**

Use one state object and slice subscriptions:

```javascript
function createStore(initial) {
  let state = Object.assign({}, initial);
  const listeners = new Map();
  return {
    get: () => state,
    patch(name, value) {
      if (Object.is(state[name], value)) return;
      state = Object.assign({}, state, { [name]: value });
      for (const fn of listeners.get(name) || []) fn(value, state);
    },
    subscribe(name, fn) {
      if (!listeners.has(name)) listeners.set(name, new Set());
      listeners.get(name).add(fn);
      return () => listeners.get(name).delete(fn);
    },
  };
}
```

Open the stream with `fetch` and `X-Agentlink-Token`, parse SSE records, and
refresh only the slices named by each event. Initial connection/reconnection
refreshes every slice. Reconnect with bounded exponential backoff; preserve the
last good slice on failure. Convert HTTP 403 into a single localized reload
action; other failures update one connection banner without stacking errors.
There must be no `setInterval` or timer-driven data refresh in `common.js`,
`inbox.js`, or `settings.js`.

- [ ] **Step 5: Render the overview and count-based top bar**

Render cards for online/total participants, messages, active requests, handler, and recent conversations. The top bar must use `online` count, never `Status.Peer`. Route clicks through `navigate` without a document reload.

- [ ] **Step 6: Verify push behavior in the local browser**

Run the app with a temporary config on an unused loopback port, open `/ui/dashboard`, and use browser developer observation to confirm:

- changing the route updates `location.pathname` and the main view without a navigation load;
- status text says the numeric online count;
- stopping the peer leaves the last cards visible and updates status from a pushed event;
- only one connection warning is present.
- browser network activity shows one long-lived events request and no periodic API requests.

Record the observed route and status in the task notes; do not submit forms or alter the user's real config.

- [ ] **Step 7: Run focused tests and commit**

Run: `go test ./internal/app`

Expected: PASS.

```powershell
git add internal/app/web/app.html internal/app/web/static/app.js internal/app/web/static/common.js internal/app/web/static/overview.js internal/app/strings.go internal/app/ui_test.go
git commit -m "feat(ui): react to dashboard state changes"
```

### Task 5: Build the Participants View and Move Network Management

**Files:**

- Modify: `internal/app/web/static/participants.js`
- Modify: `internal/app/web/static/settings.js`
- Modify: `internal/app/web/app.html`
- Modify: `internal/app/web/static/style.css`
- Modify: `internal/app/strings.go`
- Modify: `internal/app/ui_test.go`
- Modify: `internal/app/members_test.go`

**Interfaces:**

- Consumes: `ParticipantView[]`, `POST members/add`, `POST members/remove`, `navigate("inbox", {peer})`.
- Produces: `renderParticipants(items)`, `addParticipant(addr)`, and `removeParticipant(name)`.

- [ ] **Step 1: Write failing participant surface tests**

Assert the API never returns `Self`, preserves online/last-seen/version/address fields, and reports the literal sent/received totals from Task 1. Update embedded UI tests to require participant controls in the participant template and forbid `id="members"`, `id="add_peer"`, and `name="peer_addr"` inside the settings template.

- [ ] **Step 2: Run participant tests and verify RED**

Run: `go test ./internal/app -run 'Test(Participants|Member|SettingsHasNoParticipants)'`

Expected: the settings shell still contains the old controls or the participant view is absent.

- [ ] **Step 3: Implement participant rendering and actions**

Render each participant as a button-like primary row plus separate secondary actions. Use `encodeURIComponent` or `URLSearchParams` for chat navigation. Keep the existing confirmation before removal and return focus to the next logical row after removal. Update the store from returned API status, then refresh participants and summary immediately.

- [ ] **Step 4: Remove participant code from settings**

Delete `showMembers`, `showMyAddr` member rendering, remove/add handlers, and the associated settings markup. Keep the local advertised address under advanced network settings because it is configuration, not participant membership.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/app`

Expected: PASS.

```powershell
git add internal/app/web/app.html internal/app/web/static/participants.js internal/app/web/static/settings.js internal/app/web/static/style.css internal/app/strings.go internal/app/ui_test.go internal/app/members_test.go
git commit -m "feat(ui): add participant management view"
```

### Task 6: Turn Messages into Reactive Conversations and Add Toasts

**Files:**

- Modify: `internal/app/web/static/inbox.js`
- Modify: `internal/app/web/static/app.js`
- Modify: `internal/app/web/static/common.js`
- Modify: `internal/app/web/app.html`
- Modify: `internal/app/web/static/style.css`
- Modify: `internal/app/strings.go`
- Modify: `internal/app/ui_test.go`
- Modify: `scripts/e2e-tray.ps1`

**Interfaces:**

- Consumes: `GET threads?peer=`, `POST send`, participant state, router, and store.
- Produces: `selectConversation(peer, messageID)`, `renderConversationList`, `renderTimeline`, `submitMessage`, `detectIncoming(previousIDs, threads)`, and `showMessageToast(thread)`.

- [ ] **Step 1: Write failing conversation/history tests**

Add Go integration coverage for peer filtering with reserved characters and 205 stored entries. Extend the tray E2E fixture so one inbound message arrives after the dashboard is already open; assert the API exposes exactly one new inbound ID for the browser flow.

- [ ] **Step 2: Run conversation tests and verify RED**

Run: `go test ./internal/app -run 'Test(ThreadsEndpoint|CompleteConversation)'`

Expected: filtered complete-history behavior or reserved-name behavior fails until Task 2/6 integration is complete.

- [ ] **Step 3: Implement conversation list and timeline**

Keep selection in `store.selectedPeer`; fetch full history only for that peer. Patch existing message nodes by ID instead of replacing the timeline. Preserve scroll unless the user is within 80 px of the bottom or the selected `message` anchor requires scrolling. Keep composer text in `store.drafts[peer]` and restore it when switching conversations.

Render replies/status/activity as metadata attached to the owning request. Use `textContent` for all body and metadata fields. Prevent duplicate sends while the POST is active; on success clear only the selected peer's draft, patch the returned message, and refresh that conversation.

- [ ] **Step 4: Implement notification watermark and toast behavior**

Use a versioned localStorage key scoped by local node name, for example `agentlink.notifications.v1:<node>`. On the first successful fetch, seed all current incoming non-status IDs and show nothing. Later, compute unseen IDs, persist the bounded newest set, and create at most three visible toasts. Preview at most 120 Unicode code points and append `…` when truncated.

Clicking a toast must call `navigate("inbox", {peer, message})` in the same tab, select the conversation, and focus/scroll the matching message. Do not request Notification API permission.

- [ ] **Step 5: Verify reactive preservation and notification behavior in browser**

Using temporary local nodes:

- type a multiline draft, place the caret in the middle, trigger two remote events, and confirm value/focus/caret remain;
- scroll upward, trigger a status update, and confirm scroll does not jump;
- send a message from the other node and confirm one bottom-right toast contains sender plus a truncated preview;
- click it and confirm the same tab shows the correct participant and message;
- navigate with `?peer=%D0%BA%D0%B0%D1%80%D0%BB+%26+sons&message=<id>` and confirm exact selection.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/app`

Expected: PASS.

```powershell
git add internal/app/web/app.html internal/app/web/static/app.js internal/app/web/static/common.js internal/app/web/static/inbox.js internal/app/web/static/style.css internal/app/strings.go internal/app/ui_test.go scripts/e2e-tray.ps1
git commit -m "feat(ui): add reactive conversations and alerts"
```

### Task 7: Finish Reactive Settings and the Visual System

**Files:**

- Modify: `internal/app/web/static/settings.js`
- Modify: `internal/app/web/static/style.css`
- Modify: `internal/app/web/app.html`
- Modify: `internal/app/strings.go`
- Modify: `internal/app/ui_test.go`

**Interfaces:**

- Consumes: existing settings/agent/picker/update endpoints and shared store.
- Produces: reactive settings cards, busy/error states, and responsive light/dark dashboard layouts.

- [ ] **Step 1: Add failing semantic/accessibility contracts**

Require one `h1` per active view template, labelled navigation, labelled form controls, an `aria-live` result region, keyboard-focusable conversation and participant rows, and no `<fieldset>` used solely for visual layout. Keep existing Russian/dead-string checks.

- [ ] **Step 2: Run UI contracts and verify RED**

Run: `go test ./internal/app -run 'Test(UI|Pages|Settings)'`

Expected: old fieldset structure or missing shared accessibility markers fails.

- [ ] **Step 3: Make settings mutations reactive**

Split settings into Identity and pairing, Agent handler, Application, Updates, and Advanced cards. Preserve picker and update flows. After save, patch settings/status/summary from responses and keep the active route. Reload only when the API address changes or the update flow detects the new token after restart.

- [ ] **Step 4: Implement the complete CSS system**

Define tokens for canvas, sidebar, surface, elevated surface, text, muted text, border, accent, success, warning, danger, radius, and shadow under light/dark media queries. Implement desktop sidebar/two-pane conversations, the 900 px compact breakpoint, and the 640 px single-column navigation/message breakpoint. Add `:focus-visible`, reduced-motion handling, touch-sized primary controls, bounded toast stacking, and stable scroll containers.

- [ ] **Step 5: Visually verify desktop and narrow layouts**

Inspect Overview, Messages, Participants, and Settings at approximately 1440x900, 1024x768, and 390x844 in both supported color schemes. Confirm no horizontal overflow, clipped buttons, overlapping toasts, unreadable status colors, or hidden composer. Capture screenshots as temporary evidence; do not commit them.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/app`

Expected: PASS.

```powershell
git add internal/app/web/app.html internal/app/web/static/settings.js internal/app/web/static/style.css internal/app/strings.go internal/app/ui_test.go
git commit -m "feat(ui): polish reactive dashboard"
```

### Task 8: Clarify the Remote Agent Prompt

**Files:**

- Modify: `internal/worker/command.go`
- Modify: `internal/worker/command_test.go`

**Interfaces:**

- Consumes: built-in Claude/Codex `Command.Preamble` and stdin-only prompt delivery.
- Produces: explicit remote-computer role framing while preserving `ReplyStyle`, read-only flags, and resume behavior.

- [ ] **Step 1: Write a failing behavior contract**

Strengthen `TestBuiltInAgentsCarryReplyStyle` to require semantic facts, not exact prose:

```go
for _, phrase := range []string{
    "receiving computer",
    "another trusted developer's computer",
    "automatically sent back",
    "read-only",
    "Never include secrets",
} {
    if !strings.Contains(c.Preamble, phrase) {
        t.Errorf("%s preamble lacks %q", h, phrase)
    }
}
```

Retain `TestBuiltInAgentsResumeReadOnly` unchanged.

- [ ] **Step 2: Run the prompt test and verify RED**

Run: `go test ./internal/worker -run 'TestBuiltInAgents(CarryReplyStyle|ResumeReadOnly)'`

Expected: at least `receiving computer` and `automatically sent back` are absent.

- [ ] **Step 3: Rewrite `ReplyStyle` minimally**

The preamble must say the model is the handler on this receiving computer, the request came through agent-link from another trusted developer's machine, and the final answer is automatically returned. Keep language-selection, terse agent formatting, path/line evidence, read-only, and secret rules. Keep `Request:` as the final line before the untrusted body.

- [ ] **Step 4: Run worker tests and commit**

Run: `go test ./internal/worker`

Expected: PASS.

```powershell
git add internal/worker/command.go internal/worker/command_test.go
git commit -m "feat(worker): clarify remote agent context"
```

### Task 9: Reuse an Existing Dashboard Tab from the Tray

**Files:**

- Create: `internal/app/web/open.html`
- Create: `internal/app/web/static/open.js`
- Modify: `internal/app/web.go`
- Modify: `cmd/agentlink/app.go`
- Modify: `cmd/agentlink/main_test.go`
- Modify: `internal/app/app_test.go`

**Interfaces:**

- Consumes: `(*App).URL(page)`, current tray debounce, shared dashboard route.
- Produces: `/ui/open`, `launcherDecision(now, heartbeat)`, stable window name `agentlink-dashboard`, and tray calls to `a.URL("open")`.

- [ ] **Step 1: Write failing launcher and tray tests**

Add route coverage requiring `/ui/open` to embed no API token or private data and to load only same-origin `open.js`. Extract the tray destination behind a tiny pure helper so the test pins the route:

```go
func dashboardURL(a *app.App) string { return a.URL("open") }

func TestDashboardURLUsesLauncher(t *testing.T) {
    // Construct an app on a fixed loopback API address and assert the exact
    // http://127.0.0.1:<port>/ui/open URL.
}
```

Keep `TestClickDebounce` to prove double-click suppression.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./cmd/agentlink ./internal/app -run 'Test(DashboardURL|OpenPage|ClickDebounce)'`

Expected: `/ui/open` is 404 and the tray still targets settings.

- [ ] **Step 3: Implement the launcher decision and fallback**

The shell sets `window.name = "agentlink-dashboard"`, updates a timestamp heartbeat in `localStorage`, and listens for `storage`/`BroadcastChannel` activation messages. `open.js` reads the heartbeat:

```javascript
function launcherDecision(now, heartbeat) {
  const seen = Number(heartbeat);
  return Number.isFinite(seen) && now - seen >= 0 && now - seen < 5000
    ? "activate"
    : "adopt";
}
```

For `activate`, call `window.open("/ui/dashboard", "agentlink-dashboard")`, focus the returned context, signal route activation, and close the launcher when allowed. If the call returns null or the launcher cannot close, `location.replace("/ui/dashboard")`. For `adopt`, set the stable name on the launcher and replace its location with the dashboard. Never interpolate query data into script source.

- [ ] **Step 4: Point every tray/open path at the launcher**

Replace startup, left-click, and menu calls to `a.URL("settings")` with `dashboardURL(a)`. Preserve the existing one-second debounce and the settings-on-first-run behavior inside the dashboard via its warning/action state rather than a separate URL.

- [ ] **Step 5: Verify same-profile reuse and fallback**

With a temporary app instance:

- click the tray once with no dashboard open and confirm one dashboard tab;
- click again and confirm the existing named tab becomes active without a persistent second tab;
- disable popups for the local origin or simulate `window.open` returning null and confirm the launcher tab becomes a working dashboard;
- double-click the tray and confirm the debounce still yields one open action.

Document browser-specific limitations in README only if observed behavior differs materially from the spec.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./cmd/agentlink ./internal/app`

Expected: PASS.

```powershell
git add internal/app/web.go internal/app/web/open.html internal/app/web/static/open.js internal/app/app_test.go cmd/agentlink/app.go cmd/agentlink/main_test.go
git commit -m "feat(tray): reuse the dashboard browser tab"
```

### Task 10: End-to-End Verification and Final Integration

**Files:**

- Modify if evidence requires it: `scripts/e2e-tray.ps1`
- Modify if user-visible behavior changed beyond existing coverage: `README.md`

**Interfaces:**

- Consumes: the complete dashboard, existing local two-node scripts, and repository quality gate.
- Produces: fresh passing evidence for backend, worker, tray, reactive UI, and full repository checks.

- [ ] **Step 1: Run focused package tests with race detection**

Run:

```powershell
go test -race ./internal/app ./internal/worker ./cmd/agentlink
```

Expected: PASS with zero races and zero failures.

- [ ] **Step 2: Run the complete Go suite**

Run:

```powershell
go test -race ./...
```

Expected: PASS.

- [ ] **Step 3: Run affected end-to-end scripts**

Run:

```powershell
pwsh -File scripts/e2e-local.ps1
pwsh -File scripts/e2e-worker.ps1
pwsh -File scripts/e2e-parallel.ps1
pwsh -File scripts/e2e-tray.ps1
```

Expected: every script exits 0. Do not run `e2e-update.ps1` unless tray launcher changes affect restart/update behavior or `qgate -All` selects it.

- [ ] **Step 4: Perform the final browser acceptance pass**

Against temporary two-node data, verify every acceptance item from the spec: all routes react without reload, numeric online header, complete participant history/counts, in-page toast and click-through, settings without member list/self row, responsive layouts, preserved draft/focus/scroll, and tab reuse/fallback. Inspect browser console logs and require zero uncaught errors.

- [ ] **Step 5: Run the mandatory quality gate**

Run: `qgate`

Expected: exit code 0. If it fails because of product code, fix the named failure and rerun. If it crashes, misses the changed stack, or blames provably correct code, run `qgate where` and create the required GitHub issue without disabling or editing the gate.

- [ ] **Step 6: Review the complete diff for scope and secrets**

Run:

```powershell
git status --short
git diff --check
git diff --stat 43f0ee2..HEAD
git diff 43f0ee2..HEAD -- . ':!docs/superpowers/plans/**' ':!docs/superpowers/specs/**'
```

Expected: only planned source/tests/docs are present; no `config.json`, data, token, pairing code, binary, screenshot, or `graphify-out` artifact is tracked.

- [ ] **Step 7: Commit only any final evidence-driven fixes**

If Step 1-6 required source changes, stage only those exact paths and use one focused fix commit, for example:

```powershell
git add scripts/e2e-tray.ps1 README.md
git commit -m "test(ui): cover reactive dashboard flow"
```

If no files changed, do not create an empty commit.
