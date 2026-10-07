#!/usr/bin/env python3
"""Check the relative links of the Markdown files of the repository.

For every [text](target) that is not a URL: the file or directory must exist, and a #fragment must be a
heading (or an <a id>) of the Markdown file it points at. Code blocks and code spans are skipped. Exit
status 1 with one line per broken link.

    scripts/check-doc-links.py [file-or-directory ...]     (default: the whole repository)
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SKIP_DIRS = {".git", ".claude", "target", "node_modules", "vendor", "build"}

LINK = re.compile(r"(?<!\!)\[(?:[^\]\\]|\\.)*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
IMAGE = re.compile(r"\!\[(?:[^\]\\]|\\.)*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
HTML_ANCHOR = re.compile(r"<a\s+(?:[^>]*\s)?(?:id|name)=[\"']([^\"']+)[\"']")


def markdown_files(paths):
    for p in paths:
        p = os.path.abspath(p)
        if os.path.isfile(p):
            yield p
            continue
        for dirpath, dirnames, filenames in os.walk(p):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for f in sorted(filenames):
                if f.endswith(".md"):
                    yield os.path.join(dirpath, f)


def strip_code(lines):
    """The lines of a file with fenced blocks blanked and inline code spans removed."""
    out, fence = [], None
    for line in lines:
        stripped = line.lstrip()
        m = re.match(r"(`{3,}|~{3,})", stripped)
        if fence:
            if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence):
                fence = None
            out.append("")
            continue
        if m:
            fence = m.group(1)
            out.append("")
            continue
        out.append(re.sub(r"`[^`]*`", "``", line))
    return out


def slug(text):
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", text)  # a link keeps its text
    text = re.sub(r"[`*_~]", lambda m: "_" if m.group(0) == "_" else "", text)
    text = re.sub(r"<[^>]+>", "", text)
    text = text.strip().lower()
    text = re.sub(r"[^\w\- ]", "", text, flags=re.UNICODE)
    return text.replace(" ", "-")


_anchors = {}


def anchors(path):
    if path in _anchors:
        return _anchors[path]
    found, seen = set(), {}
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().split("\n")
    except OSError:
        _anchors[path] = found
        return found
    in_fence = None
    for line in lines:
        m = re.match(r"\s*(`{3,}|~{3,})", line)
        if in_fence:
            if m and m.group(1)[0] == in_fence[0] and len(m.group(1)) >= len(in_fence):
                in_fence = None
            continue
        if m:
            in_fence = m.group(1)
            continue
        h = HEADING.match(line)
        if h:
            s = slug(h.group(2))
            n = seen.get(s, 0)
            seen[s] = n + 1
            found.add(s if n == 0 else "%s-%d" % (s, n))
        for a in HTML_ANCHOR.findall(line):
            found.add(a)
    _anchors[path] = found
    return found


def check(path):
    problems = []
    with open(path, encoding="utf-8") as f:
        lines = f.read().split("\n")
    here = os.path.dirname(path)
    for no, line in enumerate(strip_code(lines), 1):
        for target in LINK.findall(line) + IMAGE.findall(line):
            if re.match(r"[a-zA-Z][a-zA-Z0-9+.-]*:", target) or target.startswith("//"):
                continue  # http:, https:, mailto:…
            file_part, _, fragment = target.partition("#")
            if file_part == "":
                dest = path
            else:
                file_part = file_part.split("?")[0]
                dest = os.path.normpath(os.path.join(ROOT if file_part.startswith("/") else here, file_part.lstrip("/") if file_part.startswith("/") else file_part))
                if not os.path.exists(dest):
                    problems.append((no, target, "no such file"))
                    continue
            if fragment and dest.endswith(".md") and os.path.isfile(dest):
                if fragment.lower() not in {a.lower() for a in anchors(dest)} and fragment not in anchors(dest):
                    problems.append((no, target, "no such heading"))
    return problems


def main(argv):
    paths = argv[1:] or [ROOT]
    bad = 0
    for path in markdown_files(paths):
        for no, target, why in check(path):
            print("%s:%d: %s: %s" % (os.path.relpath(path, ROOT), no, target, why))
            bad += 1
    if bad:
        print("%d broken link(s)" % bad, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
