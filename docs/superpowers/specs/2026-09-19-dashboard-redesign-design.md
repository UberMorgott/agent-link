# Agentlink Dashboard Redesign

## Intent

Replace the current utility-like settings and inbox pages with one coherent,
reactive dashboard for people coordinating agents across several computers.
The dashboard must make network health, conversations, participants, and agent
configuration understandable without exposing protocol terminology or mixing
participant management into general settings.

Success means a user can immediately see how many remote participants are
online, notice and open a new incoming message, inspect the complete history
with one participant, send or reply from the same conversation view, and manage
the network without seeing their own node presented as a removable participant.

## Scope

The redesign keeps the existing Go server and embedded HTML, CSS, and vanilla
JavaScript. It reuses the durable message store and membership table. It does
not add a frontend framework, database, third-party service, or wire-protocol
field.

The work includes:

- a shared dashboard shell with Overview, Messages, Participants, and Settings;
- a conversation-oriented message view;
- participant statistics derived from local message history;
- in-page incoming-message notifications;
- participant management moved out of Settings;
- a clearer remote-agent prompt;
- best-effort reuse of an already open dashboard browser tab;
- responsive, accessible light and dark themes inspired by Codex and ChatGPT.

It does not include operating-system notifications, message deletion, file
attachments, full-text search, or changes to authentication, encryption, or
peer discovery.

## Information Architecture

The web UI is one reactive application shell with a persistent sidebar on
desktop and a compact top/bottom navigation treatment on narrow screens.
Navigation changes the active view through the History API and updates only the
relevant DOM. It does not reload the document.

### Reactive Application Shell

`/ui/dashboard`, `/ui/inbox`, `/ui/participants`, and `/ui/settings` all serve
the same embedded shell and select their initial view from the route. The shell
owns a small in-memory state store for status, summary, participants,
conversations, settings, and update state. Plain JavaScript modules subscribe
views to the slices they render; no frontend framework or build step is added.

After initial load, the shell stays live:

- sidebar navigation uses `history.pushState` and supports browser back/forward;
- status, participants, conversation activity, and incoming messages refresh in
  the background and patch only changed elements;
- sending, replying, adding/removing participants, saving settings, and checking
  updates update local state immediately from each API response;
- buttons expose busy states and remain protected from duplicate submissions;
- a failed request keeps the last good data visible and surfaces a compact
  recoverable error;
- focus, scroll position, drafts, and the selected conversation are preserved
  across background refreshes;
- no normal workflow requires a manual page refresh.

A full document reload is reserved for application restart/update, token expiry,
or an unrecoverable shell-version mismatch.

### Overview

The default route is `/ui/dashboard`. It shows:

- the number of online remote participants and the total known remote count;
- current handler state and configured agent;
- message totals and pending/running request counts;
- recent conversations, ordered by latest activity;
- concise actionable setup, connection, or security warnings.

No card repeats information already visible in the global status header.

### Messages

The `/ui/inbox` view becomes a two-pane conversation page:

- the left pane lists participants with their latest message, timestamp,
  unread indicator, and online state;
- the main pane shows the complete local history with the selected participant;
- incoming and outgoing messages use familiar chat alignment while preserving
  request status, handler activity, errors, and reply relationships;
- the composer remains anchored at the bottom and sends to the selected
  participant;
- `?peer=<name>&message=<id>` selects a participant and scrolls to a specific
  message when opened from a notification.

Area fan-out remains available through a compact recipient chooser rather than
being removed from the product.

### Participants

The new `/ui/participants` view lists remote participants only. Each row shows:

- name and online/offline state;
- last-seen time when offline;
- application version and compatibility warnings;
- known addresses in a secondary details area;
- sent, received, and total message counts;
- an action that opens the complete conversation;
- a secondary destructive action for removing the participant.

Adding a participant by address also lives on this page. The local node is not
shown as a participant and the wording `(you)` is removed from this surface.

### Settings

The `/ui/settings` view contains identity, pairing code, handler selection, agent path,
working directory, autostart, updates, and advanced network options. The member
list and add/remove controls are removed. Settings are grouped into human-sized
cards instead of one long fieldset, while server-side validation and security
rules remain unchanged.

## Visual System

The dashboard uses neutral layered surfaces, a restrained blue accent, subtle
borders, and system fonts. The styling should feel closer to a desktop product
than a browser form:

- 14-16 px base typography with clear hierarchy;
- 12-16 px radii and low-contrast borders instead of default fieldsets;
- compact icon-and-label navigation;
- consistent primary, secondary, danger, and ghost buttons;
- visible focus rings and semantic status colors that are not the only signal;
- comfortable message line lengths and preserved multiline content;
- responsive layouts at approximately 900 px and 640 px breakpoints;
- `prefers-color-scheme` support without requiring a theme setting.

All user-visible text continues to come from `internal/app/strings.go`.

## Server Data Model and APIs

The existing message files remain the source of truth. `Recent(0)` provides the
complete local history; no migration is required.

The server adds view models at the application layer rather than putting UI
statistics into the network node:

- `DashboardSummary` aggregates status, message counts, active jobs, and recent
  conversations;
