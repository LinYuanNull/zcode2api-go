#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 `docs/contract/admin/SPEC.md` 第二部分里 JSON 骨架的**键顺序**对齐到样本。

为什么需要这个工具
------------------
SPEC.md 第二部分是初稿时手写的，当时样本里的键顺序**是采样器缺陷的产物**
（旧采样器把响应体解到 `map[string]any` 再编码，Go 按字典序输出）。缺陷修好后
样本已恢复真实顺序，但第二部分的骨架还是旧字典序 —— 于是同一份文档里，
第四部分说「顶层是 accounts, stats, providers, ts」，第二部分却写着字典序。

后果不是「文档不好看」：**骨架是照着写 struct 字段声明序的**，照错的顺序写，
响应体就与靶机不同形。所以这里把骨架按样本重排一遍。

做法
----
1. 每个 `### N. <METHOD> <path>` 节对应文件名前缀 `NN-` 的样本（`01-` … `22-`）。
2. 从这些样本里收集两样东西：
   - **路径键序表**：`response.body` 给响应骨架用，`request.body` 给请求骨架用；
     数组用 `[]` 作路径段（`accounts[].id`）。
   - **键集合 → 键顺序**（shapes）：`<account>`、`<fingerprint>` 这类**子骨架**在文档里
     是独立块、路径算顶层，靠路径匹配不到，但它们的**键集合**是唯一的，能对上。
     匹配用**排序后的键集合**作索引（文档块里是字母序、样本里是真实序，不排序就永远对不上）。
3. 用**容忍占位符**的解析器读骨架（骨架里有 `<number>`、`<account>`、`...` 这类
   不是合法 JSON 的记号），按收集到的顺序重排对象成员，再**按原形态**渲染回去
   （节点自身原来是单行就渲染单行 —— 只改顺序，不动排版）。
4. 只有**顺序真的变了**的块才替换；单键块与顺序正确的块原样保留。
5. 自检：重排后每个对象的键序列必须能在样本里找到出处（逐字出现过，或等于该节
   路径键序表 —— 后者用于文档明写的**合并骨架**，如第 18 节请求体）。找不到就报错，
   说明重排引入了样本里不存在的形状。

用法：`python tools/spec_reorder.py [--check]`
  --check  只报告不写回；**有漂移时以非零码退出**（CI 用），并打印怎么修。
