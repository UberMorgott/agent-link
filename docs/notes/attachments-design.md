# agent-link attachments contract (A, draft v1)

Owner: paste/drag/picker images+files in chat; agents can post files/images too; only formats both Claude Code and Codex handle.

## Facts
- Claude Code: images JPEG/PNG/GIF/WebP, ≤10MB (API). Inbox pipe = text only; hook additionalContext = text only. Read tool opens image files visually and reads text/PDF by path.
- Codex 0.155.1: `codex --image`, `codex queue --image <path>` (verified); resume has no --image. Model reads other files via shell; view_image for images by path.
- => Uniform delivery = local absolute PATHS in message text (+ `codex queue --image` for images on Codex wake as bonus).

## Allowlist (by sniffed MIME, not extension)
- images: png, jpeg, gif, webp — ≤10MB each
- text-like: txt, md, json, csv, log, yaml/toml, source code (utf-8 text sniff) — ≤10MB
- pdf — ≤10MB
- max 10 attachments/message, 50MB total/message. Anything else rejected with clear error.

## Model
- Attachment {id=sha256 hex, name (sanitized basename), mime, size}. Message gets optional `attachments: [Attachment]` (old peers ignore; body keeps a text fallback line "[attachment: name]").
- Storage: content-addressed in node data dir `attachments/<sha256>`; dedupe by hash; verify hash on receive.
- Transfer: sender node pushes blobs to peers over existing peer connection (chunked frames, capped), before/with message; receiver verifies size+hash+allowlist, else marks attachment `failed`.
- Materialize for agents: `<project folder>/.agentlink/attachments/<sha8>-<name>` (gitignored via .agentlink/.gitignore `*`) — paths inside workspace so both agents' sandboxes can read. Unread formatting/wake text lists absolute paths.
- Agent-posted: MCP `send` gains `attachments: [path]` + CLI `send --attach <path>` (repeatable). Only files inside project folder or agent temp/output dirs; same allowlist/limits; hash+copy into store.
- UI: composer paste (clipboard images), drag/drop, picker; chips with preview/remove before send; message shows image thumbnails + file chips with download (GET /attachments/<sha>).
- API: POST /attachments (multipart or raw, returns Attachment), GET /attachments/<sha>.
