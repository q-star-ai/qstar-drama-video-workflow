#!/usr/bin/env python3
"""Deterministic planning for short-drama video jobs. No network or GPU calls."""

import argparse
import json
import sys
from pathlib import Path


class ManifestError(ValueError):
    pass


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
    args = parser.parse_args()
    try:
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
