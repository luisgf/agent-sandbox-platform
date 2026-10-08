#!/usr/bin/env python3
"""Check the relative links of the Markdown files of the repository.

For every [text](target) that is not a URL: the file or directory must exist, and a #fragment must be a
heading (or an <a id>) of the Markdown file it points at. Code blocks and code spans are skipped. Exit
status 1 with one line per broken link.

When docs/README.md exists, every file under docs/ must also be reachable from it by following links
(through any number of pages): an index that leaves a page out is a page nobody finds.

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


def destinations(path):
    """The files (and directories) the Markdown file at path links to, fragments dropped."""
    here = os.path.dirname(path)
    with open(path, encoding="utf-8") as f:
        lines = f.read().split("\n")
    out = set()
    for line in strip_code(lines):
        for target in LINK.findall(line) + IMAGE.findall(line):
            if re.match(r"[a-zA-Z][a-zA-Z0-9+.-]*:", target) or target.startswith("//"):
                continue
            file_part = target.partition("#")[0].split("?")[0]
            if not file_part:
                continue
            base = ROOT if file_part.startswith("/") else here
            out.add(os.path.normpath(os.path.join(base, file_part.lstrip("/") if file_part.startswith("/") else file_part)))
    return out


def unreachable(index):
    """The files under the directory of the index that no chain of links from the index reaches."""
    docs = os.path.dirname(index)
    seen, queue = {os.path.normpath(index)}, [os.path.normpath(index)]
    while queue:
        page = queue.pop()
        if not page.endswith(".md") or not os.path.isfile(page):
            continue
        for dest in destinations(page):
            if os.path.isdir(dest):
                dest = os.path.join(dest, "README.md")
            if dest not in seen and os.path.exists(dest):
                seen.add(dest)
                queue.append(dest)
    missing = []
    for dirpath, dirnames, filenames in os.walk(docs):
        dirnames[:] = [d for d in dirnames if not d.startswith(".")]
        for f in sorted(filenames):
            full = os.path.join(dirpath, f)
            if full not in seen and not f.startswith("."):
                missing.append(full)
    return missing


def main(argv):
    paths = argv[1:] or [ROOT]
    bad = 0
    for path in markdown_files(paths):
        for no, target, why in check(path):
            print("%s:%d: %s: %s" % (os.path.relpath(path, ROOT), no, target, why))
            bad += 1
    index = os.path.join(ROOT, "docs", "README.md")
    if os.path.isfile(index) and not argv[1:]:
        for page in unreachable(index):
            print("%s: not reachable from docs/README.md" % os.path.relpath(page, ROOT))
            bad += 1
    if bad:
        print("%d broken link(s)" % bad, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
