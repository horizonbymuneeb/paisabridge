#!/usr/bin/env python3
"""Assemble the documentation site from fragments.

Each file in site/src/pages/*.html carries a small front-matter block and a body.
Pages include shared chrome with {{HEADER}} / {{FOOTER}} markers so the nav stays
identical everywhere without a template engine or a node toolchain.

    python3 site/build.py            # writes site/*.html
    python3 site/build.py --check    # fails if the output is stale (CI)
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
SRC = ROOT / "src"
PAGES = SRC / "pages"

HEADER = (SRC / "_header.html").read_text(encoding="utf8")
FOOTER = (SRC / "_footer.html").read_text(encoding="utf8")


def parse(page: Path) -> tuple[dict[str, str], str]:
    text = page.read_text(encoding="utf8")
    m = re.match(r"^---\n(.*?)\n---\n(.*)$", text, re.S)
    if not m:
        raise SystemExit(f"{page}: missing front matter")
    meta: dict[str, str] = {}
    for line in m.group(1).splitlines():
        if not line.strip():
            continue
        k, _, v = line.partition(":")
        meta[k.strip()] = v.strip()
    return meta, m.group(2)


def header_for(meta: dict[str, str]) -> str:
    nav = meta.get("nav", "")
    out = HEADER
    # Mark the active nav item without a second template language.
    for key, label in (("start", "Get started"), ("webhooks", "Webhooks"),
                       ("console", "Console"), ("api", "API"), ("ops", "Operations")):
        if key == nav:
            out = out.replace(f'<a href="{meta[key + "_href"]}">{label}</a>',
                              f'<a href="{meta[key + "_href"]}" aria-current="page">{label}</a>')
    return out


def build() -> int:
    changed = 0
    for page in sorted(PAGES.glob("*.html")):
        meta, body = parse(page)
        html = (
            header_for(meta)
            .replace("{{TITLE}}", meta["title"])
            .replace("{{DESC}}", meta["desc"])
            .replace("{{BODY}}", body)
            .replace("{{FOOTER}}", FOOTER)
        )
        out = ROOT / meta["out"]
        old = out.read_text(encoding="utf8") if out.exists() else None
        if old != html:
            out.write_text(html, encoding="utf8")
            changed += 1
    print(f"site: {changed} page(s) written")
    return changed


if __name__ == "__main__":
    n = build()
    if "--check" in sys.argv and n:
        raise SystemExit("site output is stale; run python3 site/build.py")
