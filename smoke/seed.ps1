# Smoke-test seeding + assertions against a live gateway.
# Prereqs: mock_server.py running (127.0.0.1:19871/19872), gateway running on :18089
# with GATEWAY_API_KEY=smoke-secret, DASHBOARD_PASSWORD=smoke-dashboard,
# DASHBOARD_SECRET (>=32 chars), and a FRESH database.
# Auth model: /dashboard/api/* wants a login session cookie; /v1/* wants the Bearer key.
$ErrorActionPreference = "Stop"
$base = "http://127.0.0.1:18089"
$H = @{ Authorization = "Bearer smoke-secret"; "Content-Type" = "application/json" }

function Step($n) { Write-Host "`n== $n ==" }

# Login once; every /dashboard/api call shares the cookie session.
$script:S = $null
$loginBody = @{ password = "smoke-dashboard" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "$base/dashboard/api/login" -ContentType "application/json" -Body $loginBody -SessionVariable script:S | Out-Null
Write-Host "dashboard login OK"

function Dash($method, $path, $bodyObj = $null) {
  $uri = "$base/dashboard/api/$path"
  if ($null -eq $bodyObj) {
    return Invoke-RestMethod -Method $method -Uri $uri -WebSession $script:S -ContentType "application/json"
  }
  return Invoke-RestMethod -Method $method -Uri $uri -WebSession $script:S -ContentType "application/json" -Body ($bodyObj | ConvertTo-Json -Depth 8)
}

Step "seed provider vercel-mock (keys vk-alpha / vk-beta, beta pinned gpt-oss)"
$vercel = @{
  id = "vercel"; display = "Vercel Mock"; base_url = "http://127.0.0.1:19871/v1"
  model = "kimi-k3"; weight = 1; enabled = $true
  accounts = @(
    @{ id = "vercel:alpha"; label = "alpha"; auth_key = "vk-alpha"; model = ""; enabled = $true; weight = 1 },
    @{ id = "vercel:beta"; label = "beta"; auth_key = "vk-beta"; model = "openai/gpt-oss-120b"; enabled = $true; weight = 1 }
  )
}
Dash Post "providers" $vercel | Out-Null

Step "seed provider nvidia-mock (keys nk-live / nk-dead)"
$nvidia = @{
  id = "nvidia"; display = "NVIDIA Mock"; base_url = "http://127.0.0.1:19872/v1"
  model = "openai/gpt-oss-120b"; weight = 1; enabled = $true
  accounts = @(
    @{ id = "nvidia:live"; label = "live"; auth_key = "nk-live"; model = ""; enabled = $true; weight = 1 },
    @{ id = "nvidia:dead"; label = "dead"; auth_key = "nk-dead"; model = ""; enabled = $true; weight = 1 }
  )
}
Dash Post "providers" $nvidia | Out-Null

Step "fetch upstream model pools (fills the combo form model dropdowns)"
Dash Post "providers/vercel/models/fetch" | Out-Null
Dash Post "providers/nvidia/models/fetch" | Out-Null

Step "create combo 'test': vercel[beta]->gpt-oss then nvidia[live]->kimi"
# Rerun-safe: drop a leftover combo from a previous smoke run first.
try { Dash Delete "combos/test" | Out-Null } catch {}
$combo = @{
  id = "test"; display_name = "Test Combo"; rotation = "priority"; enabled = $true
  members = @(
    @{ provider_id = "vercel"; account_id = "vercel:beta"; model = "openai/gpt-oss-120b" },
    @{ provider_id = "nvidia"; account_id = "nvidia:live"; model = "moonshotai/kimi-k3" }
  )
}
Dash Post "combos" $combo | Out-Null

Step "negative: combo with a bogus account must be REJECTED (400, not FK 500)"
$bad = @{
  id = "bad"; rotation = "priority"; enabled = $true
  members = @(@{ provider_id = "vercel"; account_id = "vercel:nope"; model = "" })
}
try {
  Dash Post "combos" $bad | Out-Null
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
# curl.exe instead of Invoke-WebRequest: Windows PowerShell 5 chokes on chunked SSE.
$streamBody = @{ model = "test"; stream = $true; messages = @(@{ role = "user"; content = "stream" }) } | ConvertTo-Json -Depth 6
$streamBody | Out-File -Encoding ascii smoke\_stream_body.json
$sseText = & curl.exe -s -N -X POST "$base/v1/chat/completions" -H "Authorization: Bearer smoke-secret" -H "Content-Type: application/json" --data-binary "@smoke/_stream_body.json" | Out-String
if ($sseText -notmatch "token=vk-beta") { Write-Host "FAIL: stream went to wrong key`n$sseText"; exit 1 }
if ($sseText -notmatch "data: \[DONE\]") { Write-Host "FAIL: stream missing [DONE]"; exit 1 }
Write-Host "PASS: SSE chunks + [DONE] on pinned key"

Step "disable beta key; the pinned member must burn, nvidia[live] must serve"
$vercel.accounts[1].enabled = $false
Dash Put "providers/vercel" $vercel | Out-Null
$r3 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "fallback please" }) } "req3"
if ($r3.choices[0].message.content -notmatch "token=nk-live") { Write-Host "FAIL: disabled pinned key did not fall through to nvidia"; exit 1 }
if ($r3.model -ne "moonshotai/kimi-k3") { Write-Host "FAIL: nvidia member-model pin lost"; exit 1 }
Write-Host "PASS: burned pinned member -> sibling member served"

