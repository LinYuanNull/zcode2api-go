#!/usr/bin/env python3
"""A5-4 领取链路的真实二进制端到端验收。

与 `internal/claim` 的单测不同，这里跑的是**真 exe**：起 `serve`，把它的出站
指向本地 MITM（`tools/mitmupstream` 的 fake 模式），于是「claim 链路到底出不出站」
这件事可以被**数出来**，而不是靠读代码相信。

两个沙盒：

  沙盒 1（凭据失效分支，有旁录样本）
    加一个 JWT 账号 → 额度探测（mitm 回 404/401/401）把它打成 `invalid`
    → 三条 claim 路由的响应必须与 A5 旁录
    `docs/contract/outbound-admin/fixtures/admin-responses.json` **逐字节一致**
    （只把旁录里的账号 id/name 换成真实生成的），
    并且**之后零出站**（观测到的实测事实：claim 只读已存状态）。

  沙盒 2（`active` 分支，未采样）
    额度探测让 mitm 回 200 ⇒ `quota` 报「成功体未采样」且**不改账号状态**
    ⇒ 账号留在 `active` ⇒ 三条 claim 路由必须 **501**，说明里点明账号与状态。

用法：
    python tools/e2e_claim.py --exe build/zcode2api-go.exe --mitm build/mitmupstream.exe
两个参数都可省略，省略时用 `go build` 现编到临时目录。`--keep` 保留临时目录。
"""
import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
FIXTURE = os.path.join(ROOT, "docs", "contract", "outbound-admin", "fixtures", "admin-responses.json")

ADMIN_KEY = "e2e-claim-key"
# 恰好两个点 ⇒ 判为 JWT（`adminapi.detectMode`）。
FAKE_JWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJlMmUtY2xhaW0ifQ.c2ln"
QUOTA_PATHS = sorted(["/api/v1/zcode-plan/usage",
                      "/api/v1/zcode-plan/billing/current",
                      "/api/v1/zcode-plan/billing/balance"])

