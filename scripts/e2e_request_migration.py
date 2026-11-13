#!/usr/bin/env python3
"""End-to-end check of the request-group -> provider migration.

Copies an existing ai-monitor database, boots the current binary against
that copy, and verifies both migrations:

  * when the retired `request_groups` / `request_items` tables are present,
    every legacy request survives with its prompt and variables, points at
    a provider (reused rather than duplicated), the sealed api key still
    decrypts, the two legacy tables are gone;
  * when the database still has the old single-model shape, every provider
    gains a `models` list with its former model, and every task / request
    gains a `model` filled with that provider's primary model;
  * restarting the service a second time is a no-op.

The test never touches the real database — it works on a copy, and all
scheduled tasks are disabled in that copy so nothing upstream is probed.

Usage: python3 scripts/e2e_request_migration.py [path-to-binary] [path-to-db]
"""
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ADMIN_PORT = 18097
PASSWORD = "LocalAdmin2026!Q"


def api(method, path, body=None, token=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"http://127.0.0.1:{ADMIN_PORT}{path}", data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = resp.read()
            return resp.status, json.loads(payload or b"null")
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload or b"null")
        except Exception:
            return e.code, payload.decode(errors="replace")


def check(label, ok, detail=""):
    print(f"  [{'PASS' if ok else 'FAIL'}] {label}" + (f" — {detail}" if detail else ""))
    if not ok:
        globals()["FAILURES"] = globals().get("FAILURES", 0) + 1
    return ok


def wait_http(timeout=20):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            status, _ = api("GET", "/healthz")
            if status == 200:
                return True
        except Exception:
            pass
        time.sleep(0.2)
    return False


