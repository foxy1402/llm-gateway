# Scoped smoke test for the vision/OCR audit fixes:
#   1. MAX_REQUEST_BODY_MB raises the incoming body cap past the old 4MiB default
#      (large base64 images no longer get a silent 413).
#   2. /v1/responses input_image parts survive translation to chat.completions
#      instead of being dropped (ResponsesToChatRequest + translateContentParts).
#   3. The cap is still enforced — an oversized body must still be rejected.
#
# Prereqs: mock_server.py running (127.0.0.1:19871), gateway running on :18089
# with GATEWAY_API_KEY=smoke-secret, DASHBOARD_PASSWORD=smoke-dashboard,
# DASHBOARD_SECRET (>=32 chars), MAX_REQUEST_BODY_MB=25 (or unset, same default),
# and a FRESH database.
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

Step "seed provider vision-test (points at the vercel-mock upstream, key vk-alpha)"
$vision = @{
  id = "vision-test"; display = "Vision Test"; base_url = "http://127.0.0.1:19871/v1"
  model = "kimi-k3"; weight = 1; enabled = $true
  accounts = @(@{ id = "vision-test:a"; label = "a"; auth_key = "vk-alpha"; model = ""; enabled = $true; weight = 1 })
}
try { Dash Delete "providers/vision-test" | Out-Null } catch {}
Dash Post "providers" $vision | Out-Null
Write-Host "PASS: provider seeded"

Step "1a: /v1/responses with input_text + input_image must forward the image (not drop it)"
$fakeImage = "data:image/png;base64," + ("A" * 400)  # deterministic stand-in for a real base64 payload
$reqBody = @{
  model = "vision-test"
  input = @(@{
    type = "message"; role = "user"
    content = @(
      @{ type = "input_text"; text = "What is in this image?" },
      @{ type = "input_image"; image_url = $fakeImage }
    )
  })
} | ConvertTo-Json -Depth 8
$resp = Invoke-RestMethod -Method Post -Uri "$base/v1/responses" -Headers $H -Body $reqBody
$text = $resp.output[0].content[0].text
Write-Host "responses output: $text"
if ($text -notmatch "images=1") { Write-Host "FAIL: image part never reached the upstream (dropped in translation)"; exit 1 }
if ($text -notmatch ("image_len=" + $fakeImage.Length)) { Write-Host "FAIL: image bytes truncated/corrupted in translation"; exit 1 }
Write-Host "PASS: input_image survived /v1/responses -> chat.completions translation intact"

Step "1b: same request, but a text-only array-content message must still work (no regression)"
$reqBody2 = @{
  model = "vision-test"
  input = @(@{ type = "message"; role = "user"; content = @(@{ type = "input_text"; text = "just text, no image" }) })
} | ConvertTo-Json -Depth 8
$resp2 = Invoke-RestMethod -Method Post -Uri "$base/v1/responses" -Headers $H -Body $reqBody2
if ($resp2.output[0].content[0].text -notmatch "images=0") { Write-Host "FAIL: text-only path regressed"; exit 1 }
Write-Host "PASS: text-only /v1/responses request unaffected"

Step "2: a ~6MiB body (over the OLD 4MiB default, under the NEW 25MiB cap) must be accepted"
$sixMiB = 6 * 1024 * 1024
$bigContent = "a" * $sixMiB
$bigBody = '{"model":"vision-test","messages":[{"role":"user","content":"' + $bigContent + '"}]}'
$r = Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $bigBody
if ($r.choices[0].message.content -notmatch "body_len=") { Write-Host "FAIL: unexpected response shape"; exit 1 }
Write-Host ("PASS: {0}MiB body accepted (would have been rejected under the old 4MiB cap): {1}" -f [math]::Round($sixMiB/1MB,1), $r.choices[0].message.content)

Step "3: a body over the 25MiB cap must still be rejected with 413"
$overCapMiB = 26 * 1024 * 1024
$overContent = "a" * $overCapMiB
$overBody = '{"model":"vision-test","messages":[{"role":"user","content":"' + $overContent + '"}]}'
try {
  Invoke-RestMethod -Method Post -Uri "$base/v1/chat/completions" -Headers $H -Body $overBody | Out-Null
  Write-Host "FAIL: oversized body was accepted (cap not enforced)"
  exit 1
} catch {
  $code = $_.Exception.Response.StatusCode.value__
  if ($code -ne 413) { Write-Host "FAIL: expected 413, got $code"; exit 1 }
  Write-Host "PASS: oversized (26MiB) body rejected with 413 -- cap still enforced"
}

Write-Host "`nALL VISION/BODY-SIZE SMOKE CHECKS PASSED"
