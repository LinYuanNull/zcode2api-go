#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""A5 管理侧出站契约采样：触发靶机的**管理面**出站（OAuth 设备码 + JWT 额度查询 + 领取），
经本地 MITM 捕获请求/响应形状。

与 A4 的 `capture_outbound.py` 的差别：
  - A4 走**网关入口**（`/v1/messages`），只需要一个 `apiKey` 账号就能触发转发；
  - A5 走**管理入口**（`/admin/api/login/*`、`/admin/api/accounts/*/refresh`、
    `/admin/api/claim/*`），需要区分两段：
      段 1（无账号）：`POST /admin/api/login/start` 与 `GET /admin/api/login/poll/{id}`
        —— 触发 `POST /api/v1/oauth/cli/init` 与 `GET /api/v1/oauth/cli/poll/{flow_id}`。
      段 2（假 JWT 账号）：靶机库里 `mode=apiKey` 的账号对 refresh 直接返回
        「仅 Coding Plan (JWT) 账号支持额度查询」、**零出站**。所以必须把库里的账号
        改写成 `mode=jwt` + 一个形状合法但签名无效的 JWT，再打 refresh / claim——
        这时靶机会真的去查 `zcode-plan/usage` 与 `billing/*`（拿到 401/404），
        于是请求侧形状与「凭据失效」分支都可采。

为什么可行（实测）：
  - 出站地址硬编码，但**走 `HTTPS_PROXY`**（管理侧实测同样走代理，含 billing）；
  - httpx 在 `trust_env=True` 时读 `SSL_CERT_FILE` ⇒ 设成本地 CA 即可解密。

产出（`--emit DIR`）：`docs/contract/outbound-admin/<NN>-<slug>.json`（脱敏后）
+ `fixtures/admin-outbound-requests.json`（本轮全部出站记录，脱敏，可复现判据）。
默认只打印，不落盘。

用法:
  python tools/capture_admin_outbound.py --target-dir D:/AiWork/ZCode/zcode2api [--emit DIR] [--keep]