- `ParticipantView` combines `node.MemberInfo` with sent and received counts and
  the latest conversation activity;
- conversation filtering groups existing `Thread` values by remote participant.

Proposed endpoints:

- `GET /ui/api/dashboard` returns `DashboardSummary`;
- `GET /ui/api/participants` returns remote `ParticipantView` values;
- `GET /ui/api/threads?peer=<name>` returns the full filtered history, while an
  absent peer may retain the current bounded recent feed for compatibility;
- existing status, send, settings, member-action, update, and picker endpoints
  remain in place.

Counts exclude status-update frames. A request and its final reply each count as
one message in their actual directions. Area messages contribute to each stored
local message once; the UI does not invent per-recipient deliveries it cannot
prove from local storage.

## Reactive Updates and Incoming Message Notifications

The open dashboard maintains one authenticated server-sent event stream to the
loopback Go server. Node membership, connection, message, worker, settings, and
update mutations publish a revision with the affected state topics. The browser
then refreshes only those API slices. No UI state is refreshed on a schedule;
SSE keepalives only keep the connection alive and never trigger data reads.

Because the control token stays in the `X-Agentlink-Token` header, the client
uses a streaming `fetch` rather than putting the token in a query string or
using native `EventSource`. Reconnect uses bounded exponential backoff and an
initial event refreshes all slices, so missed events are recovered without an
event log. The last good state stays visible while the stream reconnects.

For message events, the application store compares incoming non-status message
IDs against a watermark stored in `localStorage` for this local dashboard origin.

On first use, existing history initializes the watermark without producing a
storm of old notifications. A genuinely new incoming request or reply creates a
bottom-right toast containing the sender and a safely truncated plain-text
preview. Multiple messages stack to a small bounded count. Clicking a toast
navigates the same tab to `/ui/inbox?peer=<sender>&message=<id>`.

The toast is an in-page notification only. It does not request browser
notification permission and cannot alert when no dashboard page is open.

## Online Status

The global header never names a single arbitrary peer. Its connected state is
rendered as `N participants online`; the total known remote participant count
may appear as secondary text. Zero, unconfigured, degraded, and security-warning
states retain actionable explanations.

## Remote Agent Context

The built-in handler preamble must explicitly establish the roles and transport:

- the running model is the read-only handler agent on the receiving computer;
- the request arrived through agent-link from another trusted developer's
  computer;
- the request body may have been written by a human or another agent;
- the final response is automatically returned to that remote sender;
- it must not claim to be the remote agent or expose local secrets;
- existing concise-language and read-only rules remain intact.

The preamble stays on stdin and never moves request content into shell arguments.
Resume commands retain the same read-only limits.

## Reusing an Existing Browser Tab

The tray and its Open Browser menu target `/ui/open`, not a content page
directly. The launcher page uses a stable browsing-context name such as
`agentlink-dashboard`:

1. If a named dashboard context exists in the same browser profile, focus it
  and ask the reactive shell to select the requested dashboard route.
2. If it does not exist, turn the launcher into the dashboard and assign the
   stable name.
3. If browser popup or focus policy blocks reuse, fall back to the launcher tab
   itself so the user always reaches the dashboard.

Ordinary browsers do not expose a cross-browser API that guarantees selecting
an arbitrary pre-existing tab. The implementation therefore provides reliable
same-profile best effort without browser-specific automation, extensions, or a
WebView2 dependency. It must never create repeated tabs merely because a tray
double-click emitted two events; the existing debounce remains.

## Error Handling and Security

- Every new API remains behind the existing same-origin token middleware.
- The control server remains loopback-only with the current DNS-rebinding and
  framing protections.
- User content is inserted with `textContent`, never HTML.
- Missing/stopped node state returns empty dashboard collections plus the
  existing actionable configuration state rather than a page crash.
- A failed event stream or event-driven refresh leaves the current page usable
  and shows one compact connection warning instead of repeated alerts.
- Removing a participant retains the existing confirmation and tombstone flow.
- No secret, pairing code, config contents, or agent output is added to logs or
  browser persistence.

## Testing and Acceptance

Implementation follows test-first development. Server tests cover:

- participant aggregation and sent/received counts;
- complete peer-filtered history;
- dashboard counts and active request states;
- omission of the local node from Participants;
- all new routes and token enforcement;
- the revised remote-agent context;
- launcher routing and safe URL handling.

UI-focused tests and local browser verification cover:

- navigation between all four sections without document reloads or unintended
  new tabs, including browser back/forward;
- responsive desktop and narrow layouts;
- selecting a participant opens its complete conversation;
- a newly arriving message creates one toast and its click opens the right chat;
- old history does not create notifications on first load;
- the global header reports an online participant count;
- participant controls are absent from Settings;
- background refresh preserves the active draft, focus, and scroll position;
- tray/menu opening reuses a named dashboard tab when browser policy permits and
  falls back cleanly when it does not.

The final verification runs the relevant Go tests, the repository's prescribed
end-to-end checks affected by the change, and `qgate` from the repository root.
No completion claim is made while `qgate` is non-zero.
