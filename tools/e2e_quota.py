#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""A5-3 额度查询：**真实二进制**端到端验收（纯标准库）。

全程只走本机回环，**不打真实 z.ai**：

 1. `tools/mitmupstream --mode fake` 按规则对三条额度端点回 404/401/401
    （逐字复现 observations.md 4.3 实测的三码不一致）；
 2. 起 `zcode2api-go serve`，把 `HTTPS_PROXY` / `SSL_CERT_FILE` 指向它
    ⇒ 出站被终结在本地，可逐字节看明文；
 3. 先加一条 **apiKey** 账号（`mode != jwt` ⇒ 不触发额度查询），
    再 `PUT .../{id}` 换成 JWT —— **PUT 不触发探测**，于是起始出站数确定；
 4. `POST .../{id}/refresh` ⇒ 期望 FRESH 形态 + 恰好 3 条出站 + 同批共用 `X-Request-Id`；
 5. 再 `POST .../{id}/refresh` ⇒ 期望 CACHED 形态 + **零**出站；
 6. `POST /admin/api/accounts/refresh` ⇒ `skipped_invalid=1`、`count=0`。

为什么要「PUT 换 JWT」这一步：`POST /admin/api/accounts` 是额度查询的**主触发点**
（observations.md 4.4），直接加 JWT 账号会异步打出 3 条，起始计数就不确定了。

用法:
    python tools/e2e_quota.py --exe <zcode2api-go.exe> --mitm <mitmupstream.exe>
