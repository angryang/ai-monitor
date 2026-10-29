#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
parse-ai-io.py — 从 ai-monitor.db 解析"AI 请求/响应"历史。

数据来源：
  - tasks          任务配置（system_prompt / user_prompt / prompt_vars 等模板侧"输入"）
  - probe_runs     每次探测的实际"输入"(request_body) 与"输出"(response_head/response_tail)

用法：
  # 1) 打印所有 run 的 JSONL 到 stdout（每行一个 run）
  ./scripts/parse-ai-io.py

  # 2) 写入文件
  ./scripts/parse-ai-io.py -o outputs/ai-history.jsonl

  # 3) 按任务过滤
  ./scripts/parse-ai-io.py --task-id minimax

  # 4) 只看失败的
  ./scripts/parse-ai-io.py --status failure

  # 5) 按时间窗（默认 SQLite 默认时区 UTC；started_at 是 ISO8601）
  ./scripts/parse-ai-io.py --since 2026-10-01 --until 2026-10-18

  # 6) 自定义 DB 路径
  ./scripts/parse-ai-io.py --db /tmp/ai-monitor.db

设计要点（与 internal/probe/store.go §18.13 对齐）：
  - "输入" = 模板侧（tasks.*） + 实际下发侧（probe_runs.request_body）
  - "输出" = response_head + response_tail（受截断标志影响，外部应自行拼接/检查截断）
  - 不输出任何凭据（API key / webhook 都加密落盘，本脚本永不读）
