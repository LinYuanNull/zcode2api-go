#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""A4 出站契约采样：用假 apiKey 账号触发真实出站，经本地 MITM 捕获请求/响应。

为什么可行（实测）：
  - 新增的 apiKey 账号初始 `status=active` ⇒ 网关认为「可用」⇒ 会真的转发；
  - 出站地址硬编码，但**走 `HTTPS_PROXY`**（实测代理收到 `CONNECT zcode.z.ai:443`）；
  - httpx 在 `trust_env=True` 时读 `SSL_CERT_FILE` ⇒ 设成本地 CA 即可解密。

产出：`docs/contract/outbound/<NN>-<slug>.json`（脱敏后）+ 原始 JSONL 到临时目录。

用法:
  python tools/capture_outbound.py --target-dir D:/AiWork/ZCode/zcode2api [--out DIR] [--keep]
"""
import argparse
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
MITM = os.environ.get("MITM_EXE", os.path.join(ROOT, "tools", "mitmupstream", "mitmupstream.exe"))

ADMIN_KEY = "a4-capture-admin"
GW_KEY = "sk-a4-capture-gateway"
HOST = "127.0.0.1"


def free_port():
    s = socket.socket()
    s.bind((HOST, 0))
    p = s.getsockname()[1]
    s.close()
    return p


def post(url, body, hdr=None, timeout=60):
    h = {"Content-Type": "application/json"}
    h.update(hdr or {})
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers=h, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # noqa
        return 0, str(e).encode()


def get(url, hdr=None, timeout=20):
    req = urllib.request.Request(url, headers=hdr or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # noqa
        return 0, str(e).encode()


def delete(url, body, hdr=None, timeout=20):
    h = {"Content-Type": "application/json"}
    h.update(hdr or {})
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers=h, method="DELETE")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # noqa
        return 0, str(e).encode()


def reset_account(base, ah, tag, token):
    """删掉全部账号再加一个全新的（初始 status=active ⇒ 会真的转发一次）。

    注意 token 必须**每场景唯一** —— 靶机按 token 去重，复用同一个 token 会加不进去，
    于是复用上一轮已被标 invalid 的账号，后续请求直接 503、根本不出站。
    """
    st, d = get(base + "/admin/api/accounts", ah)
    try:
        ids = [a["id"] for a in json.loads(d)["accounts"]]
    except Exception:  # noqa
        ids = []
    if ids:
        delete(base + "/admin/api/accounts", ids, ah)
    return post(base + "/admin/api/accounts",
                {"name": tag, "provider": "zai", "tokens": [token]}, ah)


def seed_accounts(base, ah, n):
    """预置 n 个**唯一 token** 的活跃账号：每次失败的转发只消耗一个。"""
    ids = []
    for i in range(n):
        st, d = post(base + "/admin/api/accounts",
                     {"name": "a4-seed-%02d" % i, "provider": "zai",
                      "tokens": ["a4-seed-token-%04d-abcdef" % i]}, ah)
        try:
            ids += json.loads(d).get("ids", [])
        except Exception:  # noqa
            pass
    return ids


def pool_size(base, ah):
    st, d = get(base + "/admin/api/status", ah)
    try:
        return json.loads(d)["quota_pool"]["zai"]
    except Exception:  # noqa
        return -1


def wait_line(proc, marker, timeout=30):
    t0 = time.time()
    while time.time() - t0 < timeout:
        line = proc.stdout.readline()
        if not line:
            if proc.poll() is not None:
                return None
            time.sleep(0.05)
            continue
        line = line.strip()
        if line.startswith(marker):
            return line[len(marker):].strip()
    return None


def wait_target_port(logf, timeout=40):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if os.path.isfile(logf):
            txt = open(logf, encoding="utf-8", errors="replace").read()
            for line in txt.splitlines():
                if "127.0.0.1:" in line and ("Uvicorn" in line or "running" in line.lower()):
                    seg = line.split("127.0.0.1:")[-1].split()[0].strip()
                    if seg.isdigit():
                        return int(seg)
        time.sleep(0.4)
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--target-dir", required=True, help="上游 zcode2api 目录（含 cli.py 与 .venv）")
    ap.add_argument("--out", default=os.path.join(ROOT, "docs", "contract", "outbound"))
    ap.add_argument("--work", default=os.path.join(os.environ.get("TEMP", "/tmp"), "zcode_a4_capture"))
    ap.add_argument("--keep", action="store_true")
    a = ap.parse_args()

    py = os.path.join(a.target_dir, ".venv", "Scripts", "python.exe")
    if not os.path.isfile(py):
        py = os.path.join(a.target_dir, ".venv", "bin", "python")
    if not os.path.isfile(MITM):
        print("找不到 MITM 可执行文件：", MITM)
        return 1

    work = a.work
    shutil.rmtree(work, ignore_errors=True)
    os.makedirs(work, exist_ok=True)
    ca = os.path.join(work, "ca.pem")
    jsonl = os.path.join(work, "outbound.jsonl")
    rules = os.path.join(work, "rules.json")
    open(rules, "w").write("[]")

    mitm_port = free_port()
    mitm = subprocess.Popen([MITM, "--mode", "capture", "--listen", "%s:%d" % (HOST, mitm_port),
                             "--ca-out", ca, "--log", jsonl, "--hosts", "zcode.z.ai,api.z.ai"],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        addr = wait_line(mitm, "LISTEN", timeout=15)
        if not addr:
            print("MITM 未启动")
            return 1
        print("MITM 监听", addr, "CA:", ca)

        tport = free_port()
        tdata = os.path.join(work, "data")
        os.makedirs(tdata, exist_ok=True)
        logf = os.path.join(work, "serve.log")
        env = dict(os.environ)
        env["ZCODE_DATA_DIR"] = tdata
        env["ZCODE_ADMIN_KEY"] = ADMIN_KEY
        env["ZCODE_HOST"] = HOST
        env["ZCODE_PORT"] = str(tport)
        env["HTTPS_PROXY"] = "http://%s" % addr
        env["HTTP_PROXY"] = env["HTTPS_PROXY"]
        env["https_proxy"] = env["HTTPS_PROXY"]
        env["http_proxy"] = env["HTTPS_PROXY"]
        env["SSL_CERT_FILE"] = ca
        env["REQUESTS_CA_BUNDLE"] = ca
        log = open(logf, "w", encoding="utf-8", errors="replace")
        tgt = subprocess.Popen([py, "-u", "cli.py", "serve"], env=env, stdout=log,
                               stderr=subprocess.STDOUT, cwd=a.target_dir)
        try:
            port = wait_target_port(logf)
            if not port:
                print("靶机未启动：")
                print(open(logf, encoding="utf-8", errors="replace").read()[-2000:])
                return 1
            base = "http://%s:%d" % (HOST, port)
            ah = {"Authorization": "Bearer " + ADMIN_KEY}
            print("靶机端口", port)

            post(base + "/admin/api/settings", {"gateway_key": GW_KEY}, ah)
            st, d = post(base + "/admin/api/accounts",
                         {"name": "a4-probe", "provider": "zai",
                          "tokens": ["a4-fake-token-000000000000"]}, ah)
            print("加账号:", st, d[:120])
            gh = {"Authorization": "Bearer " + GW_KEY}

            scenarios = [
                ("m-string", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16,
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("m-array", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16,
                  "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]}),
                ("m-system", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16, "system": "be brief",
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("m-system-arr", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16,
                  "system": [{"type": "text", "text": "be brief"}],
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("m-multiturn", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16,
                  "messages": [{"role": "user", "content": "hi"},
                               {"role": "assistant", "content": "yo"},
                               {"role": "user", "content": "again"}]}),
                ("m-tools", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16,
                  "tools": [{"name": "get_time", "description": "d",
                             "input_schema": {"type": "object", "properties": {}}}],
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("m-stream", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16, "stream": True,
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("m-extra", "/v1/messages",
                 {"model": "GLM-5.3", "max_tokens": 16, "temperature": 0.3, "top_p": 0.9,
                  "stop_sequences": ["END"], "metadata": {"user_id": "u1"},
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("c-string", "/v1/chat/completions",
                 {"model": "GLM-5.3", "max_tokens": 16,
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("c-stream", "/v1/chat/completions",
                 {"model": "GLM-5.3", "max_tokens": 16, "stream": True,
                  "messages": [{"role": "user", "content": "hi"}]}),
                ("m-unknown-model", "/v1/messages",
                 {"model": "no-such-model-xyz", "max_tokens": 16,
                  "messages": [{"role": "user", "content": "hi"}]}),
            ]
            # 预置足量活跃账号：每个场景的失败转发只消耗一个（靶机按 token 去重，
            # 所以必须唯一 token；换账号比复用更可靠）。
            seed_accounts(base, ah, len(scenarios) + 2)
            print("预置账号后 quota_pool.zai =", pool_size(base, ah))

            for idx, (name, path, body) in enumerate(scenarios):
                st, d = post(base + path, body, gh, timeout=90)
                print("  %-14s -> %s %s (pool=%d)" % (name, st, d[:80], pool_size(base, ah)))
                time.sleep(0.8)

            # /v1/models 是否随 client/configs 变化？把 configs 请求也记录下来即可对照。
            st, d = get(base + "/v1/models", gh)
            print("  /v1/models -> %s %s" % (st, d[:200]))
        finally:
            try:
                post("http://%s:%d/api/quit" % (HOST, port), {}, {})
            except Exception:  # noqa
                pass
            tgt.terminate()
            try:
                tgt.wait(timeout=8)
            except Exception:  # noqa
                tgt.kill()
    finally:
        mitm.terminate()
        try:
            mitm.wait(timeout=5)
        except Exception:  # noqa
            mitm.kill()

    if not os.path.isfile(jsonl):
        print("没有捕获到任何出站记录")
        return 1
    recs = [json.loads(l) for l in open(jsonl, encoding="utf-8") if l.strip()]
    print("捕获 %d 条出站记录，写到 %s" % (len(recs), jsonl))
    for i, r in enumerate(recs):
        print("  [%02d] %s %s %s -> %s" % (i, r.get("host"), r.get("method"),
                                           (r.get("path") or "")[:70], r.get("resp_status")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
