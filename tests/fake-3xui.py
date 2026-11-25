#!/usr/bin/env python3
"""一台行为逼真的假 3X-UI 面板，用于端到端验证控制台的真实对接链路。

刻意照实现 3X-UI 的三条真实约束：
  1. 登录是 form 表单，成功靠 Set-Cookie（body 里没有 token）；
  2. cookie 名用新版 "3x-ui"；
  3. /panel/api 全部靠那个 cookie 鉴权。
另外还实现 addClient / delClient / resetClientTraffic，让「开户」也能真验证。

监听 0.0.0.0:20653（公网可达，以便生产机的 API 能连过来）。
"""
import json
import re
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 20653
SESSIONS = set()
TOKEN = "fake-xui-session-token-0001"

# 面板里的入站。settings 刻意存成**字符串**（3X-UI 的真实形态）。
INBOUNDS = [
    {
        "id": 1, "port": 443, "protocol": "vless", "remark": "测试主入口", "enable": True,
        "up": 1073741824, "down": 5368709120, "total": 0, "expiryTime": 0,
        "settings": json.dumps({
            "clients": [
                {"id": "uuid-alpha", "email": "alpha@test.local", "enable": True,
                 "totalGB": 10737418240, "expiryTime": 0,
                 "clientStats": {"up": 1073741824, "down": 5368709120}},
                {"id": "uuid-beta", "email": "beta@test.local", "enable": True,
                 "totalGB": 0, "expiryTime": 0,
                 "clientStats": {"up": 1024, "down": 2048}},
            ],
            "decryption": "none",
        }),
    },
    {
        "id": 2, "port": 8443, "protocol": "trojan", "remark": "测试备用", "enable": True,
        "up": 0, "down": 0, "total": 0, "expiryTime": 0,
        "settings": json.dumps({
            "clients": [
                {"password": "trojan-pw-1", "email": "gamma@test.local", "enable": True,
                 "totalGB": 5368709120, "expiryTime": 0,
                 "clientStats": {"up": 0, "down": 0}},
            ],
        }),
    },
]


def envelope(success, msg="", obj=None):
    return json.dumps({"success": success, "msg": msg, "obj": obj}).encode()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def _send(self, code, payload, cookie=None):
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        if cookie:
            self.send_header("Set-Cookie", f"3x-ui={cookie}; Path=/; HttpOnly")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _authed(self):
        raw = self.headers.get("Cookie", "")
        match = re.search(r"3x-ui=([^;]+)", raw)
        return bool(match and match.group(1) in SESSIONS)

    def do_POST(self):
        path = urlparse(self.path).path
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode()

        if path == "/login":
            form = parse_qs(body)
            if form.get("username", [""])[0] == "admin" and form.get("password", [""])[0] == "xui-test-pass":
                SESSIONS.add(TOKEN)
                self._send(200, envelope(True, "Login Successfully"), cookie=TOKEN)
            else:
                self._send(200, envelope(False, "invalid credentials"))
            return

        if not self._authed():
            self._send(401, envelope(False, "not logged in"))
            return

        if path == "/panel/api/inbounds/addClient":
            form = parse_qs(body)
            settings = form.get("settings", [""])[0]
            try:
                parsed = json.loads(settings)
                client = parsed["clients"][0]
            except Exception as exc:  # noqa: BLE001
                self._send(200, envelope(False, f"malformed settings: {exc}"))
                return
            target = next((i for i in INBOUNDS if i["id"] == int(form.get("id", ["0"])[0])), None)
            if target is None:
                self._send(200, envelope(False, "inbound not found"))
                return
            existing = json.loads(target["settings"])
            existing["clients"].append(client)
            target["settings"] = json.dumps(existing)
            print(f"[addClient] 新增 {client.get('email')} → 入站 {target['id']}，现有 {len(existing['clients'])} 个", flush=True)
            self._send(200, envelope(True, "Client added"))
            return

        if "/delClient/" in path:
            parts = path.split("/")
            inbound_id, client_id = int(parts[5]), parts[7]
            target = next((i for i in INBOUNDS if i["id"] == inbound_id), None)
            if target is None:
                self._send(200, envelope(False, "inbound not found"))
                return
            existing = json.loads(target["settings"])
            before = len(existing["clients"])
            existing["clients"] = [
                c for c in existing["clients"]
                if not (c.get("id") == client_id or c.get("password") == client_id or c.get("email") == client_id)
            ]
            target["settings"] = json.dumps(existing)
            print(f"[delClient] 入站 {inbound_id} 从 {before} 减到 {len(existing['clients'])}", flush=True)
            self._send(200, envelope(True, "Client deleted"))
            return

        if "/resetClientTraffic/" in path:
            email = path.split("/")[-1]
            print(f"[resetTraffic] {email}", flush=True)
            self._send(200, envelope(True, "Traffic reset"))
            return

        self._send(404, envelope(False, "unknown endpoint"))

    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/panel/api/inbounds/list":
            if not self._authed():
                self._send(401, envelope(False, "not logged in"))
                return
            self._send(200, envelope(True, "", INBOUNDS))
            return
        if path == "/panel/api/inbounds/onlines":
            if not self._authed():
                self._send(401, envelope(False, "not logged in"))
                return
            self._send(200, envelope(True, "", ["alpha@test.local"]))
            return
        self._send(404, envelope(False, "not found"))


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
