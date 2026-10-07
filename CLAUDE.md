## Completion gate (mandatory)

After changing files, run `aegis verify` from the repository root (owner decision
2026-09-30: Aegis replaces qgate here). Exit 0 = done; anything else = NOT done —
fix what it names and rerun. Never edit or disable the gate to pass. Include the
command and its result in your final response.

If Aegis is wrong — crashes, blames correct code, misses a stack, reports
inconclusive with nothing failed, is slow — do not work around it: SendMessage the
session "ЛОКАЛЬНЫЙ AEGIS" (command, expected, got, latency, `aegis version`), say so
in your final response. `aegis verify` itself runs gofmt, go build, go vet,
go test, aegis-lint and the web checks; run a check by hand only for what it
reported as not established (e.g. vitest in internal/app/web) until it is fixed.

The pre-commit and pre-merge-commit hooks (`lefthook.yml`) run `aegis verify`
(switched from qgate 2026-09-30).

## Releases

Standing owner authorization: after every verified change, push `main` and cut the next patch
release without asking — annotated tag `vX.Y.Z` (bump Z from the latest tag), `git push origin vX.Y.Z`,
then build and publish locally from the repository root:
`pwsh -File scripts/release.ps1 -Version X.Y.Z -Publish` (needs Go, `gh`; builds the
stripped, unpacked `dist/agentlink.exe`, writes `dist/notes.md` with `scripts/release-notes.ps1`
and runs `gh release create vX.Y.Z dist/agentlink.exe --title vX.Y.Z --notes-file dist/notes.md`).
GitHub Actions is NOT used (quota exhausted): `.github/workflows/release.yml` runs only by hand,
a pushed tag triggers nothing. Confirm with `gh release view vX.Y.Z` that the `agentlink.exe`
asset exists.
