# Prints the Markdown body of a GitHub release: the conventional commits
# between the previous v* tag and -Tag, grouped by type (feat, fix, perf),
# each bullet the commit subject without its "type(scope):" prefix, and the
# compare link. Commits land on main without pull requests, so GitHub's
# generated notes would hold only the link.
# .github/workflows/release.yml runs this on a pushed v* tag; it needs the
# full history and tags (actions/checkout with fetch-depth: 0).
param(
    [Parameter(Mandatory)][string]$Tag,
    [string]$Repo = 'UberMorgott/agent-link'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# The previous tag is the nearest v* tag reachable from the tag's parent;
# the first release has none and lists the whole history.
$prev = git describe --tags --abbrev=0 --match 'v*' "$Tag^" 2>$null
if ($LASTEXITCODE) { $prev = $null }
$range = $prev ? "$prev..$Tag" : $Tag
$subjects = @(git log $range --no-merges --pretty=%s)
if ($LASTEXITCODE) { throw "git log $range failed" }

$sections = [ordered]@{ feat = 'Новое'; fix = 'Исправления'; perf = 'Производительность' }
$groups = @{}
foreach ($s in $subjects) {
    if ($s -notmatch '^(\w+)(\([^)]*\))?!?:\s*(.+)$') { continue }
    $type = $Matches[1].ToLowerInvariant()
    if (-not $sections.Contains($type)) { continue }
    if (-not $groups.ContainsKey($type)) { $groups[$type] = [System.Collections.Generic.List[string]]::new() }
    $groups[$type].Add($Matches[3])
}

$out = [System.Collections.Generic.List[string]]::new()
foreach ($type in $sections.Keys) {
    if (-not $groups.ContainsKey($type)) { continue }
    $out.Add("## $($sections[$type])")
    $out.Add('')
    foreach ($line in $groups[$type]) { $out.Add("- $line") }
    $out.Add('')
}
if ($out.Count -eq 0) { $out.Add('Служебные изменения.'); $out.Add('') }
$out.Add($prev ?
    "**Full Changelog**: https://github.com/$Repo/compare/$prev...$Tag" :
    "**Full Changelog**: https://github.com/$Repo/commits/$Tag")
$out -join "`n"
