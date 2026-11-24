"""Fail on a link in the documentation that does not lead anywhere.

It reads README.md and every Markdown page under docs/, and for each link whose
target is a path in the repository checks that the file or directory is there,
and, when the link names a heading (a "#anchor"), that the target page has a
heading that makes that anchor. A link to a page that moved, to a heading that
was renamed, or to a file that was deleted is the failure it exists to catch;
a reader who follows such a link learns nothing.

Links to other sites (a scheme such as https:) are not fetched: the check works
offline and says nothing of whether a site is up. Code in fenced blocks and in
inline code is not read, so an example link in a code span is not a link.

An anchor is made the way GitHub makes it from a heading: the text in lower
case, everything but letters, digits, underscores, hyphens and spaces removed,
spaces turned to hyphens, and a second heading of the same text numbered -1,
-2 and so on.

Prints path:line: reason: link for each failure; exit status 1 on any, 0 on none.

Usage: doc-links.py [root]. root defaults to the current directory.
doc-links.py --self-test checks the check against a small tree of its own.
Standard library only.
"""

import os
import re
import sys
import tempfile

# A Markdown link or image: [text](target) with an optional "title" after the
# target. The target holds no space and no closing parenthesis, which is the
# form every page here uses.
LINK = re.compile(r"!?\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
CODE_SPAN = re.compile(r"`+[^`]*`+")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
FENCE = re.compile(r"^\s*(```|~~~)")
INLINE_LINK = re.compile(r"\[([^\]]*)\]\([^)]*\)")
SCHEME = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")


def pages(root):
    """Returns the paths, relative to root, of the Markdown the check reads."""
    out = []
    if os.path.isfile(os.path.join(root, "README.md")):
        out.append("README.md")
    for base, dirs, files in os.walk(os.path.join(root, "docs")):
        dirs.sort()
        for name in sorted(files):
            if name.endswith(".md"):
                out.append(os.path.relpath(os.path.join(base, name), root))
    return out


def slug(text):
    """Returns the anchor GitHub makes from a heading's text."""
    text = INLINE_LINK.sub(r"\1", text)
    text = text.replace("`", "").replace("*", "")
    text = re.sub(r"[^\w\- ]", "", text.lower())
    return text.replace(" ", "-")


def lines_outside_fences(text):
    """Yields (number, line) for the lines of text that are not in a fenced
    code block, numbered from 1."""
    fenced = False
    for number, line in enumerate(text.splitlines(), 1):
        if FENCE.match(line):
            fenced = not fenced
            continue
        if not fenced:
            yield number, line


def anchors(text):
    """Returns the set of anchors a page's headings make."""
    seen = {}
    out = set()
    for _, line in lines_outside_fences(text):
        m = HEADING.match(line)
        if not m:
            continue
        base = slug(m.group(2))
        n = seen.get(base, 0)
        seen[base] = n + 1
        out.add(base if n == 0 else "%s-%d" % (base, n))
    return out


def check_page(root, path, text, anchor_cache, hits):
    """Adds to hits one line for each link of the page at path that fails."""
    here = os.path.dirname(os.path.join(root, path))
    for number, line in lines_outside_fences(text):
        for m in LINK.finditer(CODE_SPAN.sub("", line)):
            target = m.group(1)
            if SCHEME.match(target):
                continue
            base, _, anchor = target.partition("#")
            if base == "":
                dest = os.path.join(root, path)
            else:
                dest = os.path.normpath(os.path.join(here, base))
                inside = os.path.relpath(dest, root)
                if inside == ".." or inside.startswith(".." + os.sep):
                    hits.append("%s:%d: link leaves the repository: %s" % (path, number, target))
                    continue
                if not os.path.exists(dest):
                    hits.append("%s:%d: no such file: %s" % (path, number, target))
                    continue
            if anchor and dest.endswith(".md") and os.path.isfile(dest):
                if dest not in anchor_cache:
                    with open(dest, encoding="utf-8") as f:
                        anchor_cache[dest] = anchors(f.read())
                if anchor not in anchor_cache[dest]:
                    hits.append("%s:%d: no such heading (#%s) in %s" % (path, number, anchor, os.path.relpath(dest, root)))


SELF_TEST_PAGES = {
    "README.md": "[ok](docs/running.md) [bad file](docs/missing.txt) [out](../outside.txt)\n",
    "docs/running.md": (
        "# Title\n\n## Same\n\n## Same\n\n"
        "[top](#title) [first](#same) [second](#same-1) [third](#same-2) [other](backup.md#the-b-heading-with-emphasis)\n"
        "[missing](backup.md#nothing) [site](https://example.org/x.html) [code](backup.md) `[in code](missing.txt)`\n"
        "```\n[in a fence](missing.txt)\n```\n"
    ),
    "docs/backup.md": "# The `b` heading, with *emphasis*!\n",
}

SELF_TEST_WANT = [
    "README.md:1: no such file: docs/missing.txt",
    "README.md:1: link leaves the repository: ../outside.txt",
    "docs/running.md:7: no such heading (#same-2) in docs/running.md",
    "docs/running.md:8: no such heading (#nothing) in docs/backup.md",
]


def self_test():
    """Runs the check on a tree with known good and bad links, and returns 1
    when it does not report exactly the bad ones."""
    with tempfile.TemporaryDirectory() as root:
        for path, text in SELF_TEST_PAGES.items():
            os.makedirs(os.path.dirname(os.path.join(root, path)), exist_ok=True)
            with open(os.path.join(root, path), "w", encoding="utf-8") as f:
                f.write(text)
        hits = []
        cache = {}
        for path in pages(root):
            with open(os.path.join(root, path), encoding="utf-8") as f:
                check_page(root, path, f.read(), cache, hits)
    if sorted(hits) != sorted(SELF_TEST_WANT):
        print("check-doc-links self-test: got", sorted(hits), "want", sorted(SELF_TEST_WANT), file=sys.stderr)
        return 1
    print("check-doc-links self-test: the check reports the bad links and no good one")
    return 0


def main(argv):
    if argv == ["--self-test"]:
        return self_test()
    root = argv[0] if argv else "."
    hits = []
    anchor_cache = {}
    paths = pages(root)
    for path in paths:
        with open(os.path.join(root, path), encoding="utf-8") as f:
            check_page(root, path, f.read(), anchor_cache, hits)
    for h in hits:
        print(h)
    if hits:
        print("check-doc-links: %d link(s) lead nowhere" % len(hits), file=sys.stderr)
        return 1
    print("check-doc-links: every link and anchor in %d pages resolves" % len(paths))
    return 0
