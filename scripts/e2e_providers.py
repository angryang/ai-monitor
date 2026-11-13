#!/usr/bin/env python3
"""End-to-end check of the AI service provider registry.

Starts a fake OpenAI-compatible SSE upstream, boots the ai-monitor binary
against a throwaway database, then drives the whole flow through the HTTP
API:

    login -> create -> list -> per-model check -> patch -> delete

and asserts the multi-model contract: one provider carries several model
names, saving never contacts the upstream (checking is an explicit,
per-model action), and a model still selected by an AI request cannot be
removed from the list.

The fake upstream rejects the request with 401 when the api key is not the
expected one, so the "wrong key" case can be exercised too.

Usage: python3 scripts/e2e_providers.py [path-to-binary]
"""
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

UPSTREAM_PORT = 18097
DEAD_PORT = 18096
ADMIN_PORT = 18095
PASSWORD = "LocalAdmin2026!Q"
FAKE_KEY = "sk-fake-e2e"


class FakeUpstream(BaseHTTPRequestHandler):
    """A minimal OpenAI-compatible streaming chat endpoint."""

    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        auth = self.headers.get("Authorization", "")
        if auth != f"Bearer {FAKE_KEY}":
            body = json.dumps({"error": {"message": "Incorrect API key provided"}}).encode()
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("X-Req-Id", "fake-upstream-req-1")
        self.send_header("Connection", "close")
        self.end_headers()
        payload = {"choices": [{"index": 0, "delta": {"content": "pong"}, "finish_reason": ""}]}
        self.wfile.write(f"data: {json.dumps(payload)}\n\n".encode())
        self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

    def log_message(self, *args):
        pass


def api(method, path, body=None, token=None, port=ADMIN_PORT):
    url = f"http://127.0.0.1:{port}{path}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
            return resp.status, (json.loads(payload or b"null"))
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload or b"null")
        except Exception:
            return e.code, payload.decode(errors="replace")


def wait_http(port, path="/healthz", timeout=20):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            status, _ = api("GET", path, port=port)
            if status == 200:
                return True
        except Exception:
            pass
        time.sleep(0.2)
    return False


def check(label, ok, detail=""):
    mark = "PASS" if ok else "FAIL"
    print(f"  [{mark}] {label}" + (f" — {detail}" if detail else ""))
    if not ok:
        globals()["FAILURES"] = globals().get("FAILURES", 0) + 1
    return ok


