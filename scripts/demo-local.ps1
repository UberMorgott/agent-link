#Requires -Version 7
# Live demo on one machine: two agentlink tray apps as two people, "morgott" and
# "nikita", both answering with real Claude Code (read-only) over a read-only
# working folder. Unlike scripts/e2e-*.ps1 this is not a test: it leaves both
# apps running with their tray icons and prints the two inbox URLs to open in a
# browser, after proving the round trip with one real question.
#
# State lives under $env:TEMP\agentlink-demo (never %APPDATA%\agentlink, never
# the autostart entry). Stop both apps with their tray icon -> Quit, or
# `pwsh -File scripts/demo-local.ps1 -Stop`.
[CmdletBinding()]
param(
    [switch]$Stop,
    [string]$WorkDir = 'E:\DEV\CodeDungeon',
    [int]$ApiPortA = 7530,
    [int]$ApiPortB = 7531,
    [int]$PeerPortA = 7430,
    [int]$PeerPortB = 7431,
    [int]$ConnectTimeoutSeconds = 30,
    [int]$AnswerTimeoutSeconds = 240
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
$tray = Join-Path $root 'bin/agentlink-tray.exe'
$demo = Join-Path $env:TEMP 'agentlink-demo'
$statePath = Join-Path $demo 'demo-state.json'

function Invoke-Ui([string]$Api, [hashtable]$Headers, [string]$Method, [string]$Path, $Body) {
    $p = @{ Method = $Method; Uri = "http://$Api/ui/api/$Path"; Headers = $Headers; TimeoutSec = 60 }
    if ($null -ne $Body) { $p.Body = ($Body | ConvertTo-Json -Compress); $p.ContentType = 'application/json' }
    Invoke-RestMethod @p
}

function Wait-Until([scriptblock]$Condition, [string]$What, [int]$Seconds) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ($true) {
        $v = try { & $Condition } catch { $null }
        if ($v) { return $v }
        if ((Get-Date) -gt $deadline) { throw "timed out waiting for $What" }
        Start-Sleep -Milliseconds 500
    }
}

if ($Stop) {
    if (-not (Test-Path $statePath)) { Write-Host 'Nothing to stop: no demo state file.'; return }
    $state = Get-Content -Raw $statePath | ConvertFrom-Json
    foreach ($n in $state.nodes) {
        $headers = @{ 'X-Agentlink-Token' = $n.token; 'Origin' = "http://$($n.api)"; 'Sec-Fetch-Site' = 'same-origin' }
        $quit = try { (Invoke-Ui $n.api $headers POST quit).quitting } catch { $false }
        $proc = Get-Process -Id $n.pid -ErrorAction SilentlyContinue
        if ($proc -and -not $proc.WaitForExit(10000)) { Stop-Process -Id $n.pid -Force }
        Write-Host "$($n.name): stopped (quit endpoint: $quit)"
    }
    Remove-Item -Force $statePath
    Write-Host 'Both demo apps are stopped. The demo folder stays at' $demo
    return
}

if (-not (Test-Path -PathType Container $WorkDir)) { throw "working folder $WorkDir does not exist" }
if (-not (Get-Command claude -ErrorAction SilentlyContinue)) { throw 'claude is not on PATH; the handler needs it' }
if (Test-Path $statePath) { throw "a demo is already running (state: $statePath). Stop it first: scripts/demo-local.ps1 -Stop" }

Write-Host '== build agentlink-tray.exe'
go -C $root build -ldflags '-H=windowsgui' -o $tray ./cmd/agentlink-tray
if ($LASTEXITCODE -ne 0) { throw 'build agentlink-tray failed' }

# Same secret on both sides, generated per run; it never leaves $env:TEMP.
$secret = -join ((1..48) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })
$a = @{ name = 'morgott'; api = "127.0.0.1:$ApiPortA"; peer = 'nikita'; port = $PeerPortA; peerPort = $PeerPortB }
$b = @{ name = 'nikita'; api = "127.0.0.1:$ApiPortB"; peer = 'morgott'; port = $PeerPortB; peerPort = $PeerPortA }

# A fresh demo starts with an empty inbox; only this TEMP folder is removed.
if ((Test-Path $demo) -and $demo -like "$env:TEMP*") { Remove-Item -Recurse -Force $demo }

