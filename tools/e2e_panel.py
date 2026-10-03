#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""A3 端到端：**上游自带的 frontend/ 面板**直接接本实现的后端，在真实浏览器里跑一遍。

为什么这条必须存在
------------------
`internal/contract` 的回放测试证明的是「逐字节响应与样本一致」，但样本是**我采的**，
它证明不了「面板真的能用」——面板是这份契约的**唯一真实消费者**：它会读
`accounts.stats`、把 `token_masked` 直接塞进 DOM、用 `admin_key_is_default` 决定
要不要弹告警、用 `quota_refresh_interval` 回填输入框。字段少一个、名字错一个、
空容器给成 `null`，回放测试可能照样绿，面板却会白屏或显示 0。

所以这里不查接口，**查渲染后的 DOM**：登录 → 统计卡 → 新增 → 编辑 → 启停 →
刷新 → 导出落盘 → 导入 → 删除 → 设置读写 → 监控清空，全程断言浏览器里看到的文本。

A3 / A5 边界
------------
需要打上游的分支（JWT 账号额度刷新、领取、OAuth 登录）属 A5/A6，A3 显式报错。
这类分支**不跳过**：改为断言面板上确实出现了「失败」回执 —— 把「我们还不支持」
也钉成可观测行为，而不是让测试在它上面含糊地变绿。

上游前端**不进本仓库**（许可纪律）：用 `--panel-dir`（或 `ZCODE_PANEL_DIR`）指过来。
本机常见位置会被自动探测，探不到就明确报错，不猜。

用法:
    python tools/e2e_panel.py [--panel-dir DIR] [--exe PATH] [--keep] [--port N]

