---
name: qstar-drama-video
description: Plan, generate and review Q-Star short-drama shots with locked character assets, scene previsualization, provider routing and H3 GPU fallback. Use for storyboard-to-video work; not for narrated tutorial videos.
---

# Q-Star 短剧视频工作流

先确定本次工作的剧本版本、分镜、客户批准的人物参考图和场景资产。每镜维持 5–10 秒，人物身份、服装、场景、台词、机位分别绑定，不从上一镜生成结果反推这些基准。客户指定照片不可提交到公开仓库。

1. 建立一份逐镜 manifest。角色 `identity_asset_id` 是客户批准的定妆照；每镜 `wardrobe_by_character` 固定服装；`environment_asset_id` 固定场景。参考图只以资产 ID/私有 URI 写入 manifest，实际签名 URL 由生产系统临时生成。用本 Skill 的 `scripts/drama_workflow.py validate <manifest.json>` 做确定性校验；示例在 `examples/`。
2. 镜头运动复杂或跨镜站位易漂移时，先用 CozyClay 或同等预演工具固定角色站位、运动、动作轴、道具和摄影机。把首尾帧、灰盒参考片、相机数据登记到 `previs`。CozyClay 只用于镜头与空间预演；脸、妆发与服装仍以客户素材锁定。参见 [人物一致性与预演](references/character-consistency.md)。
3. 视觉提示词用清楚的英文描述主体、动作、画面、光线和运镜；中文对白单列为 `dialogue_zh`，供原声或后期配音。不要要求生成模型精确执行每一秒；逐秒表用于拍摄意图、剪辑和验收。用 `prompt` 子命令输出镜头输入包。
4. 根据供应商实际能力与可用性，用 `plan` 子命令筛选候选通道。它只生成计划，不启动 GPU 或提交生成。上线路由时须记录实际 provider、模型、任务 ID、参考素材、费用及每次切换。已拿到任务 ID 时先恢复/轮询，避免重复付费；提交结果未知时先人工核对供应商后台，不自动重提。站点端的统一任务实现在仓库 `site-adapter/`。
5. H3 的角色参考图模式与首尾帧模式互斥。`identity_refs` 用客户身份、服装和场景图；`first_last` 用预演关键帧，但没有额外的人脸参考输入，必须标高人物身份风险并逐镜检查。灰盒视频参考对应 `video_ref` 模式，只在能力已验证的其他模型使用；H3 不声明该模式，计划会如实显示无可用通道。H3 具体启停与参数按已安装的 `minimax-h3-video` Skill 执行。
6. 视频生成后测实际时长、比例、音轨，逐镜抽查开头/中段/结尾的脸、发型、服装、场景、人物数量、镜头轴和对白。失败就标记原因并重做该镜；不要把素材缺失或身份漂移标成成功。
7. 合成字幕、配乐、对白及成片时沿用已批准脚本。网暴评论类段落要按时间线混音、字幕同步与高潮截止点复查。

Q-Star 站点每次有意重生成单独计费；成功生成可播放视频后，满意与否均不退积分，确认技术故障未成功生成则退还已扣积分。预算应计入重做次数，前期可用约 10 次作规划假设并以实际通过率修正。提交结果不明时保留原请求编号，先核对，勿重复扣费。站点实现细节见 [站点接入说明](references/site-integration.md)。

本 Skill 是原始 Q-Star 工作流指令，不包含 CozyClay 代码或客户素材。首次部署网站适配器前读 [站点接入说明](references/site-integration.md)。
