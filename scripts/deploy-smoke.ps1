<#
.SYNOPSIS
  Local "PaaS simulation" smoke test for llm-gateway. Reproduces exactly what CI
  and a real container deployment do (distroless image, named volume, nonroot
  user, mocked SSE upstream) and exercises the risky paths before you push:

    - image builds like CI (amd64 + multi-arch COPY / TARGETOS / TARGETARCH)
    - container boots as nonroot and SQLite opens (catches the CANTOPEN crash)
    - dashboard login + provider + combo CRUD through the admin API
    - API-key auth on /v1/*
    - SSE streaming through a slow mock (catches stream-timeout regressions)
    - streaming usage logged as real tokens (not zeros)

  Run:  powershell -ExecutionPolicy Bypass -File scripts\deploy-smoke.ps1
  Exits non-zero on any failure so it can gate a pre-push step.
#>
$ErrorActionPreference = 'Continue'   # docker writes benign messages to stderr
$fail = $false
function Pass($m) { Write-Host "PASS - $m" }
function Fail($m) { Write-Host "FAIL - $m"; $script:fail = $true }
function Remove-IfExists($name) { docker rm -f $name 2>&1 | Out-Null }

# --- embedded mock upstream (slow SSE to prove the header-timeout fix) ---
$mockPy = @'
import http.server, json, time
class H(http.server.BaseHTTPRequestHandler):
    def _send_sse(self):
        self.send_response(200); self.send_header("Content-Type","text/event-stream"); self.end_headers()
        for c in ["Hel","lo, ","from ","mock!"]:
            self.wfile.write(f"data: {json.dumps({"choices":[{"delta":{"content":c}}]})}\n\n".encode()); self.wfile.flush(); time.sleep(0.3)
        self.wfile.write(b'data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11}}\n\n')
        self.wfile.write(b"data: [DONE]\n\n"); self.wfile.flush()
    def do_POST(self):
        n=int(self.headers.get("Content-Length",0)); body=self.rfile.read(n).decode(); data=json.loads(body) if body else {}
        if self.path.startswith("/v1/chat/completions"):
            if data.get("stream"): self._send_sse()
            else:
                self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
                self.wfile.write(json.dumps({"id":"chatcmpl-mock","created":1,"model":data.get("model","m"),"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}).encode())
        else: self.send_response(404); self.end_headers()
    def log_message(self,*a): pass
http.server.ThreadingHTTPServer(("0.0.0.0",9099),H).serve_forever()
'@

$mockFile = Join-Path $env:TEMP 'llm-gateway-mockup.py'
$mockPy | Out-File -Encoding ascii $mockFile

# --- 1. Build exactly like the Release workflow (amd64 stage of multi-arch) ---
docker buildx build --platform linux/amd64 -t llm-gateway:local --load . | Out-Null
if ($LASTEXITCODE -ne 0) { Fail "docker build"; exit 1 }
Pass "docker build (multi-arch COPY / TARGETOS / TARGETARCH)"

# --- 2. Fresh named volume, identical to a PaaS deploy ---
Remove-IfExists gw-sim; Remove-IfExists gw-mock
docker volume rm gw-data-sim 2>&1 | Out-Null
docker network rm gw-net 2>&1 | Out-Null
docker network create gw-net | Out-Null
docker volume create gw-data-sim | Out-Null
docker run -d --name gw-mock --network gw-net -v "$mockFile`:/mockup.py" python:3.12-alpine python /mockup.py | Out-Null
docker run -d --name gw-sim --network gw-net `
  -e GATEWAY_API_KEY=gw-test -e DASHBOARD_PASSWORD=admin123 `
  -e DASHBOARD_SECRET=0123456789abcdef0123456789abcdef `
  -e DB_PATH=/data/gateway.db -v gw-data-sim:/data -p 18080:8080 `
  llm-gateway:local | Out-Null
Start-Sleep -Seconds 4

try {
  if ((Invoke-WebRequest http://localhost:18080/healthz -UseBasicParsing -TimeoutSec 10).StatusCode -eq 200) { Pass "container boots as nonroot + DB opens (no CANTOPEN)" }
} catch { Fail "boot/healthz: $($_.Exception.Message)" }

# --- 3. Dashboard login + provider + combo (same API the SPA uses) ---
try {
  Invoke-WebRequest "http://localhost:18080/dashboard/api/login" -Method POST -ContentType "application/json" -Body (@{password="admin123"}|ConvertTo-Json) -SessionVariable s -UseBasicParsing -TimeoutSec 10 | Out-Null
  Pass "dashboard login"
  Invoke-WebRequest "http://localhost:18080/dashboard/api/providers" -Method POST -WebSession $s -ContentType "application/json" -Body (@{id="mockprov";base_url="http://gw-mock:9099";auth_key="k";model="mock-model";weight=1;enabled=$true}|ConvertTo-Json) -UseBasicParsing -TimeoutSec 10 | Out-Null
  Pass "provider create"
  Invoke-WebRequest "http://localhost:18080/dashboard/api/combos" -Method POST -WebSession $s -ContentType "application/json" -Body (@{id="chatcombo";rotation="round-robin";members=@("mockprov");enabled=$true}|ConvertTo-Json) -UseBasicParsing -TimeoutSec 10 | Out-Null
  Pass "combo create"
} catch { Fail "dashboard/admin API: $($_.Exception.Message)" }

# --- 4. API-key auth ---
try {
  Invoke-WebRequest http://localhost:18080/v1/models -UseBasicParsing -TimeoutSec 10 | Out-Null; Fail "unauth /v1/models should 401"
} catch { if ($_.Exception.Response.StatusCode.value__ -eq 401) { Pass "unauth /v1 -> 401" } else { Fail "unauth status: $($_.Exception.Message)" } }

# --- 5. SSE streaming through the slow mock (catches stream-timeout bugs) ---
try {
  $h=@{Authorization="Bearer gw-test";"Content-Type"="application/json"}
  $body='{"model":"chatcombo","messages":[{"role":"user","content":"hi"}],"stream":true}'
  $resp=Invoke-WebRequest http://localhost:18080/v1/chat/completions -Method POST -Headers $h -Body $body -UseBasicParsing -TimeoutSec 30
  $text=$resp.Content
  if ($resp.StatusCode -eq 200 -and $text -match 'mock!' -and $text -match 'DONE') { Pass "SSE stream completes (4 slow chunks + DONE)" } else { Fail "SSE stream truncated: $text" }
} catch { Fail "SSE stream: $($_.Exception.Message)" }

# --- 6. Streaming usage is logged as real tokens (#12) ---
try {
  Start-Sleep -Milliseconds 400
  $logs=(Invoke-WebRequest "http://localhost:18080/dashboard/api/logs?limit=5" -WebSession $s -UseBasicParsing -TimeoutSec 10 | ConvertFrom-Json).items
  $streamLog = $logs | Where-Object { $_.endpoint -eq 'chat.completions' }
  if ($streamLog -and ($streamLog | Where-Object { $_.prompt_tokens -gt 0 -or $_.completion_tokens -gt 0 })) { Pass "streaming usage logged as real tokens" } else { Fail "streaming tokens logged as zero" }
} catch { Fail "logs check: $($_.Exception.Message)" }

# --- Cleanup ---
Remove-IfExists gw-sim; Remove-IfExists gw-mock
docker network rm gw-net 2>&1 | Out-Null
docker volume rm gw-data-sim 2>&1 | Out-Null
docker image rm llm-gateway:local 2>&1 | Out-Null
Remove-Item $mockFile -ErrorAction SilentlyContinue

if ($fail) { Write-Host "`nSMOKE TEST FAILED"; exit 1 } else { Write-Host "`nALL SMOKE TESTS PASSED" }