"""
from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

DEFAULT_DB = Path(__file__).resolve().parent.parent / "ai-monitor.db"


@dataclass
class Run:
    id: int
    task_id: str
    task_name: str
    protocol: str
    model: str
    status: str
    error: str | None
    started_at: str
    ended_at: str
    duration_ms: int
    ttft_ms: int
    itl_avg_ms: int
    itl_count: int
    chunks: int
    request_id: str | None
    request_body: str | None
    response_head: str | None
    response_tail: str | None
    request_truncated: bool
    response_truncated: bool

    # 任务配置侧"输入"（LEFT JOIN 后可能为 None）
    system_prompt: str | None = None
    user_prompt: str | None = None
    prompt_vars: str | None = None

    def to_record(self) -> dict[str, Any]:
        """把 Run 拍平成"输入/输出"两段，方便下游消费。"""
        request_body_obj = _safe_json(self.request_body)
        return {
            "id": self.id,
            "task": {
                "id": self.task_id,
                "name": self.task_name,
                "protocol": self.protocol,
                "model": self.model,
                "system_prompt": self.system_prompt,
                "user_prompt": self.user_prompt,
                "prompt_vars": self.prompt_vars,
            },
            "input": {
                "request_body_raw": self.request_body,
                "request_body_json": request_body_obj,
                "request_truncated": self.request_truncated,
            },
            "output": {
                "response_head": self.response_head,
                "response_tail": self.response_tail,
                "response_truncated": self.response_truncated,
            },
            "context": {
                "status": self.status,
                "error": self.error,
                "started_at": self.started_at,
                "ended_at": self.ended_at,
                "duration_ms": self.duration_ms,
                "ttft_ms": self.ttft_ms,
                "itl_avg_ms": self.itl_avg_ms,
                "itl_count": self.itl_count,
                "chunks": self.chunks,
                "request_id": self.request_id,
            },
        }


def _safe_json(s: str | None) -> Any:
    """request_body 是 JSON 字符串；尽力解析，解析失败就回退为原字符串。"""
    if not s:
        return None
    try:
        return json.loads(s)
    except (ValueError, TypeError):
        return s


def _build_query(args: argparse.Namespace) -> tuple[str, list[Any]]:
    """根据参数拼 SQL + params。注意 prompt_vars 是 TEXT，user 可手输 JSON 字符串。"""
    where: list[str] = []
    params: list[Any] = []

    if args.task_id:
        where.append("r.task_id = ?")
        params.append(args.task_id)
    if args.task_name:
        where.append("r.task_name = ?")
        params.append(args.task_name)
    if args.status:
        where.append("r.status = ?")
        params.append(args.status)
    if args.protocol:
        where.append("r.protocol = ?")
        params.append(args.protocol)
    if args.since:
        where.append("r.started_at >= ?")
        params.append(args.since)
    if args.until:
        where.append("r.started_at < ?")
        params.append(args.until)
    if args.request_id:
        where.append("r.request_id = ?")
        params.append(args.request_id)

    sql = """
        SELECT
            r.id, r.task_id, r.task_name, r.protocol, r.model,
            r.status, r.err,
            r.started_at, r.ended_at, r.duration_ms,
            r.ttftms, r.itl_avg_ms, r.itl_count, r.chunks,
            r.request_id, r.request_body, r.response_head, r.response_tail,
            r.request_truncated, r.response_truncated,
            t.system_prompt, t.user_prompt, t.prompt_vars
        FROM probe_runs AS r
        LEFT JOIN tasks AS t ON t.id = r.task_id
        {where_clause}
        ORDER BY r.started_at DESC, r.id DESC
        LIMIT ?
    """.format(where_clause=("WHERE " + " AND ".join(where)) if where else "")
    params.append(args.limit)
    return sql, params


def _iter_runs(conn: sqlite3.Connection, sql: str, params: list[Any]) -> Iterator[Run]:
    conn.row_factory = sqlite3.Row
    cur = conn.execute(sql, params)
    for row in cur:
        yield Run(
            id=row["id"],
            task_id=row["task_id"],
            task_name=row["task_name"],
            protocol=row["protocol"],
            model=row["model"],
            status=row["status"],
            error=row["err"],
            started_at=row["started_at"],
            ended_at=row["ended_at"],
            duration_ms=row["duration_ms"],
            ttft_ms=row["ttftms"] or 0,
            itl_avg_ms=row["itl_avg_ms"] or 0,
            itl_count=row["itl_count"] or 0,
            chunks=row["chunks"],
            request_id=row["request_id"],
            request_body=row["request_body"],
            response_head=row["response_head"],
            response_tail=row["response_tail"],
            request_truncated=bool(row["request_truncated"]),
            response_truncated=bool(row["response_truncated"]),
            system_prompt=row["system_prompt"],
            user_prompt=row["user_prompt"],
            prompt_vars=row["prompt_vars"],
        )


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[1] if __doc__ else "")
    p.add_argument("--db", default=str(DEFAULT_DB), help=f"SQLite path (default: {DEFAULT_DB})")
    p.add_argument("-o", "--out", help="输出文件；不传则写 stdout")
    p.add_argument("--task-id", help="按 task.id 过滤")
    p.add_argument("--task-name", help="按 task.name 过滤")
    p.add_argument("--status", choices=["success", "failure", "unauthorized"], help="按 status 过滤")
    p.add_argument("--protocol", choices=["openai", "anthropic", "gemini"], help="按协议过滤")
    p.add_argument("--since", help="起始时间（含），ISO8601，如 2026-10-01 或 2026-10-01T08:00:00")
    p.add_argument("--until", help="结束时间（不含），ISO8601")
    p.add_argument("--request-id", help="按上游 request_id 过滤")
    p.add_argument("--limit", type=int, default=1000, help="最多返回多少条 run（默认 1000）")
    args = p.parse_args(argv)

    if not os.path.exists(args.db):
        print(f"[error] DB not found: {args.db}", file=sys.stderr)
        return 2

    with sqlite3.connect(args.db) as conn:
        sql, params = _build_query(args)
        runs = list(_iter_runs(conn, sql, params))

    out_fp = open(args.out, "w", encoding="utf-8") if args.out else sys.stdout
    try:
        for run in runs:
            out_fp.write(json.dumps(run.to_record(), ensure_ascii=False) + "\n")
    finally:
        if out_fp is not sys.stdout:
            out_fp.close()

    print(f"[ok] wrote {len(runs)} run(s)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())