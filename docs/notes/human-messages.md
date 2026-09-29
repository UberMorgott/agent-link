# A person's message in a chat

Owner report 2026-09-29: Morgott wrote in the Aegis network project chat
(chat 52b4ba3f, message 26b0c0d6, 19:33:03Z) while KPECTIK's Codex was live.
Nobody answered and the message had no ticks.

## Root causes

- The chat had one participant. It started 2026-09-26 as Morgott's solo
  chat. KPECTIK joined the project on 2026-09-29 at 19:31Z. Joining the
  project did not add him to its active chat, so the message was fanned
  out to nobody. With no `delivery` entries, the UI had no tick to show.
- The composer sends `ask: []` in project chats. Since the per-seat picker
  was removed (690d29c), a person's message asked nobody. Peer agents
  treated it as information: they answered only under full autonomy.
- Morgott's own Claude session got the message as information: it was
  acknowledged at 19:38:49Z. The UI showed no tick for that read either.

## Rules

1. **A project member who joins is added to the project's active chat.**
   This happens when the member first appears in the member table, comes
   back after leaving, or rejoins on a new node (`mergeMembers`,
   `noteSession`, `revive`). Only the chat's owner node adds it
   (`addJoined`, through `projectChatLocked`). A member still pinned to its
   old node rotates the chat into a new one with everyone. The add runs in
   order with the member table, never in a goroutine, so a later removal is
   never undone. Only a join adds a member: one that the owner removes stays
   out.
   `SetChatMembers` now runs under `ensureMu`, so two concurrent changes
   cannot both build on the same participants and `Rev`.
2. **A person's message that names nobody asks every other participant.**
   "Names nobody" means no `ask` and no `ask_seats`. The asked participants
   are the other participants still pinned to their node (`othersAsked`).
   Their nodes wake a live session, or launch one, as for any request.
   The usual limits still apply:
   - A person's message starts a new chain at depth 0.
   - Pause, stop and autonomy-off are applied by the receiving node. It
     reports `held:<reason>` to the author (#25), and the author's UI shows
     that reason as «!» on the message.
   - The author's own sessions still get the message as information
     (`OwnHuman`), never as a request: the person is at the keyboard.
   - A message that asks this node's seats (a local chat) asks only those
     seats.
3. **Ticks on a person's own message** show, per peer, delivered, read or
   answered. They also show whether an agent on this computer has read the
   message: `own_human` and `unread`, unless the message asked seats.

## KPECTIK's side

- Rule 2 is applied by the author's node. Older peers already handle
  `Responders` (asks), so they need no update to answer.
- Rule 1 is applied by the chat owner's node. For chats owned by KPECTIK,
  his node must run the version with this change.
