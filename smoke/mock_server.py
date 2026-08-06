"""Mock OpenAI-compatible upstreams for llm-gateway smoke testing.

Two servers:
  :19871  "vercel mock" — POSTs require its own token; per-key behavior via dict.
  :19872  "nvidia mock" — same; one key is permanently quota-dead (429).

Every accepted request echoes back the received model, auth header, and a static
marker so the smoke script can assert which (key, model) pair actually served.
Writes a JSON line per request to <log_path> for after-the-fact verification.

Usage: python mock_server.py <8888-pid-file>
"""

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# (port label, port, {token: behavior}) — behavior: "ok" | "429"
SERVERS = [
    {
        "name": "vercel-mock",
        "port": 19871,
        "keys": {"vk-alpha": "ok", "vk-beta": "ok"},
        "models": ["openai/gpt-oss-120b", "kimi-k3"],
    },
    {
        "name": "nvidia-mock",
        "port": 19872,
        "keys": {"nk-live": "ok", "nk-dead": "429"},
        "models": ["moonshotai/kimi-k3", "openai/gpt-oss-120b"],
    },
]

LOG_PATH = sys.argv[1] if len(sys.argv) > 1 else "requests.log"
log_lock = threading.Lock()


def make_handler(cfg):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass  # silence

        def _record(self, entry):
            with log_lock, open(LOG_PATH, "a", encoding="utf-8") as f:
                f.write(json.dumps(entry) + "\n")

        def _send_json(self, code, obj):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path.rstrip("/").endswith("/models"):
                models = [{"id": m, "object": "model", "owned_by": cfg["name"]} for m in cfg["models"]]
                self._record({"upstream": cfg["name"], "path": self.path, "kind": "list_models"})
                self._send_json(200, {"object": "list", "data": models})
                return
            self._send_json(404, {"error": {"message": "unknown path: " + self.path}})

        def do_POST(self):
            if not self.path.rstrip("/").endswith("/chat/completions"):
                self._send_json(404, {"error": {"message": "unknown path: " + self.path}})
                return
            length = int(self.headers.get("Content-Length", 0))
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw)
            except json.JSONDecodeError:
                payload = {}
            auth = self.headers.get("Authorization", "")
            token = auth.replace("Bearer ", "", 1)
            behavior = cfg["keys"].get(token)
            self._record({
                "upstream": cfg["name"],
                "kind": "chat",
                "token": token,
                "model": payload.get("model"),
                "stream": payload.get("stream", False),
            })
            if behavior is None:
                self._send_json(401, {"error": {"message": "invalid api key", "type": "auth_error"}})
                return
            if behavior == "429":
                self._send_json(429, {"error": {"message": "quota exceeded", "type": "rate_limit"}})
                return
            model = payload.get("model", "?")
            resp = {
                "id": "chatcmpl-mock",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": f"[{cfg['name']}] served by token={token} model={model}",
                    },
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
            }
            if payload.get("stream"):
                # Stream the same content as two SSE chunks so streaming paths are
                # exercised by the smoke run as well.
                lines = [
                    "data: " + json.dumps({"id": "chatcmpl-mock", "object": "chat.completion.chunk",
                                           "model": model,
                                           "choices": [{"index": 0, "delta": {"content": resp["choices"][0]["message"]["content"]},
                                                        "finish_reason": None}]}),
                    "",
                    "data: " + json.dumps({"id": "chatcmpl-mock", "object": "chat.completion.chunk",
                                           "model": model,
                                           "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                                           "usage": resp["usage"]}),
                    "",
                    "data: [DONE]",
                    "",
                ]
                body = ("\n".join(lines) + "\n").encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Connection", "keep-alive")
                self.send_header("X-Accel-Buffering", "no")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            self._send_json(200, resp)

    return Handler


def main():
    servers = []
    for cfg in SERVERS:
        srv = ThreadingHTTPServer(("127.0.0.1", cfg["port"]), make_handler(cfg))
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        servers.append(srv)
        print(f"{cfg['name']} listening on 127.0.0.1:{cfg['port']}", flush=True)
    threading.Event().wait()  # run forever; killed by the smoke runner


if __name__ == "__main__":
    main()
