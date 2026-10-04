#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""打发布资产：两个平台的单文件可执行程序 + 源码 zip + checksums.txt。

为什么要有这个脚本
------------------
"发版产物怎么来的"必须是**可复现、可核对**的一条流水线，而不是一串手敲的命令。
它同时钉死三件事：

  1. **版本号注入**：`-X .../internal/buildinfo.Version=<ver>`，默认值带 `-dev`
     后缀，只有经这里构建的产物才会显示成正式版本号。
  2. **不含 Chromium**：资产只有两个 exe 与源码 zip。无痕验证复用**系统已装**
     的 Edge / Chrome（判定目标就是"是不是真浏览器"，没有既小又能过验证的内核）。
  3. **可复算的校验**：`checksums.txt` 覆盖全部资产，Release 说明里让用户自己核对。

用法:
    python tools/build_release.py --version v0.1.0            # 产物落 dist/
    python tools/build_release.py --version v0.1.0 --out D:/tmp/rel

源码 zip 走 `git archive HEAD`：只含**已提交**的文件（工作区的临时改动不会混进去，
也就不会出现"产物与 tag 不一致"），并且按 `.gitattributes` 的 `eol=lf` 导出。
"""
import argparse
import hashlib
import os
import shutil
import subprocess
import sys
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
VERSION_PKG = "github.com/LinYuanNull/zcode2api-go/internal/buildinfo"

# (GOOS, GOARCH, 输出文件名, 是否 GUI 子系统)
TARGETS = [
    ("windows", "amd64", "zcode2api-go-windows-amd64.exe", True),
    ("linux", "amd64", "zcode2api-go-linux-amd64", False),
]


def run(cmd, env=None, cwd=ROOT):
    r = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True)
    if r.returncode != 0:
        sys.stderr.write("命令失败: %s\n%s\n%s\n"
                         % (" ".join(cmd), r.stdout.decode("utf-8", "replace"),
                            r.stderr.decode("utf-8", "replace")))
        sys.exit(1)
    return r.stdout


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True,
                    help="写入 buildinfo.Version 的版本号，例如 v0.1.0")
    ap.add_argument("--out", default=os.path.join(ROOT, "dist"),
                    help="产物输出目录（默认 <仓库>/dist）")
    args = ap.parse_args()

    ver = args.version
    version_lit = ver[1:] if ver.startswith("v") else ver
    out = os.path.abspath(args.out)
    os.makedirs(out, exist_ok=True)

    env = dict(os.environ)
    env["CGO_ENABLED"] = "0"
    env.setdefault("PATH", "")
    go = shutil.which("go") or "go"

    assets = []
    for goos, goarch, name, gui in TARGETS:
        e = dict(env)
        e["GOOS"], e["GOARCH"] = goos, goarch
        ldflags = "-s -w -X %s.Version=%s" % (VERSION_PKG, version_lit)
        if gui:
            ldflags += " -H windowsgui"   # 否则会多一个黑控制台窗口
        dst = os.path.join(out, name)
        print("[build] %-32s %s/%s" % (name, goos, goarch))
        run([go, "build", "-trimpath", "-ldflags", ldflags, "-o", dst,
             "./cmd/zcode2api-go"], env=e)
        assets.append(dst)

    # 源码 zip：只含已提交内容，前缀目录带版本，方便解压后不散落。
    prefix = "zcode2api-go-%s/" % version_lit
    src_zip = os.path.join(out, "zcode2api-go-%s-src.zip" % version_lit)
    print("[src  ] %s（git archive HEAD）" % os.path.basename(src_zip))
    data = run(["git", "archive", "--format=zip", "--prefix", prefix, "HEAD"])
    with open(src_zip, "wb") as f:
        f.write(data)
    # 自检：zip 必须可解析、必须非空、且不含任何 exe（"不含 Chromium" 的可核对形式）。
    with zipfile.ZipFile(src_zip) as z:
        names = z.namelist()
        assert names, "源码 zip 为空"
        bad = [n for n in names if n.lower().endswith((".exe", ".so", ".dll", ".dylib"))]
        assert not bad, "源码 zip 里不应有二进制：%r" % bad[:5]
    assets.append(src_zip)

    # checksums：`sha256sum -c` 兼容格式（两个空格）。
    sums = os.path.join(out, "checksums.txt")
    lines = []
    for p in assets:
        lines.append("%s  %s" % (sha256(p), os.path.basename(p)))
    with open(sums, "w", encoding="ascii", newline="\n") as f:
        f.write("\n".join(lines) + "\n")
    assets.append(sums)

    print("\n产物（%d 个）：" % len(assets))
    for ln in lines:
        print("  " + ln)
    print("\n目录: " + out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
