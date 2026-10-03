#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""行为对照 harness：同一份「假上游规则」下，跑 Python 靶机与 Go 实现，逐项对比。

为什么需要它
------------
A4 的验收判据是「**同一请求序列下与 Python 行为逐项一致**」。真实上游给不了这个能力：
用假 token 只能拿到 401，429 / 5xx / 风控 / 验证码 / SSE 成功流全都采不到。
所以用一个本地假上游（`tools/mitmupstream --mode fake`）把上游响应**变成输入参数**，
再把两个实现的「可观察输出」对齐：

  1. **入站响应**：状态码 + 响应体（JSON 按语义比较，SSE 按逐帧文本比较）；
  2. **出站请求**：方法 + 路径 + 请求体（这是「转发是否保真」的判据）；
  3. **路由日志记号**：靶机自己打的 `>>>` / `[~]` / `<!>` 行（失败分类与切换顺序）；
  4. **入站响应头（白名单）**：`content-type` / `cache-control` / `x-custom-probe`
     —— 前三项之外的响应头（`date` / `server` / `transfer-encoding`）是服务器实现细节，不比较。

记号比较（`--no-compare-marks` 可关）：先把 `<reqid>`（16 位 hex）与 UUID 掩码成
`<reqid>` / `<uuid>`，再**丢掉 `install` 行**（安装序是并发后台任务，出现位置随时序浮动），
最后逐行比较剩下的序列 —— 这样才能真正校验「失败分类文案 + 切换顺序」。

用法
----
    # 只跑参考实现（Python 靶机），把行为打印出来（用于**发现**契约）
    python tools/behavior_diff.py --py-dir D:/AiWork/ZCode/zcode2api --scenario ok-anthropic

    # 两个实现对照（A4 验收）
    python tools/behavior_diff.py --py-dir D:/AiWork/ZCode/zcode2api \\
        --go-exe ./zcode2api-go.exe

    # 列出全部场景
    python tools/behavior_diff.py --list

设计约束
--------
- 只依赖标准库；MITM 用 `tools/mitmupstream`（`MITM_EXE` 可覆盖）。
- **不设网关 Key**：两个实现都留空 = 不校验，于是鉴权分支不干扰转发对比
  （鉴权本身已由 A3 的契约回放覆盖）。
- 假上游的 `client/configs` 一律回放真实夹具，否则账号的「安装序」会失败，
  于是根本走不到转发 —— 那是采样噪音，不是契约。
