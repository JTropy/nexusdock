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

首次迁移时旧 Compose 仍是当前生产入口，不能在站点 env 尚未建立时先调用新的 `$NEXUS_COMPOSE`。按下面顺序切换：

1. **先记录并映射旧配置，不停服务。**逐项核对旧 Compose：
   - 绝对路径 volume → `NEXUS_DATA_DIR` / `RECALL_REPO_DIR`；
   - 旧 `env_file` → 只迁移站点差异（`NEXUS_PUBLIC_URL`、`NEXUS_TRUSTED_PROXIES` 等），不要整包透传；
   - 端口映射 → `NEXUS_HTTP_BIND` / `NEXUS_HTTP_PORT`；
   - `stop_grace_period`、healthcheck、restart、只读根文件系统等结构配置由仓库 Compose 统一提供。
2. **建立新的站点 env。**此时旧服务仍在运行：

   ```bash
   cp "$NEXUS_HOME/repo/.env.example" "$NEXUS_HOME/nexusdock.env"
   chmod 600 "$NEXUS_HOME/nexusdock.env"
   # 编辑：NEXUS_DATA_DIR/RECALL_REPO_DIR 使用真实绝对路径；
   # NEXUS_PUBLIC_URL 使用公网 HTTPS Origin；
   # NEXUS_TRUSTED_PROXIES 只包含真实反代来源；
   # NEXUS_HTTP_BIND/NEXUS_HTTP_PORT 对应旧端口映射；
   # NEXUS_IMAGE 填准备部署的 sha-<短SHA> 发布镜像。
   ```

   若反向代理与 NexusDock 位于同一 Docker 网络，`NEXUS_HTTP_BIND=0.0.0.0`，并用防火墙限制来源；Trusted Proxies 只填实际代理所在网段。
3. **用旧 Compose 做最后一份 pre-deploy 快照。**首次迁移必须停旧服务后复制，不能让新 Compose 去停止一个尚未接管的容器：

   ```bash
   set -euo pipefail
   OLD_COMPOSE="docker compose -f <旧compose路径>"
   TS=$(date +%Y%m%d-%H%M%S)
   PRE_DEPLOY_SNAPSHOT="$NEXUS_HOME/backups/deploy-$TS-pre"
   mkdir -p "$PRE_DEPLOY_SNAPSHOT"

   $OLD_COMPOSE stop
   cp -a "$NEXUS_HOME/nexus-data" "$PRE_DEPLOY_SNAPSHOT/nexus-data"
   cp -a "$NEXUS_HOME/recall" "$PRE_DEPLOY_SNAPSHOT/recall"
   chmod -R go-rwx "$PRE_DEPLOY_SNAPSHOT"
   test "$(sqlite3 "$PRE_DEPLOY_SNAPSHOT/nexus-data/nexus.db" 'PRAGMA quick_check;')" = "ok"
   test "$(sqlite3 "$PRE_DEPLOY_SNAPSHOT/nexus-data/nexus.db" 'PRAGMA integrity_check;')" = "ok"
   printf '%s\n' "$PRE_DEPLOY_SNAPSHOT" > "$NEXUS_HOME/backups/last-pre-deploy"
   $OLD_COMPOSE start
   ```

   两条 PRAGMA 都必须输出 `ok`；检查失败就停止迁移。这里打开的是**停服后复制出的快照**，不是运行中的生产数据库。
4. **在替换前保存当前 known-good 镜像。**只有当前容器 healthy 才允许晋升为 `nexusdock:rollback`：

   ```bash
   test "$(docker inspect -f '{{.State.Health.Status}}' nexusdock)" = healthy
   CURRENT_IMAGE_ID="$(docker inspect -f '{{.Image}}' nexusdock)"
   docker tag "$CURRENT_IMAGE_ID" nexusdock:rollback
   ```

5. 停掉旧 Compose 并移除旧外部网络：

   ```bash
   OLD_COMPOSE="docker compose -f <旧compose路径>"
   $OLD_COMPOSE down
   docker network inspect nexusdock_default   # Containers 应为空；若仍有消费者先排查
   docker network rm nexusdock_default
   ```

   仓库 Compose 固定 project name 为 `nexusdock`，会重新创建由 Compose 管理的 `nexusdock_default`。网络删除失败时不要强行继续。
