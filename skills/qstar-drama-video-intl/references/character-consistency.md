# Character Consistency and 3D Previz

Reference sources: [the CozyClay project](https://github.com/NomaDamas/CozyClay), [its licensing note](https://github.com/NomaDamas/CozyClay/blob/main/LICENSING.md), and [a user-provided X post](https://x.com/AISuperDomain/status/2114327756532036045). As of 2026-10-26 we have reviewed the README and license; we have not yet run CozyClay in this project nor verified its rendered output against H3.

CozyClay's keyframe package can emit first/last frames, a greybox video, camera parameters and prompts. We use it as an optional previz step: lock character blocking, action and camera first, then generate the actual video. It helps with space, action and camera, but greybox characters cannot replace customer-approved faces and wardrobe.

## Per-shot input

- Character identity: a private asset ID of the approved makeup/reference shot; reuse the same ID for the same character across shots.
- Wardrobe: a private asset ID specified by story time and set; a costume change should be a scripted event, not model drift.
- Set: an asset ID for the baseline layout, lighting and prop image.
- Previz: optional first frame, last frame, greybox clip and camera parameters. Previz output requires human review and is not automatically treated as a renderable frame.
- Shot: one main action and one main camera move within 5–10 seconds; dialogue and subtitles are listed separately.

## Model input selection

| Mode | H3 input | Consistency focus |
| --- | --- | --- |
| `identity_refs` | character, wardrobe and set reference images, 1–9 total | prioritize face and clothing; previz camera info is written into the prompt, but actual spatial follow still needs QC |
| `first_last` | first frame, optionally last frame | prioritize start/end framing; cannot simultaneously submit a separate character reference image to the current H3 flow, so the face must be verified by a human |
| greybox video reference | not offered by the current H3 CLI | only connect on other providers verified to accept video references; do not pretend H3 supports it |

## QC criteria

Compare the same character shot by shot for face shape, hairline, hairstyle, makeup, and clothing color/structure; check the camera axis, character blocking, props and set. For the actual video, watch at least the start, middle and end; close-ups for the face, wide shots for wardrobe and set. Automated image similarity is only auxiliary; whether the customer's face is consistent is confirmed by a human. Label specific failures as `face_drift`, `wardrobe_drift`, `set_drift`, `camera_drift` or `audio_mismatch` for per-shot redo.

CozyClay is AGPL-3.0-or-later. This repository does not copy its code or ship a modified build; if we ever integrate or host a modified version directly, we honor its corresponding source-offer obligation first.