foreach ($n in $a, $b) {
    $dir = New-Item -ItemType Directory -Force (Join-Path $demo $n.name)
    if ($dir.FullName -notlike "$demo*") { throw "refusing to write settings outside $demo" }
    $n.config = Join-Path $dir.FullName 'config.json'
    [ordered]@{
        node = $n.name; listen = "127.0.0.1:$($n.port)"; peer_name = $n.peer
        peer_addr = "127.0.0.1:$($n.peerPort)"; secret = $secret; areas = @('demo')
        handler = 'claude'; work_dir = $WorkDir; autostart = $false; api = $n.api
    } | ConvertTo-Json | Set-Content -Path $n.config -Encoding utf8
}

try {
    Write-Host '== start both apps (tray icons visible)'
    foreach ($n in $a, $b) {
        $n.proc = Start-Process -FilePath $tray -ArgumentList @('-config', $n.config) -PassThru
        $page = Wait-Until { Invoke-WebRequest -Uri "http://$($n.api)/ui/inbox" -TimeoutSec 2 } "$($n.name) web UI" $ConnectTimeoutSeconds
        if ($page.Content -notmatch 'name="agentlink-token" content="([0-9a-f]+)"') { throw "$($n.name): no token in the inbox page" }
        $n.token = $Matches[1]
        $n.headers = @{ 'X-Agentlink-Token' = $n.token; 'Origin' = "http://$($n.api)"; 'Sec-Fetch-Site' = 'same-origin' }
        Write-Host "$($n.name): inbox page HTTP $($page.StatusCode), pid $($n.proc.Id)"
    }
    foreach ($n in $a, $b) {
        $st = Wait-Until { $s = Invoke-Ui $n.api $n.headers GET status; if ($s.connected) { $s } } "$($n.name) connected" $ConnectTimeoutSeconds
        Write-Host "$($n.name): connected to $($st.peer), handler $($st.handler)"
    }

    @{ nodes = @($a, $b | ForEach-Object { @{ name = $_.name; api = $_.api; token = $_.token; pid = $_.proc.Id } }) } |
        ConvertTo-Json -Depth 4 | Set-Content -Path $statePath -Encoding utf8

    Write-Host '== self-check: one real question morgott -> nikita, answered by Claude'
    $probe = 'Reply with the single word pong and nothing else. Do not read any files.'
    $req = Invoke-Ui $a.api $a.headers POST send @{ to = $b.name; body = $probe }
    $entry = Wait-Until {
        $e = (Invoke-Ui $a.api $a.headers GET inbox) | Where-Object { $_.direction -eq 'out' -and $_.id -eq $req.id }
        if ($e -and $e.PSObject.Properties['job_status']?.Value -eq 'completed') { $e }
    } "Claude's answer to $($req.id)" $AnswerTimeoutSeconds
    $answer = $entry.answer.Trim()
    Write-Host "answer from nikita: $answer"
    if ($answer -notmatch '(?i)pong') { throw "the handler answered, but not with the expected word: $answer" }
}
catch {
    foreach ($n in $a, $b) {
        if ($n.ContainsKey('proc') -and -not $n.proc.HasExited) { Stop-Process -Id $n.proc.Id -Force }
    }
    if (Test-Path $statePath) { Remove-Item -Force $statePath }
    throw
}

Write-Host ''
Write-Host 'Open these two pages in the browser:'
Write-Host "  morgott: http://$($a.api)/ui/inbox"
Write-Host "  nikita:  http://$($b.api)/ui/inbox"
Write-Host ''
Write-Host @"
Откройте обе ссылки в браузере - это два человека на одной машине. На странице
morgott в поле "Кому" уже стоит nikita; напишите вопрос в поле "Текст" и нажмите
"Отправить". Страница обновляется сама каждые 3 секунды: статус пройдёт
"в очереди" -> "выполняется" -> "готово", и в той же строке появится ответ,
который дал Claude на стороне nikita (он читает папку $WorkDir только на
чтение). На странице nikita то же сообщение помечено как "Входящее", и кнопкой
"Ответить" можно ответить вручную. Обратное направление работает так же: со
страницы nikita отправьте вопрос morgott.
"@
Write-Host ''
Write-Host 'Both apps stay running. To stop them: tray icon -> Quit on each, or'
Write-Host '  pwsh -File scripts/demo-local.ps1 -Stop'
