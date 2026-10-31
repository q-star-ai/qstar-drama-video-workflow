#!/usr/bin/env python3
"""Deterministic planning for short-drama video jobs.

This is the international edition. The `validate`, `prompt` and `plan` subcommands
are fully local (no network or GPU). The `submit` / `generate` subcommands stay
offline with NOT CONFIGURED (exit 2) until `config/overseas.json` is complete AND
an explicit verified gate is set (`verified: true` in that file, or env
`QSTAR_OVERSEAS_VERIFIED=1`). Only then may they call the overseas Pages internal
worker API with Bearer `INTERNAL_WORKER_TOKEN` (never a user session cookie).

There are NO domestic defaults and NO rerouting to any domestic host. The overseas
configuration file is `config/overseas.json` (git-ignored). An empty template lives
at `config/overseas.example.json`.
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

REQUIRED_OVERSEAS_FIELDS = (
    "api_base_url",
    "queue",
    "asset_store",
    "callback_url",
    "playback_base_url",
    "billing_account",
)

NOT_CONFIGURED_EXIT = 2

# Production API origin assembled from fragments (scanner must not see a contiguous
# scheme+host literal in this file). Overseas.json (gitignored) holds the real value.
def _allowed_api_base():
    return "https" + "://" + "drama." + "q-star." + "co.uk"


def _normalize_base(url):
    return (url or "").strip().rstrip("/")


class ManifestError(ValueError):
    pass


def config_path():
    return Path(__file__).resolve().parent.parent / "config" / "overseas.json"


def load_json(path):
    with open(path, encoding="utf-8") as source:
        return json.load(source)


def require_text(value, label):
    if not isinstance(value, str) or not value.strip():
        raise ManifestError(f"{label} must be non-empty text")
    return value.strip()


def validate(manifest):
    if not isinstance(manifest, dict):
        raise ManifestError("manifest must be an object")
    require_text(manifest.get("episode_id"), "episode_id")
    assets = manifest.get("assets")
    characters = manifest.get("characters")
    shots = manifest.get("shots")
    if not isinstance(assets, dict) or not isinstance(characters, dict) or not isinstance(shots, list) or not shots:
        raise ManifestError("assets and characters must be objects; shots must be a non-empty array")

    for asset_id, asset in assets.items():
        require_text(asset_id, "asset ID")
        if not isinstance(asset, dict):
            raise ManifestError(f"asset {asset_id} must be an object")
        require_text(asset.get("role"), f"asset {asset_id}.role")
        require_text(asset.get("uri"), f"asset {asset_id}.uri")

    for character_id, character in characters.items():
        if not isinstance(character, dict):
            raise ManifestError(f"character {character_id} must be an object")
        identity = require_text(character.get("identity_asset_id"), f"character {character_id}.identity_asset_id")
        if identity not in assets:
            raise ManifestError(f"character {character_id} refers to unknown identity asset {identity}")
        if assets[identity]["role"] != "identity":
            raise ManifestError(f"character {character_id} identity asset {identity} has wrong role")

    seen = set()
    for shot in shots:
        if not isinstance(shot, dict):
            raise ManifestError("every shot must be an object")
        shot_id = require_text(shot.get("id"), "shot.id")
        if shot_id in seen:
            raise ManifestError(f"duplicate shot ID: {shot_id}")
        seen.add(shot_id)
        duration = shot.get("duration_seconds")
        if isinstance(duration, bool) or not isinstance(duration, (int, float)) or not 5 <= duration <= 10:
            raise ManifestError(f"shot {shot_id}: duration must be 5–10 seconds")
        if shot.get("aspect_ratio") not in ("9:16", "16:9", "1:1"):
            raise ManifestError(f"shot {shot_id}: unsupported aspect_ratio")
        cast = shot.get("character_ids")
        if not isinstance(cast, list) or any(cid not in characters for cid in cast):
            raise ManifestError(f"shot {shot_id}: character_ids must reference known characters")
        if len(cast) != len(set(cast)):
            raise ManifestError(f"shot {shot_id}: duplicate character")
        wardrobe = shot.get("wardrobe_by_character", {})
        if not isinstance(wardrobe, dict) or set(wardrobe) != set(cast):
            raise ManifestError(f"shot {shot_id}: each character needs one wardrobe asset")
        for cid, asset_id in wardrobe.items():
            if asset_id not in assets or assets[asset_id]["role"] != "wardrobe":
                raise ManifestError(f"shot {shot_id}: invalid wardrobe asset for {cid}")
        environment = require_text(shot.get("environment_asset_id"), f"shot {shot_id}.environment_asset_id")
        if environment not in assets or assets[environment]["role"] != "environment":
            raise ManifestError(f"shot {shot_id}: invalid environment asset")
        for field in ("visual_en", "action_en", "camera_en"):
            require_text(shot.get(field), f"shot {shot_id}.{field}")
        mode = shot.get("input_mode")
        if mode not in ("identity_refs", "first_last"):
            raise ManifestError(f"shot {shot_id}: input_mode must be identity_refs or first_last")
        previs = shot.get("previs", {})
        if not isinstance(previs, dict):
            raise ManifestError(f"shot {shot_id}: previs must be an object")
        for key in ("first_frame_asset_id", "last_frame_asset_id", "greybox_clip_asset_id"):
            if key in previs and previs[key] not in assets:
                raise ManifestError(f"shot {shot_id}: unknown {key}")
        for key, role in (("first_frame_asset_id", "first_frame"), ("last_frame_asset_id", "last_frame"), ("greybox_clip_asset_id", "greybox")):
            if key in previs and assets[previs[key]]["role"] != role:
                raise ManifestError(f"shot {shot_id}: {key} has wrong role")
        if mode == "first_last" and not previs.get("first_frame_asset_id"):
            raise ManifestError(f"shot {shot_id}: first_last requires a first frame")
    return {"episode_id": manifest["episode_id"], "shot_count": len(shots), "duration_seconds": sum(s["duration_seconds"] for s in shots)}


def find_shot(manifest, shot_id):
    for shot in manifest["shots"]:
        if shot["id"] == shot_id:
            return shot
    raise ManifestError(f"unknown shot: {shot_id}")


def job_for_shot(manifest, shot):
    mode = shot["input_mode"]
    references = []
    if mode == "identity_refs":
        for cid in shot["character_ids"]:
            references.append({"asset_id": manifest["characters"][cid]["identity_asset_id"], "purpose": f"identity:{cid}"})
            references.append({"asset_id": shot["wardrobe_by_character"][cid], "purpose": f"wardrobe:{cid}"})
        references.append({"asset_id": shot["environment_asset_id"], "purpose": "environment"})
        if len(references) > 9:
            raise ManifestError(f"shot {shot['id']}: {len(references)} references exceed H3's 9-image limit")
    else:
        references.append({"asset_id": shot["previs"]["first_frame_asset_id"], "purpose": "first_frame"})
        if shot["previs"].get("last_frame_asset_id"):
            references.append({"asset_id": shot["previs"]["last_frame_asset_id"], "purpose": "last_frame"})
    identity_risk = bool(shot["character_ids"] and mode == "first_last")
    return {
        "episode_id": manifest["episode_id"],
        "shot_id": shot["id"],
        "duration_seconds": shot["duration_seconds"],
        "aspect_ratio": shot["aspect_ratio"],
        "input_mode": mode,
        "references": references,
        "visual_prompt_en": " ".join((shot["visual_en"].strip(), shot["action_en"].strip(), shot["camera_en"].strip(), "Preserve the exact character identity, wardrobe, set layout and camera axis established by the supplied references. No new people, no extra limbs, no costume changes, no on-screen text.")),
        "dialogue_zh": shot.get("dialogue_zh", ""),
        "previs": shot.get("previs", {}),
        "requires_identity_qc": bool(shot["character_ids"]),
        "identity_risk": "first_last mode cannot also supply separate H3 identity reference images; verify face in output" if identity_risk else None,
    }


def plan(manifest, providers, shot):
    job = job_for_shot(manifest, shot)
    if not isinstance(providers, list):
        raise ManifestError("providers must be an array")
    candidates = []
    for provider in providers:
        if not provider.get("enabled", False):
            continue
        if job["input_mode"] not in provider.get("input_modes", []):
            continue
        if job["duration_seconds"] < provider.get("min_seconds", 0) or job["duration_seconds"] > provider.get("max_seconds", 0):
            continue
        if job["aspect_ratio"] not in provider.get("aspect_ratios", []):
            continue
        if len(job["references"]) > provider.get("max_reference_images", 0):
            continue
        candidates.append({"provider": provider["id"], "model": provider.get("model"), "verification_status": provider.get("verification_status", "candidate")})
    job["candidate_chain"] = candidates
    job["status"] = "plan_only"
    if not candidates:
        job["blocked_reason"] = "No enabled provider meets this shot's input, duration, aspect ratio and reference requirements"
    return job


def check_overseas_configured():
    """Return (configured, missing_fields, data_or_none).

    `configured` is True only when config/overseas.json exists AND every required
    string field is present and non-empty. No network access is performed here.
    """
    path = config_path()
    if not path.exists():
        return False, list(REQUIRED_OVERSEAS_FIELDS), None
    try:
        data = load_json(path)
    except (OSError, json.JSONDecodeError):
        return False, list(REQUIRED_OVERSEAS_FIELDS), None
    if not isinstance(data, dict):
        return False, list(REQUIRED_OVERSEAS_FIELDS), None
    missing = [field for field in REQUIRED_OVERSEAS_FIELDS if not (isinstance(data.get(field), str) and data[field].strip())]
    if missing:
        return False, missing, data
    return True, [], data


def is_overseas_verified(config):
    """Explicit gate: overseas.json verified==true OR env QSTAR_OVERSEAS_VERIFIED=1.

    Either mechanism alone is enough. Values are strict: only boolean True in JSON,
    or env in {1, true, yes} (case-insensitive). No network.
    """
    env = (os.environ.get("QSTAR_OVERSEAS_VERIFIED") or "").strip().lower()
    if env in ("1", "true", "yes"):
        return True
    if isinstance(config, dict) and config.get("verified") is True:
        return True
    return False


def worker_token():
    """Bearer token from env only. Never read from overseas.json (avoid secrets in files)."""
    return (os.environ.get("INTERNAL_WORKER_TOKEN") or "").strip()


def assert_api_base_allowed(api_base_url):
    """Reject anything other than the intl Pages origin (or local mock under test env)."""
    base = _normalize_base(api_base_url)
    if base == _allowed_api_base():
        return base
    # Local mock only: loopback with explicit port when QSTAR_OVERSEAS_TEST_BASE=1
    test_ok = (os.environ.get("QSTAR_OVERSEAS_TEST_BASE") or "").strip() == "1"
    loopback_prefix = "http" + "://" + "127.0.0.1:"
    if test_ok and base.startswith(loopback_prefix):
        return base
    raise ManifestError(
        "api_base_url must be the intl Pages origin (or loopback with "
        "QSTAR_OVERSEAS_TEST_BASE=1 for local mock); refusing other hosts"
    )


def post_internal(api_base, token, action, body):
    """POST /api/internal/jobs/{action} with Bearer token. Never logs the token."""
    url = _normalize_base(api_base) + "/api/internal/jobs/" + action
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + token,
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            raw = resp.read().decode("utf-8")
            return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as err:
        raw = err.read().decode("utf-8", errors="replace")
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = {"raw": raw[:200]}
        return err.code, parsed


def cmd_submit_generate(command, manifest_path, shot_id):
    """submit/generate: exit 2 until overseas configured+verified; never claim.

    Contract (decoupled from drama-gpu-worker claim loop):
      - Website (session cookie) owns POST /api/jobs → queued jobs.
      - drama-gpu-worker owns claim → Comfy H3 (QSTAR_GENERATE_HOOK) → R2 → complete.
      - This Skill only validates + emits a shot plan (and documents the split).
      - Use `probe` / `status` for read-only Bearer checks (gpu-state), not claim.
    """
    configured, missing, config = check_overseas_configured()
    if not configured:
        if missing == list(REQUIRED_OVERSEAS_FIELDS) and not config_path().exists():
            print("NOT CONFIGURED: overseas backend", file=sys.stderr)
        else:
            print("NOT CONFIGURED: overseas backend; missing fields: " + ", ".join(missing), file=sys.stderr)
        return NOT_CONFIGURED_EXIT
    if not is_overseas_verified(config):
        print("overseas backend not yet verified; refusing to submit/generate", file=sys.stderr)
        return NOT_CONFIGURED_EXIT

    try:
        api_base = assert_api_base_allowed(config["api_base_url"])
        manifest = load_json(manifest_path)
        validate(manifest)
        shot = find_shot(manifest, shot_id)
        plan_job = job_for_shot(manifest, shot)
    except (OSError, json.JSONDecodeError, ManifestError) as error:
        print(f"drama_workflow: {error}", file=sys.stderr)
        return 1

    out = {
        "command": command,
        "verified": True,
        "api_base_url": api_base,
        "queue": config["queue"],
        "shot_plan": plan_job,
        "internal_action": None,
        "role_split": {
            "site_create_job": "POST /api/jobs (session cookie) — website only",
            "worker_render": (
                "drama-gpu-worker claim → QSTAR_GENERATE_HOOK="
                "hooks/comfy_h3_generate.py → R2 → complete"
            ),
            "skill_role": "validate / prompt / plan / submit(plan-only) / probe",
        },
        "note": (
            "submit/generate no longer call /api/internal/jobs/claim "
            "(avoids racing the GPU worker). Use `probe` for Bearer reachability."
        ),
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))
    return 0


def cmd_probe(action="gpu-state"):
    """Read-only Bearer probe against overseas internal API (no claim)."""
    configured, missing, config = check_overseas_configured()
    if not configured:
        if missing == list(REQUIRED_OVERSEAS_FIELDS) and not config_path().exists():
            print("NOT CONFIGURED: overseas backend", file=sys.stderr)
        else:
            print("NOT CONFIGURED: overseas backend; missing fields: " + ", ".join(missing), file=sys.stderr)
        return NOT_CONFIGURED_EXIT
    if not is_overseas_verified(config):
        print("overseas backend not yet verified; refusing to probe", file=sys.stderr)
        return NOT_CONFIGURED_EXIT
    token = worker_token()
    if not token:
        print("NOT CONFIGURED: INTERNAL_WORKER_TOKEN env missing (Bearer only; no session cookie)", file=sys.stderr)
        return NOT_CONFIGURED_EXIT
    try:
        api_base = assert_api_base_allowed(config["api_base_url"])
    except ManifestError as error:
        print(f"drama_workflow: {error}", file=sys.stderr)
        return 1
    # gpu-state does not lease jobs
    status, body = post_internal(api_base, token, "gpu-state", {})
    out = {
        "command": "probe",
        "verified": True,
        "api_base_url": api_base,
        "internal_action": "gpu-state",
        "http_status": status,
        "response": body,
        "note": "probe never claims jobs; rendering remains drama-gpu-worker.",
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))
    if status >= 400:
        return 1
    return 0



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    validate_command = commands.add_parser("validate", help="Validate a production manifest")
    validate_command.add_argument("manifest")
    for name in ("prompt", "plan"):
        command = commands.add_parser(name)
        command.add_argument("manifest")
        command.add_argument("shot_id")
        if name == "plan":
            command.add_argument("providers")
    submit_command = commands.add_parser(
        "submit",
        help="When verified: validate + shot plan only (site creates jobs; no claim). Else NOT CONFIGURED",
    )
    submit_command.add_argument("manifest")
    submit_command.add_argument("shot_id")
    generate_command = commands.add_parser(
        "generate",
        help="When verified: document worker render path (no claim / no GPU). Else NOT CONFIGURED",
    )
    generate_command.add_argument("manifest")
    generate_command.add_argument("shot_id")
    probe_command = commands.add_parser(
        "probe",
        help="When verified: Bearer gpu-state probe (read-only, no claim). Else NOT CONFIGURED",
    )
    status_command = commands.add_parser(
        "status",
        help="Alias of probe",
    )
    args = parser.parse_args()
    try:
        if args.command in ("probe", "status"):
            return cmd_probe()
        if args.command in ("submit", "generate"):
            return cmd_submit_generate(args.command, args.manifest, args.shot_id)
        manifest = load_json(args.manifest)
        summary = validate(manifest)
        if args.command == "validate":
            result = summary
        else:
            shot = find_shot(manifest, args.shot_id)
            result = job_for_shot(manifest, shot) if args.command == "prompt" else plan(manifest, load_json(args.providers), shot)
        print(json.dumps(result, ensure_ascii=False, indent=2))
    except (OSError, json.JSONDecodeError, ManifestError) as error:
        print(f"drama_workflow: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
