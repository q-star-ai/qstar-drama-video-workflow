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

  B. Default service URLs. Any URL whose scheme is http or https (case of the
     scheme is ignored, so HTTPS/Http count too) that is NOT on the exact URL
     whitelist below is forbidden. A URL is whitelisted only by an exact
     (normalized relative path, exact URL) pair: the path is the POSIX relative
     path from the scan root after resolving `..` components, so a file reached
     via `tests/../config/service.py` is its real location (`config/service.py`)
     and is NOT treated as a tests file. Documentation may link to public project
     pages; those are whitelisted by a (file, exact-URL) pair so a link only
     counts where it is actually expected. The test fixture whose host is
     overseas.example is whitelisted ONLY inside files whose normalized relative
     path begins with `tests/`.

  C. Default domestic URL (a 120-block host, dotted-quad ending in .120).

  D. Re-route wording that means "send to another host when the primary fails":
     the phrase that joins "fall" with "back to", the Chinese word for fallback,
     and a `default` + `_base_url` style field. NOTE: a standalone "fallback" that
     does not describe routing to another host (e.g. a generic UI fallback
     resolver) is permitted; only the back-to-another-host action is forbidden.

 Important correctness notes (from review R3):
   - The domestic-string check runs on EVERY line, fully independent of the URL
     whitelist. Whitelisting a URL on a line never suppresses the domestic check.
   - A whitelisted link on the same line as a forbidden domestic string or an
     unauthorized URL still triggers a hit.
   - This scanner does NOT exclude itself; it scans its own source. It passes
     only because the forbidden literals are never written in plaintext here.
   - Whitelisting is by normalized relative path + exact URL. A path such as
     `tests/../config/service.py` normalizes to `config/service.py` and is not
     treated as a tests file.
   - Only `.git` and `__pycache__` directories are silently skipped, and they
     are listed in the output. No inspected source file or generated/secrets
     directory is skipped. Any inspected target that cannot be read is reported
     as a hit and forces a non-zero exit.
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

# Scheme is matched case-insensitively so HTTPS/Http/HttP all count.
URL_RE = re.compile(r"[Hh][Tt][Tt][Pp][Ss]?://[^\s\"'\)]+")

# 120-block host: dotted-quad ending in .120 (a domestic default host pattern).
HOST120_RE = re.compile(r"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.120\b")

SKIP_DIRECTORIES = (".git", "__pycache__")

# --- B. URL whitelist: exact URL -> set of normalized relative paths allowed to
#         use it. The path is the POSIX relative path from the scan root after
#         `..` normalization, NOT a basename. A special marker `__TESTS_ONLY__`
#         means the URL is allowed only inside files whose normalized relative
#         path starts with `tests/`. ---
_URL_WHITELIST = {
    "https://github.com/NomaDamas/CozyClay": {
        "references/character-consistency.md",
        "README-intl.md",
        "scripts/check_no_domestic.py",
        "tests/test_not_configured.py",
    },
    "https://github.com/NomaDamas/CozyClay/blob/main/LICENSING.md": {
        "references/character-consistency.md",
        "scripts/check_no_domestic.py",
    },
    "https://x.com/AISuperDomain/status/2114327756532036045": {
        "references/character-consistency.md",
        "scripts/check_no_domestic.py",
    },
    # Test-only fixture; whitelisted solely inside files under tests/, plus this
    # scanner's own source (which references it only as a whitelist key, never as
    # a real default).
    "https://overseas.example/api": {"__TESTS_ONLY__", "scripts/check_no_domestic.py"},
}


_SKILL_ROOT = Path(__file__).resolve().parent.parent
_REPO_ROOT = _SKILL_ROOT.parent.parent


def _rel_path(path, root=None):
    """Normalized POSIX path used for whitelist lookup, anchored at FIXED roots.

    The result does not depend on the scan arguments or the cwd: files inside
    the intl skill map to skill-relative paths (e.g. `tests/x.py`), other files
    inside the repo map to repo-relative paths (e.g. `README-intl.md`), and
    anything outside the repo maps to its absolute path (never whitelisted).
    `root` is accepted for call compatibility and ignored.
    """
    real = path.resolve()
    for anchor in (_SKILL_ROOT, _REPO_ROOT):
        try:
            return real.relative_to(anchor).as_posix()
        except ValueError:
            continue
    return real.as_posix()


def _url_allowed(url, norm_rel, path):
    allowed = _URL_WHITELIST.get(url)
    if not allowed:
        return False
    in_skill_tests = path.resolve().is_relative_to(_SKILL_ROOT / "tests")
    if "__TESTS_ONLY__" in allowed and in_skill_tests:
        return True
    return norm_rel in allowed


def scan_file(path, norm_rel):
    hits = []
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError as error:
        # An inspected target that cannot be read is a hard failure: report it
        # as a hit and force a non-zero exit (never silently passed).
        return [(0, f"cannot read file: {error}")]
    for lineno, line in enumerate(text.splitlines(), 1):
        # Domestic strings: checked on EVERY line, independent of whitelist.
        if STRING_RE.search(line):
            hits.append((lineno, "forbidden domestic/fallback string: " + line.strip()))
        if HOST120_RE.search(line):
            hits.append((lineno, "matched 120-host address: " + line.strip()))
        # URL check: whitelisted only by exact (normalized relative path, URL).
        for match in URL_RE.findall(line):
            if not _url_allowed(match, norm_rel, path):
                hits.append((lineno, "default URL not whitelisted: " + match))
    return hits


def _iter_files(root):
    """Yield (path, norm_rel) for every regular file under root.

    Only `.git` and `__pycache__` directories are skipped, and they are yielded
    as skip entries instead of being silently ignored. `norm_rel` is always
    relative to the original `root`.
    """
    base = Path(root)
    base_resolved = base.resolve()
    if base.is_file() or base.is_symlink():
        yield base, _rel_path(base, base)
        return
    for path in sorted(base.rglob("*")):
        if not (path.is_file() or path.is_symlink()):
            continue
        parts = path.parts
        skip_dir = next((d for d in SKIP_DIRECTORIES if d in parts), None)
        if skip_dir:
            yield ("SKIP", path, skip_dir)
            continue
        yield (path, _rel_path(path, base))


def main():
    roots = sys.argv[1:]
    if not roots:
        roots = ["."]
    total = 0
    missing = 0
    skipped = []
    errored = 0
    for root in roots:
        base = Path(root)
        if not (base.exists() or base.is_symlink()):
            print(f"ERROR: scan target does not exist: {root}", file=sys.stderr)
            missing += 1
            continue
        for entry in _iter_files(base):
            if entry[0] == "SKIP":
                _, path, skip_dir = entry
                skipped.append(f"{path} (skipped: {skip_dir})")
                continue
            path, norm_rel = entry
            for lineno, detail in scan_file(path, norm_rel):
                total += 1
                if lineno == 0:
                    errored += 1
                where = path if lineno == 0 else f"{path}:{lineno}"
                print(f"FORBIDDEN {where}: {detail}")
    if skipped:
        print("Skipped directories/files (allowed):")
        for item in skipped:
            print("  - " + item)
    if missing:
        print(f"\n{missing} scan target(s) missing.", file=sys.stderr)
        return 2
    if errored:
        print(f"\n{errored} inspected target(s) could not be read.", file=sys.stderr)
        return 1
    if total:
        print(f"\n{total} forbidden match(es) found.")
        return 1
    print("clean: 0 forbidden matches")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
