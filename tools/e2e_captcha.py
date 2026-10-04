#!/usr/bin/env python3
"""A6 验证码链路的真实二进制端到端验收。

它验三件事，每一件都只有「真跑一次」才能回答：

  1. **自检**（`captcha --json`）
     求解器到底会拿哪个浏览器、当前 SDK 配置是什么。

  2. **真解一次**（`captcha --solve --json`）
     自写的极简 CDP 客户端能不能在系统浏览器里跑通阿里云无痕验证、产出
     `captchaVerifyParam`。这是 A6 的核心：**不 import 上游 Node 求解器**，
     也不随包分发浏览器，只驱动系统已装的 Edge/Chrome。

  3. **管理面回执**（`serve` + `GET /admin/api/claim/captcha-config`）
     响应体必须与样本 `docs/contract/admin/15-claim-captcha-config.GET.json`
     **逐字节一致**。A6 之前这里返回的是空配置 `{"enabled":false,...}`，
     与样本不符；这条断言就是那次更正的守卫。

隔离与确定性：

  - 每次跑在 `tempfile` 里新建的数据目录，`--port` 取空闲端口。
  - 出站代理**故意指向一个没人监听的端口**：让「拉上游客户配置」立刻失败，
    于是必定走「回落静态默认值」这条分支 —— 样本里的取值与默认值本来就相同，
    所以断言与「上游可达」时完全一样，但省掉了 15s 超时等待，且不依赖网络。
  - 求解那一步**需要出网到 `o.alicdn.com`**（SDK 是远程加载的）与一个已装的
    浏览器。没有这些条件时用 `--skip-solve` 跳过第 2 组，其余照跑。

用法：
    python tools/e2e_captcha.py                 # 全部跑（需要浏览器 + 出网）
    python tools/e2e_captcha.py --skip-solve    # 只跑自检与面板回执
    python tools/e2e_captcha.py --exe build/zcode2api-go.exe --keep

`--exe` 省略时用 `go build` 现编到临时目录。`--keep` 保留临时目录以便翻日志。
"""
import argparse
import base64
import binascii
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
SAMPLE = os.path.join(ROOT, "docs", "contract", "admin", "15-claim-captcha-config.GET.json")

ADMIN_KEY = "e2e-captcha-key"
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


def dead_port():
    """拿一个「刚分配就释放」的端口，用来让出站立刻失败（连接被拒）。"""
    return free_port()


def http(method, url, body=None, headers=None, timeout=30):
    data = body.encode() if isinstance(body, str) else body
    hdrs = dict(headers or {})
    if data is not None:
        hdrs.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with NO_PROXY_OPENER.open(req, timeout=timeout) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)
    except Exception as e:  # 连接被拒 / 超时等
        return 0, str(e).encode(), {}


def run(args, timeout, env=None):
    """跑一个子进程，返回 (returncode, stdout, stderr)。"""
    cp = subprocess.run(args, capture_output=True, timeout=timeout,
                        env=env if env is not None else dict(os.environ))
    return (cp.returncode,
            (cp.stdout or b"").decode("utf-8", "replace"),
            (cp.stderr or b"").decode("utf-8", "replace"))


