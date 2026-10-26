# 短剧站点统一视频任务适配器

供 drama.q-star.ink 后端（Go）与分镜前端接入的统一视频任务实现。它是独立、可测试的包，站点侧接入已基于 `qstar-drama-site-recovery` 历史快照完成（该仓库 `docs/VIDEO_JOB_INTEGRATION.md`），站点内的 `pkg/videojob` 是本目录的副本；线上后端版本仍未确认（见 [源码定位记录](../docs/site-source-audit.md)）。不要把它直接拷进 43 机上的静态构建目录。

## 组成

| 路径 | 作用 |
| --- | --- |
| `videojob/capability.go` | 服务端能力表与候选链。`H3Capability` 声明 H3 的参考图模式与首尾帧模式互斥、无灰盒视频输入 |
| `videojob/request.go` | 从分镜行绑定客户批准的人脸、服装、场景素材（资产 ID + 版本 + sha256，拒绝签名 URL）和 CozyClay 预演首尾帧/灰盒片/相机参数 |
| `videojob/job.go` | VideoJob 状态机、尝试记录、切换事件、ffprobe 结果、人工质检；`MemStore` |
| `videojob/runner.go` | worker 每次推进一步：已有外部任务 ID 只轮询不重提；提交前先落库，崩溃后进入 `needs_reconcile`；安全提示词重试与供应商切换分开记录；费用上限 |
| `videojob/h3.go` | MiniMax H3 GPU 保底通道：`prompt_id` 续查、超时、按 GPU 时长计费、只有实例所有者且无其他任务时才停机 |
| `videojob/assets.go` | 用临时签名地址下载素材并校验 sha256；与批准版本不符即拒绝提交 |
| `videojob/h3cli.go` | 包装 `minimax-h3-video` Skill 的 `h3-cloud generate`：参考图与首尾帧互斥、无灰盒输入；每次尝试独立输出目录，读取 CLI 创建的子目录中 `manifest.json` 的 `prompt_id`、`artifact_path`；进程丢失、异常退出或超时进入 `needs_reconcile`；只有 `status.state` 确认为 `stopped` 才标记成功。跨进程自动续查入口仍待实现 |
| `videojob/probe.go` | 解析 `ffprobe` 并对照时长/画幅/音轨 |
| `videojob/httpapi.go` | `GET capabilities`、`POST plan`、`POST jobs`（必须带 `Idempotency-Key`）、`GET jobs/{id}`；只入队，不在 Web 请求里跑生成 |
| `videojob/sqlstore.go`、`migrations/001_*` | Postgres 存储与增量迁移，供独立部署参考。短剧站点用 GORM（MySQL/SQLite），站点内改用 `GormJobStore` 与 AutoMigrate，不使用这两个文件 |
| `web/videoJobForm.ts` | 前端按能力表渲染输入模式；H3 下灰盒选项显示禁用及原因；首尾帧模式提示需人工核脸；任务卡显示实际模型、任务 ID、费用、切换和质检 |

## 接入边界

站点快照已把核心代码复制到 `pkg/videojob`，并使用 GORM AutoMigrate、新的 `/api/v1/video-jobs` 路由和独立 H3 worker。站点接入、数据库与上线步骤以私有站点仓库的 `docs/VIDEO_JOB_INTEGRATION.md` 为准。此处的 Postgres 迁移和 `httpapi.go` 仅供独立适配器使用，**不得作为站点迁移直接运行**。真实 H3 调用及线上部署尚未验收。

本目录的数据结构记录探测结果和核对状态；探测退避、管理员核对和积分退款属于站点服务实现，本适配器不独立执行这些动作。有意重生成应使用新的请求编号；不确定提交是否成功时需保留原编号重试。

## 验证

```bash
cd site-adapter
go vet ./... && go test ./...                       # Postgres 测试未设 DSN 时跳过
QSTAR_TEST_PG_DSN=postgres://user@127.0.0.1:5432/scratch?sslmode=disable go test ./...  # 已迁移的临时库
node --experimental-strip-types --test web/videoJobForm.test.ts
```

所有测试使用假 provider/假 H3 运行时，不调用真实模型，不产生费用。