Step "re-enable beta; 429 on first-member nvidia[dead] must fall through to nvidia[live]"
$vercel.accounts[1].enabled = $true
Dash Put "providers/vercel" $vercel | Out-Null
$combo.members[0] = @{ provider_id = "nvidia"; account_id = "nvidia:dead"; model = "openai/gpt-oss-120b" }
$combo.members[1] = @{ provider_id = "nvidia"; account_id = "nvidia:live"; model = "moonshotai/kimi-k3" }
Dash Put "combos/test" $combo | Out-Null
$r4 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "dead key first" }) } "req4"
if ($r4.choices[0].message.content -notmatch "token=nk-live") { Write-Host "FAIL: 429 did not rotate off nk-dead"; exit 1 }
if ($r4.model -ne "moonshotai/kimi-k3") { Write-Host "FAIL: expected kimi after sibling fallthrough"; exit 1 }
Write-Host "PASS: same-provider sibling pinned key caught the 429 fallthrough (audit-bug regress)"

Step "request again: nk-dead is in cooldown, must be skipped outright"
$r5 = Call-Test @{ model = "test"; messages = @(@{ role = "user"; content = "cooldown path" }) } "req5"
if ($r5.choices[0].message.content -notmatch "token=nk-live") { Write-Host "FAIL: cooled key was retried"; exit 1 }
Write-Host "PASS"

Step "direct provider: per-key model pins rotate across the account pool"
# vercel has alpha (no pin -> provider default kimi-k3) and beta (pinned gpt-oss).
# Two direct calls must hit BOTH keys, each bound to its own model.
function TokenOf($r) { if ($r.choices[0].message.content -match "token=(\S+)") { return $Matches[1] }; return "?" }
$d1 = Call-Test @{ model = "vercel"; messages = @(@{ role = "user"; content = "d1" }) } "direct1"
$d2 = Call-Test @{ model = "vercel"; messages = @(@{ role = "user"; content = "d2" }) } "direct2"
$t1 = TokenOf $d1; $t2 = TokenOf $d2
if ($t1 -eq $t2) { Write-Host "FAIL: direct rotation did not alternate keys (both $t1)"; exit 1 }
$bind = @{ "vk-alpha" = "kimi-k3"; "vk-beta" = "openai/gpt-oss-120b" }
foreach ($r in @($d1, $d2)) {
  $t = TokenOf $r
  if ($r.model -ne $bind[$t]) { Write-Host "FAIL: key $t served $($r.model), want $($bind[$t])"; exit 1 }
}
Write-Host "PASS: alpha->kimi-k3, beta->gpt-oss, alternating"

Step "request log rounding"
$logs = Dash Get "logs?limit=5"
if (-not $logs.items -or $logs.items.Count -lt 4) { Write-Host "FAIL: log rows missing"; exit 1 }
Write-Host ("PASS: {0} recent log rows" -f $logs.items.Count)

Write-Host "`nALL SMOKE CHECKS PASSED"
