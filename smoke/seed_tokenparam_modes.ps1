# Scoped smoke test for max_tokens/max_completion_tokens manual override + the
# learned-cache optimization (both proposed by the user as latency fixes on
# top of the reactive smart-mode heal):
#   1. Manual override (token_param_mode="max_completion_tokens" on the
#      provider) must make the FIRST upstream call already use the right
#      field - zero extra round trips, ever.
#   2. With no override anywhere, request #1 heals reactively (2 upstream
#      calls); request #2 for the SAME account must cost only 1 upstream call
#      because the gateway learned the answer from request #1.
#   3. A combo member's token_param_mode overrides the account/provider.
#
# Prereqs: mock_server.py running (127.0.0.1:19874 = smartmode-mock), launched
# from the repo root as `python smoke/mock_server.py smoke/requests.log` so its
# request log lands where CountCalls() reads it. Gateway running on :18089 with
# GATEWAY_API_KEY=smoke-secret, DASHBOARD_PASSWORD=smoke-dashboard,
# DASHBOARD_SECRET (>=32 chars), and a FRESH database.
$ErrorActionPreference = "Stop"
$base = "http://127.0.0.1:18089"
$H = @{ Authorization = "Bearer smoke-secret"; "Content-Type" = "application/json" }

function Step($n) { Write-Host "`n== $n ==" }

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

function CountCalls($token) {
  # mock_server.py writes its JSON-lines log to <argv1> (default "requests.log"
  # in ITS own CWD). To keep the mock and this script agreeing on one file no
  # matter where either is launched from, resolve relative to this script's
  # directory and require the mock be started from the repo root as:
  #     python smoke/mock_server.py smoke/requests.log
  $log = Join-Path $PSScriptRoot "requests.log"
  if (-not (Test-Path $log)) { return 0 }
  return (Get-Content $log | Where-Object { $_ -match "`"token`": `"$token`"" }).Count
}

Step "1) manual override: provider 'sm-manual' pinned to max_completion_tokens"
$p1 = @{
  id = "sm-manual"; display = "Manual Override"; base_url = "http://127.0.0.1:19874/v1"
  model = "openai/gpt-5.6-sol"; auth_key = "sm-k1"; weight = 1; enabled = $true
  token_param_mode = "max_completion_tokens"
}
try { Dash Delete "providers/sm-manual" | Out-Null } catch {}
Dash Post "providers" $p1 | Out-Null
$before = CountCalls "sm-k1"
$body = @{ model = "sm-manual"; messages = @(@{ role = "user"; content = "hi" }); max_tokens = 16 } | ConvertTo-Json -Depth 6
$r1 = Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body
$after = CountCalls "sm-k1"
Write-Host "response: $($r1.choices[0].message.content)"
Write-Host "upstream calls for sm-k1: $($after - $before)"
if (($after - $before) -ne 1) {
  Write-Host "FAIL: manual override should cost exactly 1 upstream call, got $($after - $before)"
  exit 1
}
Write-Host "PASS: manual override skipped the heal round trip entirely"

Step "2) no override: request #1 heals reactively (2 calls), request #2 uses the learned cache (1 call)"
$p2 = @{
  id = "sm-auto"; display = "Auto Learn"; base_url = "http://127.0.0.1:19874/v1"
  model = "openai/gpt-5.6-sol"; auth_key = "sm-k1"; weight = 1; enabled = $true
}
try { Dash Delete "providers/sm-auto" | Out-Null } catch {}
Dash Post "providers" $p2 | Out-Null

$before1 = CountCalls "sm-k1"
$body2 = @{ model = "sm-auto"; messages = @(@{ role = "user"; content = "hi" }); max_tokens = 16 } | ConvertTo-Json -Depth 6
Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body2 | Out-Null
$after1 = CountCalls "sm-k1"
$req1Calls = $after1 - $before1
Write-Host "request #1 upstream calls: $req1Calls"
if ($req1Calls -ne 2) {
  Write-Host "FAIL: first request should cost 2 calls (fail + heal), got $req1Calls"
  exit 1
}

$before2 = CountCalls "sm-k1"
Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body2 | Out-Null
$after2 = CountCalls "sm-k1"
$req2Calls = $after2 - $before2
Write-Host "request #2 upstream calls: $req2Calls"
if ($req2Calls -ne 1) {
  Write-Host "FAIL: second request should cost only 1 call (learned cache applied), got $req2Calls"
  exit 1
}
Write-Host "PASS: learned cache eliminated the extra round trip starting from request #2"

Step "3) combo member's token_param_mode overrides account/provider"
# sm-auto has NO explicit mode anywhere except the combo member pin, so the
# member's max_completion_tokens override must be what makes the first call
# succeed in exactly 1 upstream dispatch (no reactive heal round trip).
$combo = @{
  id = "sm-combo"; display_name = "Member Pin"; rotation = "all"
  enabled = $true
  members = @(@{
    provider_id = "sm-auto"; model = "openai/gpt-5.6-sol"
    token_param_mode = "max_completion_tokens"
  })
}
try { Dash Delete "combos/sm-combo" | Out-Null } catch {}
Dash Post "combos" $combo | Out-Null
$before3 = CountCalls "sm-k1"
$body3 = @{ model = "sm-combo"; messages = @(@{ role = "user"; content = "hi" }); max_tokens = 16 } | ConvertTo-Json -Depth 6
Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body3 | Out-Null
$after3 = CountCalls "sm-k1"
$req3Calls = $after3 - $before3
Write-Host "combo request upstream calls: $req3Calls"
if ($req3Calls -ne 1) {
  Write-Host "FAIL: combo member override should cost exactly 1 call, got $req3Calls"
  exit 1
}
Write-Host "PASS: combo member token_param_mode override skipped the heal round trip"

Write-Host "`nALL TOKEN-PARAM MODE SMOKE CHECKS PASSED"
