#!/usr/bin/env python3
"""End-to-end check of the (flat) AI request manager.

Starts a fake OpenAI-compatible SSE upstream, boots the ai-monitor binary
against a throwaway database, then drives the whole flow through the HTTP
API:

    login -> register providers -> create requests pointing at different
    providers -> run a mixed batch -> poll results

The defining assertion of this model is that ONE flat list and ONE batch
can span several providers, and each probe carries the coordinates of the
provider its own request points at.

Also verifies the invariants that still matter:

  * a manual run is recorded in the history (source = manual), with
    provider_id / ai_request_id provenance;
  * a manual run leaves NO trace on the monitoring signal (no Prometheus
    samples, no task_id in /metrics);
  * a provider that still has requests attached cannot be deleted (409);
  * a scheduled monitoring task never sends a system prompt, even when an
    old client still supplies one.

Usage: python3 scripts/e2e_request_manager.py [path-to-binary]
"""
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

UPSTREAM_PORT = 18099
ADMIN_PORT = 18098
PASSWORD = "LocalAdmin2026!Q"
FAKE_KEY = "sk-fake-e2e"

# (model, user prompt) pairs the fake upstream actually received.
RECEIVED = []
RECEIVED_LOCK = threading.Lock()


class FakeUpstream(BaseHTTPRequestHandler):
    """A minimal OpenAI-compatible streaming chat endpoint."""

    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}
        user = ""
        for msg in req.get("messages", []):
            if msg.get("role") == "user":
                user = str(msg.get("content", ""))
        with RECEIVED_LOCK:
            RECEIVED.append((str(req.get("model", "")), user))

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("X-Req-Id", "fake-upstream-req-1")
        # No Content-Length: the body is delimited by the connection close,
        # which is what a real SSE endpoint does.
        self.send_header("Connection", "close")
        self.end_headers()
        # The connectivity checker closes the stream as soon as it sees the
        # headers, so writes may fail with a broken pipe. That is expected.
        try:
            for piece in ("echo:", user):
                payload = {"choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": ""}]}
                self.wfile.write(f"data: {json.dumps(payload)}\n\n".encode())
                self.wfile.flush()
                time.sleep(0.02)
            final = {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
            self.wfile.write(f"data: {json.dumps(final)}\n\n".encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass
        self.close_connection = True

    def log_message(self, *args):
        pass


def api(method, path, body=None, token=None, port=ADMIN_PORT, raw=False):
    url = f"http://127.0.0.1:{port}{path}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            payload = resp.read()
            return resp.status, (payload if raw else json.loads(payload or b"null"))
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


def wait_batch(batch_id, token, timeout=30):
    """Poll a batch until it is done; return the last snapshot."""
    deadline = time.time() + timeout
    final = None
    while time.time() < deadline:
        status, final = api("GET", f"/admin/request-batches/{batch_id}", token=token)
        if status == 200 and final.get("state") == "done":
            return final
        time.sleep(0.3)
    return final


def provider_payload(name, model):
    return {
        "name": name,
        "description": "端到端验证用",
        "protocol": "openai",
        "base_url": f"http://127.0.0.1:{UPSTREAM_PORT}",
        "api_key": FAKE_KEY,
        "models": [model],
        "timeout": "10s",
        "openai": {"stream_path": "/v1/chat/completions"},
    }


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else "./ai-monitor"
    binary = os.path.abspath(binary)
    if not os.path.exists(binary):
        print(f"binary not found: {binary}")
        return 2

    workdir = tempfile.mkdtemp(prefix="aim-e2e-")
    srv = HTTPServer(("127.0.0.1", UPSTREAM_PORT), FakeUpstream)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    cfg_path = os.path.join(workdir, "config.yaml")
    with open(cfg_path, "w") as f:
        f.write(
            f"sqlite_path: {workdir}/e2e.db\n"
            f'http_addr: "127.0.0.1:{ADMIN_PORT}"\n'
            "max_concurrent: 4\n"
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

        print("\n== 2. 注册两个 AI 服务提供商 ==")
        provider_ids = {}
        for name, model in (("alpha", "model-alpha"), ("beta", "model-beta")):
            status, p = api("POST", "/admin/providers", provider_payload(name, model), token=token)
            if not check(f"创建服务商 {name}", status == 201, f"status={status} body={str(p)[:180]}"):
                return 1
            provider_ids[name] = p["id"]
            check(f"{name} 带上候选模型", p.get("models") == [model], str(p.get("models")))
            check(f"{name} 保存不触发校验", p.get("last_verified_ok") is False, str(p.get("last_verified_ok")))
            check(f"{name} 响应不含明文密钥", FAKE_KEY not in json.dumps(p))
            check(f"{name} has_api_key 为真", p.get("has_api_key") is True)

        print("\n== 3. 平铺创建三条 AI 请求，分别指向不同服务商 ==")
        specs = [
            ("first", "hello-one", "alpha", "model-alpha"),
            ("second", "hello-two", "beta", "model-beta"),
            ("third", "hello-three", "alpha", "model-alpha"),
        ]
        request_ids = []
        for name, prompt, pname, model in specs:
            status, r = api(
                "POST",
                "/admin/requests",
                {"name": name, "provider_id": provider_ids[pname], "model": model,
                 "user_prompt": prompt},
                token=token,
            )
            if not check(f"创建请求 {name}", status == 201, f"status={status} body={str(r)[:180]}"):
                return 1
            request_ids.append(r["id"])
            check(f"{name} 内嵌服务商 {pname}", (r.get("provider") or {}).get("name") == pname,
                  str((r.get("provider") or {}).get("name")))
            check(f"{name} 选中了 {model}", r.get("model") == model, str(r.get("model")))

        print("\n== 4. 平铺列表（没有请求组层） ==")
        status, listing = api("GET", "/admin/requests", token=token)
        requests = (listing or {}).get("requests") or []
        check("列表返回 200", status == 200, f"status={status}")
        check("一次返回全部 3 条", len(requests) == 3, str(len(requests)))
        check("列表里没有 groups 包装", "groups" not in (listing or {}), str(list((listing or {}).keys())))
        used = {(r.get("provider") or {}).get("name") for r in requests}
        check("同一列表覆盖两个服务商", used == {"alpha", "beta"}, str(used))

        print("\n== 5. 指向不存在服务商的请求被拒绝 ==")
        status, err = api(
            "POST", "/admin/requests",
            {"name": "orphan", "provider_id": "prv-gone", "model": "model-alpha",
             "user_prompt": "x"}, token=token,
        )
        check("未知服务商返回 400", status == 400, f"status={status}")
        check("错误码为 unknown_provider", (err or {}).get("error", {}).get("code") == "unknown_provider",
              str((err or {}).get("error", {}).get("code")))

        print("\n== 5b. 不在候选列表里的模型被拒绝 ==")
        status, err = api(
            "POST", "/admin/requests",
            {"name": "bad-model", "provider_id": provider_ids["alpha"],
             "model": "model-beta", "user_prompt": "x"}, token=token,
        )
        check("未知模型返回 400", status == 400, f"status={status}")
        check("错误码为 unknown_model", (err or {}).get("error", {}).get("code") == "unknown_model",
              str((err or {}).get("error", {}).get("code")))

        print("\n== 6. 跨服务商批量执行 ==")
        status, batch = api("POST", "/admin/requests/run", {"request_ids": request_ids}, token=token)
        if not check("批量执行返回 202", status == 202, f"status={status} body={batch}"):
            return 1
        batch_id = batch["batch_id"]
        check("批次总数为 3", batch.get("total") == 3, str(batch.get("total")))

        print("\n== 7. 轮询批次结果 ==")
        final = wait_batch(batch_id, token)
        if not check("批次完成", final and final.get("state") == "done", json.dumps(final)[:400]):
            return 1
        check("三条都成功", final.get("succeeded") == 3, f"succeeded={final.get('succeeded')}")
        check("没有失败", final.get("failed") == 0, f"failed={final.get('failed')}")
        results = final.get("results") or []
        check("结果条数为 3", len(results) == 3, str(len(results)))
        result_providers = {r.get("provider_name") for r in results}
        check("结果覆盖两个服务商", result_providers == {"alpha", "beta"}, str(result_providers))
        for r in results:
            label = r.get("request_name")
            check(f"请求 {label} 捕获到输出", "echo:" in (r.get("output") or ""), repr(r.get("output"))[:120])
            check(f"请求 {label} 记录了耗时", r.get("duration_ms", 0) > 0)
            check(f"请求 {label} 带 X-Req-Id", bool(r.get("x_request_id")))
        # The fake upstream saw the model of each provider, not one shared one.
        with RECEIVED_LOCK:
            seen_models = {m for m, _ in RECEIVED}
        check("上游确实收到两个模型的请求", {"model-alpha", "model-beta"} <= seen_models, str(seen_models))

        print("\n== 8. 手动执行不污染监控信号 ==")
        status, metrics = api("GET", "/metrics", raw=True)
        text = metrics.decode() if isinstance(metrics, bytes) else str(metrics)
        check("/metrics 不含手动任务的 task_id", "req-" not in text)
        check("/metrics 不含服务商标签", "e2e-group" not in text and "alpha" not in text)

        print("\n== 9. 手动执行落进历史（带服务商溯源） ==")
        conn = sqlite3.connect(os.path.join(workdir, "e2e.db"))
        rows = conn.execute(
            "SELECT task_id, source, batch_id, provider_id, ai_request_id, status, output_content "
            "FROM probe_runs ORDER BY id"
        ).fetchall()
        conn.close()
        manual = [r for r in rows if r[1] == "manual"]
        check("每条请求各落一条 manual 记录", len(manual) == 3, f"total={len(rows)} manual={len(manual)}")
        if manual:
            row = manual[0]
            check("记录了 batch_id", row[2] == batch_id, str(row[2]))
            check("记录了 provider_id", row[3] in provider_ids.values(), str(row[3]))
            check("记录了 ai_request_id", row[4] in request_ids, str(row[4]))
            check("记录了输出内容", "echo:" in (row[6] or ""), repr(row[6])[:120])
        history_providers = {r[3] for r in manual}
        check("历史覆盖两个服务商", len(history_providers) == 2, str(history_providers))

        print("\n== 10. 服务商被引用时不允许删除 ==")
        status, err = api("DELETE", f"/admin/providers/{provider_ids['alpha']}", token=token)
        check("删除被引用服务商返回 409", status == 409, f"status={status}")
        check("错误码为 provider_in_use", (err or {}).get("error", {}).get("code") == "provider_in_use",
              str((err or {}).get("error", {}).get("code")))
        details = ((err or {}).get("error", {}).get("details") or {}).get("requests") or []
        check("错误里列出受影响的请求", len(details) == 2, str(details))

        print("\n== 11. 执行时自定义请求内容 ==")
        custom_word = "CUSTOM-E2E-CONTENT"
        status, batch3 = api(
            "POST",
            "/admin/requests/run",
            {
                "request_ids": [request_ids[0]],
                "overrides": {
                    request_ids[0]: {
                        "system_prompt": "CUSTOM-E2E-SYSTEM",
                        "user_prompt": "say ${word}",
                        "prompt_vars": {"word": custom_word},
                    }
                },
            },
            token=token,
        )
        if not check("带自定义内容执行返回 202", status == 202, f"status={status} body={batch3}"):
            return 1
        final3 = wait_batch(batch3["batch_id"], token)
        check("自定义批次完成", bool(final3 and final3.get("state") == "done"))
        out3 = ((final3 or {}).get("results") or [{}])[0].get("output") or ""
        check("自定义内容确实发给了上游", custom_word in out3, repr(out3)[:160])

        status, saved = api("GET", f"/admin/requests/{request_ids[0]}", token=token)
        check("预定义请求没有被改写", saved.get("user_prompt") == "hello-one", str(saved.get("user_prompt")))
        check("预定义变量没有被写入", not saved.get("prompt_vars"), str(saved.get("prompt_vars")))

        status, batch4 = api(
            "POST", "/admin/requests/run", {"request_ids": [request_ids[0]]}, token=token
        )
        out4 = ((wait_batch(batch4["batch_id"], token) or {}).get("results") or [{}])[0].get("output") or ""
        check("下一次执行回到预定义内容", "hello-one" in out4 and custom_word not in out4, repr(out4)[:160])

        status, _ = api(
            "POST", "/admin/requests/run",
            {"request_ids": [request_ids[0]], "overrides": {request_ids[0]: {"user_prompt": "   "}}},
            token=token,
        )
        check("空白自定义内容返回 400", status == 400, f"status={status}")

        status, _ = api("POST", "/admin/requests/run", {"request_ids": request_ids}, token=token)
        check("执行多条仍返回 202", status == 202, f"status={status}")

        print("\n== 12. PATCH 可以把请求改指向另一个服务商 ==")
        status, patched = api(
            "PATCH", f"/admin/requests/{request_ids[0]}",
            {"provider_id": provider_ids["beta"], "name": "renamed",
             "model": "model-beta"}, token=token,
        )
        check("改指向返回 200", status == 200, f"status={status}")
        check("provider_id 已更新", patched.get("provider_id") == provider_ids["beta"], str(patched.get("provider_id")))
        check("提示词保持不变", patched.get("user_prompt") == "hello-one", str(patched.get("user_prompt")))
        check("内嵌服务商同步刷新", (patched.get("provider") or {}).get("name") == "beta",
              str((patched.get("provider") or {}).get("name")))

        print("\n== 13. 自定义内容落进运行历史 ==")
        conn = sqlite3.connect(os.path.join(workdir, "e2e.db"))
        inputs = [
            r[0] for r in conn.execute("SELECT input_content FROM probe_runs WHERE source='manual'").fetchall()
        ]
        conn.close()
        check("历史里能查到自定义输入", any(custom_word in (v or "") for v in inputs), f"rows={len(inputs)}")
        check("历史里仍能查到预定义输入", any("hello-one" in (v or "") for v in inputs))

        print("\n== 14. 旧路由已下线，未登录执行被拒 ==")
        status, _ = api("GET", "/admin/request-groups", token=token)
        check("/admin/request-groups 返回 404", status == 404, f"status={status}")
        status, _ = api("POST", "/admin/requests/run", {"request_ids": request_ids})
        check("未登录执行返回 401", status == 401, f"status={status}")

        print("\n== 15. 监控任务不使用系统提示词 ==")
        # A monitoring task names the provider it probes; the upstream
        # coordinates live on the provider, not on the task.
        status, mtask = api(
            "POST", "/admin/tasks",
            {
                "name": "e2e-monitor",
                "provider_id": provider_ids["alpha"],
                "model": "model-alpha",
                "user_prompt": "monitor-ping",
                "system_prompt": "LEGACY-SYSTEM",
                "schedule": "*/1 * * * * *",
                "notify": {"enabled": False},
            },
            token=token,
        )
        if not check("可以创建监控任务", status == 201, f"status={status} body={mtask}"):
            return 1
        check("创建响应不回显 system_prompt", "system_prompt" not in mtask)
        check("任务引用了服务商", mtask.get("provider_id") == provider_ids["alpha"], str(mtask.get("provider_id")))
        check("任务选定了模型", mtask.get("model") == "model-alpha", str(mtask.get("model")))
        check(
            "任务内嵌了服务商",
            (mtask.get("provider") or {}).get("name") == "alpha",
            str((mtask.get("provider") or {}).get("name")),
        )

        status, mview = api("GET", "/admin/tasks/e2e-monitor", token=token)
        check("详情响应不回显 system_prompt", status == 200 and "system_prompt" not in mview, f"status={status}")
        conn = sqlite3.connect(os.path.join(workdir, "e2e.db"))
        stored_system = conn.execute("SELECT system_prompt FROM tasks WHERE id='e2e-monitor'").fetchone()
        conn.close()
        check("数据库里不保留系统提示词", bool(stored_system) and not (stored_system[0] or ""), str(stored_system))

        status, _ = api("POST", "/admin/tasks/e2e-monitor/start", {}, token=token)
        check("启动监控任务", status == 200, f"status={status}")
        scheduled = []
        deadline = time.time() + 15
        while time.time() < deadline:
            status, runs = api("GET", "/admin/tasks/e2e-monitor/runs?limit=5", token=token)
            scheduled = (runs or {}).get("runs", []) if status == 200 else []
            if scheduled:
                break
            time.sleep(0.3)
        api("POST", "/admin/tasks/e2e-monitor/stop", {}, token=token)
        check("定时探测已产生历史", bool(scheduled), f"rows={len(scheduled)}")
        if scheduled:
            inp = scheduled[0].get("input_content") or ""
            check("定时探测输入没有 system 块", "[system]" not in inp, repr(inp)[:120])
            check("定时探测仍发送用户提示词", "monitor-ping" in inp, repr(inp)[:120])
            check("定时探测不会带上旧系统提示词", "LEGACY-SYSTEM" not in inp, repr(inp)[:120])

        failures = globals().get("FAILURES", 0)
        print(f"\n{'=' * 52}")
        print(f"结果：{'全部通过' if failures == 0 else f'{failures} 项失败'}")
        print("=" * 52)
        return 0 if failures == 0 else 1
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        srv.shutdown()
        shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
