# 徒步线路许可与队伍调度平台（trail-permit-dispatch）

面向户外徒步（驴友）组织方的后端系统：以**线路每日许可配额**为核心资源，治理「计划 → 队员登记 → 许可占用 → 复核放行 → 在途打点 → 事故处置 → 许可结算 → 审计」的完整生命周期。

系统不是游记记录、不是通用预约工具：许可名额是有限且需要原子占用的真实资源，在途打点带有截止时间与超时升级，事故按严重度驱动队伍强制中止，费用有明确结算终点，全链路可审计。

## 两条相互依赖的公开业务路径

| 路径 | 步骤 | 说明 |
| --- | --- | --- |
| A. 准入与成行 | 建队 → 批量登记队员与免责签署 → 申请许可 → 线路管理员复核 → 放行出行 | 申请许可在同一事务内完成配额占用、状态推进、待结算生成、派单作业入队与审计写入 |
| B. 在途与收尾 | 打点上报 → 超时扫描 → 事故升级/处置 → 结束行程 → 许可结算 | 只能作用于路径 A 产出的 `confirmed` / `on_trail` 队伍；取消与中止必须回退路径 A 占用的名额 |

## 目录结构

```text
cmd/server                     进程入口、信号处理与优雅关闭
internal/apperr                稳定错误码、HTTP 状态映射、可重试判定
internal/clock                 业务时区、出行日、固定时钟（可测试）
internal/domain                实体、值对象、四套状态机、分页与筛选值对象
internal/security              PBKDF2-HMAC-SHA256 口令、不可逆会话令牌
internal/storage/sqlite        连接与 pragma、上下文事务管理器、版本化迁移
internal/repository            持久化契约（接口与参数结构）
internal/repository/sqliterepo SQLite 实现：条件更新、乐观锁、返回值隔离
internal/idempotency           幂等键生命周期与响应重放
internal/audit                 审计事件写入（业务数据，不是日志）
internal/service/auth          登录、可撤销会话、退出、过期清理、账号供给
internal/service/catalog       线路与许可窗口治理
internal/service/dispatch      队伍生命周期、打点、超时扫描
internal/service/incident      事故登记、升级链、处置与归档
internal/service/settlement    许可费用结算、减免与失败停放
internal/worker                作业租约、指数退避、永久失败、重启恢复
internal/httpjson              JSON 信封、请求解码、操作者上下文
internal/middleware            请求 ID、访问日志、panic 恢复、鉴权与角色闸门
internal/httpapi               路由表与处理器
internal/bootstrap             依赖装配、健康/就绪、空库初始化、HTTP 生命周期
migrations                     内置版本化 SQL（0001/0002/0003）
```

## 数据库与迁移

- 真实关系数据库：SQLite（`modernc.org/sqlite` 纯 Go 驱动，`CGO_ENABLED=0`）。
- pragma：`foreign_keys(1)`、`journal_mode(wal)`、`busy_timeout(8000)`、写事务 `_txlock=immediate`。
- 迁移：`schema_migrations(version,name,checksum,applied_at)`；空库可建，重复启动幂等；**已应用迁移的内容校验和发生漂移时直接阻断启动**，不静默重解释历史数据。

13 张有实际关系的表：

| 表 | 关键约束 |
| --- | --- |
| `users` | `ux_users_email`、角色与状态 CHECK |
| `sessions` | `ux_sessions_token_hash`、`user_id` 外键、撤销与过期时间 |
| `trails` | `ux_trails_code`、难度/配额/人数区间 CHECK、`version` |
| `permit_windows` | `ux_permit_windows_trail_day`、`quota_reserved <= quota_total` CHECK、`version` |
| `checkpoints` | `ux_checkpoints_trail_seq`、截止分钟数 CHECK |
| `parties` | `ux_parties_code`、线路/领队/许可窗口外键、`state` CHECK、`version` |
| `party_members` | `ux_party_members_ref`（同队编号唯一） |
| `checkpoint_reports` | `ux_checkpoint_reports_party_checkpoint`（同队同打点唯一） |
| `incidents` | 部分唯一索引 `ux_incidents_party_kind_open`（仅约束未处置事故） |
| `settlements` | `ux_settlements_party`（一队一笔）、`version` |
| `audit_events` | 按对象/操作者/请求的业务索引 |
| `jobs` | 到期索引、租约字段、尝试次数与上限 CHECK |
| `idempotency_records` | `ux_idempotency_scope_key`（作用域+键+操作者） |

## 事务、并发与恢复

- **跨实体事务**：`RequestPermit` 在一个事务内完成「配额占用 + 队伍状态推进 + 待结算生成 + 派单作业入队 + 审计写入 + 幂等记录」，任一步失败整体回滚（并发失败的申请不会留下待结算记录）。
- **配额不变量**：占用由条件 SQL 保证 —— `WHERE version = ? AND closed_at IS NULL AND quota_reserved + ? <= quota_total`；名额不足返回 `quota_exhausted`，版本漂移返回 `version_conflict`。
- **乐观锁**：`parties`、`trails`、`incidents`、`settlements` 均按 `version` 条件更新，0 行受影响即冲突；队伍更新同时校验来源状态。
- **幂等**：`Idempotency-Key` 按「作用域 + 键 + 操作者」隔离；同键同请求重放原响应，同键不同请求返回 `conflict`，过期记录视为未使用。
- **重启恢复**：作业租约到期后被回收（进程启动时与每轮轮询都会执行），已占用名额与队伍状态持久化在库中，重启后继续推进。
- **context 传播**：HTTP → middleware → service → repository → `database/sql` 全链路；取消与超时统一映射为 `deadline_exceeded`。

## 后台作业

