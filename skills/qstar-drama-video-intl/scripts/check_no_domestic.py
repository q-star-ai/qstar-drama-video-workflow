#!/usr/bin/env python3
"""Static scan forbidding domestic hosts, URLs-as-defaults and fallback wording.

Usage:
    python3 scripts/check_no_domestic.py [paths ...]

Exit code 0 means clean. Exit code 1 means one or more forbidden patterns matched.
Exit code 2 means a requested scan target does not exist (treated as a hard error).

This script is intended as a CI check for the future PR. No PR is opened in the
draft stage; the script and this explanation are committed only.

What is forbidden (the rule, described without spelling out the exact literals so
this very file cannot self-match the scan it enforces):

  A. Domestic production hosts. Concretely:
     - a specific domestic IP on the 43.x block (ends in .128);
     - specific production domains under a q-star host (the bare host and the
       drama subdomain);
     - a specific domestic video host (video.<vendor>.top) and its vendor name;
     - a specific other vendor name;
     - a slug that joins "drama" with the 43 block number (domestic drama host).
     These are matched as exact substrings. The substrings are assembled at
     runtime from fragments (see _DOMESTIC_FRAGMENTS) so they never appear in
     plaintext in this file.

  B. Default service URLs. Any URL whose scheme is http or https that is NOT on
     the exact URL whitelist below is forbidden. Documentation may link to public
     project pages; those are whitelisted by a (file, exact-URL) pair so a link
     only counts where it is actually expected. The test fixture whose host is
     overseas.example is whitelisted ONLY inside files under a `tests/` directory.

  C. Default domestic URL (a 120-block host, dotted-quad ending in .120).

  D. Re-route wording that means "send to another host when the primary fails":
     the phrase that joins "fall" with "back to", the Chinese word for fallback,
     and a `default` + `_base_url` style field. NOTE: a standalone "fallback" that
     does not describe routing to another host (e.g. a generic UI fallback
     resolver) is permitted; only the back-to-another-host action is forbidden.

Important correctness notes (from review):
  - The domestic-string check runs on EVERY line, fully independent of the URL
    whitelist. Whitelisting a URL on a line never suppresses the domestic check.
  - A whitelisted link on the same line as a forbidden domestic string or an
    unauthorized URL still triggers a hit.
  - This scanner does NOT exclude itself; it scans its own source. It passes
    only because the forbidden literals are never written in plaintext here.
"""

import re
import sys
from pathlib import Path

# --- A. Domestic hosts, assembled from fragments so they never appear literally. ---
_DOMESTIC_FRAGMENTS = (
    ("43", ".136.99.128"),          # domestic IP on the 43 block
    ("drama.", "q-star", ".ink"),   # drama subdomain
    ("q-star", ".ink"),             # bare host
    ("video.", "ye", "guilai", ".top"), # domestic video host
    ("ye", "guilai"),               # vendor name
    ("mai", "dila"),                # another vendor name
    ("drama-", "43"),               # drama host slug
)

# Fallback phrases (also assembled to avoid literal self-match where useful).
_FALLBACK_FRAGMENTS = (
    ("fall", "back to"),   # re-route phrase "<other host>"
    ("\u56de\u9000",),     # Chinese: fallback / re-route
    ("default_", "base_url"),
)

DOMESTIC_STRINGS = ["".join(parts) for parts in _DOMESTIC_FRAGMENTS]
FALLBACK_STRINGS = ["".join(parts) for parts in _FALLBACK_FRAGMENTS]
FORBIDDEN_STRINGS = DOMESTIC_STRINGS + FALLBACK_STRINGS

STRING_RE = re.compile("|".join(re.escape(s) for s in FORBIDDEN_STRINGS), re.IGNORECASE)

URL_RE = re.compile(r"https?://[^\s\"'\)]+")

# 120-block host: dotted-quad ending in .120 (a domestic default host pattern).
HOST120_RE = re.compile(r"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.120\b")

SKIP_SUFFIXES = (".png", ".jpg", ".jpeg", ".gif", ".mp4", ".pyc")

# --- B. URL whitelist: exact URL -> set of relative file names allowed to use it. ---
_URL_WHITELIST = {
    "https://github.com/NomaDamas/CozyClay": {
        "character-consistency.md",
        "README-intl.md",
        "check_no_domestic.py",
        "test_not_configured.py",
        "sample_doc.md",
    },
    "https://github.com/NomaDamas/CozyClay/blob/main/LICENSING.md": {
        "character-consistency.md",
        "check_no_domestic.py",
    },
    "https://x.com/AISuperDomain/status/2114327756532036045": {
        "character-consistency.md",
        "check_no_domestic.py",
    },
    # Test-only fixture; whitelisted solely inside files under tests/, plus this
    # scanner's own source (which only references it as a rule definition).
    "https://overseas.example/api": {"__TESTS_ONLY__", "check_no_domestic.py"},
}


def _file_key(path):
    """Return (basename, is_tests) for whitelist decisions."""
    parts = path.parts
    basename = path.name
    is_tests = "tests" in parts
    return basename, is_tests


def _url_allowed(url, path):
    allowed_files = _URL_WHITELIST.get(url)
    if not allowed_files:
        return False
    basename, is_tests = _file_key(path)
    if basename in allowed_files:
        return True
    if "__TESTS_ONLY__" in allowed_files:
        return is_tests
    return False


def scan_file(path):
    hits = []
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError as error:
        return [(0, f"cannot read: {error}")]
    for lineno, line in enumerate(text.splitlines(), 1):
        # Domestic strings: checked on EVERY line, independent of whitelist.
        if STRING_RE.search(line):
            hits.append((lineno, "forbidden domestic/fallback string: " + line.strip()))
        if HOST120_RE.search(line):
            hits.append((lineno, "matched 120-host address: " + line.strip()))
        # URL check: whitelisted only by exact (file, URL) pair.
        for match in URL_RE.findall(line):
            if not _url_allowed(match, path):
                hits.append((lineno, "default URL not whitelisted: " + match))
    return hits


def main():
    roots = sys.argv[1:]
    if not roots:
        roots = ["."]
    total = 0
    missing = 0
    for root in roots:
        base = Path(root)
        if not base.exists():
            print(f"ERROR: scan target does not exist: {root}", file=sys.stderr)
            missing += 1
            continue
        files = [base] if base.is_file() else [
            p for p in base.rglob("*") if p.is_file() and p.suffix not in SKIP_SUFFIXES
        ]
        for path in files:
            if any(part in (".git", "__pycache__", "generated", "customer-assets", "secrets") for part in path.parts):
                continue
            for lineno, detail in scan_file(path):
                total += 1
                where = path if lineno == 0 else f"{path}:{lineno}"
                print(f"FORBIDDEN {where}: {detail}")
    if missing:
        print(f"\n{missing} scan target(s) missing.", file=sys.stderr)
        return 2
    if total:
        print(f"\n{total} forbidden match(es) found.")
        return 1
    print("clean: 0 forbidden matches")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
