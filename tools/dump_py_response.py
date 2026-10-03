#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 Python 基线（上游 zcode2api）在指定场景下的响应**完整**打印出来。

为什么需要它：`tools/behavior_diff.py` 的 `show()` 只显示响应前 600B / 出站体前 400B，
而 `docs/contract/outbound/behavior.md` §二 要的是**逐字节**形状（键序、帧数、
`data: [DONE]` 有没有、SSE 帧之间怎么分隔）。写 A4 的 chat 转换时就是靠这个工具拿到
了完整的四帧序列。

用法（在仓库根）：

    python tools/dump_py_response.py                       # 跑默认几个 chat 场景
    python tools/dump_py_response.py ok-anthropic ok-stream
    python tools/dump_py_response.py --py-dir D:/AiWork/ZCode/zcode2api <场景名>…

场景名取自 `behavior_diff.py` 的 SCENARIOS（`python tools/behavior_diff.py --list`）。
输出到 stdout：每个场景的响应头、`repr(raw_text)`（含 `\\n` 转义，便于数帧）、
出站体 repr、以及路由记号。（`--py-dir` 缺省与 behavior_diff.py 一致。）
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import behavior_diff as H  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--py-dir", default=r"D:/AiWork/ZCode/zcode2api",
                    help="上游 zcode2api 目录（含 cli.py 与 .venv）")
    ap.add_argument("--timeout", type=int, default=60, help="单个入站请求的等待秒数")
    ap.add_argument("names", nargs="*",
                    help="场景名（缺省：A4 的 5 个 chat 场景）")
    a = ap.parse_args()

    names = a.names or ["ok-openai", "ok-openai-stream", "chat-sse-on-nonstream",
                        "chat-stream-from-json", "stream-string-false"]
    work = os.path.join(os.environ.get("TEMP", "/tmp"), "zcode_dump_py")

    for n in names:
        if n not in H.SCENARIOS:
            print("!! 未知场景 %r（用 --list 看可用）" % n, file=sys.stderr)
            return 2
        sc = dict(H.SCENARIOS[n])
        sc["_name"] = n
        r = H.run_scenario("py", sc, work, a.py_dir, "", keep=False, timeout=a.timeout)
        print("=" * 72)
        print("### %s  —  %s" % (n, sc["note"]))
        if r.get("error"):
            print("ERROR:", r["error"][:800])
            continue
        for i, s in enumerate(r["steps"]):
            print("-- step %d: %s %s -> %s  ctype=%r  len=%d" % (
                i, s["req"]["method"], s["req"]["path"], s["status"], s["ctype"], s["raw_len"]))
            print("HEADERS:", json.dumps(s.get("headers") or {}, ensure_ascii=False))
            print("RAW_BODY_REPR:")
            print(repr(s["raw_text"]))
            for o in s["outbound"]:
                print("OUTBOUND_RAW:", repr(o["raw_body"]))
        print("MARKS:")
        for m in r.get("marks", []):
            print("   ", m)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