6. 直接进入第 4 节拉起仓库 Compose，再执行第 5 节验证；**首次迁移已经在本节完成 pre-deploy 快照，不需要重复执行第 3 节。**

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

## 3. 部署前：保存 known-good 与 pre-deploy 快照

每次日常部署都先保存**当前已验证版本**，再对停服后的数据做 pre-deploy 快照。这个快照是发生 Schema Migration 后能够真正退回旧二进制的恢复点，不能用部署失败后才生成的快照代替。

```bash
set -euo pipefail

# 当前容器必须仍是 healthy；它才有资格成为上一 known-good。
test "$(docker inspect -f '{{.State.Health.Status}}' nexusdock)" = healthy
CURRENT_IMAGE_ID="$(docker inspect -f '{{.Image}}' nexusdock)"
docker tag "$CURRENT_IMAGE_ID" nexusdock:rollback

TS=$(date +%Y%m%d-%H%M%S)
PRE_DEPLOY_SNAPSHOT="$NEXUS_HOME/backups/deploy-$TS-pre"
mkdir -p "$PRE_DEPLOY_SNAPSHOT"

$NEXUS_COMPOSE stop

# 数据快照落盘（控制面 + Recall，含私密笔记 age 加密副本）。
cp -a "$NEXUS_HOME/nexus-data" "$PRE_DEPLOY_SNAPSHOT/nexus-data"
cp -a "$NEXUS_HOME/recall" "$PRE_DEPLOY_SNAPSHOT/recall"
chmod -R go-rwx "$PRE_DEPLOY_SNAPSHOT"

# 只检查停服后复制出的快照；不要从 macOS 宿主 sqlite3 打开运行中的 bind-mount 生产库。
test "$(sqlite3 "$PRE_DEPLOY_SNAPSHOT/nexus-data/nexus.db" 'PRAGMA quick_check;')" = "ok"
test "$(sqlite3 "$PRE_DEPLOY_SNAPSHOT/nexus-data/nexus.db" 'PRAGMA integrity_check;')" = "ok"
printf '%s\n' "$PRE_DEPLOY_SNAPSHOT" > "$NEXUS_HOME/backups/last-pre-deploy"

$NEXUS_COMPOSE start
```

- macOS 自带 `sqlite3`；Linux 宿主机用包管理器安装。
- 两条 PRAGMA 任一不为 `ok` 就停止部署，先处理快照/磁盘问题。若脚本因此在服务停止状态退出，排障前先明确是否要恢复原服务，不要继续升级。
- `last-pre-deploy` 只记录最近一次部署前快照路径；第 6 节的 Schema downgrade 必须从这里恢复。
- Recall 是纯文件（Markdown + JSON + 卡片），停机后 `cp -a` 即一致。

## 4. 部署中：命令序列

```bash
set -euo pipefail
cd "$NEXUS_HOME/repo"
git fetch --tags
git checkout <目标 tag 或提交>
git status --porcelain        # 必须为空，确认 checkout 干净
REV=$(git rev-parse HEAD)
SHORT=${REV:0:7}

# 第 3 节或首次迁移必须已经留下可恢复的 pre-deploy 快照。
test -s "$NEXUS_HOME/backups/last-pre-deploy"
PRE_DEPLOY_SNAPSHOT="$(cat "$NEXUS_HOME/backups/last-pre-deploy")"
test -d "$PRE_DEPLOY_SNAPSHOT/nexus-data"
test -d "$PRE_DEPLOY_SNAPSHOT/recall"

# 站点 env 固定到该提交对应的发布镜像（sha-<短SHA> 由发布流水线推送）。
sed -i.bak "s|^NEXUS_IMAGE=.*|NEXUS_IMAGE=ghcr.io/uvwt/nexusdock:sha-${SHORT}|" \
  "$NEXUS_HOME/nexusdock.env"

$NEXUS_COMPOSE pull           # pull 失败说明该提交尚未发布镜像，先走发布流程
$NEXUS_COMPOSE up -d --wait   # 等 healthcheck 变 healthy
test "$(docker inspect -f '{{.State.Health.Status}}' nexusdock)" = healthy

# revision 一致性是门禁，不只是人工观察输出。
IMAGE_REV="$(docker inspect -f '{{ index .Config.Labels "org.opencontainers.image.revision" }}' nexusdock)"
test "$IMAGE_REV" = "$REV"
```