def provider_body(**overrides):
    body = {
        "name": "e2e-provider",
        "description": "端到端验证用",
        "protocol": "openai",
        "base_url": f"http://127.0.0.1:{UPSTREAM_PORT}",
        "api_key": FAKE_KEY,
        "models": ["fake-model-a", "fake-model-b"],
        "timeout": "10s",
        "openai": {"stream_path": "/v1/chat/completions"},
    }
    body.update(overrides)
    return body


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else "./ai-monitor"
    binary = os.path.abspath(binary)
    if not os.path.exists(binary):
        print(f"binary not found: {binary}")
        return 2

    workdir = tempfile.mkdtemp(prefix="aim-prov-e2e-")
    srv = HTTPServer(("127.0.0.1", UPSTREAM_PORT), FakeUpstream)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    cfg_path = os.path.join(workdir, "config.yaml")
    with open(cfg_path, "w") as f:
        f.write(
            f"sqlite_path: {workdir}/e2e.db\n"
            f'http_addr: "127.0.0.1:{ADMIN_PORT}"\n'
            "max_concurrent: 4\n"
            "provider_check_timeout: 8s\n"
        )

    env = dict(os.environ)
    env.update(
        {
            "AI_MONITOR_MASTER_KEY": "0123456789abcdef0123456789abcdef",
            "AI_MONITOR_JWT_SECRET": "0123456789abcdef0123456789abcdef",
            "AI_MONITOR_ADMIN_USERNAME": "admin",
            "AI_MONITOR_ADMIN_PASSWORD": PASSWORD,
            "WECOM_WEBHOOK": "https://qyapi.weixin.qq.com/cgi-api/webhook/send?key=test",
        }
    )
    proc = subprocess.Popen(
        [binary, "-config", cfg_path],
        env=env,
        stdout=open(os.path.join(workdir, "server.log"), "w"),
        stderr=subprocess.STDOUT,
    )

    try:
        if not wait_http(ADMIN_PORT):
            print("server did not come up; log:")
            print(open(os.path.join(workdir, "server.log")).read()[-4000:])
            return 2

        print("\n== 1. 登录 ==")
        status, body = api("POST", "/auth/login", {"username": "admin", "password": PASSWORD})
        if not check("登录成功", status == 200 and "token" in (body or {}), f"status={status}"):
            return 1
        token = body["token"]

        print("\n== 2. 保存不再强制连通性校验 ==")
        status, prov = api(
            "POST", "/admin/providers",
            provider_body(name="dead-provider", base_url=f"http://127.0.0.1:{DEAD_PORT}"),
            token=token,
        )
        if not check("不可达的上游仍可保存（201）", status == 201, f"status={status} body={prov}"):
            return 1
        check("响应里没有校验结论", "check" not in prov, json.dumps(prov)[:200])
        check("未标记为已校验", prov.get("last_verified_ok") is False, str(prov.get("last_verified_ok")))
        dead_id = prov["id"]

        # Only the explicit check route touches the upstream.
        status, res = api(
            "POST", "/admin/providers/check",
            provider_body(name="scratch", base_url=f"http://127.0.0.1:{DEAD_PORT}",
                          model="fake-model-a"),
            token=token,
        )
        check("按模型测试返回 200", status == 200, f"status={status} body={res}")
        check("结论为不通", (res or {}).get("ok") is False, json.dumps(res)[:200])
        check("结论类型为 network_error", (res or {}).get("kind") == "network_error", str((res or {}).get("kind")))
        check("结论标明被测模型", (res or {}).get("model") == "fake-model-a", str((res or {}).get("model")))
        status, listing = api("GET", "/admin/providers", token=token)
        check("校验没有额外创建服务商", len(listing.get("providers") or []) == 1, json.dumps(listing)[:200])
        status, _ = api("DELETE", f"/admin/providers/{dead_id}", token=token)
        check("清理掉不可达的服务商", status == 200, f"status={status}")

        print("\n== 3. 按模型测试能区分密钥错误 ==")
        status, res = api(
            "POST", "/admin/providers/check",
            provider_body(name="bad-key", api_key="sk-wrong", model="fake-model-b"),
            token=token,
        )
        check("校验返回 200", status == 200, f"status={status} body={res}")
        check("结论类型为 unauthorized", (res or {}).get("kind") == "unauthorized", str((res or {}).get("kind")))
        check("带上了上游的状态码", (res or {}).get("status_code") == 401, str((res or {}).get("status_code")))
        check("带上了上游自己的说明", "Incorrect API key" in ((res or {}).get("message") or ""),
              str((res or {}).get("message"))[:120])
        check("被测模型是第二个", (res or {}).get("model") == "fake-model-b", str((res or {}).get("model")))

        print("\n== 4. 多模型服务商可以保存 ==")
        status, prov = api("POST", "/admin/providers", provider_body(), token=token)
        if not check("创建成功返回 201", status == 201, f"status={status} body={prov}"):
            return 1
        pid = prov["id"]
        check("响应不包含明文 API Key", FAKE_KEY not in json.dumps(prov))
        check("has_api_key 为真", prov.get("has_api_key") is True)
        check("候选模型为两个", prov.get("models") == ["fake-model-a", "fake-model-b"],
              str(prov.get("models")))
        check("保存不触发校验", "check" not in prov and prov.get("last_verified_ok") is False,
              json.dumps(prov)[:200])
        for model in ("fake-model-a", "fake-model-b"):
            status, res = api("POST", "/admin/providers/check",
                              provider_body(name="scratch", model=model), token=token)
            check(f"模型 {model} 单独测试通过", status == 200 and (res or {}).get("ok") is True
                  and (res or {}).get("model") == model, json.dumps(res)[:200])

        print("\n== 5. 列表与详情 ==")
        status, listing = api("GET", "/admin/providers", token=token)
        providers = listing.get("providers") or []
        check("列表里有一条", len(providers) == 1, str(len(providers)))
        check("列表不泄露明文密钥", FAKE_KEY not in json.dumps(listing))
        check("列表里的服务商带 models", providers[0].get("models") == ["fake-model-a", "fake-model-b"],
              str(providers[0].get("models")))
        status, one = api("GET", f"/admin/providers/{pid}", token=token)
        check("可以按 id 取到详情", status == 200 and one.get("id") == pid, f"status={status}")
        check("详情里没有旧的 model 字段", "model" not in one, json.dumps(one)[:200])

        print("\n== 6. 用已存的密钥按模型复验 ==")
        status, res = api("POST", f"/admin/providers/{pid}/check",
                          {"model": "fake-model-b"}, token=token)
        check("复验返回 200", status == 200, f"status={status} body={res}")
        check("复验结论为通", (res or {}).get("ok") is True, json.dumps(res)[:200])
        check("复验的是指定模型", (res or {}).get("model") == "fake-model-b", str((res or {}).get("model")))
        status, res = api("POST", f"/admin/providers/{pid}/check",
                          {"model": "does-not-exist"}, token=token)
        check("未知模型返回 400", status == 400, f"status={status} body={res}")

        print("\n== 6b. 只校验不落库 ==")
        status, res = api("POST", "/admin/providers/check",
                          provider_body(name="scratch", model="fake-model-a"), token=token)
        check("校验返回 200", status == 200, f"status={status} body={res}")
        check("结论为通", (res or {}).get("ok") is True, json.dumps(res)[:200])
        status, listing = api("GET", "/admin/providers", token=token)
        check("校验没有创建服务商", len(listing.get("providers") or []) == 1, json.dumps(listing)[:200])

        print("\n== 7. 重名校验 ==")
        status, body = api("POST", "/admin/providers", provider_body(), token=token)
        check("重名返回 409", status == 409, f"status={status} body={body}")

        print("\n== 8. 更新候选模型（不带密钥）==")
        status, updated = api(
            "PATCH", f"/admin/providers/{pid}",
            {"models": ["fake-model-b", "fake-model-c"]}, token=token,
        )
        if not check("更新返回 200", status == 200, f"status={status} body={updated}"):
            return 1
        check("候选模型已更新", updated.get("models") == ["fake-model-b", "fake-model-c"],
              str(updated.get("models")))
        check("名称保持不变", updated.get("name") == "e2e-provider", str(updated.get("name")))
        check("密钥仍然存在", updated.get("has_api_key") is True)

        print("\n== 9. 保存不校验，地址直接更新 ==")
        status, body = api(
            "PATCH", f"/admin/providers/{pid}",
            {"base_url": f"http://127.0.0.1:{DEAD_PORT}"}, token=token,
        )
        check("不可达地址也返回 200", status == 200, f"status={status} body={body}")
        status, one = api("GET", f"/admin/providers/{pid}", token=token)
        check("地址确实被改掉了", one.get("base_url") == f"http://127.0.0.1:{DEAD_PORT}",
              str(one.get("base_url")))
        status, _ = api("PATCH", f"/admin/providers/{pid}",
                        {"base_url": f"http://127.0.0.1:{UPSTREAM_PORT}"}, token=token)
        check("地址改回可达的上游", status == 200, f"status={status}")

        print("\n== 10. 仍被引用的模型不能移除 ==")
        status, req = api("POST", "/admin/requests", {
            "name": "uses-model-c", "provider_id": pid, "model": "fake-model-c",
            "user_prompt": "hi",
        }, token=token)
        if not check("创建引用模型的 AI 请求", status == 201, f"status={status} body={req}"):
            return 1
        status, body = api("PATCH", f"/admin/providers/{pid}",
                           {"models": ["fake-model-b"]}, token=token)
        check("移除被引用模型返回 409", status == 409, f"status={status} body={body}")
        err = (body or {}).get("error") or {}
        check("错误码为 model_in_use", err.get("code") == "model_in_use", str(err.get("code")))
        details = err.get("details") or {}
        check("错误里点名了请求", "uses-model-c" in (details.get("requests") or []),
              json.dumps(details)[:200])
        status, one = api("GET", f"/admin/providers/{pid}", token=token)
        check("被拒绝的修改没有落库", one.get("models") == ["fake-model-b", "fake-model-c"],
              str(one.get("models")))
        status, _ = api("DELETE", f"/admin/requests/{req['id']}", token=token)
        check("清理掉请求", status == 200, f"status={status}")
        status, body = api("PATCH", f"/admin/providers/{pid}",
                           {"models": ["fake-model-b"]}, token=token)
        check("引用解除后可以移除", status == 200, f"status={status} body={body}")

        print("\n== 11. 删除 ==")
        status, body = api("DELETE", f"/admin/providers/{pid}", token=token)
        check("删除返回 200", status == 200, f"status={status} body={body}")
        status, listing = api("GET", "/admin/providers", token=token)
        check("列表已清空", len(listing.get("providers") or []) == 0, json.dumps(listing)[:200])
        status, body = api("DELETE", f"/admin/providers/{pid}", token=token)
        check("删除不存在的返回 404", status == 404, f"status={status}")

        failures = globals().get("FAILURES", 0)
        if failures:
            print(f"\n{e2e_failures_summary(failures)}")
            return 1
        print("\n全部通过 ✅")
        return 0
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        srv.shutdown()


def e2e_failures_summary(failures):
    return f"有 {failures} 项断言失败 ❌"


if __name__ == "__main__":
    sys.exit(main())