"""

import argparse
import io
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)


def find_mitm():
    """按顺序找 MITM 可执行文件：环境变量 → 仓库内 → 临时目录。"""
    cands = [os.environ.get("MITM_EXE", ""),
             os.path.join(ROOT, "tools", "mitmupstream", "mitmupstream.exe"),
             os.path.join(ROOT, "tools", "mitmupstream", "mitmupstream"),
             os.path.join(os.environ.get("TEMP", "/tmp"), "mitmupstream.exe"),
             "/tmp/mitmupstream.exe"]
    for c in cands:
        if c and os.path.isfile(c):
            return c
    return ""


MITM = find_mitm()
FIXTURES = os.path.join(ROOT, "docs", "contract", "outbound", "fixtures")
CONFIGS_FIXTURE = os.path.join(FIXTURES, "client-configs.json")

HOST = "127.0.0.1"
ADMIN_KEY = "a4-harness-admin"

# 靶机日志带 ANSI 色码（非 TTY 下也带），匹配记号行前先剥掉。
ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")
MARKS = (">>>", "[+]", "[~]", "[!]", "<!>")

# 比较入站响应头时的白名单 —— 其余（date / server / transfer-encoding / content-length）
# 是服务器实现细节（uvicorn 用 chunked，Go 的 net/http 可能给 Content-Length），不属契约。
HDR_WHITELIST = ("content-type", "cache-control", "x-custom-probe")

# 记号行里的易变字段：`<reqid>`（16 位小写 hex）与安装序的 UUID。
REQID_RE = re.compile(r"\b[0-9a-f]{16}\b")
UUID_RE = re.compile(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b")


def norm_marks(marks):
    """把记号行归一化成可逐行比较的序列。

    做两件事：
      - 掩码易变字段（`<reqid>` / `<uuid>`）；
      - **丢掉 `install` 行** —— 安装序是**并发后台任务**（见 behavior.md 第四节），
        它插在哪两条 `[~]` 之间纯看时序，拿它做逐行比较必然假失败。
    """
    out = []
    for m in marks:
        if " install " in m:
            continue
        s = UUID_RE.sub("<uuid>", m)
        s = REQID_RE.sub("<reqid>", s)
        out.append(s)
    return out

# 假上游里「与场景无关」的两条基础规则：账号安装序要用。
BASE_RULES = [
    {"host": "zcode.z.ai", "method": "GET", "path_suffix": "/client/configs",
     "status": 200, "body_file": CONFIGS_FIXTURE},
    {"host": "zcode.z.ai", "method": "POST", "path_suffix": "/event/report",
     "status": 200, "body": '{"code":0,"msg":"","logid":"harness"}'},
]

# Anthropic Messages API 的 200 形状。**这是公开规范定义的形状**（不是从靶机采来的），
# 用来测「成功回执如何透传 / 转换」；真实上游的 200 回执仍属未覆盖（无真实账号）。
ANTHROPIC_OK = json.dumps({
    "id": "msg_harness",
    "type": "message",
    "role": "assistant",
    "model": "GLM-5.3",
    "content": [{"type": "text", "text": "hello"}],
    "stop_reason": "end_turn",
    "stop_sequence": None,
    "usage": {"input_tokens": 3, "output_tokens": 1},
}, ensure_ascii=False)

# 逐帧 SSE：真实 Anthropic 流每帧都带 `event:` 行，所以用 chunks_raw 原样发。
ANTHROPIC_SSE = [
    'event: message_start\ndata: {"type":"message_start","message":{"id":"msg_harness","type":"message","role":"assistant","model":"GLM-5.3","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}',
    'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}',
    'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}',
    'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}',
    'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}',
    'event: message_stop\ndata: {"type":"message_stop"}',
]

REQ_MESSAGES = {"model": "GLM-5.3", "max_tokens": 16,
                "messages": [{"role": "user", "content": "hi"}]}
REQ_MESSAGES_STREAM = dict(REQ_MESSAGES, stream=True)
REQ_CHAT = {"model": "GLM-5.3", "max_tokens": 16,
            "messages": [{"role": "user", "content": "hi"}]}
REQ_CHAT_STREAM = dict(REQ_CHAT, stream=True)


def rule_messages(**kw):
    r = {"host": "api.z.ai", "method": "POST", "path_suffix": "/v1/messages"}
    r.update(kw)
    return r


SCENARIOS = {
    # ---- 成功回执：透传 vs 转换 ----
    "ok-anthropic": {
        "note": "上游 200（Anthropic 形状）→ 入站应原样回执",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "ok-stream": {
        "note": "上游 SSE 分块 → 入站应逐帧透传",
        "rules": [rule_messages(status=200, chunks_raw=ANTHROPIC_SSE)],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES_STREAM)],
    },
    "ok-openai": {
        "note": "chat/completions 命中上游 200 → 入站应转成 OpenAI 形状",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/chat/completions", REQ_CHAT)],
    },
    "ok-openai-stream": {
        "note": "chat/completions + stream → 入站应转成 OpenAI SSE",
        "rules": [rule_messages(status=200, chunks_raw=ANTHROPIC_SSE)],
        "steps": [("POST", "/v1/chat/completions", REQ_CHAT_STREAM)],
    },
    "sse-to-nonstream": {
        "note": "**已更正**：/v1/messages + 入站无 stream + 上游 SSE → 200 **原样透传 SSE**（不是 502）",
        "rules": [rule_messages(status=200, chunks_raw=ANTHROPIC_SSE)],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "chat-sse-on-nonstream": {
        "note": "chat/completions + 入站 stream 为**假值** + 上游 SSE → 502 upstream_error（502 只在这里）",
        "rules": [rule_messages(status=200, chunks_raw=ANTHROPIC_SSE)],
        "steps": [("POST", "/v1/chat/completions", REQ_CHAT)],
    },
    "chat-stream-from-json": {
        "note": "chat/completions + stream:true + 上游 **JSON** → 也会被转成 OpenAI SSE",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/chat/completions", REQ_CHAT_STREAM)],
    },
    "stream-string-false": {
        "note": "stream 用 **Python 真值**：\"false\" 也是真值 ⇒ 走流式（>>> 印 stream、chat 走 SSE 转换）",
        "rules": [rule_messages(status=200, chunks_raw=ANTHROPIC_SSE)],
        "steps": [("POST", "/v1/chat/completions", dict(REQ_CHAT, stream="false"))],
    },
    "route-preview-multiturn": {
        "note": ">>> 第 4 字段取**最后一条 user** 消息的文本（multiturn 印 again），且不影响出站",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/messages", {
            "model": "GLM-5.3", "max_tokens": 16,
            "messages": [{"role": "user", "content": "hi"},
                         {"role": "assistant", "content": "yo"},
                         {"role": "user", "content": "again"}]})],
    },
    "model-unknown": {
        "note": "目录外模型照常转发，model 原样写进出站体（不做模型名校验）",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/messages", {
            "model": "no-such-model-xyz", "max_tokens": 16,
            "messages": [{"role": "user", "content": "hi"}]})],
    },
    "ct-passthrough": {
        "note": "入站 Content-Type 照搬上游（text/* 追加 charset）；上游自定义头不透传；总是加 cache-control: no-cache",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK,
                                headers={"Content-Type": "text/x-probe",
                                         "X-Custom-Probe": "abc"})],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    # ---- 上游错误：分类与冷却 ----
    "upstream-401": {
        "note": "上游 401 → 错误体是否原样透传？账号是否被标 invalid？",
        "rules": [rule_messages(status=401,
                                body='{"error":{"message":"token expired or incorrect","type":"401"}}')],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-429": {
        "note": "上游 429 → 状态码是否透传？是否进冷却？（慢：按 Retry-After 退避重试）",
        "rules": [rule_messages(status=429,
                                body='{"error":{"message":"rate limited","type":"429"}}',
                                headers={"Retry-After": "7"})],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-429-retryafter": {
        "note": "Retry-After: 2 → 退避是否跟着头走？（与 upstream-429 的 7s 对照）",
        "rules": [rule_messages(status=429,
                                body='{"error":{"message":"rate limited","type":"429"}}',
                                headers={"Retry-After": "2"})],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-403": {
        "note": "上游 403 → 与 401 同类（切账号）还是别类？",
        "rules": [rule_messages(status=403,
                                body='{"error":{"message":"forbidden","type":"403"}}')],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-400": {
        "note": "上游 400 → 参数错，是否直接透传给客户端？",
        "rules": [rule_messages(status=400,
                                body='{"error":{"message":"bad model","type":"400"}}')],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-abort": {
        "note": "上游直接断连 → 「连接失败」分支如何分类与切换？",
        "rules": [rule_messages(abort=True)],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-500": {
        "note": "上游 500 → 状态码是否透传？",
        "rules": [rule_messages(status=500,
                                body='{"error":{"message":"boom","type":"500"}}')],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-200-badjson": {
        "note": "上游 200 但体不是 JSON → 入站如何回执？",
        "rules": [rule_messages(status=200, body="not json at all")],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    "upstream-200-error": {
        "note": "上游 200 但体是错误体（业务失败）→ 入站回执？",
        "rules": [rule_messages(status=200,
                                body='{"error":{"message":"quota exceeded","type":"quota"}}')],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    # ---- 切换与冷却：先失败 N 次再成功 ----
    "fail-then-ok": {
        "note": "第 1 个账号 401、第 2 个成功 → 是否切换、切换后是否成功",
        "rules": [rule_messages(times=1, status=401,
                                body='{"error":{"message":"token expired or incorrect","type":"401"}}'),
                  rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
    },
    # ---- 配置面：configs 挂掉时模型表是否回落 ----
    "configs-down": {
        "note": "阻断 client/configs → /v1/models 是否回落内置常量？（observations #26 待钉死项）",
        "rules": [{"host": "zcode.z.ai", "method": "GET", "path_suffix": "/client/configs",
                   "status": 502, "body": "upstream down"},
                  rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("GET", "/v1/models", None),
                  ("POST", "/v1/messages", REQ_MESSAGES)],
    },
    # ---- 未实现/边界 ----
    "no-account": {
        "note": "池为空 → 503 no_available_account（A3 已覆盖，这里做回归）",
        "rules": [rule_messages(status=200, body=ANTHROPIC_OK)],
        "steps": [("POST", "/v1/messages", REQ_MESSAGES)],
        "accounts": 0,
    },
}


# ---------------------------------------------------------------- 基础设施

def free_port():
    s = socket.socket()
    s.bind((HOST, 0))
    p = s.getsockname()[1]
    s.close()
    return p


def http(method, url, body=None, hdr=None, timeout=60, raw=False):
    h = {"Content-Type": "application/json"}
    h.update(hdr or {})
    data = None
    if body is not None:
        data = body if isinstance(body, (bytes, str)) else json.dumps(body).encode()
        if isinstance(data, str):
            data = data.encode()
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)
    except Exception as e:  # noqa
        return 0, str(e).encode(), {}


def hget(hdr, name, default=""):
    """按 HTTP 语义取响应头（大小写不敏感）。

    踩过坑：`dict(HTTPMessage)` 保留**原始大小写**，服务端发 `content-type` 时
    `d["Content-Type"]` 直接取不到 —— 于是把「有 Content-Type」误判成「没有」。
    """
    for k, v in (hdr or {}).items():
        if k.lower() == name.lower():
            return v
    return default


def wait_file_line(path, marker, timeout=30):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if os.path.isfile(path):
            for line in io.open(path, encoding="utf-8", errors="replace"):
                if line.startswith(marker):
                    return line[len(marker):].strip()
        time.sleep(0.1)
    return None


def wait_target_port(logf, timeout=45):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if os.path.isfile(logf):
            for line in io.open(logf, encoding="utf-8", errors="replace"):
                if "127.0.0.1:" in line and ("Uvicorn" in line or "running" in line.lower()):
                    seg = line.split("127.0.0.1:")[-1].split()[0].strip()
                    if seg.isdigit():
                        return int(seg)
        time.sleep(0.4)
    return None


def wait_go_port(base_url, timeout=30):
    t0 = time.time()
    while time.time() - t0 < timeout:
        st, _, _ = http("GET", base_url + "/v1/models", timeout=3)
        if st:
            return True
        time.sleep(0.3)
    return False


class Target:
    """一个被测实现（Python 靶机或 Go 二进制）。"""

    def __init__(self, kind, work, py_dir=None, go_exe=None):
        self.kind = kind
        self.work = work
        self.data = os.path.join(work, "data")
        os.makedirs(self.data, exist_ok=True)
        self.logf = os.path.join(work, "target.log")
        self.port = free_port()
        self.base = "http://%s:%d" % (HOST, self.port)
        self.proc = None
        self.log = None
        self.py_dir = py_dir
        self.go_exe = go_exe

    def start(self, mitm_addr, ca):
        env = dict(os.environ)
        env["ZCODE_DATA_DIR"] = self.data
        env["ZCODE_ADMIN_KEY"] = ADMIN_KEY
        env["ZCODE_HOST"] = HOST
        env["ZCODE_PORT"] = str(self.port)
        env["HTTPS_PROXY"] = "http://" + mitm_addr
        env["HTTP_PROXY"] = env["HTTPS_PROXY"]
        env["https_proxy"] = env["HTTPS_PROXY"]
        env["http_proxy"] = env["HTTPS_PROXY"]
        env["SSL_CERT_FILE"] = ca
        env["REQUESTS_CA_BUNDLE"] = ca
        if self.kind == "py":
            py = os.path.join(self.py_dir, ".venv", "Scripts", "python.exe")
            if not os.path.isfile(py):
                py = os.path.join(self.py_dir, ".venv", "bin", "python")
            cmd = [py, "-u", "cli.py", "serve"]
            cwd = self.py_dir
        else:
            cmd = [self.go_exe, "serve", "--data-dir", self.data, "--host", HOST,
                   "--port", str(self.port)]
            cwd = os.path.dirname(os.path.abspath(self.go_exe)) or "."
        self.log = io.open(self.logf, "w", encoding="utf-8", errors="replace")
        self.proc = subprocess.Popen(cmd, env=env, stdout=self.log,
                                     stderr=subprocess.STDOUT, cwd=cwd)
        if self.kind == "py":
            return wait_target_port(self.logf) is not None
        return wait_go_port(self.base)

    def admin(self, method, path, body=None):
        return http(method, self.base + path, body,
                    {"Authorization": "Bearer " + ADMIN_KEY})

    def seed(self, n):
        for i in range(n):
            self.admin("POST", "/admin/api/accounts",
                       {"name": "h-%02d" % i, "provider": "zai",
                        "tokens": ["h-token-%04d-abcdef" % i]})

    def marks(self):
        if not os.path.isfile(self.logf):
            return []
        out = []
        for ln in io.open(self.logf, encoding="utf-8", errors="replace"):
            s = ANSI_RE.sub("", ln).lstrip().rstrip()
            if s.startswith(MARKS):
                out.append(s)
        return out

    def stop(self):
        try:
            http("POST", self.base + "/api/quit", {}, timeout=5)
        except Exception:  # noqa
            pass
        if self.proc:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=8)
            except Exception:  # noqa
                self.proc.kill()
        if self.log:
            self.log.close()


class FakeUpstream:
    def __init__(self, work):
        self.work = work
        self.rules_path = os.path.join(work, "rules.json")
        self.ca = os.path.join(work, "ca.pem")
        self.jsonl = os.path.join(work, "outbound.jsonl")
        self.logf = os.path.join(work, "mitm.log")
        self.port = free_port()
        self.addr = "%s:%d" % (HOST, self.port)
        self.proc = None
        self.log = None

    def start(self, rules):
        io.open(self.rules_path, "w", encoding="utf-8", newline="").write(
            json.dumps(rules, ensure_ascii=False, indent=2))
        # MITM 的 stderr 必须落文件：给没人读的 PIPE 会在写满后把代理卡死。
        self.log = io.open(self.logf, "w", encoding="utf-8", errors="replace")
        self.proc = subprocess.Popen(
            [MITM, "--mode", "fake", "--listen", self.addr, "--ca-out", self.ca,
             "--log", self.jsonl, "--rules", self.rules_path, "--log-connects"],
            stdout=self.log, stderr=subprocess.STDOUT, text=True)
        if not wait_file_line(self.logf, "LISTEN", timeout=15):
            raise RuntimeError("MITM 未启动：\n" +
                               io.open(self.logf, encoding="utf-8", errors="replace").read()[-1500:])

    def records(self):
        if not os.path.isfile(self.jsonl):
            return []
        return [json.loads(l) for l in io.open(self.jsonl, encoding="utf-8") if l.strip()]

    def stop(self):
        if self.proc:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except Exception:  # noqa
                self.proc.kill()
        if self.log:
            self.log.close()


# ---------------------------------------------------------------- 归一化

VOLATILE_KEYS = ("id", "created", "created_at", "logid", "install_id", "event_id")


def norm_json(x):
    """把 JSON 里的易变字段抹掉，键排序，便于语义比较。"""
    if isinstance(x, dict):
        return {k: ("<volatile>" if k in VOLATILE_KEYS else norm_json(v))
                for k, v in sorted(x.items())}
    if isinstance(x, list):
        return [norm_json(v) for v in x]
    return x


def norm_body(raw, ctype=""):
    text = raw.decode("utf-8", "replace")
    if "text/event-stream" in ctype or text.startswith("event:") or text.startswith("data:"):
        # SSE：逐帧比较，但把 data 里的易变字段抹掉
        frames = []
        for frame in text.split("\n\n"):
            frame = frame.strip("\n")
            if not frame:
                continue
            out = []
            for ln in frame.splitlines():
                if ln.startswith("data:"):
                    payload = ln[5:].strip()
                    try:
                        payload = json.dumps(norm_json(json.loads(payload)),
                                             ensure_ascii=False, sort_keys=True)
                    except Exception:  # noqa
                        pass
                    out.append("data: " + payload)
                else:
                    out.append(ln)
            frames.append("\n".join(out))
        return frames
    try:
        return norm_json(json.loads(text))
    except Exception:  # noqa
        return text


def norm_outbound(recs):
    """只保留真正转发到上游的请求（方法 + 路径 + body），去掉安装序的 configs/事件。

    同时给出 `body`（语义，键已排序）与 `raw_body`（**逐字节，含键序**）——
    键顺序本身是可观测契约（见 observations.md #24/#25），归一化会把它抹掉。
    """
    out = []
    for r in recs:
        if r.get("kind") == "connect":
            continue
        path = (r.get("path") or "").split("?")[0]
        if path.endswith("/client/configs") or path.endswith("/event/report"):
            continue
        raw_body = r.get("req_body_text") or ""
        body = None
        try:
            body = norm_json(json.loads(raw_body)) if raw_body else None
        except Exception:  # noqa
            body = raw_body
        out.append({"method": r.get("method"), "path": path, "host": r.get("host"),
                    "body": body, "raw_body": raw_body})
    return out


# ---------------------------------------------------------------- 执行

def run_scenario(kind, sc, work, py_dir, go_exe, keep, timeout=60):
    name = sc["_name"]
    d = os.path.join(work, name)
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d, exist_ok=True)

    up = FakeUpstream(d)
    up.start(BASE_RULES + sc["rules"])
    tgt = Target(kind, d, py_dir=py_dir, go_exe=go_exe)
    result = {"target": kind, "scenario": name, "note": sc["note"], "steps": []}
    try:
        if not tgt.start(up.addr, up.ca):
            result["error"] = "被测实现未启动：" + \
                io.open(tgt.logf, encoding="utf-8", errors="replace").read()[-1500:]
            return result
        tgt.seed(sc.get("accounts", 2))
        for method, path, body in sc["steps"]:
            n0 = len(up.records())
            st, raw, hdr = http(method, tgt.base + path, body, timeout=timeout)
            time.sleep(0.4)
            ctype = hget(hdr, "Content-Type")
            result["steps"].append({
                "req": {"method": method, "path": path, "body": body},
                "status": st,
                "ctype": ctype,
                # 响应头按**小写键**存全量，比较时只取白名单（见 HDR_WHITELIST）。
                "headers": {k.lower(): v for k, v in (hdr or {}).items()},
                "body": norm_body(raw, ctype),
                # raw_text 用于**逐字节**判「转发保真」（键顺序也是契约）。
                "raw_text": raw.decode("utf-8", "replace"),
                "raw_len": len(raw),
                "outbound": norm_outbound(up.records()[n0:]),
            })
        result["marks"] = tgt.marks()
    finally:
        tgt.stop()
        up.stop()
        if not keep:
            shutil.rmtree(d, ignore_errors=True)
    return result


def show(res):
    print("=== %s / %s ===" % (res["target"], res["scenario"]))
    print("    场景：", res["note"])
    if res.get("error"):
        print("    !! ", res["error"][:800])
        return
    for i, s in enumerate(res["steps"]):
        print("  [%d] %s %s -> %s  (ctype=%r, %dB)" % (
            i, s["req"]["method"], s["req"]["path"], s["status"], s["ctype"], s["raw_len"]))
        wh = {k: hget(s.get("headers") or {}, k) for k in HDR_WHITELIST}
        print("      响应头（白名单）:", json.dumps(wh, ensure_ascii=False))
        print("      响应原文:", json.dumps(s["raw_text"][:600], ensure_ascii=False))
        if s["outbound"]:
            for o in s["outbound"]:
                print("      出站: %s %s%s" % (o["host"], o["method"], o["path"]))
                print("            原文:", json.dumps(o["raw_body"][:400], ensure_ascii=False))
        else:
            print("      出站: （无）")
    if res.get("marks"):
        print("  --- 路由记号 ---")
        for m in res["marks"][-12:]:
            print("      ", m[:150])


def diff(a, b, compare_marks=True):
    """逐项对比两次运行，返回差异行。"""
    def sem(outs):
        return [{k: v for k, v in o.items() if k != "raw_body"} for o in outs]

    out = []
    if len(a["steps"]) != len(b["steps"]):
        out.append("步骤数不同：%d vs %d" % (len(a["steps"]), len(b["steps"])))
    for i, (x, y) in enumerate(zip(a["steps"], b["steps"])):
        if x["status"] != y["status"]:
            out.append("[%d] 状态码 %s vs %s" % (i, x["status"], y["status"]))
        if x["ctype"] != y["ctype"]:
            out.append("[%d] Content-Type %r vs %r" % (i, x["ctype"], y["ctype"]))
        for name in HDR_WHITELIST:
            hx = hget(x.get("headers") or {}, name)
            hy = hget(y.get("headers") or {}, name)
            if hx != hy:
                out.append("[%d] 响应头 %s: %r vs %r" % (i, name, hx, hy))
        if x["body"] != y["body"]:
            out.append("[%d] 响应体（语义）不同\n      py: %s\n      go: %s" % (
                i, json.dumps(x["body"], ensure_ascii=False)[:400],
                json.dumps(y["body"], ensure_ascii=False)[:400]))
        if sem(x["outbound"]) != sem(y["outbound"]):
            out.append("[%d] 出站请求（语义）不同\n      py: %s\n      go: %s" % (
                i, json.dumps(sem(x["outbound"]), ensure_ascii=False)[:400],
                json.dumps(sem(y["outbound"]), ensure_ascii=False)[:400]))
        for ox, oy in zip(x["outbound"], y["outbound"]):
            if ox["raw_body"] != oy["raw_body"]:
                out.append("[%d] 出站 body **逐字节不同**（键序/空白也算差异）\n"
                           "      py: %s\n      go: %s" % (
                               i, json.dumps(ox["raw_body"][:300], ensure_ascii=False),
                               json.dumps(oy["raw_body"][:300], ensure_ascii=False)))
        if x["raw_text"] != y["raw_text"]:
            out.append("[%d] 响应体**逐字节不同**（键序/空白也算差异；含易变字段时属预期）\n"
                       "      py: %s\n      go: %s" % (
                           i, json.dumps(x["raw_text"][:300], ensure_ascii=False),
                           json.dumps(y["raw_text"][:300], ensure_ascii=False)))
    if compare_marks:
        mx = norm_marks(a.get("marks") or [])
        my = norm_marks(b.get("marks") or [])
        if mx != my:
            out.append("路由记号序列不同（已掩码 <reqid>/<uuid>、已丢 install 行）\n"
                       "      py: %s\n      go: %s" % (
                           json.dumps(mx, ensure_ascii=False)[:600],
                           json.dumps(my, ensure_ascii=False)[:600]))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--py-dir", default="D:/AiWork/ZCode/zcode2api",
                    help="上游 zcode2api 目录（含 cli.py 与 .venv）")
    ap.add_argument("--go-exe", default="", help="Go 实现的可执行文件；给了就做对照")
    ap.add_argument("--scenario", default="", help="只跑这些场景（逗号分隔，子串匹配）")
    ap.add_argument("--list", action="store_true", help="列出全部场景")
    ap.add_argument("--work", default=os.path.join(os.environ.get("TEMP", "/tmp"), "zcode_a4_harness"))
    ap.add_argument("--keep", action="store_true", help="保留工作目录（含假上游日志）")
    ap.add_argument("--timeout", type=int, default=90,
                    help="单个入站请求的等待秒数（429/500 场景会退避重试，需放宽）")
    ap.add_argument("--no-compare-marks", action="store_true",
                    help="不比较路由记号序列（默认比较：掩码 <reqid>/<uuid>、丢掉 install 行）")
    a = ap.parse_args()

    if a.list:
        for n, s in SCENARIOS.items():
            print("%-20s %s" % (n, s["note"]))
        return 0

    if not os.path.isfile(MITM):
        print("找不到 MITM 可执行文件。先编译：")
        print("  go build -o tools/mitmupstream/mitmupstream.exe ./tools/mitmupstream")
        print("或用 MITM_EXE=<路径> 指定。")
        return 1
    if not os.path.isfile(CONFIGS_FIXTURE):
        print("找不到 client/configs 夹具：", CONFIGS_FIXTURE)
        return 1

    names = [n for n in SCENARIOS if not a.scenario or
             any(w.strip() in n for w in a.scenario.split(",") if w.strip())]
    if not names:
        print("没有匹配的场景。可用：", ", ".join(SCENARIOS))
        return 1

    os.makedirs(a.work, exist_ok=True)
    bad = 0
    for n in names:
        sc = dict(SCENARIOS[n])
        sc["_name"] = n
        rpy = run_scenario("py", sc, a.work, a.py_dir, "", a.keep, a.timeout)
        show(rpy)
        if a.go_exe:
            rgo = run_scenario("go", sc, a.work, a.py_dir, a.go_exe, a.keep, a.timeout)
            show(rgo)
            d = diff(rpy, rgo, compare_marks=not a.no_compare_marks)
            if d:
                bad += 1
                print("  >>> 差异 %d 项：" % len(d))
                for line in d:
                    print("      " + line)
            else:
                print("  >>> 一致 ✓")
        print()
    if a.go_exe:
        print("对照完成：%d/%d 个场景存在差异" % (bad, len(names)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
