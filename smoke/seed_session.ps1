# Smoke-test seeding + assertions against a live gateway.
# Prereqs: mock_server.py running (127.0.0.1:19871/19872), gateway running on :18089
# with GATEWAY_API_KEY=smoke-secret and a FRESH database.
$ErrorActionPreference = "Stop"
$base = "http://127.0.0.1:18089"
$H = @{ Authorization = "Bearer smoke-secret"; "Content-Type" = "application/json" }
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/login" -ContentType "application/json" -Body (@{ password = "smoke-dashboard" } | ConvertTo-Json) -SessionVariable S | Out-Null

function Step($n) { Write-Host "`n== $n ==" }

Step "seed provider vercel-mock (keys vk-alpha / vk-beta, beta pinned gpt-oss)"
$vercel = @{
  id = "vercel"; display = "Vercel Mock"; base_url = "http://127.0.0.1:19871/v1"
  model = "kimi-k3"; weight = 1; enabled = $true
  accounts = @(
    @{ id = "vercel:alpha"; label = "alpha"; auth_key = "vk-alpha"; model = ""; enabled = $true; weight = 1 },
    @{ id = "vercel:beta"; label = "beta"; auth_key = "vk-beta"; model = "openai/gpt-oss-120b"; enabled = $true; weight = 1 }
  )
}
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/providers" -Headers $H -WebSession $S -Body ($vercel | ConvertTo-Json -Depth 6) | Out-Null

Step "seed provider nvidia-mock (keys nk-live / nk-dead)"
$nvidia = @{
  id = "nvidia"; display = "NVIDIA Mock"; base_url = "http://127.0.0.1:19872/v1"
  model = "openai/gpt-oss-120b"; weight = 1; enabled = $true
  accounts = @(
    @{ id = "nvidia:live"; label = "live"; auth_key = "nk-live"; model = ""; enabled = $true; weight = 1 },
    @{ id = "nvidia:dead"; label = "dead"; auth_key = "nk-dead"; model = ""; enabled = $true; weight = 1 }
  )
}
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/providers" -Headers $H -WebSession $S -Body ($nvidia | ConvertTo-Json -Depth 6) | Out-Null

Step "fetch upstream model pools (fills the combo form model dropdowns)"
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/providers/vercel/models/fetch" -Headers $H -WebSession $S | Out-Null
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/providers/nvidia/models/fetch" -Headers $H -WebSession $S | Out-Null

Step "create combo 'test': vercel[beta]->gpt-oss then nvidia[live]->kimi"
$combo = @{
  id = "test"; display_name = "Test Combo"; rotation = "priority"; enabled = $true
  members = @(
    @{ provider_id = "vercel"; account_id = "vercel:beta"; model = "openai/gpt-oss-120b" },
    @{ provider_id = "nvidia"; account_id = "nvidia:live"; model = "moonshotai/kimi-k3" }
  )
}
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/combos" -Headers $H -WebSession $S -Body ($combo | ConvertTo-Json -Depth 6) | Out-Null

Step "negative: combo with a bogus account must be REJECTED (400)"
$bad = @{
  id = "bad"; rotation = "priority"; enabled = $true
  members = @(@{ provider_id = "vercel"; account_id = "vercel:nope"; model = "" })
}
try {
  Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/combos" -Headers $H -WebSession $S -Body ($bad | ConvertTo-Json -Depth 6) | Out-Null
  Write-Host "FAIL: bogus account pin accepted"
  exit 1
} catch {
  $code = $_.Exception.Response.StatusCode.value__
  if ($code -ne 400) { Write-Host "FAIL: expected 400, got $code"; exit 1 }
  Write-Host "PASS: rejected with 400"
}

function Call-Test($bodyObj, $tag) {
  $r = Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body ($bodyObj | ConvertTo-Json -Depth 6)
  $content = $r.choices[0].message.content
  Write-Host ("[{0}] model={1} content={2}" -f $tag, $r.model, $content)
  return $r
}