只用标准库：CDP 的 WebSocket 客户端是手写的（socket + base64 + struct），
不为一个验证脚本引入 playwright/puppeteer 这类重依赖。
"""
import argparse
import base64
import json
import os
import random
import re
import shutil
import socket
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
HOME = os.path.join(HERE, "e2e_home")          # 固定隔离目录（跑前清空，便于排查）
LOG = os.path.join(HOME, "serve.log")
DL = os.path.join(HOME, "downloads")

ADMIN_KEY = "e2e-admin-key"
GATEWAY_KEY = "sk-e2e-gateway-abcdef123456"
# zai + 恰好 2 个点 ⇒ jwt（判据见 internal/adminapi.detectMode）。
JWT_TOKEN = "eyJhbGciOiJI.eyJzdWIiOiJlMmUifQ.sigzzz"
API_TOKEN = "sk-e2e-plain-api-key-0001"

sys.stdout.reconfigure(encoding="utf-8")
results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name
          + ("" if ok else "  ← " + str(detail)[:300]), flush=True)


def skip(name, why):
    results.append((name, None, why))
    print("[SKIP] " + name + "  ← " + why, flush=True)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def find_edge():
    for p in (os.environ.get("ZCODE_EDGE") or os.environ.get("EDGE"),
              r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe",
              r"C:\Program Files\Microsoft\Edge\Application\msedge.exe",
              os.path.expandvars(r"%LOCALAPPDATA%\Microsoft\Edge\Application\msedge.exe")):
        if p and os.path.exists(p):
            return p
    return None


def find_panel_dir(explicit):
    cands = [explicit, os.environ.get("ZCODE_PANEL_DIR"), os.environ.get("ZCODE_FRONTEND_DIR")]
    # 便利探测：工作区里上游仓库的常见位置（找不到就报错，不猜）。
    parent = os.path.dirname(ROOT)
    for rel in ("ZCode/zcode2api/frontend", "zcode2api/frontend"):
        cands.append(os.path.join(parent, *rel.split("/")))
    for c in cands:
        if c and os.path.isfile(os.path.join(c, "admin", "login.html")):
            return os.path.abspath(c)
    return None


def wait_http(url, timeout=25):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=3) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()
        except Exception:
            time.sleep(0.3)
    return None, b""


def wait_json(url, timeout=25):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=3) as r:
                return json.loads(r.read())
        except Exception:
            time.sleep(0.3)
    return None


# ── 最小 CDP 客户端（WebSocket 帧读写只用标准库）──────────────────

class WS:
    def __init__(self, url):
        m = re.match(r"ws://([^:/]+):(\d+)(/.*)", url)
        host, port, path = m.group(1), int(m.group(2)), m.group(3)
        self.sock = socket.create_connection((host, port), timeout=30)
        key = base64.b64encode(bytes(random.getrandbits(8) for _ in range(16))).decode()
        req = (f"GET {path} HTTP/1.1\r\nHost: {host}:{port}\r\n"
               "Upgrade: websocket\r\nConnection: Upgrade\r\n"
               f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.sock.sendall(req.encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.sock.recv(4096)
        if b"101" not in buf.split(b"\r\n")[0]:
            raise RuntimeError("WebSocket 握手失败: " + buf[:200].decode("latin1"))
        self.buf = buf.split(b"\r\n\r\n", 1)[1]
        self.mid = 0

    def _recv_exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(max(4096, n - len(self.buf)))
            if not chunk:
                raise RuntimeError("连接关闭")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def send(self, text):
        payload = text.encode()
        header = bytearray([0x81])
        n = len(payload)
        if n < 126:
            header.append(0x80 | n)
        elif n < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", n)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", n)
        mask = bytes(random.getrandbits(8) for _ in range(4))
        header += mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(bytes(header) + masked)

    def recv(self):
        b0, b1 = self._recv_exact(2)
        opcode = b0 & 0x0F
        length = b1 & 0x7F
        if length == 126:
            length = struct.unpack(">H", self._recv_exact(2))[0]
        elif length == 127:
            length = struct.unpack(">Q", self._recv_exact(8))[0]
        data = self._recv_exact(length)
        if opcode == 0x8:
            raise RuntimeError("服务端关闭连接")
        if opcode == 0x9:
            return None
        return data.decode("utf-8", "replace")

    def call(self, method, params=None, timeout=40):
        self.mid += 1
        mid = self.mid
        self.send(json.dumps({"id": mid, "method": method, "params": params or {}}))
        end = time.time() + timeout
        while time.time() < end:
            msg = self.recv()
            if not msg:
                continue
            obj = json.loads(msg)
            if obj.get("id") == mid:
                if "error" in obj:
                    raise RuntimeError(f"{method} 出错：{obj['error']}")
                return obj.get("result", {})
        raise TimeoutError(method)

    def close(self):
        try:
            self.sock.close()
        except Exception:
            pass


def wait_file(folder, suffix, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        try:
            for f in os.listdir(folder):
                if f.endswith(suffix) and not f.endswith(".crdownload"):
                    p = os.path.join(folder, f)
                    if os.path.getsize(p) > 0:
                        return p
        except FileNotFoundError:
            pass
        time.sleep(0.3)
    return None


# ── 主流程 ──────────────────────────────────────────────────

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--panel-dir", default="", help="上游 frontend/ 目录（或 ZCODE_PANEL_DIR）")
    ap.add_argument("--exe", default=os.environ.get("ZCODE_GO_EXE", ""), help="被测可执行文件")
    ap.add_argument("--port", type=int, default=0, help="固定端口（默认随机）")
    ap.add_argument("--keep", action="store_true", help="结束后保留隔离目录与进程日志")
    args = ap.parse_args()

    panel = find_panel_dir(args.panel_dir)
    if not panel:
        print("未找到上游面板目录。请用 --panel-dir 或 ZCODE_PANEL_DIR 指向 "
              "上游 frontend/（其下应有 admin/login.html）。", file=sys.stderr)
        return 2
    edge = find_edge()
    if not edge:
        print("未找到 Edge。请用 EDGE 环境变量指定 msedge.exe 路径。", file=sys.stderr)
        return 2

    # 固定隔离目录：只清「会影响断言」的三样，整个 HOME 递归删会被批量删除保护拦下。
    for rel in ("data", "downloads", "serve.log"):
        p = os.path.join(HOME, *rel.split("/"))
        if os.path.isdir(p):
            shutil.rmtree(p, ignore_errors=True)
        elif os.path.isfile(p):
            try:
                os.remove(p)
            except OSError:
                pass
    os.makedirs(DL, exist_ok=True)

    exe = args.exe
    if not exe:
        exe = os.path.join(HOME, "zcode2api-go.exe")
        os.makedirs(HOME, exist_ok=True)
        print("构建被测可执行文件…", flush=True)
        r = subprocess.run(["go", "build", "-o", exe, "./cmd/zcode2api-go"],
                           cwd=ROOT, capture_output=True, text=True)
        if r.returncode != 0:
            print("构建失败：\n" + r.stderr, file=sys.stderr)
            return 2
    if not os.path.isfile(exe):
        print("可执行文件不存在：" + exe, file=sys.stderr)
        return 2

    port = args.port or free_port()
    cdp_port = free_port()
    base = f"http://127.0.0.1:{port}"
    data_dir = os.path.join(HOME, "data")

    logf = open(LOG, "w", encoding="utf-8", errors="replace")
    srv = subprocess.Popen(
        [exe, "serve", "--host", "127.0.0.1", "--port", str(port),
         "--data-dir", data_dir, "--panel-dir", panel,
         "--admin-key", ADMIN_KEY, "--gateway-key", GATEWAY_KEY],
        stdout=logf, stderr=subprocess.STDOUT)
    edge_proc = None
    ws = None
    try:
        st, body = wait_http(base + "/meta")
        check("服务已监听且 /meta 可读", st == 200 and b"version" in body, f"{st} {body[:120]!r}")
        if st != 200:
            print("服务未就绪，日志：\n" + open(LOG, encoding="utf-8", errors="replace").read()[-1500:])
            return 1

        # 静态出口与重定向：面板靠 /static/* 拿 css/js，靠 / 跳到登录页。
        st, _ = wait_http(base + "/admin/login")
        check("/admin/login 返回 200", st == 200, st)
        with urllib.request.urlopen(base + "/static/js/auth.js", timeout=5) as r:
            check("静态 JS 可达且 Content-Type 正确",
                  r.status == 200 and "javascript" in (r.headers.get("Content-Type") or ""),
                  r.headers.get("Content-Type"))

        edge_proc = subprocess.Popen([
            edge, "--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars",
            "--force-device-scale-factor=1", "--window-size=1500,1100",
            f"--remote-debugging-port={cdp_port}",
            f"--user-data-dir={os.path.join(HOME, 'edge-profile')}",
            base + "/",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        info = wait_json(f"http://127.0.0.1:{cdp_port}/json", timeout=40)
        page = next((t for t in (info or []) if t.get("type") == "page"), None)
        if not page:
            print("未找到浏览器页面目标")
            return 1
        ws = WS(page["webSocketDebuggerUrl"])
        ws.call("Runtime.enable")
        ws.call("Page.enable")
        try:
            ws.call("Network.enable")
            # 面板引了 cdn.jsdelivr.net 的字体 CSS；离线时会让 load 事件一直悬着。
            ws.call("Network.setBlockedURLs", {"urls": ["*cdn.jsdelivr.net*"]})
        except Exception:
            pass

        def js(expr, timeout=40):
            r = ws.call("Runtime.evaluate",
                        {"expression": expr, "returnByValue": True, "awaitPromise": True},
                        timeout=timeout)
            if "exceptionDetails" in r:
                return {"__err": json.dumps(r["exceptionDetails"], ensure_ascii=False)[:300]}
            return r.get("result", {}).get("value")

        def wait_true(expr, timeout=25, interval=0.3):
            end = time.time() + timeout
            last = None
            while time.time() < end:
                last = js(expr)
                if last:
                    return last
                time.sleep(interval)
            return last

        def toast():
            """读当前全部 toast 文本。

            上游 toast 是**动态追加**的：容器是 `#toast-container`，每条是
            `.toast > .toast-content`，3 秒后自删 —— 页面里**没有** `#toast` 这个 id。
            早期版本按 `#toast` 读，永远拿到空串，于是所有 toast 断言「静默失败」
            （等待超时 → 文案为空 → 判为不符），看起来像后端没返回，实则是取错了元素。
            """
            return js("([...document.querySelectorAll('#toast-container .toast-content')]"
                      ".map(e=>e.textContent).join('\\n'))") or ""

        def wait_toast(sub, timeout=25):
            """等到 toast 里出现 sub，返回当时的**全部** toast 文本（含 sub 的那条）。

            返回捕获值而不是让调用方再读一次：toast 3 秒自删，`wait_true` 命中后
            再 `toast()` 很可能已经空了 —— 这正是「等到了却断言失败」的来源。
            """
            end = time.time() + timeout
            last = ""
            while time.time() < end:
                last = toast()
                if sub in last:
                    return last
                time.sleep(0.25)
            return last

        time.sleep(1.5)
        js("window.__errs=[];window.addEventListener('error',e=>window.__errs.push(String(e.message)));"
           "window.addEventListener('unhandledrejection',e=>window.__errs.push('reject:'+String(e.reason)));1")

        dl_ok = True
        try:
            ws.call("Browser.setDownloadBehavior",
                    {"behavior": "allow", "downloadPath": DL, "eventsEnabled": True})
        except Exception as e:
            dl_ok = False
            print("  下载行为设置失败：", e)

        # ── ① 登录：错密码被拒、对密码进账号页
        #
        # profile 是**固定目录**（跑前不删，删 1000+ 文件会被批量删除保护拦下），
        # 于是上一次运行留下的 `zcode2api_admin_key` 会让登录页的 IIFE 直接跳去
        # 账号页，整段登录流程被跳过（脚本假绿）。所以先清 localStorage 再强制回登录页。
        js("try{localStorage.clear()}catch(e){}")
        js("location.href='/admin/login'")
        wait_true("location.pathname==='/admin/login'", timeout=20)
        check("根路径经 307 链落到 /admin/login", js("location.pathname") == "/admin/login",
              js("location.pathname"))

        js("document.getElementById('key').value='wrong-key'")
        js("document.querySelector('.login-form .btn').click()")
        t = wait_toast("密码无效", timeout=15)
        check("错误后台密码被拒（toast 提示密码无效）且未跳转",
              "密码无效" in t and js("location.pathname") == "/admin/login",
              f"{t!r} @ {js('location.pathname')}")

        js("document.getElementById('key').value=%s" % json.dumps(ADMIN_KEY))
        js("document.querySelector('.login-form .btn').click()")
        wait_true("location.pathname==='/admin/accounts'", timeout=20)
        check("正确后台密码登录成功并进入账号页", js("location.pathname") == "/admin/accounts",
              js("location.pathname"))

        # ── ② 账号页：统计卡 + 空池
        wait_true("document.getElementById('s-total')"
                  "&&document.getElementById('s-total').textContent==='0'", timeout=20)
        check("统计卡渲染（四张，账户总数=0）",
              js("document.querySelectorAll('.stat-cell .stat-num').length") == 4
              and js("document.getElementById('s-total').textContent") == "0",
              js("document.getElementById('s-total').textContent"))
        check("空池显示空状态文案",
              "暂无账号" in (js("document.getElementById('acct-list').textContent") or ""),
              js("document.getElementById('acct-list').textContent"))

        def add_accounts(tokens):
            js("document.querySelector('button[onclick=\"openAdd()\"]').click()")
            wait_true("document.getElementById('modal-add').classList.contains('open')", timeout=15)
            js("document.getElementById('add-tokens').value=%s" % json.dumps(tokens))
            js("document.getElementById('add-provider').value='zai'")
            js("document.querySelector('#modal-add .dialog-btn-primary').click()")

        def rows():
            return js("document.querySelectorAll('#acct-list .acct-row').length")

        # ── ③ 先只加一个 API Key：让「全量刷新」走靶机那条 count=0 的分支
        add_accounts(API_TOKEN)
        wait_true("document.querySelectorAll('#acct-list .acct-row').length===1", timeout=25)
        check("新增 API Key 后列表 1 行（#tbl-count 同步）",
              rows() == 1 and js("document.getElementById('tbl-count').textContent") == "1",
              f"rows={rows()} count={js('document.getElementById(\"tbl-count\").textContent')}")
        check("表头 6 列结构完整（凭证/状态/最近请求/历史用量/额度信息/操作）",
              js("[...document.querySelectorAll('#acct-list .acct-head span')]"
                 ".map(s=>s.textContent).join('|')")
              == "凭证 / 账号|可用状态|最近请求|历史用量|额度信息|操作",
              js("[...document.querySelectorAll('#acct-list .acct-head span')]"
                 ".map(s=>s.textContent).join('|')"))
        check("API Key 行只有「编辑 / 删除 / 启停」（无刷新与领取按钮）",
              js("document.querySelector('#acct-list .acct-row')"
                 ".querySelectorAll('.acct-actions button').length") == 2,
              js("document.querySelector('#acct-list .acct-row')"
                 ".querySelectorAll('.acct-actions button').length"))
        check("统计卡账户总数更新为 1",
              js("document.getElementById('s-total').textContent") == "1",
              js("document.getElementById('s-total').textContent"))

        # ── ④ 全量刷新（池里只有 apiKey ⇒ 上游无需调用，count=0）
        js("document.getElementById('btn-refresh-all').click()")
        t = wait_toast("刷新完成", timeout=25)
        check("全量刷新（仅 API Key）回执「刷新完成：成功 0，失败 0」",
              "刷新完成" in t and "成功 0" in t and "失败 0" in t, t)

        # ── ⑤ 再加一个 JWT：验证 provider+点数 判据真的把 mode 落到界面
        add_accounts(JWT_TOKEN)
        wait_true("document.querySelectorAll('#acct-list .acct-row').length===2", timeout=25)
        texts = js("[...document.querySelectorAll('#acct-list .acct-row')].map(r=>r.textContent)") or []
        check("新增 JWT 后列表 2 行，且自动命名为 zai-1 / zai-2",
              rows() == 2 and any("zai-1" in t for t in texts) and any("zai-2" in t for t in texts),
              str(texts)[:260])
        check("凭据单元格识别两种模式（JWT / API Key）—— mode 判据落到界面",
              any("JWT" in t for t in texts) and any("API Key" in t for t in texts),
              str(texts)[:260])
        check("凭据以掩码展示（明文不出现在 DOM）",
              all(JWT_TOKEN not in t and API_TOKEN not in t for t in texts), str(texts)[:260])
        check("JWT 行 5 个操作按钮、API Key 行 2 个（按 mode 分叉）",
              # JWT 行 = 刷新额度 + 自动领取 + 手动领取 + 编辑 + 删除 = 5；
              # API Key 行 = 编辑 + 删除 = 2。启停是 <label class="switch">，不算 button。
              js("[...document.querySelectorAll('#acct-list .acct-row')]"
                 ".map(r=>r.querySelectorAll('.acct-actions button').length).sort().join(',')")
              == "2,5",
              js("[...document.querySelectorAll('#acct-list .acct-row')]"
                 ".map(r=>r.querySelectorAll('.acct-actions button').length).join(',')"))

        # ── ⑥ 编辑改名
        js("document.querySelector('#acct-list .acct-row button[title=编辑]').click()")
        wait_true("document.getElementById('modal-edit').classList.contains('open')", timeout=15)
        check("编辑弹窗带出当前名称",
              (js("document.getElementById('edit-name').value") or "") in ("zai-1", "zai-2"),
              js("document.getElementById('edit-name').value"))
        js("document.getElementById('edit-name').value='E2E 改名'")
        js("document.querySelector('#modal-edit .dialog-btn-primary').click()")
        wait_true("[...document.querySelectorAll('#acct-list .acct-row')]"
                  ".some(r=>r.textContent.includes('E2E 改名'))", timeout=25)
        check("编辑改名生效", bool(wait_true(
            "[...document.querySelectorAll('#acct-list .acct-row')]"
            ".some(r=>r.textContent.includes('E2E 改名'))", timeout=5)))

        # ── ⑦ 启停
        idx = js("[...document.querySelectorAll('#acct-list .acct-row')]"
                 ".findIndex(r=>r.textContent.includes('E2E 改名'))")
        js("document.querySelectorAll('#acct-list .acct-row')[%d].querySelector('.switch').click()" % idx)
        wait_true("document.querySelectorAll('#acct-list .acct-row')[%d]"
                  ".classList.contains('is-disabled')" % idx, timeout=25)
        check("停用后该行进入 is-disabled 态",
              js("document.querySelectorAll('#acct-list .acct-row')[%d]"
                 ".classList.contains('is-disabled')" % idx))

        # ── ⑧ A5 接缝：JWT 单账号刷新必须**显式失败**，不能假装成功
        jidx = js("[...document.querySelectorAll('#acct-list .acct-row')]"
                  ".findIndex(r=>r.textContent.includes('JWT'))")
        if jidx is not None and jidx >= 0:
            js("document.querySelectorAll('#acct-list .acct-row')[%d]"
               ".querySelector('button[title=刷新额度]').click()" % jidx)
            t = wait_toast("刷新失败", timeout=25)
            check("JWT 额度刷新（属 A5）显式失败而非假装成功",
                  "刷新失败" in t and "尚未实现" in t, t)
        else:
            skip("JWT 额度刷新（A5 接缝）", "未找到 JWT 行")

        # ── ⑨ 导出：走浏览器真实下载，断言文件字节
        js("document.querySelector('button[onclick=\"doExport()\"]').click()")
        fp = wait_file(DL, ".json", timeout=20) if dl_ok else None
        got = None
        if fp:
            try:
                got = json.loads(open(fp, encoding="utf-8").read())
            except Exception as e:
                got = {"__parse_err": str(e)}
        check("导出触发真实下载且是可解析 JSON",
              isinstance(got, dict) and "providers" in got, f"file={fp} got={str(got)[:160]}")
        entries = (got or {}).get("providers", {}).get("zai", [])
        secrets = {e.get("secret") for e in entries if isinstance(e, dict)}
        modes = {e.get("mode") for e in entries if isinstance(e, dict)}
        check("导出含两个账号的**明文**凭据（字节通路没被动过）",
              secrets == {JWT_TOKEN, API_TOKEN}, f"zai={str(entries)[:220]}")
        check("导出带上 mode（导出→导入往返才保真）",
              modes == {"jwt", "apiKey"}, f"modes={modes}")

        # ── ⑩ 导入（面板走隐藏 file input）
        payload = {"version": 1, "providers": {"zai": [
            {"name": "e2e-imp-a", "mode": "apiKey", "secret": "sk-e2e-imp-a"},
            {"name": "e2e-imp-b", "mode": "jwt", "secret": "eyJhbGciOiJI.eyJzdWIiOiJpIn0.sigimp"},
        ]}}
        imp = os.path.join(DL, "import-payload.json")
        with open(imp, "w", encoding="utf-8") as f:
            json.dump(payload, f)
        doc = ws.call("DOM.getDocument", {"depth": -1})
        node = ws.call("DOM.querySelector", {"nodeId": doc["root"]["nodeId"], "selector": "#import-file"})
        ws.call("DOM.setFileInputFiles", {"nodeId": node["nodeId"], "files": [imp]})
        wait_true("document.querySelectorAll('#acct-list .acct-row').length===4", timeout=25)
        check("导入 2 个账号后列表变为 4 行",
              js("document.querySelectorAll('#acct-list .acct-row').length") == 4,
              js("document.querySelectorAll('#acct-list .acct-row').length"))

        # ── ⑪ 删除（确认框）
        idx = js("[...document.querySelectorAll('#acct-list .acct-row')]"
                 ".findIndex(r=>r.textContent.includes('E2E 改名'))")
        js("document.querySelectorAll('#acct-list .acct-row')[%d]"
           ".querySelector('button[title=删除]').click()" % idx)
        wait_true("document.getElementById('modal-confirm').classList.contains('open')", timeout=15)
        check("删除前弹出确认框（文案含不可撤销提示）",
              "不可撤销" in (js("document.getElementById('confirm-body').textContent") or ""),
              js("document.getElementById('confirm-body').textContent"))
        js("document.querySelector('#modal-confirm .dialog-btn-danger').click()")
        wait_true("document.querySelectorAll('#acct-list .acct-row').length===3", timeout=25)
        check("确认后删除生效（3 行且被删账号消失）",
              js("document.querySelectorAll('#acct-list .acct-row').length") == 3
              and js("[...document.querySelectorAll('#acct-list .acct-row')]"
                     ".some(r=>r.textContent.includes('E2E 改名'))") is False,
              js("document.querySelectorAll('#acct-list .acct-row').length"))

        # ── ⑫ 设置页：掩码回显 + 落库
        js("location.href='/admin/settings'")
        wait_true("document.getElementById('admin-key')"
                  "&&document.getElementById('admin-key').value.length>0", timeout=20)
        check("设置页回填后台密码掩码", (js("document.getElementById('admin-key').value") or "") != "",
              js("document.getElementById('admin-key').value"))
        check("设置页回填网关密钥掩码（非空，说明 gateway_key_set 被正确读到）",
              (js("document.getElementById('gateway-key').value") or "") != "",
              js("document.getElementById('gateway-key').value"))
        check("默认口令提示按 admin_key_is_default 显示",
              js("document.getElementById('admin-key-default-hint').style.display") == "block",
              js("document.getElementById('admin-key-default-hint').style.display"))
        check("对话端点按当前 origin 渲染",
              (js("document.getElementById('endpoint').textContent") or "").endswith("/v1/messages"),
              js("document.getElementById('endpoint').textContent"))

        js("document.getElementById('quota-interval').value='1855'")
        js("document.querySelector('button[onclick=\"saveSettings()\"]').click()")
        t = wait_toast("已保存", timeout=20)
        check("设置页保存返回成功", "已保存" in t, t)
        js("location.reload()")
        wait_true("document.getElementById('quota-interval')"
                  "&&document.getElementById('quota-interval').value==='1855'", timeout=20)
        check("重新加载后刷新间隔按提交值落库（1855）",
              js("document.getElementById('quota-interval').value") == "1855",
              js("document.getElementById('quota-interval').value"))

        # ── ⑬ 监控页：结构渲染 + 清空
        js("location.href='/admin/monitoring'")
        wait_true("!!document.getElementById('btn-clear')", timeout=20)
        time.sleep(1.2)
        check("监控页渲染统计卡与三个页签",
              js("document.querySelectorAll('#stat-grid .stat-num').length") >= 6
              and js("document.querySelectorAll('.mon-tab').length") == 3,
              f"stats={js('document.querySelectorAll(\"#stat-grid .stat-num\").length')} "
              f"tabs={js('document.querySelectorAll(\".mon-tab\").length')}")
        check("空监控显示空状态文案（不是白屏）",
              "没有请求" in (js("document.getElementById('list').textContent") or ""),
              js("document.getElementById('list').textContent"))
        js("document.getElementById('btn-clear').click()")
        t = wait_toast("已清空", timeout=20)
        check("清空监控返回成功回执", "已清空" in t, t)
        check("清空后列表仍为空状态",
              js("document.querySelectorAll('#list .mon-row').length") == 0,
              js("document.querySelectorAll('#list .mon-row').length"))

        errs = js("window.__errs") or []
        check("全程无 JS 运行时错误", not errs, str(errs)[:300])

    finally:
        if ws:
            ws.close()
        if srv.poll() is None:
            # 先发中断信号走优雅退出（关库），再兜底 kill。
            try:
                srv.terminate()
                for _ in range(20):
                    if srv.poll() is not None:
                        break
                    time.sleep(0.2)
            except Exception:
                pass
            if srv.poll() is None:
                srv.kill()
        if edge_proc and edge_proc.poll() is None:
            edge_proc.kill()
        logf.close()
        if not args.keep:
            shutil.rmtree(os.path.join(HOME, "edge-profile"), ignore_errors=True)

    passed = sum(1 for _, ok, _ in results if ok)
    skipped = sum(1 for _, ok, _ in results if ok is None)
    total = len(results) - skipped
    print("\n" + "=" * 46 + f"\n{passed}/{total} 通过"
          + (f"（跳过 {skipped}）" if skipped else ""))
    failed = [n for n, ok, _ in results if ok is False]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
        print("\n服务日志尾部：\n" + open(LOG, encoding="utf-8", errors="replace").read()[-1500:])
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
