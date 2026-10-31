---
name: qstar-drama-video-intl
description: International edition of the Q-Star short-drama storyboard-to-video skill. Provides local, deterministic validate / prompt / plan for locked character assets, scene previsualization and provider routing. submit / generate stay offline until overseas.json is verified; they emit a shot plan only (website creates jobs; drama-gpu-worker renders). Use probe/status for Bearer reachability without claiming. Use for storyboard-to-video planning; not for narrated tutorial videos.
---

# Q-Star Short-Drama Video Workflow (International Edition)

This is the English / international draft of the Q-Star short-drama workflow. It mirrors the local planning behavior of the domestic edition but is explicitly **not** wired to any domestic production host, and its overseas backend is not configured yet.

For every job, establish the script version, shot breakdown, customer-approved character reference images, and scene assets. Keep each shot 5–10 seconds. Bind character identity, wardrobe, set, dialogue and camera axis per shot; do not reverse-engineer these baselines from prior generated frames. Customer-supplied photos must never be committed to a public repository.

1. Build a per-shot manifest. The character `identity_asset_id` is the customer-approved makeup/reference shot; each shot's `wardrobe_by_character` fixes the outfit; `environment_asset_id` fixes the set. Reference images are written into the manifest only as asset IDs / private URIs; the actual signed URLs are generated temporarily by the production system. Validate deterministically with this Skill's `scripts/drama_workflow.py validate <manifest.json>`; see the `examples/` folder.
2. When camera movement is complex or cross-shot blocking tends to drift, first lock character blocking, motion, action axis, props and camera with CozyClay or an equivalent previz tool. Register the first/last frames, greybox reference clip and camera data under `previs`. CozyClay is used only for shot and spatial previz; face, hair/makeup and wardrobe remain locked by customer assets. See [Character Consistency and Previz](references/character-consistency.md).
3. Write visual prompts in clear English describing subject, action, frame, light and camera. List Chinese dialogue separately as `dialogue_zh` for original audio or later dubbing. Do not require the generation model to execute every second precisely; the per-second table is for shooting intent, editing and acceptance. Use the `prompt` subcommand to emit the shot input package.
4. Filter candidate channels with the `plan` subcommand based on each provider's actual capability and availability. It only produces a plan; it does not start a GPU or submit generation. When routing to production you must record the actual provider, model, task ID, reference material, cost and every switch. If you already hold a task ID, resume/poll first to avoid duplicate paid submissions.
5. H3's identity-reference mode and first/last-frame mode are mutually exclusive. `identity_refs` uses customer identity, wardrobe and set images; `first_last` uses previz keyframes but, lacking an extra face-reference input, must flag character-identity risk and be checked shot by shot. Greybox video export must not be assumed to be accepted as a video reference by H3; use it only on other models whose capability is verified. **Render path**: `drama-gpu-worker` + `QSTAR_GENERATE_HOOK=/workspace/work/drama-gpu-worker/hooks/comfy_h3_generate.py` (ComfyUI MiniMax-H3: `identity_refs`→r2v, `first_last`→i2v, no refs→t2v). Do **not** depend on an installed `minimax-h3-video` Skill.
6. After video generation, measure actual duration, aspect ratio and audio track; spot-check each shot's face, hair, wardrobe, set, headcount, camera axis and dialogue at the start, middle and end. On failure, mark the reason and redo that shot; never label missing assets or identity drift as success.
7. When compositing subtitles, score, dialogue and final cut, reuse the approved script. For cyberbullying-comment segments, re-check timeline mixing, subtitle sync and the climax cut-off point.

**Role split (no claim racing):**
- Website: `POST /api/jobs` (session cookie) creates queued jobs.
- `drama-gpu-worker`: claim → Comfy H3 hook → R2 → complete.
- This Skill: `validate` / `prompt` / `plan`; when overseas is verified, `submit`/`generate` emit the shot plan only (no `/claim`); `probe`/`status` Bearer-calls `gpu-state` for reachability.

This Skill is the raw Q-Star workflow instruction set; it contains no CozyClay code or customer assets. Before the first website adapter deployment, read [Site Integration Notes](references/site-integration.md).

> **Overseas Pages deploy and GPU power-on are still NOT done.** Without `verified: true` / `QSTAR_OVERSEAS_VERIFIED=1`, `submit`/`generate`/`probe` exit 2 with zero network. Do not reuse domestic production data or customer assets.
