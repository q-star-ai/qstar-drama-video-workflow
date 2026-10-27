# Q-Star 短剧视频工作流

供 Codex、Claude Code、OpenCode 调用的短剧分镜到视频 Skill。它提供人物/服装/场景绑定、CozyClay 预演资产登记、模型能力筛选和 H3 保底路线的确定性计划工具。它**不会**自行启动 GPU、提交付费生成或部署网站。

## 安装

从仓库根目录运行：

```bash
./install.sh
```

安装器为当前用户的 Codex、Claude Code、OpenCode、OpenClaw 技能目录创建指向本仓库 Skill 的软链，不覆盖已有技能。云端 agent 克隆同一仓库后也运行该命令，再重开 agent 会话使技能目录更新生效。在本仓库内工作的 agent 无需安装：`.claude/skills/` 与 `.agents/skills/` 已指向同一 Skill。

## 使用

自然语言调用 `$qstar-drama-video`，或直接运行：

```bash
python3 skills/qstar-drama-video/scripts/drama_workflow.py validate skills/qstar-drama-video/examples/episode.json
python3 skills/qstar-drama-video/scripts/drama_workflow.py prompt skills/qstar-drama-video/examples/episode.json 01
python3 skills/qstar-drama-video/scripts/drama_workflow.py plan skills/qstar-drama-video/examples/episode.json 01 skills/qstar-drama-video/examples/providers.json
```

`plan` 只输出符合输入能力的候选通道。示例 `providers.json` 是能力形状演示，模型版本、可用库存、价格和身份一致性效果都需要在实际环境核实。生产网站的接入任务见 [站点接入说明](skills/qstar-drama-video/references/site-integration.md)，统一视频任务实现见 [site-adapter](site-adapter/README.md)。

CozyClay 预演思路来自 [NomaDamas/CozyClay](https://github.com/NomaDamas/CozyClay)。本仓库没有复制或修改其代码，CozyClay 自身使用 AGPL-3.0-or-later。

本仓库不包含客户素材、密钥、签名 URL 或生产配置。
