#!/usr/bin/env python3
"""End-to-end check of the "monitoring task probes a provider" model.

A monitoring task no longer carries the upstream coordinates. It names the
AI service provider it probes (provider_id) and owns only the prompt, the
schedule, the canary group and the notification switch.

This script starts a fake OpenAI-compatible SSE upstream, boots the
ai-monitor binary against a throwaway database, then drives the whole flow
through the HTTP API:

    login -> create provider -> create task -> probe runs -> guard rails

and asserts the contract of the feature:

  - a task is created with a provider_id, and the response nests the
    provider (protocol / url / candidate models) instead of repeating it;
  - a task that names an unknown provider is rejected;
  - a provider that is still used by a task cannot be deleted;
  - the scheduled probe actually reaches the provider's upstream — the run
    history proves the binding happened at probe time.

Usage: python3 scripts/e2e_task_providers.py [path-to-binary]
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

UPSTREAM_PORT = 18094
ADMIN_PORT = 18093
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
        "name": "e2e-upstream",
        "description": "端到端验证用",
        "protocol": "openai",
        "base_url": f"http://127.0.0.1:{UPSTREAM_PORT}",
        "api_key": FAKE_KEY,
        "models": ["fake-model", "fake-model-2"],
        "timeout": "10s",
        "openai": {"stream_path": "/v1/chat/completions"},
    }
    body.update(overrides)
    return body


def task_body(provider_id, **overrides):
    body = {
        "name": "e2e-task",
        "provider_id": provider_id,
        "model": "fake-model",
        "user_prompt": "ping",
        "schedule": "@every 2s",
        "canary_group": "default",
        "notify": {"enabled": False, "channel": "wecom"},
    }
    body.update(overrides)
    return body


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else "./ai-monitor"
    binary = os.path.abspath(binary)
    if not os.path.exists(binary):
        print(f"binary not found: {binary}")
        return 2

    workdir = tempfile.mkdtemp(prefix="aim-task-prov-e2e-")
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

        print("\n== 2. 注册服务商 ==")
        status, prov = api("POST", "/admin/providers", provider_body(), token=token)
        if not check("创建成功返回 201", status == 201, f"status={status} body={prov}"):
            return 1
        pid = prov["id"]

        print("\n== 3. 任务引用服务商 ==")
        status, task = api("POST", "/admin/tasks", task_body(pid), token=token)
        if not check("创建成功返回 201", status == 201, f"status={status} body={task}"):
            return 1
        tid = task["id"]
        check("任务带上了 provider_id", task.get("provider_id") == pid, str(task.get("provider_id")))
        nested = task.get("provider") or {}
        check("响应里嵌套了服务商", nested.get("id") == pid, json.dumps(nested)[:200])
        check("嵌套的服务商带上了协议", nested.get("protocol") == "openai", str(nested.get("protocol")))
        check("嵌套的服务商带上了候选模型", nested.get("models") == ["fake-model", "fake-model-2"],
              str(nested.get("models")))
        check(
            "嵌套的服务商带上了地址",
            nested.get("base_url") == f"http://127.0.0.1:{UPSTREAM_PORT}",
            str(nested.get("base_url")),
        )
        # The upstream coordinates must not be duplicated on the task.
        check("任务不再自带 base_url", "base_url" not in task, json.dumps(task)[:200])
        check("任务不再自带 api_key", "api_key" not in task, json.dumps(task)[:200])
        check("任务不再自带 protocol", "protocol" not in task, json.dumps(task)[:200])
        check("任务自带选中的模型", task.get("model") == "fake-model", str(task.get("model")))

        print("\n== 4. 未知服务商被拒绝 ==")
        status, body = api("POST", "/admin/tasks", task_body("prv-does-not-exist", name="ghost"), token=token)
        check("返回 400", status == 400, f"status={status} body={body}")
        err = ((body or {}).get("error") or {})
        check("错误码为 unknown_provider", err.get("code") == "unknown_provider", str(err.get("code")))

        print("\n== 5. 仍被任务引用的服务商不能删除 ==")
        status, body = api("DELETE", f"/admin/providers/{pid}", token=token)
        check("删除返回 409", status == 409, f"status={status} body={body}")
        err = ((body or {}).get("error") or {})
        check("错误码为 provider_in_use", err.get("code") == "provider_in_use", str(err.get("code")))
        details = err.get("details") or {}
        check("错误里点名了占用它的任务", "e2e-task" in (details.get("tasks") or []), json.dumps(details)[:200])

        print("\n== 6. 调度探测真的走了服务商的上游 ==")
        status, started = api("POST", f"/admin/tasks/{tid}/start", {}, token=token)
        if not check("启动任务返回 200", status == 200, f"status={status} body={started}"):
            return 1
        check("任务已启用", started.get("enabled") is True, str(started.get("enabled")))

        runs = []
        deadline = time.time() + 20
        while time.time() < deadline:
            status, payload = api("GET", f"/admin/tasks/{tid}/runs?limit=10", token=token)
            runs = (payload or {}).get("runs") or []
            if runs:
                break
            time.sleep(0.5)
        if not check("产生了执行记录", len(runs) > 0, str(len(runs))):
            return 1
        run = runs[0]
        check("执行成功", run.get("status") == "success", json.dumps(run)[:300])
        check("执行记录带上了服务商的协议", run.get("protocol") == "openai", str(run.get("protocol")))
        check("执行记录带上了任务选中的模型", run.get("model") == "fake-model", str(run.get("model")))
        # The X-Req-Id is generated by the client and sent upstream, so the
        # run history carries the outbound trace id (not the upstream's own).
        rid = run.get("request_id") or ""
        check("执行记录带上了请求追踪 ID", len(rid) == 36 and rid.count("-") == 4, rid)
        check("来源是定时调度", run.get("source") == "schedule", str(run.get("source")))

        print("\n== 7. 停用任务 ==")
        status, stopped = api("POST", f"/admin/tasks/{tid}/stop", {}, token=token)
        check("停止返回 200", status == 200, f"status={status} body={stopped}")
        check("任务已停用", stopped.get("enabled") is False, str(stopped.get("enabled")))

        print("\n== 8. 删除任务后可以删除服务商 ==")
        status, body = api("DELETE", f"/admin/tasks/{tid}", token=token)
        check("删除任务返回 200", status == 200, f"status={status} body={body}")
        status, body = api("DELETE", f"/admin/providers/{pid}", token=token)
        check("删除服务商返回 200", status == 200, f"status={status} body={body}")

        failures = globals().get("FAILURES", 0)
        if failures:
            print(f"\n有 {failures} 项断言失败 ❌")
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


if __name__ == "__main__":
    sys.exit(main())