def table_names(db_path):
    conn = sqlite3.connect(db_path)
    names = {r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    conn.close()
    return names


def boot(binary, cfg_path, env, log_path):
    proc = subprocess.Popen(
        [binary, "-config", cfg_path],
        env=env,
        stdout=open(log_path, "w"),
        stderr=subprocess.STDOUT,
    )
    if not wait_http():
        print("server did not come up; log:")
        print(open(log_path).read()[-4000:])
        proc.terminate()
        return None
    return proc


def stop(proc):
    if proc is None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    binary = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else "./ai-monitor")
    src_db = os.path.abspath(sys.argv[2] if len(sys.argv) > 2 else os.path.join(root, "ai-monitor.db"))
    if not os.path.exists(binary):
        print(f"binary not found: {binary}")
        return 2
    if not os.path.exists(src_db):
        print(f"database not found: {src_db}")
        return 2

    # The service reads its master key / jwt secret from the environment.
    # Reuse the local dev secrets so the copied ciphertext still decrypts.
    secrets = {}
    secret_file = os.path.join(root, ".local-secrets")
    if os.path.exists(secret_file):
        for line in open(secret_file, encoding="utf-8"):
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                secrets[k.strip()] = v.strip().strip('"').strip("'")
    if "AI_MONITOR_JWT_SECRET" not in secrets:
        secrets["AI_MONITOR_JWT_SECRET"] = "0123456789abcdef0123456789abcdef"
    if "AI_MONITOR_MASTER_KEY" not in secrets:
        secrets["AI_MONITOR_MASTER_KEY"] = "0123456789abcdef0123456789abcdef"
    secrets.update({
        "AI_MONITOR_ADMIN_USERNAME": "admin",
        "AI_MONITOR_ADMIN_PASSWORD": PASSWORD,
        "WECOM_WEBHOOK": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test",
    })
    env = dict(os.environ)
    env.update(secrets)

    workdir = tempfile.mkdtemp(prefix="aim-migrate-")
    db_path = os.path.join(workdir, "migrate.db")
    # The live service keeps uncheckpointed pages in a -wal sidecar, so a
    # plain file copy would silently drop recently created tables. SQLite's
    # online backup gives us a consistent snapshot that includes WAL.
    src_conn = sqlite3.connect(f"file:{src_db}?mode=ro", uri=True)
    dst_conn = sqlite3.connect(db_path)
    src_conn.backup(dst_conn)
    dst_conn.close()
    src_conn.close()

    conn = sqlite3.connect(db_path)
    tables = table_names(db_path)
    legacy_before = {}
    if "request_groups" in tables:
        legacy_before = {
            "groups": conn.execute("SELECT count(*) FROM request_groups").fetchone()[0],
            "items": conn.execute("SELECT count(*) FROM request_items").fetchone()[0],
            "item_ids": [r[0] for r in conn.execute("SELECT id FROM request_items")],
            "prompts": [r[0] for r in conn.execute("SELECT user_prompt FROM request_items ORDER BY sort_order")],
            "provider_ids": [r[0] for r in conn.execute("SELECT id FROM ai_providers")],
            "provider_names": [r[0] for r in conn.execute("SELECT name FROM ai_providers")],
        }
    # Snapshot the old single-model shape: providers carry a lone `model`
    # column, tasks and requests carry only a provider_id.
    multi_before = {}
    if "ai_providers" in tables:
        pcols = {r[1] for r in conn.execute("PRAGMA table_info(ai_providers)")}
        if "models" not in pcols and "model" in pcols:
            multi_before["providers"] = dict(conn.execute("SELECT id, model FROM ai_providers").fetchall())
            for t in ("tasks", "ai_requests"):
                if t in tables:
                    tcols = {r[1] for r in conn.execute(f"PRAGMA table_info({t})")}
                    if "model" not in tcols:
                        multi_before[t] = conn.execute(f"SELECT id, provider_id FROM {t}").fetchall()
    # Never let the copy fire a scheduled probe at a real upstream.
    if "tasks" in tables:
        conn.execute("UPDATE tasks SET enabled = 0")
        conn.commit()
    conn.close()

    if not legacy_before and not multi_before:
        print("source database already has the current schema; nothing to migrate")
        return 0

    cfg_path = os.path.join(workdir, "config.yaml")
    with open(cfg_path, "w") as f:
        f.write(f"sqlite_path: {db_path}\nhttp_addr: \"127.0.0.1:{ADMIN_PORT}\"\nmax_concurrent: 1\n")

    print("\n== 1. 迁移前 ==")
    if legacy_before:
        print(f"  旧请求组 {legacy_before['groups']} 个 / 旧请求 {legacy_before['items']} 条")
        print(f"  已有服务商 {legacy_before['provider_names']}")
    else:
        print("  旧请求组已于早前的版本迁移完毕")
    print(f"  单模型服务商 {len(multi_before.get('providers', {}))} 个；"
          f"待回填任务 {len(multi_before.get('tasks', []))} 条 / "
          f"请求 {len(multi_before.get('ai_requests', []))} 条")

    proc = None
    try:
        log_path = os.path.join(workdir, "server.log")
        proc = boot(binary, cfg_path, env, log_path)
        if proc is None:
            return 2

        print("\n== 2. 迁移后 ==")
        names = table_names(db_path)
        status, login = api("POST", "/auth/login", {"username": "admin", "password": PASSWORD})
        if not check("登录成功", status == 200 and "token" in (login or {}), f"status={status}"):
            return 1
        token = login["token"]

        if legacy_before:
            check("request_groups 已删除", "request_groups" not in names,
                  str(sorted(n for n in names if n.startswith("request"))))
            check("request_items 已删除", "request_items" not in names)
            check("ai_requests 已创建", "ai_requests" in names)

            status, listing = api("GET", "/admin/requests", token=token)
            requests = (listing or {}).get("requests") or []
            check("请求列表返回 200", status == 200, f"status={status}")
            check(f"旧 {legacy_before['items']} 条请求全部保留", len(requests) == legacy_before["items"],
                  f"got={len(requests)}")
            check("id 保持不变", {r["id"] for r in requests} == set(legacy_before["item_ids"]))
            check("提示词保持不变", sorted(r["user_prompt"] for r in requests) == sorted(legacy_before["prompts"]),
                  str([r["user_prompt"] for r in requests]))
            check("每条请求都有 provider_id", all(r.get("provider_id") for r in requests))
            check("每条请求都内嵌了服务商", all((r.get("provider") or {}).get("name") for r in requests))

            status, providers = api("GET", "/admin/providers", token=token)
            plist = (providers or {}).get("providers") or []
            check("没有产生重复服务商", len(plist) == len(legacy_before["provider_ids"]),
                  f"before={len(legacy_before['provider_ids'])} after={len(plist)}")
            used = {r["provider_id"] for r in requests}
            check("请求指向已有的服务商", used <= set(legacy_before["provider_ids"]), str(used))

        if multi_before.get("providers"):
            conn = sqlite3.connect(db_path)
            pcols = [r[1] for r in conn.execute("PRAGMA table_info(ai_providers)")]
            check("ai_providers 新增 models 列", "models" in pcols, str(pcols))
            rows = conn.execute("SELECT id, models, model FROM ai_providers").fetchall()
            conn.close()
            for pid, models_json, legacy_model in rows:
                expected = multi_before["providers"].get(pid, legacy_model)
                try:
                    models = json.loads(models_json)
                except Exception:
                    models = None
                check(f"服务商 {pid[:8]} 的原模型进了候选列表", models == [expected],
                      f"models={models_json!r} want=[{expected}]")
                check(f"服务商 {pid[:8]} 的遗留列仍是主模型", legacy_model == expected,
                      f"model={legacy_model!r}")
            for t in ("tasks", "ai_requests"):
                expected_rows = multi_before.get(t)
                if not expected_rows:
                    continue
                conn = sqlite3.connect(db_path)
                tcols = [r[1] for r in conn.execute(f"PRAGMA table_info({t})")]
                check(f"{t} 新增 model 列", "model" in tcols, str(tcols))
                got = dict(conn.execute(f"SELECT id, model FROM {t}").fetchall())
                conn.close()
                for rid, provider_id in expected_rows:
                    expected = multi_before["providers"].get(provider_id)
                    check(f"{t} {rid[:12]} 回填了服务商主模型", got.get(rid) == expected,
                          f"model={got.get(rid)!r} want={expected!r}")
            status, providers = api("GET", "/admin/providers", token=token)
            plist = (providers or {}).get("providers") or []
            check("服务商响应带 models 数组", all(isinstance(p.get("models"), list) and p["models"]
                                              for p in plist), str([p.get("models") for p in plist]))
            check("服务商密钥仍可解密", all(not p.get("secret_error") for p in plist),
                  str([p.get("secret_error") for p in plist]))
            check("服务商密钥仍已配置", all(p.get("has_api_key") for p in plist))

        print("\n== 3. 重启后不重复迁移 ==")
        stop(proc)
        proc = boot(binary, cfg_path, env, log_path)
        if proc is None:
            return 2
        status, login = api("POST", "/auth/login", {"username": "admin", "password": PASSWORD})
        token = (login or {}).get("token", "")
        status, listing2 = api("GET", "/admin/requests", token=token)
        again = (listing2 or {}).get("requests") or []
        if legacy_before:
            check("再次启动请求数不变", len(again) == legacy_before["items"], f"got={len(again)}")
            check("再次启动 id 不变", {r["id"] for r in again} == set(legacy_before["item_ids"]))
        if multi_before.get("providers"):
            conn = sqlite3.connect(db_path)
            models = conn.execute("SELECT id, models FROM ai_providers ORDER BY id").fetchall()
            task_models = conn.execute("SELECT id, model FROM tasks ORDER BY id").fetchall()
            conn.close()
            check("再次启动候选模型不变", all(json.loads(m) == [multi_before["providers"][pid]]
                                       for pid, m in models), str(models))
            check("再次启动任务模型没有空值", all(m for _, m in task_models), str(task_models))

        failures = globals().get("FAILURES", 0)
        print(f"\n{'=' * 52}")
        print(f"结果：{'全部通过' if failures == 0 else f'{failures} 项失败'}")
        print("=" * 52)
        return 0 if failures == 0 else 1
    finally:
        stop(proc)
        shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
