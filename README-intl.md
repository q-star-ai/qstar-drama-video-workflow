# Q-Star Short-Drama Video Workflow (International Edition)

An English / international draft of the Q-Star short-drama storyboard-to-video skill for Codex, Claude Code and OpenCode. It provides deterministic local planning: character/wardrobe/set binding, CozyClay previz asset registration, provider capability filtering and an H3 fallback route. It does **not** start a GPU, submit paid generation or deploy a site on its own.

This edition is the international counterpart of `qstar-drama-video`. It does **not** wire to, and must never fall back to, any domestic production host. The overseas execution backend is a separate workstream that is **not configured** in this draft.

> **Overseas testing and deployment have NOT been done.**

## Install

From the repository root run:

```bash
./install.sh
```

The installer creates symlinks into the current user's Codex, Claude Code, OpenCode and agents skill directories for both the domestic and international skills, without overwriting any existing skill. A cloud agent clones the same repo and runs the same command, then restarts its session so the skill directory refreshes.

## Usage

Invoke `$qstar-drama-video-intl` in natural language, or run directly:

```bash
python3 skills/qstar-drama-video-intl/scripts/drama_workflow.py validate skills/qstar-drama-video-intl/examples/episode.json
python3 skills/qstar-drama-video-intl/scripts/drama_workflow.py prompt skills/qstar-drama-video-intl/examples/episode.json 01
python3 skills/qstar-drama-video-intl/scripts/drama_workflow.py plan skills/qstar-drama-video-intl/examples/episode.json 01 skills/qstar-drama-video-intl/examples/providers.json
```

`validate`, `prompt` and `plan` are fully local and deterministic. The `submit` and `generate` subcommands exist but stop immediately with `NOT CONFIGURED` (exit code 2) until the overseas backend is wired up and verified. They make no network request and have no domestic default or fallback.

### Overseas configuration

Copy the empty template and fill it only when the overseas backend is ready:

```bash
cp skills/qstar-drama-video-intl/config/overseas.example.json skills/qstar-drama-video-intl/config/overseas.json
```

`config/overseas.json` is git-ignored. The example template contains **no default values** — only the field names `api_base_url`, `queue`, `asset_store`, `callback_url`, `playback_base_url` and `billing_account`, all empty. Do not reuse domestic production data or customer assets.

`plan` only outputs candidate channels that match the input capability. The example `providers.json` demonstrates the capability shape; model versions, available inventory, pricing and identity-consistency results must be verified in the real environment. Site-side integration tasks are described in [Site Integration Notes](skills/qstar-drama-video-intl/references/site-integration.md).

The CozyClay previz idea comes from [NomaDamas/CozyClay](https://github.com/NomaDamas/CozyClay). This repository does not copy or modify its code; CozyClay itself is AGPL-3.0-or-later.

This repository contains no customer assets, keys, signed URLs or production configuration.
