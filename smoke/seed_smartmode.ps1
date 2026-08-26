# Scoped smoke test for max_tokens/max_completion_tokens "smart mode":
#   Reproduces the live Lightning AI failure (model "openai/gpt-5.6-sol" hard-
#   rejects max_tokens with "this model is not supported MaxTokens, please use
#   MaxCompletionTokens", 500, plain text) and proves the gateway auto-detects
#   this, swaps the field, and transparently retries on the SAME key - the
#   client sends max_tokens and just sees a normal 200, no config needed.
#
# Prereqs: mock_server.py running (127.0.0.1:19874 = smartmode-mock), gateway
# running on :18089 with GATEWAY_API_KEY=smoke-secret,
# DASHBOARD_PASSWORD=smoke-dashboard, DASHBOARD_SECRET (>=32 chars), and a
# FRESH database.
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

Step "seed DIRECT provider 'smartmode' -> smartmode-mock (rejects max_tokens, accepts max_completion_tokens)"
$sm = @{
  id = "smartmode"; display = "Smart Mode Test"; base_url = "http://127.0.0.1:19874/v1"
  model = "openai/gpt-5.6-sol"; auth_key = "sm-k1"; weight = 1; enabled = $true
}
try { Dash Delete "providers/smartmode" | Out-Null } catch {}
Dash Post "providers" $sm | Out-Null
Write-Host "PASS: provider seeded"

Step "client sends max_tokens (legacy field) - must succeed transparently"
$body = @{ model = "smartmode"; messages = @(@{ role = "user"; content = "hi" }); max_tokens = 16 } | ConvertTo-Json -Depth 6
$r = Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body
$content = $r.choices[0].message.content
Write-Host "response content: $content"
if ($content -notmatch "token=sm-k1") {
  Write-Host "FAIL: expected a successful response served by sm-k1 after auto-heal, got: $content"
  exit 1
}
Write-Host "PASS: max_tokens request auto-healed to max_completion_tokens and succeeded with a single client-visible 200"

Step "verify the dashboard logged a SUCCESSFUL attempt, not the intermediate 500"
# The request log is written by an async goroutine (p.log fires and forgets),
# so a brief settle prevents racing the write that the next step asserts on.
Start-Sleep -Milliseconds 400
$logs = Dash Get "logs?limit=5"
$hit = $logs.items | Where-Object { $_.provider_used -like "smartmode*" -and $_.status -eq 200 } | Select-Object -First 1
if (-not $hit) { Write-Host "FAIL: no successful smartmode log row found (client must never see the intermediate 500)"; exit 1 }
Write-Host ("PASS: logged provider_used={0} status={1}" -f $hit.provider_used, $hit.status)

Step "client sends max_completion_tokens (new field) directly - must work with no retry needed"
$body2 = @{ model = "smartmode"; messages = @(@{ role = "user"; content = "hi" }); max_completion_tokens = 16 } | ConvertTo-Json -Depth 6
$r2 = Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body2
if ($r2.choices[0].message.content -notmatch "token=sm-k1") {
  Write-Host "FAIL: max_completion_tokens request should succeed on the first try"
  exit 1
}
Write-Host "PASS: max_completion_tokens request succeeded directly (no healing needed)"

Write-Host "`nALL SMART MODE SMOKE CHECKS PASSED"
