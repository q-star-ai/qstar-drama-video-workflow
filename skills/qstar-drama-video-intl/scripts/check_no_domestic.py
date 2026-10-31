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

# --- E. Narrow exemptions (Tiger 2026-10-31 14:57). ---
# E1. Git-ignored files (e.g. a local config/overseas.json) are never shipped,
#     so a DIRECTORY walk skips them and lists them as skipped. A file named
#     explicitly on the command line is still scanned. Tracked files are never
#     skipped.
# E2. Tracked example configs (`*.example.json`) may (a) carry a `"_comment"`
#     line that names forbidden hosts as a warning, and (b) use loopback URLs
#     (127.0.0.1 / localhost) as example values. Every other line/URL in those
#     files, and every line in all other files, is checked exactly as before.
_EXAMPLE_COMMENT_RE = re.compile(r'^\s*"_comment"\s*:')
_LOOPBACK_URL_RE = re.compile(r"(?i)^https?://(127\.0\.0\.1|localhost)(:\d+)?(/[^\s\"']*)?$")


def _is_example_config(path):
    return path.name.lower().endswith(".example.json")


def _git_ignored(path):
    import subprocess
    try:
        proc = subprocess.run(["git", "-C", str(path.resolve().parent), "check-ignore", "-q",
                               "--no-index", str(path.resolve())],
                              capture_output=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return False
    if proc.returncode != 0:
        return False
    # Never treat a tracked file as ignored.
    tracked = subprocess.run(["git", "-C", str(path.resolve().parent), "ls-files", "--error-unmatch",
                              str(path.resolve())], capture_output=True, timeout=10)
    return tracked.returncode != 0

# --- B. URL whitelist, split by PURPOSE (R5). ---
# URLs are built from fragments so this source file itself contains no URL
# literal (the scanner scans itself and must stay clean without self-exclusion).
_H = "https" + "://"
_P = "skills/qstar-drama-video-intl/"

# B1. Public documentation links. Allowed ONLY when ALL hold:
#   - the file is a Markdown document (.md) listed for that URL;
#   - the URL is a Markdown link target `](URL)` (autolinks/bare URLs rejected);
#   - the line has no configuration/default-value context (see CONFIG_CONTEXT_RE).
# Anything else (assignment, JSON value, code constant, bare URL) is treated as a
# default service address and reported.
_DOC_LINKS = {
    _H + "github.com/NomaDamas/CozyClay": {
        _P + "references/character-consistency.md",
        "README-intl.md",
    },
    _H + "github.com/NomaDamas/CozyClay/blob/main/LICENSING.md": {
        _P + "references/character-consistency.md",
    },
    _H + "x.com/AISuperDomain/status/2114327756532036045": {
        _P + "references/character-consistency.md",
    },
}

# B2. Test fixtures: allowed only inside the intl skill's own tests/ directory
# (scanner regression fixtures; never shipped as runtime defaults).
_TEST_FIXTURE_URLS = set(_DOC_LINKS) | {_H + "overseas.example/api"}

# Configuration / default-value context: key names or assignment syntax that
# would turn a URL into a service address.
CONFIG_CONTEXT_RE = re.compile(
    r"(?i)(api_base_url|base_url|baseurl|endpoint|host|callback_url|playback_base_url"
    r"|asset_store|queue|billing_account|default|url\s*[:=]|[=:]\s*[\"']?\s*h[t]tps?://)"
)

# Kept for backward compatibility with callers/tests that inspect the table.
_URL_WHITELIST = {u: set(v) for u, v in _DOC_LINKS.items()}
_URL_WHITELIST[_H + "overseas.example/api"] = {"__TESTS_ONLY__"}


_SKILL_ROOT = Path(__file__).resolve().parent.parent
_REPO_ROOT = _SKILL_ROOT.parent.parent


def _rel_path(path, root=None):
    """Repo-relative POSIX path (single fixed anchor, so no cross-root collision).

    Skill files map to `skills/qstar-drama-video-intl/...`, repo files to e.g.
    `README-intl.md`; anything outside the repo maps to its absolute path and
    is never whitelisted. `root` is accepted for compatibility and ignored.
    """
    real = path.resolve()
    try:
        return real.relative_to(_REPO_ROOT).as_posix()
    except ValueError:
        return real.as_posix()


def _in_skill_tests(path):
    return path.resolve().is_relative_to(_SKILL_ROOT / "tests")


def _url_allowed(url, norm_rel, path, line=None, ctx=None):
    """Purpose-aware whitelist check.

    `line` is the full source line. When omitted (legacy callers) the doc-link
    purpose cannot be proven, so only the test-fixture purpose can allow it.
    """
    if url in _TEST_FIXTURE_URLS and _in_skill_tests(path):
        return True
    allowed = _DOC_LINKS.get(url)
    if not allowed or norm_rel not in allowed:
        return False
    if path.suffix.lower() != ".md" or line is None:
        return False
    if CONFIG_CONTEXT_RE.search(line):
        return False
    if ctx is None or ctx.get("in_fence") or ctx.get("prev_config"):
        return False
    start = ctx.get("start")
    if start is None:
        return False
    # Bind to THIS occurrence: must be `](URL)` at this exact position.
    if line[max(0, start - 2):start] != "](" or line[start + len(url):start + len(url) + 1] != ")":
        return False
    lb = line.rfind("[", 0, start)
    if lb < 0:
        return False
    prefix = line[:lb]
    # Not inside inline code / string literal / assignment.
    if any(ch in prefix for ch in "`\"'=") or line.count("`") % 2:
        return False
    return True


def scan_file(path, norm_rel):
    hits = []
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError as error:
        # An inspected target that cannot be read is a hard failure: report it
        # as a hit and force a non-zero exit (never silently passed).
        return [(0, f"cannot read file: {error}")]
    return scan_text(text, path, norm_rel)


def scan_text(text, path, norm_rel):
    hits = []
    in_fence = False
    prev_config = False
    for lineno, line in enumerate(text.splitlines(), 1):
        stripped = line.strip()
        example_comment = _is_example_config(path) and bool(_EXAMPLE_COMMENT_RE.match(line))
        fence_line = stripped.startswith("```") or stripped.startswith("~~~")
        # Domestic strings: checked on EVERY line, independent of whitelist.
        if STRING_RE.search(line) and not example_comment:
            hits.append((lineno, "forbidden domestic/fallback string: " + line.strip()))
        if HOST120_RE.search(line):
            hits.append((lineno, "matched 120-host address: " + line.strip()))
        # URL check: whitelisted only by exact (normalized relative path, URL).
        for m in URL_RE.finditer(line):
            if _is_example_config(path) and _LOOPBACK_URL_RE.match(m.group(0)):
                continue
            ctx = {"start": m.start(), "in_fence": in_fence or fence_line,
                   "prev_config": prev_config}
            if not _url_allowed(m.group(0), norm_rel, path, line, ctx):
                hits.append((lineno, "default URL not whitelisted: " + m.group(0)))
        if fence_line:
            in_fence = not in_fence
        # Conservative cross-line config context: a key line (`key:` / `key =`)
        # or any config keyword taints the next non-blank line.
        if stripped:
            prev_config = bool(CONFIG_CONTEXT_RE.search(line)) or stripped.endswith((":", "=", "{", "[", ","))
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
        if _git_ignored(path):
            yield ("SKIP", path, "git-ignored")
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
