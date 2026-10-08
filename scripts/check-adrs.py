#!/usr/bin/env python3
"""Check docs/adr: every ADR has a valid status and a row in docs/adr/README.md with the same one.

    scripts/check-adrs.py

Exit status 1 with one line per problem.
"""
import glob
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ADR = os.path.join(ROOT, "docs", "adr")
STATES = ("Propuesta", "Aceptada", "Obsoleta")


def main():
    problems = []
    readme = open(os.path.join(ADR, "README.md"), encoding="utf-8").read()
    rows = {}
    for line in readme.split("\n"):
        m = re.match(r"\|\s*\[(\d{4})\]\(([^)]+)\)\s*\|(.*)\|\s*([^|]*?)\s*\|\s*$", line)
        if m:
            rows[m.group(1)] = (m.group(2), re.sub(r"[*]", "", m.group(4)).split("(")[0].strip())
    seen = set()
    for path in sorted(glob.glob(os.path.join(ADR, "[0-9][0-9][0-9][0-9]-*.md"))):
        name = os.path.basename(path)
        num = name[:4]
        seen.add(num)
        text = open(path, encoding="utf-8").read()
        title = re.match(r"# ADR-(\d{4}): .+", text)
        if not title or title.group(1) != num:
            problems.append("%s: the first line must be '# ADR-%s: <title>'" % (name, num))
        m = re.search(r"^- \*\*Estado:\*\* (.+)$", text, re.M)
        if not m:
            problems.append("%s: no '- **Estado:**' line" % name)
            continue
        status = m.group(1).strip()
        if not (status in STATES or re.fullmatch(r"Sustituida por \d{4}", status)):
            problems.append("%s: status %r is not one of %s or 'Sustituida por NNNN'" % (name, status, ", ".join(STATES)))
        row = rows.get(num)
        if not row:
            problems.append("%s: not listed in docs/adr/README.md" % name)
        else:
            if row[0] != name:
                problems.append("%s: README links %s" % (name, row[0]))
            if row[1] != status:
                problems.append("%s: README says %r, the ADR %r" % (name, row[1], status))
    for num in rows:
        if num not in seen:
            problems.append("README lists %s, which does not exist" % num)
    for p in problems:
        print(p)
    if problems:
        print("%d problem(s)" % len(problems), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
