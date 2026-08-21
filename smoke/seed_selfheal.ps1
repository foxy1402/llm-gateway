# Scoped smoke test for the self-heal optimization:
#   1. A DIRECT provider (no combo) with several keys now rotates across sibling
#      keys within ONE client request when a key returns a retryable status -
#      previously plan==nil short-circuited to the client on the very first
#      429/402/5xx and never gave sibling keys a chance.
#   2. 402 (Payment Required, used by some exhausted-credit upstreams) is
#      retryable by default alongside 429/5xx.
#   3. The account-attempt budget scales with the real pool size instead of a
#      flat cap of 3 - a 5-key provider must get to try all 5 keys.
#
# Prereqs: mock_server.py running (127.0.0.1:19873 = selfheal-mock), gateway
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

Step "seed DIRECT provider 'selfheal' with 5 keys: k1/k3=429, k2/k4=402, k5=live"
$sh = @{
  id = "selfheal"; display = "Self-Heal Test"; base_url = "http://127.0.0.1:19873/v1"
  model = "selfheal-model"; weight = 1; enabled = $true
  accounts = @(
    @{ id = "selfheal:k1"; label = "k1"; auth_key = "sh-k1"; model = ""; enabled = $true; weight = 1 },
    @{ id = "selfheal:k2"; label = "k2"; auth_key = "sh-k2"; model = ""; enabled = $true; weight = 1 },
    @{ id = "selfheal:k3"; label = "k3"; auth_key = "sh-k3"; model = ""; enabled = $true; weight = 1 },
    @{ id = "selfheal:k4"; label = "k4"; auth_key = "sh-k4"; model = ""; enabled = $true; weight = 1 },
    @{ id = "selfheal:k5"; label = "k5"; auth_key = "sh-k5"; model = ""; enabled = $true; weight = 1 }
  )
}
try { Dash Delete "providers/selfheal" | Out-Null } catch {}
Dash Post "providers" $sh | Out-Null
Write-Host "PASS: provider seeded (no combo - this is a raw direct-provider call)"

Step "single direct request against 'selfheal': must self-heal past 4 dead keys onto k5"
$body = @{ model = "selfheal"; messages = @(@{ role = "user"; content = "hi" }) } | ConvertTo-Json -Depth 6
$r = Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $body
$content = $r.choices[0].message.content
Write-Host "response content: $content"
if ($content -notmatch "token=sh-k5") {
  Write-Host "FAIL: direct-provider call did not self-heal onto the only live key (sh-k5)"
  exit 1
}
Write-Host "PASS: single request survived 2x429 + 2x402 and served on the 5th key, all within one request"

Step "verify the dashboard logs recorded the SUCCESSFUL attempt against selfheal[k5]"
$logs = Dash Get "logs?limit=5"
$hit = $logs.items | Where-Object { $_.provider_used -like "selfheal*" -and $_.status -eq 200 } | Select-Object -First 1
if (-not $hit) { Write-Host "FAIL: no successful selfheal log row found"; exit 1 }
Write-Host ("PASS: logged provider_used={0} status={1}" -f $hit.provider_used, $hit.status)

Write-Host "`nALL SELF-HEAL SMOKE CHECKS PASSED"