results = []
NO_PROXY_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def check(name, cond, detail=""):
    results.append((name, bool(cond)))
    print(f"[{'PASS' if cond else 'FAIL'}] {name}" +
          (f"\n        {detail}" if not cond and detail else ""), flush=True)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def http(method, url, body=None, headers=None, timeout=30):
    data = body.encode() if isinstance(body, str) else body
    hdrs = dict(headers or {})
    if data is not None:
        hdrs.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with NO_PROXY_OPENER.open(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # 连接被拒 / 超时等
        return 0, str(e).encode()


def api(method, base, path, body=None, timeout=30):
    return http(method, base + path, body,
                {"Authorization": "Bearer " + ADMIN_KEY}, timeout)


def jbody(raw):
    """把响应体解成 JSON；解不动就返回 None。"""
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        return None


# ── MITM（fake 模式）─────────────────────────────────────────

def start_mitm(mitm_exe, workdir, tag, rules):
    """起一个 fake MITM，返回 (proc, port, log_path)。"""
    rules_path = os.path.join(workdir, f"rules-{tag}.json")
    with open(rules_path, "w", encoding="utf-8") as f:
        json.dump(rules, f, ensure_ascii=False)
    ca_path = os.path.join(workdir, f"ca-{tag}.pem")
    log_path = os.path.join(workdir, f"out-{tag}.jsonl")
    port = free_port()
    proc = subprocess.Popen(
        [mitm_exe, "--mode", "fake", "--listen", f"127.0.0.1:{port}",
         "--rules", rules_path, "--ca-out", ca_path, "--log", log_path,
         "--log-connects"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    # 等端口真的能连（mitm 起来很快，但别竞态）。
    deadline = time.time() + 10
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return proc, port, log_path, ca_path
        except OSError:
            if proc.poll() is not None:
                raise RuntimeError(f"mitm({tag}) 启动失败，退出码 {proc.returncode}")
            time.sleep(0.1)
    raise RuntimeError(f"mitm({tag}) 端口 {port} 一直连不上")


def recorded_requests(log_path):
    """读 MITM 日志，返回 (HTTP 请求记录, CONNECT 记录)。"""
    reqs, conns = [], []
    if not os.path.isfile(log_path):
        return reqs, conns
    with open(log_path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except Exception:
                continue
            (conns if rec.get("kind") == "connect" else reqs).append(rec)
    return reqs, conns


# ── 沙盒 ────────────────────────────────────────────────────

def start_serve(exe, workdir, tag, proxy_port):
    data_dir = os.path.join(workdir, f"data-{tag}")
    os.makedirs(data_dir, exist_ok=True)
    port = free_port()
    env = dict(os.environ)
    env.update({
        "HTTPS_PROXY": f"http://127.0.0.1:{proxy_port}",
        "HTTP_PROXY": f"http://127.0.0.1:{proxy_port}",
        "SSL_CERT_FILE": os.path.join(workdir, f"ca-{tag}.pem"),
        "NO_PROXY": "", "no_proxy": "",
        "ZCODE_QUOTA_REFRESH_INTERVAL": "0",
        "ZCODE_ADMIN_KEY": ADMIN_KEY,
    })
    proc = subprocess.Popen([exe, "serve", "--port", str(port), "--data-dir", data_dir],
                            env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    deadline = time.time() + 20
    while time.time() < deadline:
        st, raw = http("GET", base + "/meta", timeout=2)
        if st == 200:
            return proc, base, data_dir
        if proc.poll() is not None:
            raise RuntimeError(f"serve({tag}) 启动失败，退出码 {proc.returncode}")
        time.sleep(0.2)
    raise RuntimeError(f"serve({tag}) 一直没就绪")


def add_jwt_account(base, name):
    st, raw = api("POST", base, "/admin/api/accounts",
                  json.dumps({"provider": "zai", "tokens": [FAKE_JWT], "name": name}))
    doc = jbody(raw) or {}
    ids = doc.get("ids") or []
    return st, (ids[0] if ids else None), raw


def account_of(base, acc_id):
    st, raw = api("GET", base, "/admin/api/accounts")
    doc = jbody(raw) or {}
    for a in doc.get("accounts") or []:
        if a.get("id") == acc_id:
            return a
    return None


def wait_status(base, acc_id, want, timeout=25):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = account_of(base, acc_id)
        if last and last.get("status") == want:
            return last
        time.sleep(0.3)
    return last


def load_fixture():
    with open(FIXTURE, encoding="utf-8") as f:
        doc = json.load(f)
    return {r["label"]: r for r in doc["records"]}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", default="")
    ap.add_argument("--mitm", default="")
    ap.add_argument("--keep", action="store_true")
    args = ap.parse_args()

    workdir = tempfile.mkdtemp(prefix="e2e_claim_")
    procs = []
    try:
        exe, mitm = args.exe, args.mitm
        if not exe:
            exe = os.path.join(workdir, "zcode2api-go.exe")
            subprocess.run(["go", "build", "-o", exe, "./cmd/zcode2api-go"],
                           cwd=ROOT, check=True)
        if not mitm:
            mitm = os.path.join(workdir, "mitmupstream.exe")
            subprocess.run(["go", "build", "-o", mitm, "./tools/mitmupstream"],
                           cwd=ROOT, check=True)

        fx = load_fixture()
        for label in ("claim/preview", "claim", "claim/manual"):
            check(f"旁录夹具含 {label} 记录", label in fx)

        # ── 沙盒 1：凭据失效分支（有样本） ────────────────────
        # 规则按上游真实行为：usage→404（text/plain）、billing/*→401 空体。
        rules_fail = [
            {"host": "zcode.z.ai", "path_suffix": "/zcode-plan/usage", "status": 404,
             "headers": {"Content-Type": "text/plain"}, "body": "404 page not found"},
            {"host": "zcode.z.ai", "path_suffix": "/zcode-plan/billing/current",
             "status": 401, "headers": {"Content-Type": "text/plain"}, "body": ""},
            {"host": "zcode.z.ai", "path_suffix": "/zcode-plan/billing/balance",
             "status": 401, "headers": {"Content-Type": "text/plain"}, "body": ""},
        ]
        mt1, proxy1, log1, _ = start_mitm(mitm, workdir, "fail", rules_fail)
        procs.append(mt1)
        srv1, base1, data1 = start_serve(exe, workdir, "fail", proxy1)
        procs.append(srv1)

        st, raw = http("GET", base1 + "/meta")
        check("serve 起来且 /meta 可用", st == 200, f"{st} {raw[:120]}")
        check("/meta 只有一个键 version",
              (jbody(raw) or {}).keys() == {"version"}, raw[:120])

        # 无鉴权 ⇒ 401（claim 路由也要鉴权，别以为它免鉴权）
        st, _ = http("GET", base1 + "/admin/api/claim/preview")
        check("claim/preview 无凭证 → 401", st == 401, str(st))

        st, acc_id, raw = add_jwt_account(base1, "a5-claim-probe")
        check("新增 JWT 账号", st == 200 and acc_id, f"{st} {raw[:160]}")
        if not acc_id:
            return 1

        acc = wait_status(base1, acc_id, "invalid")
        check("额度探测把账号打成 invalid（mitm 回 404/401/401）",
              (acc or {}).get("status") == "invalid", json.dumps(acc, ensure_ascii=False)[:200])

        # 探测过后：出站应当恰好是那三条，且没有未匹配到规则的 599。
        time.sleep(0.5)  # 等 MITM 把日志追平（它先写响应、后落日志）
        reqs, _ = recorded_requests(log1)
        check("探测出站恰好 3 条", len(reqs) == 3,
              f"{[r.get('path') for r in reqs]}")
        check("探测命中三条额度端点",
              sorted(r.get("path") for r in reqs) == QUOTA_PATHS,
              str([r.get("path") for r in reqs]))
        check("没有未匹配规则的 599",
              not any(r.get("resp_status") == 599 for r in reqs),
              str([r.get("resp_status") for r in reqs]))

        baseline = len(reqs)
        name = (acc or {}).get("name") or ""

        def expect_recorded(label, method, path, body, want_body):
            """断言响应体与旁录逐字节一致（id/name 换成真实的）。"""
            st, raw = api(method, base1, path, body)
            got = raw.decode("utf-8", "replace").strip()
            want = (want_body
                    .replace('"a5-jwt-probe-ef234ed1"', f'"{acc_id}"')
                    .replace('"a5-jwt-probe"', f'"{name}"'))
            check(f"{label} 与旁录逐字节一致", st == 200 and got == want,
                  f"status={st}\n        got  {got}\n        want {want}")

        expect_recorded("claim/preview", "GET", "/admin/api/claim/preview", None,
                        fx["claim/preview"]["body_text"])
        expect_recorded("claim", "POST", "/admin/api/claim", "{}",
                        fx["claim"]["body_text"])
        expect_recorded("claim/manual", "POST", "/admin/api/claim/manual",
                        json.dumps({"account_id": acc_id,
                                    "captcha_verify_param": "e2e-param"}),
                        fx["claim/manual"]["body_text"])

        # 手动领取的两条校验分支（旁录之外，另有 16-* 样本）
        st, raw = api("POST", base1, "/admin/api/claim/manual",
                      json.dumps({"captcha_verify_param": "p"}))
        check("claim/manual 缺 account_id → 400 {detail:缺少 account_id}",
              st == 400 and raw.decode() == '{"detail":"缺少 account_id"}',
              f"{st} {raw[:120]}")
        st, raw = api("POST", base1, "/admin/api/claim/manual",
                      json.dumps({"account_id": "ghost", "captcha_verify_param": "p"}))
        check("claim/manual 非 JWT / 不存在 → 404 {detail:JWT 账号不存在}",
              st == 404 and raw.decode() == '{"detail":"JWT 账号不存在"}',
              f"{st} {raw[:120]}")

        # account_ids 过滤（推论语义：空 = 全部；给了就只领那几个）
        st, raw = api("POST", base1, "/admin/api/claim", json.dumps({"account_ids": ["ghost"]}))
        check("[推论] claim 的 account_ids 是过滤器（不存在的 id ⇒ 空 outcomes）",
              st == 200 and raw.decode() == '{"outcomes":[],"summary":{"ok":0,"fail":0}}',
              f"{st} {raw[:160]}")

        # captcha-config：诚实降级（未接 A6 ⇒ 空配置），不是 501 也不是假参数
        st, raw = api("GET", base1, "/admin/api/claim/captcha-config")
        cfg = jbody(raw) or {}
        check("claim/captcha-config 键序为样本的 enabled,scene_id,region,prefix",
              st == 200 and list(cfg.keys()) == ["enabled", "scene_id", "region", "prefix"],
              f"{st} {raw[:160]}")
        check("未接 A6 时 captcha-config 是空配置（enabled=false）",
              cfg.get("enabled") is False, raw[:160])

        # ★ 本轮最核心的实测事实：claim 三条链路**零出站**。
        time.sleep(0.5)
        reqs, conns = recorded_requests(log1)
        check("claim 链路零出站（只读已存状态）", len(reqs) == baseline,
              f"探测后 {baseline} 条，claim 之后 {len(reqs)} 条："
              f"{[r.get('path') for r in reqs[baseline:]]}")

        # ── `claim` 子命令：同一个数据目录，直接跑 CLI ─────────
        # 先放掉库文件再跑（Windows 上 SQLite 的文件锁比较硬）。
        srv1.kill()
        srv1.wait()
        time.sleep(0.5)
        cp = subprocess.run([exe, "claim", "--data-dir", data1],
                            capture_output=True, env=dict(os.environ))
        out = (cp.stdout or b"").decode("utf-8", "replace")
        err = (cp.stderr or b"").decode("utf-8", "replace")
        check("claim 子命令列出失败明细（账号名 + 凭据失效文案）",
              name in out and "凭证失效，请重新授权" in out, f"stdout={out!r}")
        check("claim 子命令汇总「成功 0 / 失败 1」",
              "成功 0 / 失败 1" in out, f"stdout={out!r}")
        check("claim 子命令在没有账号领取成功时**非零退出**",
              cp.returncode != 0, f"rc={cp.returncode} stderr={err!r}")

        # ── 沙盒 2：active 分支（未采样 ⇒ 必须 501） ──────────
        rules_ok = [
            {"host": "zcode.z.ai", "path_suffix": p, "status": 200,
             "headers": {"Content-Type": "application/json"}, "body": "{}"}
            for p in ("/zcode-plan/usage", "/zcode-plan/billing/current",
                      "/zcode-plan/billing/balance")
        ]
        mt2, proxy2, log2, _ = start_mitm(mitm, workdir, "ok", rules_ok)
        procs.append(mt2)
        srv2, base2, _ = start_serve(exe, workdir, "ok", proxy2)
        procs.append(srv2)

        st, acc2, raw = add_jwt_account(base2, "a5-active-probe")
        check("沙盒 2：新增 JWT 账号", st == 200 and acc2, f"{st} {raw[:160]}")
        if not acc2:
            return 1
        # 200 成功体未采样 ⇒ quota 报错且**不改账号状态** ⇒ 账号留在 active。
        time.sleep(2.0)
        accd = account_of(base2, acc2) or {}
        check("200 成功体未采样时账号保持 active（探测失败不改状态）",
              accd.get("status") == "active", json.dumps(accd, ensure_ascii=False)[:200])
        st, raw = api("POST", base2, f"/admin/api/accounts/{acc2}/refresh", "{}")
        check("active 账号 refresh → 501（额度成功体未采样）",
              st == 501, f"{st} {raw[:160]}")

        # MITM 是**先写响应、后落日志**，客户端可能在日志落盘前就返回了 ——
        # 取基线前先让日志追平，否则会把上一批的尾巴算成 claim 的出站。
        time.sleep(0.5)
        baseline2 = len(recorded_requests(log2)[0])

        for label, method, path, body in (
                ("claim/preview", "GET", "/admin/api/claim/preview", None),
                ("claim", "POST", "/admin/api/claim", "{}"),
                ("claim/manual", "POST", "/admin/api/claim/manual",
                 json.dumps({"account_id": acc2, "captcha_verify_param": "p"}))):
            st, raw = api(method, base2, path, body)
            text = raw.decode("utf-8", "replace")
            check(f"active 账号 {label} → 501", st == 501, f"{st} {text[:160]}")
            check(f"active 账号 {label} 的说明点明账号与状态",
                  acc2 in text and "status=active" in text, text[:220])

        time.sleep(0.5)
        reqs2, _ = recorded_requests(log2)
        check("沙盒 2 只有额度探测出站（claim 零出站）", len(reqs2) == baseline2,
              f"基线 {baseline2} 条，之后 {len(reqs2)} 条："
              f"{[r.get('path') for r in reqs2[baseline2:]]}")
        check("沙盒 2 的出站全部落在三条额度端点上",
              reqs2 and all(r.get("path") in QUOTA_PATHS for r in reqs2),
              str([r.get("path") for r in reqs2]))
    finally:
        for p in procs:
            try:
                if p.poll() is None:
                    p.kill()
            except Exception:
                pass
        if args.keep:
            print(f"\n临时目录保留在 {workdir}")
        else:
            shutil.rmtree(workdir, ignore_errors=True)

    passed = sum(1 for _, ok in results if ok)
    print(f"\n{'=' * 46}\n{passed}/{len(results)} 通过")
    failed = [n for n, ok in results if not ok]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