"""
import argparse
import base64
import gzip
import json
import os
import re
import shutil
import socket
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
MITM = os.environ.get("MITM_EXE", os.path.join(ROOT, "tools", "mitmupstream", "mitmupstream.exe"))

# 靶机日志带 ANSI 色码（非 TTY 下也带），做记号行匹配前先剥掉。
ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")

ADMIN_KEY = "a5-capture-admin"
HOST = "127.0.0.1"

# 一个形状合法、签名无效的 JWT：header 段是 {"alg":"HS256"} 的直接 base64url。
# 只要 `sub` 是稳定值，靶机就会把它当 JWT 账号去查额度。
# （`sub` 值本身会随真实账号变化，故夹具里按取值规则脱敏。）
FAKE_JWT = (
    "eyJhbGciOiJIUzI1NiJ9."
    "eyJzdWIiOiJhNXByb2JlIiwidHlwIjoiY29kZSJ9."
    "a5probesignaturenotvalid0000000000000"
)

# 出站记录 → 夹具 slug 的匹配表（按 host + path 前缀，先精确后前缀）。
ROUTES = [
    ("oauth-init", "POST", "zcode.z.ai", "/api/v1/oauth/cli/init"),
    ("oauth-poll", "GET", "zcode.z.ai", "/api/v1/oauth/cli/poll/"),
    ("plan-usage", "GET", "zcode.z.ai", "/api/v1/zcode-plan/usage"),
    ("plan-billing-current", "GET", "zcode.z.ai", "/api/v1/zcode-plan/billing/current"),
    ("plan-billing-balance", "GET", "zcode.z.ai", "/api/v1/zcode-plan/billing/balance"),
]


def free_port():
    s = socket.socket()
    s.bind((HOST, 0))
    p = s.getsockname()[1]
    s.close()
    return p


def req(method, url, body=None, hdr=None, timeout=60):
    h = {"Content-Type": "application/json"}
    h.update(hdr or {})
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # noqa
        return 0, str(e).encode()


def wait_port(logf, timeout=40):
    """等 uvicorn 打出监听行。**必须取最后一条**——日志是追加写的，上一轮的旧行还在。"""
    t0 = time.time()
    port = None
    while time.time() - t0 < timeout:
        if os.path.isfile(logf):
            txt = open(logf, encoding="utf-8", errors="replace").read()
            for line in txt.splitlines():
                if "127.0.0.1:" in line and ("Uvicorn" in line or "running" in line.lower()):
                    seg = line.split("127.0.0.1:")[-1].split()[0].strip()
                    if seg.isdigit():
                        port = int(seg)
        if port is not None:
            return port
        time.sleep(0.3)
    return port


def wait_line(path, marker, timeout=15):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if os.path.isfile(path):
            for line in open(path, encoding="utf-8", errors="replace"):
                if line.startswith(marker):
                    return line[len(marker):].strip()
        time.sleep(0.1)
    return None


def start_target(py, env, target_dir, logf):
    log = open(logf, "a", encoding="utf-8", errors="replace")
    return subprocess.Popen([py, "-u", "cli.py", "serve"], env=env, stdout=log,
                            stderr=subprocess.STDOUT, cwd=target_dir)


def stop_target(proc, base):
    try:
        req("POST", base + "/api/quit", {}, {})
    except Exception:  # noqa
        pass
    time.sleep(2.5)
    proc.terminate()
    try:
        proc.wait(timeout=8)
    except Exception:  # noqa
        proc.kill()


def inject_jwt(db, jwt, status="invalid"):
    """把库里全部账号改写成 mode=jwt + 假 JWT。

    `status` 决定随后的行为（**实测判定**）：
      - `active`：启动会对它自刷一次额度；随后 `refresh` 会**真查上游**
        （响应走 `{ok:false,result:{error:…}}` 分支），查失败后转 `invalid`。
      - `invalid`（默认）：启动与 `refresh` 都**不再查上游**，`refresh` 直接回
        `{ok:false,message:…}` 的缓存分支。
    """
    if not os.path.isfile(db):
        return 0
    c = sqlite3.connect(db)
    n = 0
    for rid, data in list(c.execute("select id, data from accounts")):
        try:
            j = json.loads(data)
        except Exception:  # noqa
            continue
        j["mode"] = "jwt"
        j["jwt_token"] = jwt
        j["api_key"] = None
        j["status"] = status
        c.execute("update accounts set mode='jwt', status=?, data=? where id=?",
                  (status, json.dumps(j, ensure_ascii=False), rid))
        n += 1
    c.commit()
    c.close()
    return n


# ---------------------------------------------------------------- 脱敏

HEX32_RE = re.compile(r"^[0-9a-f]{32}$")
HEX64_RE = re.compile(r"^[0-9a-f]{64}$")
JWT_RE = re.compile(r"^[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*$")


def redact_header_value(name, value):
    low = name.lower()
    if low == "authorization":
        return "<redacted:bearer>"
    if low == "cookie":
        return "<redacted:cookie>"
    if low == "set-cookie":
        # 保留 cookie **名**与属性，只抹掉取值（visitor_id 等是持久跟踪标识）。
        return re.sub(r"^([^=;]+)=[^;]*", r"\1=<redacted:value>", value)
    if low in ("x-device-mid", "device-mid"):
        # 设备指纹**原值**按 ../README.md 规则一律脱敏（形状是 UUID）。
        return "<redacted:device-mid>"
    if JWT_RE.match(value):
        return "<redacted:jwt>"
    if HEX64_RE.match(value):
        return "<redacted:hex64>"
    if HEX32_RE.match(value):
        return "<redacted:hex32>"
    return value


def redact_url(url):
    """authorize_url：保留域名与参数**形状**，只把逐次生成的取值脱敏。"""
    out = re.sub(r"(state=)[0-9a-fA-F]{16,}", r"\1<redacted:state>", url)
    out = re.sub(r"(flow_id=)[0-9a-fA-F]{16,}", r"\1<redacted:flow-id>", out)
    return out


def redact_path(path):
    """poll 路径里带 flow_id（32 hex）。"""
    return re.sub(r"(/oauth/cli/poll/)[0-9a-fA-F]{16,}", r"\1<redacted:flow-id>", path)


def redact_headers(h, resp=False):
    out = {}
    for k, vs in (h or {}).items():
        if resp and k.lower() in ("eagleid", "x-request-id"):
            # 响应侧追踪号由上游**逐次生成**，按取值规则脱敏；
            # 请求侧的 X-Request-Id 是本工具关心的契约（三条并发请求共用同一值），保留。
            vs = ["<redacted:hex32>"] if isinstance(vs, list) else "<redacted:hex32>"
        if isinstance(vs, list):
            out[k] = [redact_header_value(k, v) for v in vs]
        else:
            out[k] = redact_header_value(k, str(vs))
    return out


def redact_body_text(txt):
    if not txt:
        return txt
    # 出站体里出现 state / flow_id 的取值时一并脱敏（保持键与结构）。
    # `device_mid` 是逐安装生成的设备标识（fingerprint 里那个），与请求头 X-Device-Mid 同性质。
    txt = re.sub(r'"(state|flow_id|poll_token|logid|install_id|event_id|device_mid)":\s*"[^"]*"',
                 r'"\1": "<redacted:value>"', txt)
    txt = re.sub(r"(state=)[0-9a-fA-F]{16,}", r"\1<redacted:state>", txt)
    txt = re.sub(r"(flow_id=)[0-9a-fA-F]{16,}", r"\1<redacted:flow-id>", txt)
    return txt


def show(v, n=300):
    """把任意响应片段转成可安全回显的字符串（同样脱敏，绝不回显凭据）。"""
    if isinstance(v, (bytes, bytearray)):
        v = v.decode("utf-8", "replace")
    return redact_body_text(str(v))[:n]


# 管理侧（入站）响应的旁录：A5 新出现的分支（poll failed/expired、refresh 的
# 「新鲜」与「缓存」两种形态）必须留证，否则实现时无从对照。
ADMIN_RESP = []


def note_admin(label, status, body):
    ADMIN_RESP.append({"label": label, "status": status,
                       "body_text": redact_body_text(
                           body.decode("utf-8", "replace") if isinstance(body, (bytes, bytearray))
                           else str(body))})
    return status, body


def classify(rec):
    host = (rec.get("host") or "").lower()
    meth = (rec.get("method") or "").upper()
    path = rec.get("path") or ""
    for slug, m, h, p in ROUTES:
        if meth == m and (host == h or host.endswith("." + h)) and path.startswith(p):
            return slug
    return None


def load_recs(jsonl):
    if not os.path.isfile(jsonl):
        return []
    return [json.loads(l) for l in open(jsonl, encoding="utf-8") if l.strip()]


def quota_n(jsonl):
    """已捕获的额度类出站条数（判断「这一步有没有真查上游」用）。"""
    return sum(1 for r in load_recs(jsonl) if "zcode-plan" in (r.get("path") or ""))


def decode_body(r):
    """上游响应是 gzip 时，MITM 的 resp_body_text 是空的 ⇒ 必须自己解。"""
    txt = r.get("resp_body_text") or ""
    if txt:
        return txt
    b64 = r.get("resp_body_b64") or ""
    if not b64:
        return ""
    try:
        raw = base64.b64decode(b64)
    except Exception:  # noqa
        return ""
    enc = " ".join((r.get("resp_headers") or {}).get("Content-Encoding") or []).lower()
    if "gzip" in enc:
        try:
            raw = gzip.decompress(raw)
        except Exception:  # noqa
            pass
    return raw.decode("utf-8", "replace")


def emit(recs, outdir, stamp, target_note):
    os.makedirs(outdir, exist_ok=True)
    fxdir = os.path.join(outdir, "fixtures")
    os.makedirs(fxdir, exist_ok=True)

    # 先收全（按到达顺序，并发请求的到达顺序不稳定），再按**语义固定序**落盘。
    idx = {}
    raw = []
    skipped = {}
    for r in recs:
        if r.get("kind") == "connect":
            continue
        slug = classify(r)
        if slug is None:
            # A5 只关心管理侧出站；`event/report`、`client/configs` 属 A4，不在这里重复收录。
            key = "%s %s" % (r.get("method"), (r.get("path") or "").split("?")[0])
            skipped[key] = skipped.get(key, 0) + 1
            continue
        rec = {
            "host": r.get("host"),
            "method": r.get("method"),
            "path": redact_path(r.get("path") or ""),
            "req_headers": redact_headers(r.get("req_headers")),
            "req_body_text": redact_body_text(r.get("req_body_text")),
            "resp_status": r.get("resp_status"),
            "resp_headers": redact_headers(r.get("resp_headers"), resp=True),
            "resp_body_text": redact_body_text(decode_body(r)),
        }
        raw.append(rec)
        if slug not in idx:
            idx[slug] = rec

    # 编号必须按语义序，不能按到达序：额度三条（usage / billing/current / billing/balance）
    # 是同一毫秒并发发出的，到达顺序在两次运行间会变 ⇒ 按到达序编号会让文件名来回漂移。
    # 落盘前先清掉旧的 `NN-*.json`：否则上一轮若编过别的名字，会与新一轮并存成假契约。
    for old in sorted(os.listdir(outdir)):
        if re.match(r"^\d\d-.*\.json$", old):
            os.remove(os.path.join(outdir, old))
    order = {s: i for i, (s, _m, _h, _p) in enumerate(ROUTES)}
    for n, slug in enumerate(sorted(idx, key=lambda s: order.get(s, 10 ** 6)), 1):
        rec = idx[slug]
        doc = {
            "_about": "出站契约样本：网关的**管理面** → 上游。A5（额度/领取/登录）的实现依据。",
            "_captured": stamp,
            "_target": target_note,
            "_method": "本地 MITM（tools/mitmupstream，自签 CA + 叶子证书终结 TLS）；"
                       "靶机设 HTTPS_PROXY 与 SSL_CERT_FILE 指向本工具；管理侧出站同样走代理",
            "route": "%s https://%s%s" % (rec["method"], rec["host"], rec["path"]),
            "method": rec["method"],
            "request": {
                "headers": rec["req_headers"],
                "body_text": rec["req_body_text"],
            },
            "response": {
                "status": rec["resp_status"],
                "headers": rec["resp_headers"],
                "body_text": rec["resp_body_text"],
            },
            "notes": ["由 tools/capture_admin_outbound.py 自动生成；取值已按 ../README.md 规则脱敏。"
                      "上游响应为 gzip ⇒ 这里的 body_text 是解压后的明文。"],
        }
        fn = os.path.join(outdir, "%02d-%s.%s.json" % (n, slug, rec["method"]))
        # `newline="\n"`：仓库策略是 `* text=auto eol=lf`（工作树也用 LF），
        # 契约 JSON 必须与 A4 的 Go 采样产物同为纯 LF；不加会写出 CRLF（Windows 下 open 默认翻译）。
        open(fn, "w", encoding="utf-8", newline="\n").write(
            json.dumps(doc, ensure_ascii=False, indent=2) + "\n")
        print("  写出", os.path.relpath(fn, ROOT))

    open(os.path.join(fxdir, "admin-outbound-requests.json"), "w", encoding="utf-8", newline="\n").write(
        json.dumps({"_about": "A5 管理侧出站全部捕获记录（脱敏、按到达顺序）",
                    "_captured": stamp,
                    "_excluded": skipped,
                    "_excluded_note": "以上是同轮捕获但**不属 A5 范围**的请求（A4 的 client/configs 与 "
                                      "event/report），已在 A4 的 outbound 契约里覆盖，此处不重复收录。",
                    "records": raw}, ensure_ascii=False, indent=2) + "\n")
    print("  写出", os.path.relpath(os.path.join(fxdir, "admin-outbound-requests.json"), ROOT))

    if ADMIN_RESP:
        open(os.path.join(fxdir, "admin-responses.json"), "w", encoding="utf-8", newline="\n").write(
            json.dumps({"_about": "A5 采样期间管理侧（入站）响应的旁录：本轮新出现的分支留证",
                        "_captured": stamp,
                        "records": ADMIN_RESP}, ensure_ascii=False, indent=2) + "\n")
        print("  写出", os.path.relpath(os.path.join(fxdir, "admin-responses.json"), ROOT))
    return idx


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--target-dir", required=True, help="上游 zcode2api 目录（含 cli.py 与 .venv）")
    ap.add_argument("--out", default=os.path.join(ROOT, "docs", "contract", "outbound-admin"))
    ap.add_argument("--work", default=os.path.join(os.environ.get("TEMP", "/tmp"), "zcode_a5_capture"))
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--emit", action="store_true", help="把脱敏夹具写入 --out（默认只打印）")
    ap.add_argument("--hosts", default="zcode.z.ai,api.z.ai")
    ap.add_argument("--log-connects", action="store_true")
    a = ap.parse_args()

    py = os.path.join(a.target_dir, ".venv", "Scripts", "python.exe")
    if not os.path.isfile(py):
        py = os.path.join(a.target_dir, ".venv", "bin", "python")
    if not os.path.isfile(MITM):
        print("找不到 MITM 可执行文件：", MITM)
        return 1

    T0 = time.time()
    print("T0=%.6f" % T0)
    work = a.work
    shutil.rmtree(work, ignore_errors=True)
    os.makedirs(work, exist_ok=True)
    ca = os.path.join(work, "ca.pem")
    jsonl = os.path.join(work, "outbound.jsonl")
    rules = os.path.join(work, "rules.json")
    open(rules, "w").write("[]")
    logf = os.path.join(work, "serve.log")
    mitmlog = os.path.join(work, "mitm.log")

    mitm_port = free_port()
    mitm_cmd = [MITM, "--mode", "capture", "--listen", "%s:%d" % (HOST, mitm_port),
                "--ca-out", ca, "--log", jsonl, "--hosts", a.hosts]
    if a.log_connects:
        mitm_cmd.append("--log-connects")
    # MITM 的记录也打到 stderr。**必须落文件，不能给 PIPE** —— 无人读的 PIPE 写满（~64 KB）
    # 就会把代理卡死在 write 上，表现为「请求发出去了但后面一条记录都没有」。
    mitm = subprocess.Popen(mitm_cmd, stdout=open(mitmlog, "w", encoding="utf-8", errors="replace"),
                            stderr=subprocess.STDOUT, text=True)
    tgt = None
    port = None
    try:
        addr = wait_line(mitmlog, "LISTEN", timeout=15)
        if not addr:
            print("MITM 未启动：")
            print(open(mitmlog, encoding="utf-8", errors="replace").read()[-2000:])
            return 1
        print("MITM 监听", addr, "CA:", ca)

        tdata = os.path.join(work, "data")
        os.makedirs(tdata, exist_ok=True)
        env = dict(os.environ)
        env.update({"ZCODE_DATA_DIR": tdata, "ZCODE_ADMIN_KEY": ADMIN_KEY, "ZCODE_HOST": HOST,
                    "HTTPS_PROXY": "http://%s" % addr, "HTTP_PROXY": "http://%s" % addr,
                    "https_proxy": "http://%s" % addr, "http_proxy": "http://%s" % addr,
                    "SSL_CERT_FILE": ca, "REQUESTS_CA_BUNDLE": ca})
        tport = free_port()
        env["ZCODE_PORT"] = str(tport)
        tgt = start_target(py, env, a.target_dir, logf)
        port = wait_port(logf)
        if not port:
            print("靶机未启动：")
            print(open(logf, encoding="utf-8", errors="replace").read()[-2000:])
            return 1
        base = "http://%s:%d" % (HOST, port)
        ah = {"Authorization": "Bearer " + ADMIN_KEY}
        print("靶机端口", port)

        def call(label, method, path, body):
            """打一条管理接口，旁录响应，并按需打印（脱敏）。"""
            t = time.time()
            note_admin(label, *req(method, base + path, body, ah))
            st = ADMIN_RESP[-1]["status"]
            d = ADMIN_RESP[-1]["body_text"].encode()
            print("[+%.3fs] [%s] -> %s %s" % (t - T0, label, st, show(d, 300)))
            time.sleep(0.4)
            return st, d

        # ---- 段 1：无账号 → OAuth 设备码 ----
        st, d = note_admin("login/start", *req("POST", base + "/admin/api/login/start", {}, ah))
        print("[+%.3fs] [login/start] -> %s %s" % (time.time() - T0, st, show(d, 300)))
        flow_id = None
        try:
            flow_id = json.loads(d)["flow_id"]
        except Exception:  # noqa
            pass
        if flow_id:
            # **必须按上游声明的 `poll_interval_sec`（本样本为 2s）节流**：实测 0.5s 间隔连打，
            # 第 2 次出站会被上游限流成 429（ESA 层），采到的是限流样本而不是规范路径。
            for i in range(3):
                st, d = note_admin("login/poll#%d" % (i + 1),
                                  *req("GET", base + "/admin/api/login/poll/%s" % flow_id, None, ah))
                print("[login/poll#%d] -> %s %s" % (i + 1, st, show(d, 200)))
                time.sleep(2.2)
        # 未知 / 过期 flow：采「expired」分支的出站形状（上游可能直接 404/返回 expired）。
        st, d = note_admin("login/poll-unknown",
                          *req("GET", base + "/admin/api/login/poll/unknown-flow", None, ah))
        print("[login/poll unknown] ->", st, show(d, 200))
        st, d = note_admin("claim/captcha-config",
                          *req("GET", base + "/admin/api/claim/captcha-config", None, ah))
        print("[claim/captcha-config] ->", st, show(d, 200))

        # ---- 段 2：新增 JWT 账号 → 额度查询 ----
        # **实测订正**：管理侧额度查询的**主触发点是 `POST /admin/api/accounts` 本身**
        # （token 形状是 JWT ⇒ 新增时立刻校验），**不是**启动、也不是 refresh。
        # 启动对 `active` 账号也会刷一次；`invalid` 账号永不再查（需重新授权）。
        n0 = quota_n(jsonl)
        st, d = note_admin("accounts-add", *req(
            "POST", base + "/admin/api/accounts",
            {"name": "a5-jwt-probe", "provider": "zai", "tokens": [FAKE_JWT]}, ah))
        print("[+%.3fs] [add account] -> %s %s" % (time.time() - T0, st, show(d, 200)))
        jid = None
        try:
            jid = json.loads(d)["ids"][0]
        except Exception:  # noqa
            pass
        time.sleep(1.5)
        print(">>> add 触发的额度出站 = %d 条（account=%s）" % (quota_n(jsonl) - n0, jid))
        # add 之后该账号已被判 invalid ⇒ refresh 命中「缓存」分支（零出站）。
        call("accounts/refresh(status=invalid→期望缓存)", "POST",
             "/admin/api/accounts/%s/refresh" % jid, {})
        print(">>> 阶段标记：以上为 refresh 段，以下为 claim 段")
        # claim 三条：**均不出站**（只读已存状态）。
        call("claim/preview", "GET", "/admin/api/claim/preview", None)
        call("claim", "POST", "/admin/api/claim", {})
        call("claim/manual", "POST", "/admin/api/claim/manual",
             {"account_id": jid, "captcha_verify_param": "probe"})

        # ---- 段 3：把状态改回 active 并让 interval=0 ⇒ 采 refresh 的「新鲜」分支 ----
        print(">>> 段 3：status=active + interval=0 → 重启")
        db = os.path.join(tdata, "accounts.db")
        for attempt in range(3):
            st, d = req("PUT", base + "/admin/api/settings",
                        {"quota_refresh_interval": 0, "claim_round_interval": 0,
                         "account_concurrency": 2}, ah)
            time.sleep(0.8)
            stop_target(tgt, base)
            tgt = None
            n = inject_jwt(db, FAKE_JWT, status="active")
            tgt = start_target(py, env, a.target_dir, logf)
            port = wait_port(logf)
            base = "http://%s:%d" % (HOST, port)
            iv = None
            for _ in range(6):
                time.sleep(0.6)
                try:
                    _, d = req("GET", base + "/admin/api/settings", None, ah)
                    iv = json.loads(d)["quota_refresh_interval"]
                    break
                except Exception:  # noqa
                    iv = None
            print("[+%.3fs] 重启 attempt=%d port=%s interval=%r status=active(%d 个)"
                  % (time.time() - T0, attempt + 1, port, iv, n))
            if iv == 0:
                break
            time.sleep(1.0)
        # 启动对 active 账号会自刷一次；给它跑完，避免与随后的 refresh 竞态。
        time.sleep(4.0)
        nq0 = quota_n(jsonl)
        print(">>> 启动自刷后额度出站累计 = %d" % nq0)
        call("accounts/refresh#1(status=active→期望新鲜)", "POST",
             "/admin/api/accounts/%s/refresh" % jid, {})
        print(">>> refresh#1 新增额度出站 = %d" % (quota_n(jsonl) - nq0))
        call("accounts/refresh#2(已转 invalid→期望缓存)", "POST",
             "/admin/api/accounts/%s/refresh" % jid, {})
    finally:
        if tgt is not None:
            stop_target(tgt, "http://%s:%d" % (HOST, port) if port else "")
        mitm.terminate()
        try:
            mitm.wait(timeout=5)
        except Exception:  # noqa
            mitm.kill()

    if os.path.isfile(logf):
        marks = (">>>", "[+]", "[~]", "[!]", "<!>", "[claim]")
        keep = []
        for ln in open(logf, encoding="utf-8", errors="replace"):
            s = ANSI_RE.sub("", ln).lstrip()
            if s.startswith(marks):
                keep.append(s.rstrip())
        if keep:
            print("--- 靶机记号行（末 40 条）---")
            for ln in keep[-40:]:
                print("   ", ln)
            print("---")

    recs = load_recs(jsonl)
    print("捕获 %d 条记录" % len(recs))
    for i, r in enumerate(recs):
        if r.get("kind") == "connect":
            print("  [%02d] CONNECT %s %s" % (i, r.get("host"), r.get("error") or ""))
        else:
            print("  [%02d] %s %s %s -> %s" % (i, r.get("host"), r.get("method"),
                                               redact_path(r.get("path") or "")[:70],
                                               r.get("resp_status")))
            # 打印同样走脱敏：**绝不把凭据回显到控制台/日志**。
            if r.get("req_body_text"):
                print("        body: %s" % redact_body_text(r["req_body_text"])[:200])
            if r.get("req_headers"):
                print("        auth: %s" % redact_header_value(
                    "Authorization", (r["req_headers"].get("Authorization") or [""])[0]))
        if r.get("resp_body_text"):
            print("        resp: %s" % redact_body_text(r["resp_body_text"])[:200])

    if a.emit:
        stamp = time.strftime("%Y-%m-%d")
        note = "dengyie/zcode2api（独立临时数据目录 + 注入的假 JWT 账号）"
        print("--- 写出夹具 ---")
        emit(recs, a.out, stamp, note)

    if not a.keep:
        shutil.rmtree(work, ignore_errors=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
