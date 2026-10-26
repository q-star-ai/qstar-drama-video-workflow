# 短剧站点统一视频任务适配器

供 drama.q-star.ink 后端（Go）与分镜前端接入的统一视频任务实现。它是独立、可测试的包，**尚未合入站点源码**：站点权威仓库与部署版本还没确认（见 [源码定位记录](../docs/site-source-audit.md)）。不要把它直接拷进 43 机上的静态构建目录。

## 组成

| 路径 | 作用 |
| --- | --- |
| `videojob/capability.go` | 服务端能力表与候选链。`H3Capability` 声明 H3 的参考图模式与首尾帧模式互斥、无灰盒视频输入 |
| `videojob/request.go` | 从分镜行绑定客户批准的人脸、服装、场景素材（资产 ID + 版本 + sha256，拒绝签名 URL）和 CozyClay 预演首尾帧/灰盒片/相机参数 |
| `videojob/job.go` | VideoJob 状态机、尝试记录、切换事件、ffprobe 结果、人工质检；`MemStore` |
| `videojob/runner.go` | worker 每次推进一步：已有外部任务 ID 只轮询不重提；提交前先落库，崩溃后进入 `needs_reconcile`；安全提示词重试与供应商切换分开记录；费用上限 |
| `videojob/h3.go` | MiniMax H3 GPU 保底通道：`prompt_id` 续查、超时、按 GPU 时长计费、只有实例所有者且无其他任务时才停机 |
| `videojob/probe.go` | 解析 `ffprobe` 并对照时长/画幅/音轨 |
| `videojob/httpapi.go` | `GET capabilities`、`POST plan`、`POST jobs`（必须带 `Idempotency-Key`）、`GET jobs/{id}`；只入队，不在 Web 请求里跑生成 |
| `videojob/sqlstore.go`、`migrations/001_*` | Postgres 存储与增量迁移；旧 VideoGeneration 通过 `legacy_video_generation_id` 关联，不改旧表 |
| `web/videoJobForm.ts` | 前端按能力表渲染输入模式；H3 下灰盒选项显示禁用及原因；首尾帧模式提示需人工核脸；任务卡显示实际模型、任务 ID、费用、切换和质检 |

## 接入步骤（站点源码确认后）

1. 在站点 Go 服务中引入本包（同仓库 vendor 或 `go get` 本仓库的 `site-adapter` 模块），执行 `migrations/001_video_jobs.up.sql`。先在数据库快照上演练，记录迁移前版本。
2. 在路由中 `(&videojob.API{...}).Routes(mux, "/api/video")`，能力表从服务端配置读取。把旧网页里写死的 `provider: "doubao"` 改为调用 `/api/video/plan` 的候选链。
3. 旧 `generateVideoWithFailover` 的上游错误分类映射到 `ProviderError.Class`：参数/素材错误 → `invalid_input`；审核拒绝 → `content_safety`；限流/无资源 → `capacity`；网络 → `transient`；提交请求发出但未拿到响应 → `submit_unknown`（禁止自动重提）。把现有豆包调用包成 `Provider`。
4. 独立 worker 进程循环取 `queued/running` 任务调用 `Runner.Step`。H3 worker 用已安装的 `minimax-h3-video` Skill 实现 `H3Runtime`（启动/提交/状态/下载入库/停机），凭证只放在 worker 的受限配置里。
5. 成片入统一资产存储后跑 `ffprobe -v error -print_format json -show_format -show_streams`，`ParseFFprobe` → `CheckOutput` → `RecordProbe`；人工按开头/中段/结尾质检后 `RecordQC`。只有 `succeeded` 才在前端显示完成。

## 验证

```bash
cd site-adapter
go vet ./... && go test ./...                       # Postgres 测试未设 DSN 时跳过
QSTAR_TEST_PG_DSN=postgres://user@127.0.0.1:5432/scratch?sslmode=disable go test ./...  # 已迁移的临时库
node --experimental-strip-types --test web/videoJobForm.test.ts
```

所有测试使用假 provider/假 H3 运行时，不调用真实模型，不产生费用。