def jbody(raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        return None


def sample_body():
    """样本响应体的紧凑 JSON（键序与样本一致）。"""
    with open(SAMPLE, encoding="utf-8") as f:
        doc = json.load(f)
    return doc, json.dumps(doc["response"]["body"], ensure_ascii=False,
                           separators=(",", ":"))


def decode_param(param):
    """把 captchaVerifyParam 解成 JSON 对象；解不动返回 None。

    真实取值是标准 base64（带 `=` 填充），但也容忍 URL-safe 变体与缺填充，
    免得因为一个填充字符把「解出来了」误判成「没解出来」。
    """
    raw = param.strip()
    pad = "=" * (-len(raw) % 4)
    for decoder in (base64.b64decode, base64.urlsafe_b64decode):
        try:
            text = decoder(raw + pad).decode("utf-8")
            doc = json.loads(text)
        except (binascii.Error, ValueError, UnicodeDecodeError):
            continue
        if isinstance(doc, dict):
            return doc
    return None


# ── 第 1、2 组：captcha 自检与真解 ───────────────────────────

def phase_cli(exe, workdir, timeout, skip_solve, sample):
    print("\n── 第 1 组：captcha 自检（captcha --json）──", flush=True)
    rc, out, err = run([exe, "captcha", "--json"], timeout=30)
    check("captcha 自检退出码为 0", rc == 0, f"rc={rc} stderr={err!r}")

    rep = jbody(out.encode())
    check("自检输出是合法 JSON", isinstance(rep, dict), f"stdout={out!r}")
    if not isinstance(rep, dict):
        return None

    browser = (rep.get("browser") or "").strip()
    check("探测到了系统浏览器", bool(browser) and os.path.isfile(browser),
          f"browser={browser!r}")
    check("探测到的浏览器是 Edge 或 Chrome",
          "msedge" in browser.lower() or "chrome" in browser.lower(),
          f"browser={browser!r}")

    check("SDK 配置逐字段等于样本 15-claim-captcha-config",
          rep.get("config") == sample["response"]["body"],
          f"got={rep.get('config')!r} want={sample['response']['body']!r}")

    if skip_solve:
        print("\n── 第 2 组：真解一次（--skip-solve，跳过）──", flush=True)
        return rep

    print("\n── 第 2 组：真解一次（captcha --solve --json）──", flush=True)
    rc, out, err = run([exe, "captcha", "--solve", "--json"], timeout=timeout)
    rep2 = jbody(out.encode()) or {}
    check("求解退出码为 0", rc == 0,
          f"rc={rc} stdout={out!r} stderr={err!r}")
    param = (rep2.get("captcha_verify_param") or "").strip()
    check("产出了 captchaVerifyParam", bool(param),
          f"error={rep2.get('error')!r} stderr={err!r}")
    if not param:
        return rep

    doc = decode_param(param)
    check("凭据是 base64 包着的 JSON 对象", doc is not None,
          f"param({len(param)} 字符)={param[:120]!r}")
    if doc is None:
        return rep
    check("凭据里的 sceneId 与配置一致",
          doc.get("sceneId") == rep.get("config", {}).get("scene_id"),
          f"sceneId={doc.get('sceneId')!r} config={rep.get('config')!r}")
    check("凭据含非空 certifyId", bool(doc.get("certifyId")), f"certifyId={doc.get('certifyId')!r}")
    check("凭据含非空 securityToken", bool(doc.get("securityToken")),
          f"securityToken 长度={len(doc.get('securityToken') or '')}")
    return rep


# ── 第 3 组：管理面回执 ──────────────────────────────────────

def phase_admin(exe, workdir, want_body, upstream_offline):
    print("\n── 第 3 组：管理面回执（serve + captcha-config）──", flush=True)
    data_dir = os.path.join(workdir, "data-admin")
    os.makedirs(data_dir, exist_ok=True)
    port = free_port()

    env = dict(os.environ)
    env["ZCODE_ADMIN_KEY"] = ADMIN_KEY
    if upstream_offline:
        # 指向没人监听的端口 ⇒ 出站立刻失败 ⇒ 必定走「回落默认值」。
        env["HTTP_PROXY"] = env["HTTPS_PROXY"] = f"http://127.0.0.1:{upstream_offline}"
        env["NO_PROXY"] = env["no_proxy"] = ""

    proc = subprocess.Popen([exe, "serve", "--port", str(port), "--data-dir", data_dir],
                            env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    try:
        deadline = time.time() + 20
        ready = False
        while time.time() < deadline:
            st, _, _ = http("GET", base + "/meta", timeout=2)
            if st == 200:
                ready = True
                break
            if proc.poll() is not None:
                break
            time.sleep(0.2)
        check("serve 启动并就绪（/meta 200）", ready,
              f"rc={proc.poll()}")
        if not ready:
            return

        st, raw, _ = http("GET", base + "/admin/api/claim/captcha-config", timeout=40)
        check("不带凭证时 captcha-config 为 401", st == 401, f"status={st} body={raw[:200]!r}")

        st, raw, hdrs = http("GET", base + "/admin/api/claim/captcha-config", timeout=40,
                             headers={"Authorization": "Bearer " + ADMIN_KEY})
        check("带凭证时 captcha-config 为 200", st == 200, f"status={st} body={raw[:200]!r}")
        got = raw.decode("utf-8", "replace")
        check("captcha-config 响应体与样本 15-* 逐字节一致", got == want_body,
              f"\n        got  {got}\n        want {want_body}")
        ctype = hdrs.get("Content-Type", "")
        check("Content-Type 为 application/json", ctype.startswith("application/json"),
              f"Content-Type={ctype!r}")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()


# ── main ────────────────────────────────────────────────────

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", default="", help="已编好的 zcode2api-go.exe（省略则 go build）")
    ap.add_argument("--skip-solve", action="store_true",
                    help="跳过真解（没有浏览器 / 出不了网时用）")
    ap.add_argument("--timeout", type=int, default=180, help="单次求解的等待秒数")
    ap.add_argument("--keep", action="store_true", help="保留临时目录")
    args = ap.parse_args()

    workdir = tempfile.mkdtemp(prefix="e2e_captcha_")
    print(f"临时目录: {workdir}", flush=True)
    try:
        exe = args.exe
        if not exe:
            exe = os.path.join(workdir, "zcode2api-go.exe")
            print("go build ./cmd/zcode2api-go …", flush=True)
            subprocess.run(["go", "build", "-o", exe, "./cmd/zcode2api-go"],
                           cwd=ROOT, check=True)
        check("被测二进制存在", os.path.isfile(exe), exe)

        sample, want_body = sample_body()

        offline = dead_port()
        print(f"出站代理指向空闲端口 {offline} ⇒ 拉上游必定立刻失败（走回落分支）", flush=True)

        phase_cli(exe, workdir, args.timeout, args.skip_solve, sample)
        phase_admin(exe, workdir, want_body, offline)
    finally:
        if args.keep:
            print(f"\n保留临时目录: {workdir}")
        else:
            shutil.rmtree(workdir, ignore_errors=True)

    ok = sum(1 for _, p in results if p)
    total = len(results)
    print(f"\n=== {ok}/{total} 项通过 ===")
    if ok != total:
        for name, passed in results:
            if not passed:
                print(f"  失败: {name}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
