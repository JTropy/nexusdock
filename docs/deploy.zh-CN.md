# NexusDock 生产部署 Runbook

面向生产站点的一次性迁移、日常部署、部署后验证、回滚与清理。所有命令都在生产宿主机上由人执行；文中路径均为示例占位，按站点实际替换。

唯一结构源原则：仓库 `docker-compose.yml` 是唯一的 Compose 结构定义，生产不得手工维护第二份 Compose；站点差异（数据路径、端口、公网地址、Trusted Proxies、镜像标签）只写在站点 env 文件里。

## 0. 站点文件约定

以下用 `$NEXUS_HOME` 表示生产站点根目录（示例 `/srv/nexusdock`，按实际替换）：

| 路径 | 用途 |
| --- | --- |
| `$NEXUS_HOME/repo` | 仓库 checkout，部署时切到目标提交 |
| `$NEXUS_HOME/nexusdock.env` | 站点 env 文件（从仓库 `.env.example` 复制，`chmod 600`，不进仓库） |
| `$NEXUS_HOME/nexus-data` | 控制面数据（容器内 `/var/lib/nexus`，其中 `nexus.db` 为控制库） |
| `$NEXUS_HOME/recall` | 共享 Recall 与私密笔记数据（容器内 `/recall`） |
| `$NEXUS_HOME/backups` | 部署快照目录（`deploy-<时间戳>` 命名） |

后续所有 Compose 命令统一走这个别名（每个新 shell 重新 export 一次）：

```bash
export NEXUS_HOME=/srv/nexusdock
export NEXUS_COMPOSE="docker compose -f $NEXUS_HOME/repo/docker-compose.yml --env-file $NEXUS_HOME/nexusdock.env"
```

注意：生产只消费发布镜像，不要在生产 `docker compose build`；本地构建仅供开发。

## 1. 一次性迁移：从手工 Compose 切换到仓库 Compose + 站点 env

1. 先按第 3 节完成一次完整备份（快照 + PRAGMA 检查）。
2. 记录旧手工 Compose 与仓库版本的差异，逐项映射，不再保留旧文件：
   - 绝对路径 volume → `NEXUS_DATA_DIR` / `RECALL_REPO_DIR`（站点 env 中写绝对路径）；
   - `env_file` → 只把站点差异映射进站点 env 的对应变量（`NEXUS_PUBLIC_URL`、`NEXUS_TRUSTED_PROXIES` 等）；其余容器内变量镜像已有安全默认，不要整包透传；
   - 端口映射 → `NEXUS_HTTP_BIND` / `NEXUS_HTTP_PORT`；
   - `stop_grace_period`、healthcheck、restart 与只读根文件系统等最小权限约束已内置于仓库 Compose，无需站点配置。
3. 停掉旧容器：

   ```bash
   docker compose -f <旧compose路径> down
   ```

4. 移除旧外部网络 `nexusdock_default`。仓库 Compose 不声明外部网络（生产消费者只有 NexusDock 自己），固定 project name 为 `nexusdock` 后，Compose 会以同名网络重建为 Compose 管理网络：

   ```bash
   docker network inspect nexusdock_default   # Containers 列表应只包含 nexusdock
   docker network rm nexusdock_default
   ```

   若 `docker network rm` 失败，说明仍有其它容器连接该网络，与“只有 NexusDock 消费”的现状矛盾，先停下排查，不要强行继续。

5. 编写站点 env（逐项对照 `.env.example` 注释）：

   ```bash
   cp $NEXUS_HOME/repo/.env.example $NEXUS_HOME/nexusdock.env
   chmod 600 $NEXUS_HOME/nexusdock.env
   # 编辑：NEXUS_DATA_DIR/RECALL_REPO_DIR 指向 $NEXUS_HOME 下真实数据目录；
   # NEXUS_PUBLIC_URL 设为公网 HTTPS Origin；
   # NEXUS_TRUSTED_PROXIES 收敛为真实反代来源，例如 127.0.0.1,::1,192.168.227.0/24，
   # 不再放行全部 RFC1918（10/8、172.16/12、192.168/16）；
   # NEXUS_HTTP_BIND/NEXUS_HTTP_PORT 对应旧端口映射；
   # NEXUS_IMAGE 填发布镜像标签（见第 2 节）。
   ```

   若反向代理是与 NexusDock 同 Docker 网络的容器，`NEXUS_HTTP_BIND` 需改为 `0.0.0.0`（并用防火墙限制来源），Trusted Proxies 填该网络网段。

6. 按第 4 节命令序列首次拉起，再执行第 5 节验证清单。

## 2. 镜像与提交的对应关系

