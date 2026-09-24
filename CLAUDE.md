<!-- quality-gate -->
## Completion gate (mandatory)

After changing files, run `qgate` from the repository root. Use `qgate -All` when
dependencies, build configuration, generated files or several stacks changed.

Exit code 0 means done. Anything else means NOT done: the output names the exact
failures -- fix them and run it again. Do not report completion while the gate is
failing, and never edit or disable the gate to make it pass. Include the command
you ran and its pass/fail result in your final response.

If `qgate` is unavailable, report that as a blocker, do not skip it. It installs with
`irm https://raw.githubusercontent.com/UberMorgott/quality-gate/main/bootstrap.ps1 | iex`

If the gate itself is wrong -- it crashes, blames code that is provably correct,
misses a whole stack, or cannot be satisfied at all -- do not work around it and do
not disable it. Open an issue against the gate and say so in your final response:

```powershell
qgate where   # install path + commit, paste this into the issue
gh issue create --repo UberMorgott/quality-gate --title "<what broke>" --body "<qgate output, the command you ran, the file it blamed, `qgate where` output>"
```
<!-- /quality-gate -->

## Releases

Standing owner authorization: after every verified change, push `main` and cut the next patch
release without asking — annotated tag `vX.Y.Z` (bump Z from the latest tag), `git push origin vX.Y.Z`,
then build and publish locally from the repository root:
`pwsh -File scripts/release.ps1 -Version X.Y.Z -Publish` (needs Go, `upx`, `gh`; builds the
stripped UPX-packed `dist/agentlink.exe`, writes `dist/notes.md` with `scripts/release-notes.ps1`
and runs `gh release create vX.Y.Z dist/agentlink.exe --title vX.Y.Z --notes-file dist/notes.md`).
GitHub Actions is NOT used (quota exhausted): `.github/workflows/release.yml` runs only by hand,
a pushed tag triggers nothing. Confirm with `gh release view vX.Y.Z` that the `agentlink.exe`
asset exists.