Step "request 1: combo test (expect beta key + member model gpt-oss)"
$r1 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "hi" }) } "req1"
if ($r1.model -ne "openai/gpt-oss-120b") { Write-Host "FAIL: member-model pin lost"; exit 1 }
if ($r1.choices[0].message.content -notmatch "token=vk-beta") { Write-Host "FAIL: expected beta key"; exit 1 }
Write-Host "PASS: exact (vercel, vk-beta, gpt-oss) served"

Step "request 2: same request again (priority must stick to first member)"
$r2 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "hi again" }) } "req2"
if ($r2.model -ne "openai/gpt-oss-120b") { Write-Host "FAIL: priority drifted"; exit 1 }
Write-Host "PASS"

Step "streaming request through the same combo"
$sse = Invoke-WebRequest -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body (@{ model = "test"; stream = $true; messages = @(@{ role = "user"; content = "stream" }) } | ConvertTo-Json -Depth 6)
$sseText = [System.Text.Encoding]::UTF8.GetString($sse.Content)
if ($sseText -notmatch "token=vk-beta") { Write-Host "FAIL: stream went to wrong key`n$sseText"; exit 1 }
if ($sseText -notmatch "data: \[DONE\]") { Write-Host "FAIL: stream missing [DONE]"; exit 1 }
Write-Host "PASS: SSE chunks + [DONE] on pinned key"

Step "disable beta key; the pinned member must burn, nvidia[live] must serve"
$vercel.accounts[1].enabled = $false
Invoke-RestMethod -Method Put -Uri "$base/dashboard/api/providers/vercel" -Headers $H -WebSession $S -Body ($vercel | ConvertTo-Json -Depth 6) | Out-Null
$r3 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "fallback please" }) } "req3"
if ($r3.choices[0].message.content -notmatch "token=nk-live") { Write-Host "FAIL: disabled pinned key did not fall through to nvidia"; exit 1 }
if ($r3.model -ne "moonshotai/kimi-k3") { Write-Host "FAIL: nvidia member-model pin lost"; exit 1 }
Write-Host "PASS: burned pinned member -> sibling member served"

Step "re-enable beta, then hit a real 429 on the first priority member (sibling fallthrough)"
$vercel.accounts[1].enabled = $true
Invoke-RestMethod -Method Put -Uri "$base/dashboard/api/providers/vercel" -Headers $H -WebSession $S -Body ($vercel | ConvertTo-Json -Depth 6) | Out-Null
# Repoint combo at nvidia: dead key first, live sibling second.
$combo.members[0] = @{ provider_id = "nvidia"; account_id = "nvidia:dead"; model = "openai/gpt-oss-120b" }
$combo.members[1] = @{ provider_id = "nvidia"; account_id = "nvidia:live"; model = "moonshotai/kimi-k3" }
Invoke-RestMethod -Method Put -Uri "$base/dashboard/api/combos/test" -Headers $H -WebSession $S -Body ($combo | ConvertTo-Json -Depth 6) | Out-Null
$r4 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "dead key first" }) } "req4"
if ($r4.choices[0].message.content -notmatch "token=nk-live") { Write-Host "FAIL: 429 did not rotate off nk-dead"; exit 1 }
if ($r4.model -ne "moonshotai/kimi-k3") { Write-Host "FAIL: expected kimi after sibling fallthrough"; exit 1 }
Write-Host "PASS: same-provider sibling pinned key caught the 429 fallthrough (audit-bug regress)"

Step "request again: nk-dead is in cooldown, must be skipped outright"
$r5 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "cooldown path" }) } "req5"
if ($r5.choices[0].message.content -notmatch "token=nk-live") { Write-Host "FAIL: cooled key was retried"; exit 1 }
Write-Host "PASS"

Step "request log rounding"
$logs = Invoke-RestMethod -Uri "$base/dashboard/api/logs?limit=5" -Headers $H -WebSession $S
if (-not $logs.items -or $logs.items.Count -lt 4) { Write-Host "FAIL: log rows missing"; exit 1 }
Write-Host ("PASS: {0} recent log rows" -f $logs.items.Count)

Write-Host "`nALL SMOKE CHECKS PASSED"

