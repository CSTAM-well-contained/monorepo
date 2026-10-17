#!/usr/bin/env python3
"""Split the guide into two documents.

out/.work/guide/*.md : the guide, with long code blocks replaced by a reference box
out/.work/code/*.md  : the Code Companion, with every moved listing numbered per chapter,
                   plus the full source of infra/pulumi for Step 2.

Rules: a fenced block moves when it has more than LIMIT lines (LIMIT_SHELL for shell,
because short command sequences are steps you type while following the guide).
Blocks inside callout boxes (::: done / warn / ...) stay unless they are very long.
"""
import glob
import os
import re
import shutil

HERE = os.path.dirname(os.path.abspath(__file__))
GUIDE = os.path.abspath(os.path.join(HERE, ".."))
SRC = os.path.join(GUIDE, "chapters")
OUT_G = os.path.join(GUIDE, "out", ".work", "guide")
OUT_C = os.path.join(GUIDE, "out", ".work", "code")
REPO = os.path.abspath(os.path.join(GUIDE, "..", ".."))

LIMIT, LIMIT_SHELL, LIMIT_IN_BOX = 12, 20, 30
SHELL = {"bash", "sh", "shell", "console"}
LANG_NAME = {"go": "Go", "sql": "SQL", "bash": "shell", "sh": "shell", "yaml": "YAML", "json": "JSON",
             "haproxy": "HAProxy config", "conf": "config", "proto": "protobuf", "text": "text",
             "ts": "TypeScript", "tsx": "TypeScript (React)", "ini": "systemd unit", "makefile": "Makefile",
             "nft": "nftables", "markdown": "Markdown", "toml": "TOML", "dockerfile": "Dockerfile", "hcl": "HCL"}

FENCE = re.compile(r"^(\s*)(`{3,}|~{3,})\s*\{?\.?([A-Za-z0-9_+-]*)")


def title_of(lines, lang):
    """First comment line that names a file or says what the block is."""
    for l in lines[:4]:
        t = l.strip()
        m = re.match(r"^(//|#|--|;)\s*(.+)$", t)
        if m and not t.startswith("#!") and not t.startswith("#cloud-config"):
            return m.group(2).strip().rstrip(":")
    return None


def split_chapter(path, chap_no, chap_title):
    lines = open(path, encoding="utf-8").read().split("\n")
    out, listings = [], []
    i, depth, heading = 0, 0, chap_title
    while i < len(lines):
        line = lines[i]
        hm = re.match(r"^(#{2,3})\s+(.*)", line)
        if hm and not line.startswith("#!"):
            heading = re.sub(r"\s*\{.*\}\s*$", "", hm.group(2))
        if re.match(r"^:::\s*\w", line):
            depth += 1
        elif line.strip() == ":::" and depth > 0:
            depth -= 1
        fm = FENCE.match(line)
        if not fm:
            out.append(line)
            i += 1
            continue
        indent, fence, lang = fm.group(1), fm.group(2), fm.group(3).lower()
        j = i + 1
        while j < len(lines) and not lines[j].strip().startswith(fence):
            j += 1
        body = lines[i + 1:j]
        n = len(body)
        limit = LIMIT_IN_BOX if depth > 0 else (LIMIT_SHELL if lang in SHELL else LIMIT)
        if n > limit:
            num = f"{chap_no}.{len(listings) + 1}"
            ttl = title_of(body, lang) or f"{LANG_NAME.get(lang, lang or 'code')} for “{heading}”"
            listings.append((num, ttl, heading, lang, body))
            out.append("")
            out.append("::: listing")
            out.append(f"**Listing {num}** · {ttl} · {LANG_NAME.get(lang, lang or 'code')}, {n} lines → *Code Companion*, listing {num}")
            out.append(":::")
            out.append("")
        else:
            out.extend(lines[i:j + 1])
        i = j + 1
    return "\n".join(out), listings


def pulumi_listings(chap_no, start):
    order = ["Pulumi.yaml", "Pulumi.prod.yaml.example", "config.go", "network.go", "secgroups.go",
             "ports.go", "servers.go", "main.go"]
    res = []
    for k, f in enumerate(order):
        p = os.path.join(REPO, "infra", "pulumi", f)
        if not os.path.exists(p):
            continue
        lang = "go" if f.endswith(".go") else "yaml"
        body = open(p, encoding="utf-8").read().rstrip("\n").split("\n")
        res.append((f"{chap_no}.{start + k}", f"infra/pulumi/{f}", "2.1 The Pulumi program", lang, body))
    return res


def main():
    for d in (OUT_G, OUT_C):
        shutil.rmtree(d, ignore_errors=True)
        os.makedirs(d)
    total_moved = 0
    for path in sorted(glob.glob(os.path.join(SRC, "*.md"))):
        name = os.path.basename(path)
        first = open(path, encoding="utf-8").readline().strip()
        m = re.match(r"^#\s+(\d+)\.\s+(.*)$", first)
        if not m:  # part dividers: guide only
            shutil.copy(path, os.path.join(OUT_G, name))
            continue
        chap_no, chap_title = m.group(1), m.group(2)
        guide_md, listings = split_chapter(path, chap_no, chap_title)
        if name.startswith("11-"):
            listings += pulumi_listings(chap_no, len(listings) + 1)
        open(os.path.join(OUT_G, name), "w", encoding="utf-8").write(guide_md)
        total_moved += len(listings)
        if not listings:
            continue
        c = [f"# {chap_no}. {chap_title}", ""]
        for num, ttl, heading, lang, body in listings:
            c.append(f"## Listing {num} – {ttl}")
            c.append("")
            c.append(f"*Guide section: {heading}*")
            c.append("")
            c.append(f"```{lang}" if lang else "```")
            c.extend(body)
            c.append("```")
            c.append("")
        open(os.path.join(OUT_C, name), "w", encoding="utf-8").write("\n".join(c))
    print(f"moved {total_moved} listings")


if __name__ == "__main__":
    main()