镜像拉不起来（`pull` 报 manifest 不存在）说明目标提交还没发布镜像；不要用本地构建顶替生产部署。

## 5. 部署后验证清单

逐项确认新版本是否可以继续承载生产流量：

1. **就绪检查**：`curl -fsS http://127.0.0.1:${NEXUS_HTTP_PORT}/ready` 返回 `{"ok":true,...}`（HTTP 200）。容器 healthcheck 变 healthy 也依赖它。
2. **控制库完整**：应用启动时会自动对控制库执行一次 `quick_check`，检查失败就不会进入 healthy。运行中的生产库不要再从 macOS 宿主使用 `sqlite3` 打开；若需要人工完整性复核，按第 3 节停服复制快照后只检查副本。
3. **Recall 可用**：Web 控制台打开一篇 Recall 文档，并执行一次关键词搜索。
4. **Workflow 可用**：发布一个测试模板 → 关键词匹配命中 → 退役该模板。
5. **MCP 工具可用**：用固定 MCP Access Token 的客户端连 `<公网域名>/mcp`，调用一个 NexusDock 自有工具（如 Recall 搜索）。
6. **AgentDock 节点重连**：节点在优雅关闭窗口内断开，新容器起来后自动重连；控制台节点恢复在线，Runtime 视图能打开实时状态。
7. **公网认证可用**：浏览器走公网 HTTPS 域名登录管理员账号，确认 `NEXUS_PUBLIC_URL` 与 Trusted Proxies 生效（登录与页面跳转不回落到 http、不报 cookie 域错误）。

全部通过后**不要立刻覆盖 `nexusdock:rollback`**。第 3 节在部署前已经把上一版已验证镜像保存为 rollback；它应在本次观察窗口内继续指向上一 known-good。等下一次部署开始前，如果当前版本仍 healthy，再由第 3 节把当前版本晋升为新的 rollback。

## 6. 回滚

回滚时先保存**故障现场**，再切回上一 known-good 镜像。`failed` 快照只用于保留新版本运行后的现场；如果旧二进制因为 Schema Migration 无法读取当前数据，真正恢复的数据源必须是部署前的 `pre` 快照。

```bash
set -euo pipefail

# 1) 找到这次部署前的恢复点；缺失时不要继续做数据级回滚。
test -s "$NEXUS_HOME/backups/last-pre-deploy"
PRE_DEPLOY_SNAPSHOT="$(cat "$NEXUS_HOME/backups/last-pre-deploy")"
test -d "$PRE_DEPLOY_SNAPSHOT/nexus-data"
test -d "$PRE_DEPLOY_SNAPSHOT/recall"

# 2) 停止当前故障版本并保存现场。这个快照可能已经是新 Schema，不能拿来给旧二进制降级。
TS=$(date +%Y%m%d-%H%M%S)
FAILED_SNAPSHOT="$NEXUS_HOME/backups/deploy-$TS-failed"
mkdir -p "$FAILED_SNAPSHOT"
$NEXUS_COMPOSE stop
cp -a "$NEXUS_HOME/nexus-data" "$FAILED_SNAPSHOT/nexus-data"
cp -a "$NEXUS_HOME/recall" "$FAILED_SNAPSHOT/recall"
chmod -R go-rwx "$FAILED_SNAPSHOT"
printf '%s\n' "$FAILED_SNAPSHOT" > "$NEXUS_HOME/backups/last-failed"

# 3) 先尝试旧镜像直接读取当前数据；没有不兼容 Schema 时可保留部署后的业务数据。
sed -i.bak "s|^NEXUS_IMAGE=.*|NEXUS_IMAGE=nexusdock:rollback|" "$NEXUS_HOME/nexusdock.env"
if $NEXUS_COMPOSE up -d --wait; then
  test "$(docker inspect -f '{{.State.Health.Status}}' nexusdock)" = healthy
  echo "rollback image is healthy with current data"
else
  echo "rollback image failed with current data; inspect logs before deciding on data restore" >&2
fi
```

只有日志明确表明旧镜像无法读取新 Schema/新数据格式时，才执行下面的数据恢复。恢复前再次停止容器；把失败现场留在 `FAILED_SNAPSHOT`，然后恢复**部署前**的数据：