"""

import argparse
import io
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ADMIN_KEY = "1234"
JWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhNS1lMmUifQ.sig-000000"

# 实测（observations.md 4.2）：额度查询 GET 上的**全部**请求头，一个不多一个不少。
WANT_HEADERS = {
    "Accept", "Accept-Encoding", "Authorization", "Connection", "Content-Type",
    "Http-Referer", "User-Agent", "X-Client-Language", "X-Client-Timezone",
    "X-Device-Mid", "X-Os-Category", "X-Os-Version", "X-Platform",
    "X-Release-Channel", "X-Request-Id", "X-Title", "X-Zcode-App-Version",
}

FAILURES = []
PASSES = []


def check(name, cond, detail=""):
    if cond:
        PASSES.append(name)
        print("  PASS  %s" % name)
    else:
        FAILURES.append("%s %s" % (name, detail))
        print("  FAIL  %s  %s" % (name, detail))


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def http(method, url, body=None, token=None, timeout=30):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    # 绝不走系统代理：本机的 serve 是 127.0.0.1。
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def read_records(path):
    out = []
    if not os.path.exists(path):
        return out
    with io.open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            if rec.get("kind") != "connect":
                out.append(rec)
    return out


def wait_http(url, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        try:
            st, _ = http("GET", url, timeout=2)
            if st == 200:
                return True
        except Exception:
            time.sleep(0.2)
    return False


def wait_records(path, want, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if len(read_records(path)) >= want:
            return True
        time.sleep(0.1)
    return False


def tail(stderr_text, n=25):
    lines = (stderr_text or "").strip().splitlines()
    return "\n".join(lines[-n:])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", required=True, help="zcode2api-go.exe")
    ap.add_argument("--mitm", required=True, help="mitmupstream.exe")
    ap.add_argument("--keep", action="store_true", help="保留临时目录（排查用）")
    args = ap.parse_args()

    work = tempfile.mkdtemp(prefix="a53verify-")
    data_dir = os.path.join(work, "data")
    os.makedirs(data_dir, exist_ok=True)
    ca_path = os.path.join(work, "ca.pem")
    log_path = os.path.join(work, "out.jsonl")
    rules_path = os.path.join(work, "rules.json")

    # 三条端点的实测响应码（逐字复现 observations.md 4.3）。
    rules = [
        {"host": "zcode.z.ai", "method": "GET", "path_suffix": "/zcode-plan/usage",
         "status": 404, "headers": {"Content-Type": "text/plain; charset=utf-8"},
         "body": "404 page not found\n"},
        {"host": "zcode.z.ai", "method": "GET", "path_suffix": "/zcode-plan/billing/current",
         "status": 401},
        {"host": "zcode.z.ai", "method": "GET", "path_suffix": "/zcode-plan/billing/balance",
         "status": 401},
    ]
    with io.open(rules_path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(rules, f, ensure_ascii=False)

    mitm_port = free_port()
    api_port = free_port()

    mitm = subprocess.Popen(
        [args.mitm, "--mode", "fake", "--rules", rules_path,
         "--ca-out", ca_path, "--log", log_path,
         "--listen", "127.0.0.1:%d" % mitm_port],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    srv = None
    try:
        # 等 CA 写出来 + 代理在监听（LISTEN 行在 stdout）。
        end = time.time() + 20
        while time.time() < end and not os.path.exists(ca_path):
            if mitm.poll() is not None:
                raise SystemExit("mitmupstream 提前退出:\n" + tail(mitm.stderr.read().decode("utf-8", "replace")))
            time.sleep(0.1)
        if not os.path.exists(ca_path):
            raise SystemExit("等不到 CA 证书")

        env = dict(os.environ)
        proxy = "http://127.0.0.1:%d" % mitm_port
        env["HTTPS_PROXY"] = proxy
        env["HTTP_PROXY"] = proxy
        env["SSL_CERT_FILE"] = ca_path
        env.pop("NO_PROXY", None)
        env.pop("no_proxy", None)
        # 置 0 ⇒ 不设时间窗，每次 refresh 都真查（采样器段 3 就是这么采到的）。
        env["ZCODE_QUOTA_REFRESH_INTERVAL"] = "0"

        srv = subprocess.Popen(
            [args.exe, "serve", "--data-dir", data_dir,
             "--port", str(api_port), "--admin-key", ADMIN_KEY],
            env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

        base = "http://127.0.0.1:%d" % api_port
        if not wait_http(base + "/meta"):
            raise SystemExit("serve 未就绪:\n" + tail(srv.stderr.read().decode("utf-8", "replace") if srv.stderr else ""))

        # ── 1. 加 apiKey 账号（零出站）────────────────────────
        st, body = http("POST", base + "/admin/api/accounts",
                        {"provider": "zai", "tokens": ["sk-plain-a53"], "name": "a5-e2e"}, ADMIN_KEY)
        acc_id = json.loads(body)["ids"][0]
        check("新增 apiKey 账号 200", st == 200, "status=%d body=%s" % (st, body))

        # ── 2. PUT 换成 JWT（不触发探测）─────────────────────
        st, body = http("PUT", base + "/admin/api/accounts/" + acc_id,
                        {"token": JWT}, ADMIN_KEY)
        check("换成 JWT 成功", st == 200, "status=%d body=%s" % (st, body))
        time.sleep(0.5)  # 让可能存在的异步探测落地（PUT 按设计不探）
        before = len(read_records(log_path))
        check("PUT 不触发出站", before == 0, "出站 %d 条（want 0）" % before)

        # ── 3. 第一次 refresh：FRESH（真查上游）───────────────
        st, body = http("POST", base + "/admin/api/accounts/%s/refresh" % acc_id, None, ADMIN_KEY)
        check("refresh#1 HTTP 200", st == 200, "status=%d" % st)
        obj = json.loads(body)
        check("refresh#1 键序 ok,result,account",
              list(obj.keys()) == ["ok", "result", "account"], "keys=%s" % list(obj.keys()))
        check("refresh#1 result 是嵌套对象 {error:…}",
              isinstance(obj.get("result"), dict)
              and obj["result"].get("error") == "凭证失效，请重新授权",
              "result=%r" % obj.get("result"))
        check("refresh#1 ok=false", obj.get("ok") is False, "ok=%r" % obj.get("ok"))
        check("refresh#1 account.status=invalid",
              obj.get("account", {}).get("status") == "invalid",
              "status=%r" % obj.get("account", {}).get("status"))
        check("refresh#1 account.last_error 已写入",
              obj.get("account", {}).get("last_error") == "凭证失效，请重新授权",
              "last_error=%r" % obj.get("account", {}).get("last_error"))
        check("refresh#1 account 键序 22 键",
              list(obj.get("account", {}).keys())[:4] == ["id", "name", "provider", "mode"]
              and len(obj.get("account", {})) == 22,
              "keys=%s" % list(obj.get("account", {}).keys()))

        if not wait_records(log_path, before + 3):
            check("refresh#1 触发 3 条出站", False, "只看到 %d 条" % len(read_records(log_path)))
        else:
            check("refresh#1 触发 3 条出站", True)

        batch = read_records(log_path)[before:before + 3]
        paths = sorted(r["path"] for r in batch)
        check("三条端点各一次",
              paths == sorted(["/api/v1/zcode-plan/usage",
                               "/api/v1/zcode-plan/billing/current",
                               "/api/v1/zcode-plan/billing/balance"]),
              "paths=%s" % paths)
        rids = {r["req_headers"].get("X-Request-Id", [""])[0] for r in batch}
        check("同批共用同一个 X-Request-Id", len(rids) == 1, "rids=%s" % rids)
        auths = {r["req_headers"].get("Authorization", [""])[0] for r in batch}
        check("Authorization 用账号自己的 JWT",
              auths == {"Bearer " + JWT}, "auths=%s" % auths)

        # ── 4. 出站头逐字对照样本（observations.md 4.2）────────
        one = [r for r in batch if r["path"].endswith("/usage")][0]
        got_headers = set(one["req_headers"].keys())
        check("usage 请求头**恰好** 17 个（与样本同集合）",
              got_headers == WANT_HEADERS,
              "多=%s 少=%s" % (sorted(got_headers - WANT_HEADERS), sorted(WANT_HEADERS - got_headers)))
        g = lambda k: one["req_headers"].get(k, [""])[0]  # noqa: E731
        check("X-Os-Category 映射", g("X-Os-Category") in ("macos", "windows"),
              "X-Os-Category=%r" % g("X-Os-Category"))
        check("X-Platform = <platform>-<arch>", "-" in g("X-Platform"),
              "X-Platform=%r" % g("X-Platform"))
        check("X-Title 逐字 Z Code@electron", g("X-Title") == "Z Code@electron",
              "X-Title=%r" % g("X-Title"))
        check("X-Release-Channel=stable", g("X-Release-Channel") == "stable",
              "X-Release-Channel=%r" % g("X-Release-Channel"))
        check("X-Zcode-App-Version=3.14.4", g("X-Zcode-App-Version") == "3.14.4",
              "X-Zcode-App-Version=%r" % g("X-Zcode-App-Version"))
        check("User-Agent=ZCode/3.14.4", g("User-Agent") == "ZCode/3.14.4",
              "User-Agent=%r" % g("User-Agent"))
        check("GET 上带 Content-Type: application/json",
              g("Content-Type") == "application/json", "Content-Type=%r" % g("Content-Type"))
        # 平台与 os_version 同域（不交叉）—— observations.md 4.2 与 fingerprint-shape.json。
        plat = g("X-Platform").split("-")[0]
        osv = g("X-Os-Version")
        if plat == "darwin":
            check("os_version 与 platform 同域（darwin → 2x.y.z）",
                  osv.startswith("2"), "os_version=%r" % osv)
        elif plat == "win32":
            check("os_version 与 platform 同域（win32 → 10.0.xxxxx）",
                  osv.startswith("10.0."), "os_version=%r" % osv)
        else:
            check("X-Platform 平台取值在枚举内", False, "X-Platform=%r" % g("X-Platform"))

        # ── 5. 第二次 refresh：CACHED（零出站）────────────────
        st, body = http("POST", base + "/admin/api/accounts/%s/refresh" % acc_id, None, ADMIN_KEY)
        obj = json.loads(body)
        check("refresh#2 HTTP 200", st == 200, "status=%d" % st)
        check("refresh#2 键序 ok,message,account",
              list(obj.keys()) == ["ok", "message", "account"], "keys=%s" % list(obj.keys()))
        check("refresh#2 message 是顶层字符串",
              obj.get("message") == "凭证失效，请重新授权", "message=%r" % obj.get("message"))
        check("refresh#2 ok=false", obj.get("ok") is False, "ok=%r" % obj.get("ok"))
        time.sleep(0.6)
        after = len(read_records(log_path))
        check("refresh#2 **零出站**（invalid 后永不再查）", after == before + 3,
              "出站 %d 条（want %d）" % (after, before + 3))

        # ── 6. 全量刷新：invalid 进 skipped ───────────────────
        st, body = http("POST", base + "/admin/api/accounts/refresh", {"all": True}, ADMIN_KEY)
        obj = json.loads(body)
        check("refresh all HTTP 200", st == 200, "status=%d" % st)
        check("refresh all skipped_invalid=1、count=0",
              obj.get("skipped_invalid") == 1 and obj.get("count") == 0,
              "body=%s" % body)
        check("refresh all 键序与样本一致",
              list(obj.keys()) == ["summary", "count", "skipped_cooling", "skipped_invalid"],
              "keys=%s" % list(obj.keys()))
    finally:
        for p in (srv, mitm):
            if p is None:
                continue
            try:
                p.terminate()
                p.wait(timeout=10)
            except Exception:
                try:
                    p.kill()
                except Exception:
                    pass
        if not args.keep:
            import shutil
            shutil.rmtree(work, ignore_errors=True)
        else:
            print("工作目录保留在 %s" % work)

    print("\n%d 项通过 / %d 项失败" % (len(PASSES), len(FAILURES)))
    for f in FAILURES:
        print("  - " + f)
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
