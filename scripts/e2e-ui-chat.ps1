# Chat operations shared by the isolated tray and update scenarios.
function Open-TestChat([hashtable]$a, [hashtable]$b) {
    $chat = Invoke-Ui $a POST projects/legacy/chats @{ participants = @($b.name) }
    $a.chat = $b.chat = $chat.id
    Wait-Until { (Invoke-Ui $b GET "projects/legacy/chats/$($b.chat)").id -eq $b.chat } 'chat on node-b' | Out-Null
}

function Send-ChatMessage([hashtable]$n, [string]$Peer, [string]$Text) {
    Invoke-Ui $n POST projects/legacy/send @{ chat_id = $n.chat; ask = @($Peer); body = $Text }
}

function Get-Inbox([hashtable]$n) {
    (Invoke-Ui $n GET "projects/legacy/chats/$($n.chat)/messages") | Where-Object { -not $_.PSObject.Properties['kind']?.Value }
}

function Find-Reply([hashtable]$n, [string]$Id) {
    Get-Inbox $n | Where-Object { $_.direction -eq 'in' -and $_.PSObject.Properties['reply_to']?.Value -eq $Id }
}