"""
import argparse
import collections
import io
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SPEC = os.path.join(ROOT, "docs", "contract", "admin", "SPEC.md")
SAMPLES = os.path.join(ROOT, "docs", "contract", "admin")

sys.stdout.reconfigure(encoding="utf-8")


# ── 键顺序收集 ──────────────────────────────────────────────

def collect_order(value, path, out, shapes):
    """把 value 里的对象键顺序累加进 out[path]（先见者在前）。

    同时把「键集合 → 键顺序」记进 shapes：子骨架（如 `<account>`、`<fingerprint>`）
    在文档里是独立块、路径为顶层，靠路径匹配不到，但**键集合**是唯一的，能对上。
    """
    if isinstance(value, dict):
        keys = out.setdefault(path, [])
        for k, v in value.items():
            if k not in keys:
                keys.append(k)
            collect_order(v, path + "." + k if path else k, out, shapes)
        if len(value) >= 2:
            ks = tuple(sorted(value.keys()))
            prev = shapes.get(ks)
            if prev is None:
                shapes[ks] = list(value.keys())
            elif prev != list(value.keys()):
                # 同一个键集合在不同样本里顺序不同 —— 这不是「文档没对齐」，
                # 是样本之间自相矛盾，必须报出来而不是随便挑一个。
                print("  ! 键集合 %s 在样本中出现两种顺序：%s / %s"
                      % (list(ks), prev, list(value.keys())))
    elif isinstance(value, list):
        for item in value:
            collect_order(item, path + "[]", out, shapes)


def order_for(section_no, use_request):
    """汇总该节全部样本的键顺序。返回 (path_order, shapes)。"""
    out = collections.OrderedDict()
    shapes = {}
    prefix = "%02d-" % section_no
    for name in sorted(os.listdir(SAMPLES)):
        if not name.startswith(prefix):
            continue
        p = os.path.join(SAMPLES, name)
        if not name.endswith(".json"):
            continue
        try:
            with io.open(p, encoding="utf-8") as f:
                sample = json.load(f, object_pairs_hook=collections.OrderedDict)
        except Exception as e:  # 样本坏掉是另一件事，这里不掩盖
            print("  跳过 %s：%s" % (name, e))
            continue
        body = (sample.get("request") or {}).get("body") if use_request \
            else (sample.get("response") or {}).get("body")
        if body is None:
            continue
        collect_order(body, "", out, shapes)
    return out, shapes


# ── 容忍占位符的骨架解析 ────────────────────────────────────

class Node:
    __slots__ = ("kind", "items", "text", "inline")

    def __init__(self, kind, items=None, text="", inline=True):
        self.kind = kind          # obj | arr | scalar
        self.items = items or []  # obj: [(key, Node)] / arr: [Node]
        self.text = text          # scalar 的原文
        self.inline = inline      # 原块里是不是写在一行（决定渲染回哪种形态）

    def keys(self, path, out):
        """收集 (path, [key...]) 用于变更检测。"""
        if self.kind == "obj":
            out.append((path, [k for k, _ in self.items]))
            for k, v in self.items:
                v.keys(path + "." + k if path else k, out)
        elif self.kind == "arr":
            for v in self.items:
                v.keys(path + "[]", out)


class ParseError(Exception):
    pass


def parse(text, i=0):
    i = _ws(text, i)
    if i >= len(text):
        raise ParseError("意外结束")
    c = text[i]
    if c == "{":
        return _parse_obj(text, i)
    if c == "[":
        return _parse_arr(text, i)
    if c == '"':
        return _parse_str(text, i)
    return _parse_bare(text, i)


def _ws(text, i):
    while i < len(text) and text[i] in " \t\r\n":
        i += 1
    return i


def _parse_str(text, i):
    j = i + 1
    while j < len(text):
        if text[j] == "\\":
            j += 2
            continue
        if text[j] == '"':
            return Node("scalar", text=text[i:j + 1]), j + 1
        j += 1
    raise ParseError("字符串未闭合")


def _parse_bare(text, i):
    j = i
    while j < len(text) and text[j] not in ",}]":
        j += 1
    return Node("scalar", text=text[i:j].strip()), j


def _unquote(tok):
    """把 `"accounts"` 还原成 `accounts`（键要拿去和样本比，不能带引号）。"""
    try:
        return json.loads(tok)
    except Exception:
        return tok[1:-1] if len(tok) >= 2 else tok


def _parse_obj(text, i):
    start = i
    i = _ws(text, i + 1)
    items = []
    if i < len(text) and text[i] == "}":
        return Node("obj", items, inline="\n" not in text[start:i + 1]), i + 1
    while True:
        i = _ws(text, i)
        if text[i] != '"':
            raise ParseError("对象键必须是字符串，位置 %d：%r" % (i, text[i:i + 20]))
        key_node, i = _parse_str(text, i)
        i = _ws(text, i)
        if text[i] != ":":
            raise ParseError("缺少冒号，位置 %d" % i)
        val, i = parse(text, i + 1)
        items.append((_unquote(key_node.text), val))
        i = _ws(text, i)
        if i < len(text) and text[i] == ",":
            i += 1
            continue
        if i < len(text) and text[i] == "}":
            return Node("obj", items, inline="\n" not in text[start:i + 1]), i + 1
        raise ParseError("对象里出现意外字符，位置 %d：%r" % (i, text[i:i + 20]))


def _parse_arr(text, i):
    start = i
    i = _ws(text, i + 1)
    items = []
    if i < len(text) and text[i] == "]":
        return Node("arr", items, inline="\n" not in text[start:i + 1]), i + 1
    while True:
        val, i = parse(text, i)
        items.append(val)
        i = _ws(text, i)
        if i < len(text) and text[i] == ",":
            i += 1
            continue
        if i < len(text) and text[i] == "]":
            return Node("arr", items, inline="\n" not in text[start:i + 1]), i + 1
        raise ParseError("数组里出现意外字符，位置 %d：%r" % (i, text[i:i + 20]))


# ── 重排与渲染 ──────────────────────────────────────────────

def reorder(node, order, shapes, path=""):
    if node.kind == "obj":
        ks = tuple(k for k, _ in node.items)
        # 键集合优先（子骨架也能对上），退回落路径顺序。
        known = shapes.get(tuple(sorted(ks))) or order.get(path, [])
        idx = {k: n for n, k in enumerate(known)}
        orig = [k for k, _ in node.items]
        items = [(k, reorder(v, order, shapes, path + "." + k if path else k))
                 for k, v in node.items]
        # 已知键按样本顺序；未知键保持原相对顺序、排在已知键之后（安全网）。
        items.sort(key=lambda kv: (0, idx[kv[0]]) if kv[0] in idx
                   else (1, orig.index(kv[0])))
        return Node("obj", items, inline=node.inline)
    if node.kind == "arr":
        return Node("arr", [reorder(v, order, shapes, path + "[]") for v in node.items],
                    inline=node.inline)
    return node


def render(node, indent=0):
    if node.kind == "obj":
        if not node.items:
            return "{}"
        if node.inline:
            return "{ " + ", ".join("%s: %s" % (json.dumps(k, ensure_ascii=False),
                                                render(v, indent))
                                    for k, v in node.items) + " }"
        inner = ",\n".join("  " * (indent + 1) + "%s: %s"
                           % (json.dumps(k, ensure_ascii=False), render(v, indent + 1))
                           for k, v in node.items)
        return "{\n" + inner + "\n" + "  " * indent + "}"
    if node.kind == "arr":
        if not node.items:
            return "[]"
        if node.inline:
            return "[ " + ", ".join(render(v, indent) for v in node.items) + " ]"
        inner = ",\n".join("  " * (indent + 1) + render(v, indent + 1)
                           for v in node.items)
        return "[\n" + inner + "\n" + "  " * indent + "]"
    return node.text


# ── 主流程 ──────────────────────────────────────────────────

BLOCK_RE = re.compile(r"```json\n(.*?)```", re.S)
SECTION_RE = re.compile(r"(?m)^### (\d+)\. ")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true",
                    help="只报告不写回；有漂移时以非零码退出（CI 用）")
    args = ap.parse_args()

    text = io.open(SPEC, encoding="utf-8", newline="").read()
    marks = list(SECTION_RE.finditer(text))
    if not marks:
        print("未找到任何 `### N. ` 分节")
        return 1

    # 把文档切成 [前言][(节号, 起始, 结束)…]
    spans = []
    for n, m in enumerate(marks):
        end = marks[n + 1].start() if n + 1 < len(marks) else len(text)
        spans.append((int(m.group(1)), m.start(), end))

    changed = 0
    out = []
    cursor = 0
    for sec_no, start, end in spans:
        out.append(text[cursor:start])
        body = text[start:end]
        # 该节里每个块前面最近的说明行决定它是请求骨架还是响应骨架。
        pieces = []
        pos = 0
        for m in BLOCK_RE.finditer(body):
            head = body[:m.start()].rstrip().split("\n")
            intro = head[-1] if head else ""
            use_request = "请求体" in intro
            block = m.group(1)
            new_block = fix_block(sec_no, block, use_request)
            if new_block is not None:
                changed += 1
                print("  [%02d] %-46s %s" % (sec_no, intro.strip()[:46],
                                             "请求" if use_request else "响应"))
                pieces.append(body[pos:m.start()])
                pieces.append("```json\n" + new_block + "```")
                pos = m.end()
        pieces.append(body[pos:])
        out.append("".join(pieces))
        cursor = end
    out.append(text[cursor:])
    result = "".join(out)

    print("\n需改动的块：%d" % changed)
    if args.check:
        if changed:
            print("SPEC.md 的骨架键序与样本不一致（跑 `python tools/spec_reorder.py` 修）")
            return 1
        print("SPEC.md 骨架键序与样本一致")
        return 0
    if changed:
        io.open(SPEC, "w", encoding="utf-8", newline="").write(result)
        print("已写回 " + SPEC)
    return 0


def fix_block(sec_no, block, use_request):
    """返回重排后的块文本；顺序未变或解析失败时返回 None（保持原样）。"""
    order, shapes = order_for(sec_no, use_request)
    if not order:
        return None
    try:
        node, end = parse(block)
    except ParseError as e:
        print("  ! 第 %02d 节解析失败，保持原样：%s" % (sec_no, e))
        return None
    if _ws(block, end) != len(block):
        print("  ! 第 %02d 节块尾有多余内容，保持原样" % sec_no)
        return None

    before = []
    node.keys("", before)
    new = reorder(node, order, shapes)
    after = []
    new.keys("", after)
    if before == after:
        return None
    _verify(sec_no, new, shapes, order)
    # 只改键序，不动排版：块尾的换行原样带回去（形态由各节点自身的 inline 决定）。
    tail = block[len(block.rstrip()):]
    return render(new) + tail


def _verify(sec_no, node, shapes, order):
    """自检：块里每个对象的键序列，必须能在样本里找到出处。

    出处有两种，都算通过：
      1. 某个样本里**逐字出现过**同一键集合 —— 这是常态；
      2. 该节的「路径键序表」里就是这个顺序 —— 用于文档明写的**合并骨架**
         （如第 18 节请求体：三个样本键集不同，文档给的是并集，任何单个样本
         都凑不出这个键集合）。
    两者都不满足，就说明重排引入了样本里不存在的形状（漏键/拼错），必须报出来。
    """
    seen = []
    node.keys("", seen)
    for path, keys in seen:
        if len(keys) < 2:
            continue  # 空对象 / 单键对象没有「顺序」可言
        if tuple(sorted(keys)) in shapes:
            continue
        if list(keys) == order.get(path):
            continue
        print("  ! 第 %02d 节 %s 的键序列既不在样本中、也不在该节键序表里：%s"
              % (sec_no, path or "<root>", keys))


if __name__ == "__main__":
    sys.exit(main())