| 作业 | 触发 | 行为 |
| --- | --- | --- |
| `notify_dispatch` | 许可占用成功 | 向救援基地通报出行计划并写审计 |
| `settle_party` | 行程结束 | 结算许可费用；永久失败时把结算停放为 `failed` |
| `escalate_incident` | 事故登记/上一次升级 | 按严重度延迟升级，达到上限后自然终止 |
| `sweep_overdue_checkpoints` | 周期性 | 扫描在途队伍的必经打点超时，登记 `major` 事故（同队同节点只登记一次） |
| `expire_sessions` | 周期性 | 撤销超过有效期的会话 |

失败处理：`Backoff(attempts) = base * 2^(attempts-1)`（上限截断）；`not_found` / `invalid_argument` / `permission_denied` / `state_invalid` 视为不可重试并立即停放；处理器 panic 被捕获为失败而不会拖垮进程。

## 身份与权限

- `POST /api/v1/auth/login` 签发 Bearer 令牌，数据库只保存令牌的 SHA-256；`POST /api/v1/auth/logout` 立即撤销；`POST /api/v1/auth/sessions/revoke-all` 撤销该账号全部会话；到期会话由 `expire_sessions` 清理。
- 两个业务角色：`leader`（建队、登记队员、申请许可、打点、结束、登记事故）与 `ranger`（线路与许可窗口治理、复核、放行、事故处置、结算减免、审计查询）。
- 领队只能操作与查看自己的队伍（伪造 `leader_id` 无效）；鉴权失败映射 401，越权映射 403。

## HTTP API

统一信封：成功 `{"data": ...}`，失败 `{"error":{"code","message","field","request_id"}}`，并回显 `X-Request-ID`。

```text
GET  /healthz                                             存活
GET  /readyz                                              就绪（数据库、schema 版本、必要基础数据、队列深度）
POST /api/v1/auth/login                                   登录
GET  /api/v1/auth/me                                      当前账号与活跃会话数
POST /api/v1/auth/logout                                  退出并撤销当前会话
POST /api/v1/auth/sessions/revoke-all                     撤销全部会话
POST /api/v1/accounts                                     ranger 创建账号
GET  /api/v1/trails                                       线路分页（region/status/sort_by/desc/page/size）
GET  /api/v1/trails/{code}                                线路详情与打点
POST /api/v1/trails                                       ranger 登记线路
POST /api/v1/trails/{code}/status                         ranger 调整线路状态
POST /api/v1/trails/{code}/checkpoints                    ranger 追加打点
GET  /api/v1/trails/{code}/permit-windows                 许可窗口列表（from/to）
POST /api/v1/trails/{code}/permit-windows                 ranger 开放许可窗口
POST /api/v1/trails/{code}/permit-windows/{day}/close     ranger 关闭许可窗口
GET  /api/v1/parties                                      队伍分页（state/hike_day_from/hike_day_to/trail_code/search）
POST /api/v1/parties                                      leader 建队
GET  /api/v1/parties/{code}                               队伍详情（队员与打点）
POST /api/v1/parties/{code}/members                       leader 批量登记队员（逐项结果）
POST /api/v1/parties/{code}/permit-request                leader 申请许可（支持 Idempotency-Key）
POST /api/v1/parties/{code}/approve                       ranger 复核
POST /api/v1/parties/{code}/dispatch                      ranger 放行
POST /api/v1/parties/{code}/checkpoints                   leader 打点上报
POST /api/v1/parties/{code}/complete                      leader 结束行程
POST /api/v1/parties/{code}/cancel                        leader 或 ranger 取消
GET  /api/v1/parties/{code}/settlement                    队伍结算
GET  /api/v1/incidents                                    事故分页（state/severity/party_code）
POST /api/v1/incidents                                    登记事故
GET  /api/v1/incidents/{id}                               事故详情
POST /api/v1/incidents/{id}/resolve                       ranger 处置
POST /api/v1/incidents/{id}/close                         ranger 归档
GET  /api/v1/settlements                                  结算分页
POST /api/v1/settlements/{id}/waive                       ranger 减免
GET  /api/v1/audit-events                                 ranger 审计分页
```

## 本地运行

```bash
cp .env.example .env
go run ./cmd/server
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

空库首次启动会创建一个 `ranger`、一个 `leader`、两条线路（`AOMEN-RIDGE`、`QINGXI-VALLEY`）及未来三天的许可窗口；库中已存在线路管理员时自动跳过。

初始口令没有默认值：未设置 `SEED_RANGER_PASSWORD` / `SEED_LEADER_PASSWORD` 时，系统会生成一次性口令并在启动日志中以 `one_time_password` 输出一次，请登录后立即修改。仓库中不包含任何可用凭据。

## 验证命令

```bash
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

## 容器

```bash
docker buildx build --platform linux/amd64 -t trail-permit-dispatch:amd64 --load .
docker buildx build --platform linux/arm64 -t trail-permit-dispatch:arm64 --load .
docker image inspect --format '{{.Os}}/{{.Architecture}}' trail-permit-dispatch:amd64
docker run -d -p 8080:8080 --name trail-permit trail-permit-dispatch:amd64
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

镜像基于 `golang:1.22.5-alpine3.20` 构建、`alpine:3.20` 运行，`CGO_ENABLED=0`，非 root 用户 `trail`，数据目录 `/app/data`，入口 `./cmd/server` 编译出的 `trail-permit-server`。

## 配置

见 `.env.example`。所有配置在启动时集中校验（例如作业租约必须大于轮询间隔、许可单价必须为正、密码派生迭代次数不得过低），校验失败进程直接退出并说明原因。启动日志只输出脱敏摘要，不打印任何口令。
