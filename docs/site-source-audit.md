# drama.q-star.ink 源码与部署定位记录

检查日期：2026-10-26（云端会话，无 SSH，无站点网络访问）

## 结论

**尚未找到站点前后端的权威源码仓库，也未确认当前部署版本。** 本仓库的 `site-adapter/` 因此以独立包交付，等定位完成后再合入。

## 已检查

| 来源 | 结果 |
| --- | --- |
| 授权 GitHub 账号可见的 15 个仓库（q-star-ai、hubtiger123） | 无短剧站点前后端；`qstar-video-pipeline` 是教程旁白视频，`agent-skills` 是电商视频 Skill，`qstar-ecommerce-agent` 是电商 Agent |
| `hubtiger123/qstar-workspace` 的 `TOOLS.md` | 记录“业务机”托管 drama 业务代码；与“43 机”的对应关系未确认 |
| `https://drama.q-star.ink/` | 云端会话的网络策略拒绝连接，未能读取前端构建或接口 |
| 现有 Skill 的 `references/site-integration.md` | 依据本机发现的旧 Go 服务与网页快照，未证明为现网源码 |

## 需在有权限的机器上执行（只读）

```bash
# 43 机：找出服务进程、可执行文件与静态目录对应的源码
systemctl list-units --type=service | grep -i -E 'drama|video'
systemctl cat <service>            # ExecStart、WorkingDirectory
ls -l /proc/$(pgrep -f <binary>)/exe
nginx -T 2>/dev/null | grep -n -A20 'drama.q-star.ink'   # root 静态目录与反代上游
# 若部署目录是 git 工作区
git -C <dir> remote -v; git -C <dir> rev-parse HEAD; git -C <dir> status --short
# 二进制内的构建信息（Go 1.18+）
go version -m <binary>             # vcs.revision / vcs.modified
# 数据库迁移基线
psql "$DRAMA_DB" -c '\dt' -c "select * from schema_migrations order by 1 desc limit 5"
```

把结果（仓库 URL、commit、是否有未提交改动、数据库迁移版本）补进本文件。120 机已停用，不作为目标。