- 发布流水线（`publish-image.yml`）先跑与 CI 相同的完整验证，再构建推送；镜像标签 `sha-<短SHA>` 与目标提交一一对应。
- 构建时注入 `git describe` 版本与完整提交 SHA：镜像内二进制 buildinfo、OCI `org.opencontainers.image.revision` 标签、`sha-<短SHA>` 标签三者一致；流水线的 verify job 会在发布镜像里校验二进制中包含目标提交 SHA。
- 生产部署固定 `NEXUS_IMAGE=ghcr.io/uvwt/nexusdock:sha-<短SHA>`（或 `agentdockio/nexusdock:sha-<短SHA>`），不要用 `latest` 部署生产。

部署前可自行核验镜像 revision 与提交一致：

```bash
docker inspect -f '{{ index .Config.Labels "org.opencontainers.image.revision" }}' \
  ghcr.io/uvwt/nexusdock:sha-<短SHA>
# 输出必须等于 git rev-parse HEAD（目标提交完整 SHA）
```

## 3. 部署前：备份

控制库是 SQLite（`$NEXUS_HOME/nexus-data/nexus.db`，rollback journal 模式）；容器内没有 sqlite3，备份在宿主机做。推荐维护窗口内停机备份，一致性与完整性最有保障：

```bash
TS=$(date +%Y%m%d-%H%M%S)
SNAP="$NEXUS_HOME/backups/deploy-$TS"
mkdir -p "$SNAP"

$NEXUS_COMPOSE stop

# 数据快照落盘（控制面 + Recall，含私密笔记 age 加密副本）
cp -a "$NEXUS_HOME/nexus-data" "$SNAP/nexus-data"
cp -a "$NEXUS_HOME/recall" "$SNAP/recall"
chmod -R go-rwx "$SNAP"

# 完整性检查必须针对备份副本执行，两条都必须输出 ok
sqlite3 "$SNAP/nexus-data/nexus.db" 'PRAGMA quick_check;'
sqlite3 "$SNAP/nexus-data/nexus.db" 'PRAGMA integrity_check;'

# 检查通过后再恢复运行；任何一条不 ok 都立即停止部署并排查
$NEXUS_COMPOSE start
```

- macOS 自带 `sqlite3`；Linux 宿主机用包管理器安装。
- `quick_check`/`integrity_check` 不输出 `ok` 时，说明库文件已损坏，先恢复最近可用快照再排查磁盘/文件系统，不要带着损坏库升级。
- Recall 是纯文件（Markdown + JSON + 卡片），停机后 `cp -a` 即一致。

## 4. 部署中：命令序列

```bash
cd "$NEXUS_HOME/repo"
git fetch --tags
git checkout <目标 tag 或提交>
git status --porcelain        # 必须为空，确认 checkout 干净
REV=$(git rev-parse HEAD)
SHORT=${REV:0:7}

# 站点 env 固定到该提交对应的发布镜像（sha-<短SHA> 由发布流水线推送）
sed -i.bak "s|^NEXUS_IMAGE=.*|NEXUS_IMAGE=ghcr.io/uvwt/nexusdock:sha-${SHORT}|" \
  "$NEXUS_HOME/nexusdock.env"

$NEXUS_COMPOSE pull           # pull 失败说明该提交尚未发布镜像，先走发布流程
$NEXUS_COMPOSE up -d --wait   # 等 healthcheck 变 healthy
docker inspect -f '{{.State.Health.Status}}' nexusdock

# revision 一致性：镜像 revision 必须等于本次部署提交
docker inspect -f '{{ index .Config.Labels "org.opencontainers.image.revision" }}' nexusdock
# 输出应等于 $REV
```

镜像拉不起来（`pull` 报 manifest 不存在）说明目标提交还没发布镜像；不要用本地构建顶替生产部署。

## 5. 部署后验证清单

逐项确认后再打回滚标记：

1. **就绪检查**：`curl -fsS http://127.0.0.1:${NEXUS_HTTP_PORT}/ready` 返回 `{"ok":true,...}`（HTTP 200）。容器 healthcheck 变 healthy 也依赖它。
2. **控制库完整**：应用启动时会自动对控制库执行 `quick_check`，启动失败即退出；容器 healthy 即代表通过。可选再对现网库跑一次 `PRAGMA quick_check`。
3. **Recall 可用**：Web 控制台打开一篇 Recall 文档，并执行一次关键词搜索。
4. **Workflow 可用**：发布一个测试模板 → 关键词匹配命中 → 退役该模板。
5. **MCP 工具可用**：用固定 MCP Access Token 的客户端连 `<公网域名>/mcp`，调用一个 NexusDock 自有工具（如 Recall 搜索）。
6. **AgentDock 节点重连**：节点在优雅关闭窗口内断开，新容器起来后自动重连；控制台节点恢复在线，Runtime 视图能打开实时状态。
7. **公网认证可用**：浏览器走公网 HTTPS 域名登录管理员账号，确认 `NEXUS_PUBLIC_URL` 与 Trusted Proxies 生效（登录与页面跳转不回落到 http、不报 cookie 域错误）。

