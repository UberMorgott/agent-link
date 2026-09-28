# Swaps a freshly built executable into its live path the way the self-updater
# does (dot-sourced by release.ps1, tested by swap.test.ps1): the running file
# is renamed aside (a running executable cannot be deleted, only renamed) and
# removed later by the app's cleanup (internal/selfupdate OldPath and its
# numbered slots).

# Move-Aside renames Path to a free .<name>.old[.N] slot next to it and returns
# the slot, or $null when Path does not exist.
function Move-Aside([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    $old = Join-Path (Split-Path $Path -Parent) ('.' + (Split-Path $Path -Leaf) + '.old')
    for ($slot = 0; $slot -le 64; $slot++) {
        $target = if ($slot) { "$old.$slot" } else { $old }
        if (Test-Path -LiteralPath $target) {
            # Still running an older build: try the next slot.
            try { Remove-Item -LiteralPath $target -Force -ErrorAction Stop } catch { continue }
        }
        [IO.File]::Move($Path, $target)
        return $target
    }
    throw "no free slot to move $Path aside"
}

# Install-Built moves Built to Exe. When that fails the parked old executable
# is moved back, so Exe is never left missing.
function Install-Built([string]$Built, [string]$Exe) {
    $parked = Move-Aside $Exe
    try {
        [IO.File]::Move($Built, $Exe)
    }
    catch {
        if ($parked -and -not (Test-Path -LiteralPath $Exe)) { [IO.File]::Move($parked, $Exe) }
        throw
    }
}