```bash
set -euo pipefail
PRE_DEPLOY_SNAPSHOT="$(cat "$NEXUS_HOME/backups/last-pre-deploy")"
FAILED_SNAPSHOT="$(cat "$NEXUS_HOME/backups/last-failed")"
test -d "$PRE_DEPLOY_SNAPSHOT/nexus-data"
test -d "$FAILED_SNAPSHOT/nexus-data"
$NEXUS_COMPOSE stop

# 保留回滚尝试后最终停盘状态；mv 只改目录位置，不再复制一遍大数据。
mv "$NEXUS_HOME/nexus-data" "$FAILED_SNAPSHOT/live-nexus-data-after-rollback-attempt"
mv "$NEXUS_HOME/recall" "$FAILED_SNAPSHOT/live-recall-after-rollback-attempt"
chmod -R go-rwx "$FAILED_SNAPSHOT"

cp -a "$PRE_DEPLOY_SNAPSHOT/nexus-data" "$NEXUS_HOME/nexus-data"
cp -a "$PRE_DEPLOY_SNAPSHOT/recall" "$NEXUS_HOME/recall"

$NEXUS_COMPOSE up -d --wait
test "$(docker inspect -f '{{.State.Health.Status}}' nexusdock)" = healthy
```

这样即使数据级回滚丢弃了新版本运行期间产生的实时状态，那部分数据仍完整保留在 `FAILED_SNAPSHOT`，后续可以人工对比和择项恢复。回滚后至少复跑第 5 节的 1、3、5、6、7 项。

`nexusdock:rollback` 本地标记不存在时（例如换机后首次回滚），改用上一个已验证提交的 `sha-<短SHA>` 发布镜像；但数据级 downgrade 仍必须使用对应部署前的 `pre` 快照，不能使用 `failed` 快照。

## 7. 保留策略与清理

- **部署快照**：普通历史保留最近 **N=5** 个 `deploy-*-pre` / `deploy-*-failed` 目录；此外，`last-pre-deploy` 与 `last-failed` 当前指向的恢复点无条件保护，即使它们暂时超出 N=5。这样清理不会把仍被回滚流程引用的快照删掉。
- **镜像**：`nexusdock:rollback` 始终保留，它代表下一次故障时可立即切回的上一 known-good；本机的 `nexusdock:deploy-*`、`nexusdock:smoke-*` 与多余的 `nexusdock:rollback-*`（迁移前遗留命名）按下面命令清理，`sha-` 缓存镜像建议保留最近 3 个。GHCR / Docker Hub 仓库侧的标签保留策略不在本机清理范围。
- **禁止全局清理**：不要运行 `docker system prune`、`docker image prune -a`、`docker volume prune`、`docker builder prune -a`——宿主机上还有其它项目的镜像、卷与构建缓存，全局 prune 是破坏性操作。只按下面命令清理 NexusDock 自有对象。

清理命令都先 dry-run 看列表，确认后再执行删除；输出为空即跳过。

```bash
# 部署快照：保护当前 pre/failed 恢复点，其余历史按时间戳倒序额外保留最近 5 个。
PROTECTED_PRE="$(cat "$NEXUS_HOME/backups/last-pre-deploy" 2>/dev/null || true)"
PROTECTED_FAILED="$(cat "$NEXUS_HOME/backups/last-failed" 2>/dev/null || true)"
list_old_snapshots() {
  find "$NEXUS_HOME/backups" -maxdepth 1 -type d -name 'deploy-*' \
    | sort -r \
    | awk -v pre="$PROTECTED_PRE" -v failed="$PROTECTED_FAILED" '$0 != pre && $0 != failed' \
    | tail -n +6
}
list_old_snapshots
list_old_snapshots | while IFS= read -r dir; do rm -rf -- "$dir"; done

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

推送 `v*` 标签或手动触发 `publish-image.yml` 后：先执行与 CI 相同的完整验证（`make ci` + 生成物检查）→ 构建并推送多架构镜像（始终生成 `sha-<短SHA>`；正式稳定 tag 另外生成 semver/latest，预发布 tag 不推进 latest）→ verify job 使用**本次 build-push 返回的不可变 manifest digest**分别从 GHCR 与 Docker Hub 拉取同一产物，校验 OCI revision、二进制内完整 revision 与 healthcheck。验证不依赖 `latest`/semver alias，因此手动从非默认分支发布也不会误验其它提交。生产只消费 verify 通过的 `sha-<短SHA>` 镜像。