全部通过后维护回滚标记：

```bash
docker tag "$(grep '^NEXUS_IMAGE=' "$NEXUS_HOME/nexusdock.env" | cut -d= -f2-)" nexusdock:rollback
```

`nexusdock:rollback` 永远指向“最近一个通过验证的版本”，随每次部署成功滚动更新。

## 6. 回滚

```bash
# 1) 站点 env 切回回滚镜像
sed -i.bak "s|^NEXUS_IMAGE=.*|NEXUS_IMAGE=nexusdock:rollback|" "$NEXUS_HOME/nexusdock.env"

# 2) 先对当前数据补一个快照（沿用 deploy- 命名，纳入第 7 节保留策略），便于回滚后数据对比/恢复
TS=$(date +%Y%m%d-%H%M%S)
SNAP="$NEXUS_HOME/backups/deploy-$TS"
mkdir -p "$SNAP"
$NEXUS_COMPOSE stop
cp -a "$NEXUS_HOME/nexus-data" "$SNAP/nexus-data"
cp -a "$NEXUS_HOME/recall" "$SNAP/recall"
chmod -R go-rwx "$SNAP"

# 3) 拉起回滚版本并验证
$NEXUS_COMPOSE up -d --wait
docker inspect -f '{{.State.Health.Status}}' nexusdock
```

回滚后至少复跑第 5 节的 1、3、5、6、7 项。

数据注意：如果新版本迁移过控制库 schema，回滚后的旧镜像可能读不懂新数据。此时用回滚前快照恢复数据：`stop` 后把 `$SNAP/nexus-data`、`$SNAP/recall` 覆盖回数据目录，再 `up -d --wait`。恢复前确认快照 PRAGMA 检查是 `ok`。

`nexusdock:rollback` 本地标记不存在时（例如换机后首次回滚），改用上一个已验证提交的 `sha-<短SHA>` 发布镜像，操作方式相同。

## 7. 保留策略与清理

- **部署快照**：保留最近 **N=5** 个 `deploy-*` 目录。理由：个人项目部署频率低，5 份快照覆盖最近数周的回滚窗口；单份快照体积 ≈ 控制库 + Recall 全量，5 份占用可控；更久远的恢复依赖发布镜像 + 重新初始化，靠堆积快照得不偿失。
- **镜像**：保留最近一个 `nexusdock:rollback`；本机的 `nexusdock:deploy-*`、`nexusdock:smoke-*` 与多余的 `nexusdock:rollback-*`（迁移前遗留命名）按下面命令清理，`sha-` 缓存镜像建议保留最近 3 个。GHCR / Docker Hub 仓库侧的标签保留策略不在本机清理范围。
- **禁止全局清理**：不要运行 `docker system prune`、`docker image prune -a`、`docker volume prune`、`docker builder prune -a`——宿主机上还有其它项目的镜像、卷与构建缓存，全局 prune 是破坏性操作。只按下面命令清理 NexusDock 自有对象。

清理命令都先 dry-run 看列表，确认后再执行删除；输出为空即跳过。

```bash
# 部署快照：按时间戳倒序，保留最近 5 个
find "$NEXUS_HOME/backups" -maxdepth 1 -type d -name 'deploy-*' | sort -r | tail -n +6
find "$NEXUS_HOME/backups" -maxdepth 1 -type d -name 'deploy-*' | sort -r | tail -n +6 \
  | while read -r dir; do rm -rf "$dir"; done

# 本机镜像：deploy- / smoke- 前缀保留最近 3 个
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^nexusdock:(deploy|smoke)-' | head -n 3
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^nexusdock:(deploy|smoke)-' | tail -n +4 \
  | while read -r image; do docker rmi "$image"; done

# 本机镜像：rollback- 前缀只保留最近 1 个（nexusdock:rollback 主标记不在此列）
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^nexusdock:rollback-' | head -n 1
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^nexusdock:rollback-' | tail -n +2 \
  | while read -r image; do docker rmi "$image"; done

# 本机 sha- 缓存镜像：保留最近 3 个（docker images 默认按创建时间倒序）
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^(ghcr.io/uvwt/nexusdock|agentdockio/nexusdock):sha-' | head -n 3
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^(ghcr.io/uvwt/nexusdock|agentdockio/nexusdock):sha-' | tail -n +4 \
  | while read -r image; do docker rmi "$image"; done
```

## 8. 与发布流程的关系

推送 `v*` 标签或手动触发 `publish-image.yml` 后：先执行与 CI 相同的完整验证（`make ci` + 生成物检查）→ 构建并推送多架构镜像（`sha-<短SHA>`、semver、latest 标签，版本信息来自构建提交）→ verify job 拉起两个 registry 的镜像做 healthcheck 与二进制 revision 校验。部署只消费 verify 通过的镜像。
