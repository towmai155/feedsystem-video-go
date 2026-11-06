# P0 并发问题：根因与修复方案

对应报告：`test/load/P0-BASELINE-20261022.md`（复现命令 `./test/load/run-p0.ps1`）

## 0. 一句话结论

报告里三类看着不相干的失败，其实是同一条链：

**业务代码从未配置过连接池 / 连接生命周期 → 并发一上来，几乎每个请求都在新建一条 MySQL 连接 → 连接建立与认证的开销把 MySQL 打慢 → 鉴权查询超过 2s 超时 → 被映射成 503「authentication temporarily unavailable」；同一原因造成 `driver: bad connection` 和 `dial tcp ...:13307 ... refused`（13307 是 Docker 发布端口的代理）。**

写路径的问题（点赞 P99 33s、重复点赞放大热度 11 次、13 条通知丢失）是**另外两个独立缺陷**：副作用（热度、通知）发生在唯一约束之前；消费者单 goroutine 吞错误后照样 ACK。

所以：**先修连接层（决定性、改动最小），再修写路径的原子性和消费侧的 ack 语义。**

## 1. 证据对照

| 现象 | 报告数据 | 代码位置 | 机制 |
| --- | --- | --- | --- |
| 匿 hot 25 = 9802 RPS / P95 3.8ms，auth hot 25 只有 1364 RPS / P95 42ms | 相差 7.2 倍，而两者读的是同一份缓存 | `middleware/jwt/jwt.go:49`、`account/session.go:38` | 带 token 的请求每个都打一次 MySQL；匿名请求走 `SoftJWTAuth`，没有 Authorization 头就完全不查库 |
| auth hot 300：89.50% 返回 503，错误 `authentication temporarily unavailable` | 41707 次请求里 37417 失败，P95 491.8ms | `account/session.go:44-47` 把任意 DB 错误包成 `ErrServiceUnavailable`；`apierror/errors.go` 映射为 503 | 每次请求 2s 的 `lookupCtx` 到点或连接报错，就整体 fail-closed 成 503 |
| 失败阶段 MySQL `Max_used_connections` 只有 103（上限 400） | 报告第 67 行 | `db/db.go:16-26`（改前无任何池参数） | 成本不在「同时开多少连接」，而在「不停新建连接」：`MaxIdleConns` 默认 2，回池的连接立刻被关；`ConnMaxLifetime=0` 永不轮换 |
| `driver: bad connection`（anon cold 25，298 次） | 报告读表 | 同上 | 复用了一条服务端已关闭的连接（churn 的典型症状） |
| `dial tcp 127.0.0.1:13307: connectex ... actively refused` | detail hot/distributed 300 | 同上 | 13307 是 Compose 发布到宿主机的 MySQL 端口，经过 Docker Desktop 的端口代理；连接风暴把代理的 accept 队列/本机临时端口打满，代理直接拒绝 |
| 写 like 25/100 全部 2000/2000 成功，但落库 P99 33.8s / 33.3s，MQ 积压 ~3152 | 写表 | `worker/likeworker.go:94-119`、`cmd/worker/main.go:109-131` | HTTP 成功只代表「事件发出去了」。消费端是单 goroutine、全局 prefetch 50、每条消息 4+ 次串行查询，吞吐约 200~400 msg/s，2000 条就要排十几秒 |
| 300 并发 like 只成功 338/2000 | 写表 | `video/like_service.go:31-106` | 每个请求 2 次查询（IsExist + IsLiked）+ 2 次同步发布，全部挤在同一条 amqp Channel 和同一个池上 |
| 重复点赞：1 条记录、DB 热度 1、Redis 热度 11、通知 11 | `duplicate_like_correctness` | `like_service.go`：先 `IsLiked` 再发事件；唯一约束在 `like_entity.go:7-8`，只有消费者插入时才生效 | check-then-act：判重和写入之间没有原子性，11 个并发请求都通过了判重，各自发了 +1 热度和通知；DB 唯一键只挡住了行 |
| 245 条评论只有 232 条通知，队列已排空 | 报告第 68/69 行 | `worker/notificationworker.go:114`：`Scan(&authorID)` 的 error 被丢弃，authorID=0 时 `return nil`，外层随即 `Ack` | 基础设施错误被当成「视频不存在」，消息被永久 ACK 掉 |

## 2. 根因拆解

### 2.1 连接池裸奔（主因，一次性解释 503 / bad connection / refused）

改前 `db.NewDB` 只拼了 DSN 就 `gorm.Open`，`database/sql` 的默认值是：

- `MaxOpenConns = 0`：**无上限**，300 个并发请求可以同时开 300 条连接；
- `MaxIdleConns = 2`：多出来的连接一还回去就被关闭；
- `ConnMaxLifetime = 0` / `ConnMaxIdleTime = 0`：永不轮换、永不因空闲回收。

于是稳定状态是「池里只有 2 条热连接，其余全部现开现关」。MySQL 8 默认 `caching_sha2_password`，每条新连接都要做一次完整的握手/认证（RSA 运算），加上 TCP 三次握手、Docker 端口代理转发，单条连接成本是毫秒级。25 并发时每波就要建 20+ 条连接，正好对上「1364 RPS ≈ 18ms/请求」；300 并发时更糟，于是：连接建得慢 → 鉴权查询排到 2s 超时 → 503。

也解释了为什么 `Max_used_connections` 只有 103：**瓶颈是「连接的创建速率」，不是「连接的持有数量」**。把 `max_connections` 从 400 调到 4000 不会有任何改善。

`interpolateParams` 也一直没开，简单查询默认走预处理语句，每条语句多出 PREPARE/EXECUTE/CLOSE 的往返。鉴权、点赞这类高频单语句查询上这是纯浪费。

### 2.2 鉴权路径每请求一次库

`account/session.go:28-50` 的注释明确要求「每次请求走主库、不得缓存」，出发点是撤销即时生效。代价是：任何带 token 的读请求都要一次 MySQL RTT，热 feed 的读放大直接变成 DB 放大。这一层即使池修好了，也是把 MySQL 变成整个读路径的单点——6 个 feed 场景里 3 个带 token。

### 2.3 读路径的额外放大

- `feed/service.go:106-144`：一页 N 个视频 miss 时起 N 个 goroutine，每个各查一行（`GetByIDs(ctx, []uint{id})`）。`feed/repo.go` 的 `GetByIDs` 本来就支持 `IN ?` 批量，这里没用上。:135 还把查询错误静默吞掉，接口返回空列表，故障被掩盖。
- `feed/service.go:206-219`：冷数据的 singleflight key 里带了 `reqTime`，而游标翻页的 reqTime 每个请求都不同 → 合并率接近 0，等于没有。
- `video_service.go:142-149`：缓存 miss 后又把同一个 GET 重复发了一遍；拿不到锁时 5×20ms 轮询，把缓存穿透的延迟放大 100ms。

这些不是 300 并发崩溃的主因（主因是 2.1），但它们决定了池修好之后的天花板。

### 2.4 写路径：副作用早于唯一约束

`like_service.go` 的顺序是 `IsExist` → `IsLiked` → 发热度事件 → 发通知事件 → 返回 nil。真正落库发生在消费者里。所以：

- 判重是 check-then-act，11 个并发请求全部通过；
- 热度和通知在 HTTP 路径上就已经发出去了，DB 的唯一键只能挡住 `likes` 表那一行；
- 写请求的「成功」不代表已落库，P99 33s 是排队时间，不是写时间。

正解是把顺序倒过来：**先做幂等插入（`INSERT ... ON DUPLICATE KEY` / gorm `clause.OnConflict{DoNothing:true}`），只有 `RowsAffected == 1` 时才 +1 热度、发通知。** 唯一约束从「事后兜底」变成「判重本身」，既去掉竞态，也去掉每个请求的两次查询。

### 2.5 消费侧吞吐与 ack 语义

- `cmd/worker/main.go:109-131`：一个 Channel、一次 `Qos(50)`、4 个 consumer 各跑一个 goroutine。amqp091 的 Channel 不是并发安全的，这样既不能并行消费，也让发布和消费互相阻塞（HTTP 路径发布用的也是 `rabbitMQ.go:98-115` 里那同一个 `r.Ch`）。
- `notificationworker.go:114`：`Scan` 的错误被丢弃，authorID=0 被当成「视频不存在」直接 ACK。**基础设施错误必须 Nack/requeue，只有确定不可重试的情况才能 ACK。**

## 3. 修复方案

> 落地状态：P0、P2、P4 与 P3 的「每消费者独立 Channel + ack/Nack 语义」已实现并通过 `go build` / `go vet` / 测试包编译；P1 评估后放弃（理由见 P1 小节）；P3 里的批量写、多 goroutine 消费、publisher confirm 尚未做，留作下一轮。

### P0（已实现）

`backend/internal/db/db.go`

- DSN 增加 `interpolateParams=true&timeout=5s&readTimeout=10s&writeTimeout=10s`；
- `SetMaxOpenConns(64)`、`SetMaxIdleConns(64)`（与 maxOpen 相等，避免还池即关）、`SetConnMaxLifetime(30m)`、`SetConnMaxIdleTime(5m)`；
- 可用 `MYSQL_MAX_OPEN_CONNS` / `MYSQL_MAX_IDLE_CONNS` 覆盖，不引入配置结构体变更。

`backend/internal/middleware/redis/redis.go`

- `PoolSize=64`、`MinIdleConns=8`、`DialTimeout=2s`、`ReadTimeout=1s`、`WriteTimeout=1s`、`PoolTimeout=2s`、`ConnMaxIdleTime=5m`。

`backend/cmd/main.go`

- 用显式 `http.Server` 替换 `gin.Run`：`ReadHeaderTimeout=10s`、`ReadTimeout=30s`、`IdleTimeout=120s`；
- **故意不设 `WriteTimeout`**：`/notification/stream` 是 SSE 长连接，写超时会掐断事件流。

预期：auth hot 300 的 503 消失，anon cold 300 的 `bad connection` / `refused` 消失。这是本次最值得先验证的一步。

### P1 鉴权缓存（评估后不做）

`account/session.go:33-34` 的原注释写明：「Cached session state must never grant access or be written back here.」——每请求查主库是这个模块的显式设计约束（撤销要即时生效），不属于需要修的 bug。因此本轮没有加 Redis 会话缓存，鉴权路径的 503 完全交给 P0 的连接池/超时修复。如果之后仍要降低鉴权对 DB 的依赖，正确做法不是缓存会话，而是把 `FindSession` 换成一次覆盖索引、可并发的点查，或引入独立的鉴权缓存服务并接受「撤销有秒级延迟」的产品决策。

### P2 点赞/评论改成「先插入、再发事件」（已实现）

```go
res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&like)
if res.Error != nil {
    return res.Error
}
if res.RowsAffected == 0 { // 已点过
    return ErrAlreadyLiked   // handler 映射为 200 {already_liked:true}
}
// 只有这里才 ChangeLikesCount(+1)、发热度事件、发通知
```

- `like_service.go`：`Like`/`Unlike` 改为单事务「`SELECT` 视频 → 幂等插入/删除 → 一条 `UPDATE` 同时加 `likes_count` 与 `popularity`」；提交后才发热度事件，且**只有首次插入成功才发通知**，重复点赞直接返回 `ErrAlreadyLiked`，不再产生任何事件。
- `like_handler.go`：`ErrAlreadyLiked`/`ErrNotLiked` 映射为 200 幂等响应（原来是 500），其余错误走 `apierror.ClassifyHTTPStatus`。
- `comment_service.go`：`Publish` 改为事务「`SELECT` 视频 → `Create(comment)` → 一条 `UPDATE` 加 `popularity`」，提交后才发评论通知、热度事件与 `@` 通知。
- 事件结构新增 `Applied bool`（`likeMQ.go` / `commentMQ.go`）：新事件 `Applied=true`，消费者看到就不再重复落库；滚动发布期间队列里 `applied=false` 的旧消息仍按原异步逻辑处理，不会丢写。
- 同步落库后，写请求的「成功」就等于已落库，落库 P99 从 33s 回到一次事务的耗时。

### P3 消费者（部分实现）

- 已实现：每个消费 goroutine 独享一个 amqp Channel（`RabbitMQ.NewChannel()`），`cmd/worker/main.go` 里 social/like/comment/popularity 各自 `Qos(50)`；API 进程里 3 个 notification 消费者也各开一个 Channel。发布侧改为独立 `pubCh` + `sync.Mutex` 串行化，并在发布失败时丢弃坏 Channel、下次重建——原来 HTTP 处理器并发复用同一个 `r.Ch`，amqp091 明确不支持这种用法。
- 已实现：`notificationworker.go` 的 `Scan` 错误不再丢弃，抽成 `videoAuthorID(ctx, videoID) (uint, error)`，查询失败直接 `return err` 走 Nack/requeue；所有 worker 的「超过重试次数」从 `Ack` 改成 `Nack(false,false)` 送入 DLX，不再静默丢消息。
- 未做：批量写（`CreateInBatches`）、每队列多 goroutine、publisher confirm。写路径改成同步后队列只剩通知流量（每条约 2 次查询），当前吞吐足够；等通知量级上来再做。

### P4 feed 读路径（已实现）

- `GetVideoByIDs` 的 L3 改为一次 `GetByIDs(ctx, missIDs)`（配合 singleflight，key 为 id 集合的摘要），不再一页起 N 个 goroutine 各查一行；错误向上返回，不再静默返回空列表；Redis 回写改为一次异步批量。
- `GetDetail` 删掉重复的第二次 GET，Redis 非 miss 错误直接走 DB，不再走锁轮询。
- 未做：冷数据 singleflight key 去掉 `reqTime`。冷分页本身是合法不同游标，合并收益有限，留待观测后再定。

### P5 观测（部分实现）

- 已实现：`observability/pprof.go` 的 `NewPprofMux(databases ...*sql.DB)` / `NewPprofServer(..., databases ...*sql.DB)` 增加 `GET /debug/dbstats`，直接输出 `sql.DBStats`（`InUse`/`Idle`/`WaitCount`/`WaitDuration`/`MaxOpenConnections`）；`cmd/main.go:85-90`、`cmd/worker/main.go:167-172` 已把连接池传进去。下次压测可以直接区分「池排队」和「DB 慢」。
- 已实现：`internal/db/db_test.go` 的 `TestConcurrentBurstsReuseBoundedPool`——3 轮 40 并发打 `SELECT CONNECTION_ID(), SLEEP(0.005)`，断言物理连接数不超过 `MYSQL_MAX_OPEN_CONNS`、三轮之间连接被复用、且确实触发过池排队。需要 `FEEDSYSTEM_TEST_MYSQL_DSN`，未设置时自动 Skip。
- 未做：API 日志里给每个 5xx 带 `stage`（auth / feed / detail）和根因错误串。

## 4. 验证

已执行（本轮）：

```text
go build ./...                                   exit 0
go vet ./...                                     exit 0
go test -run '^$' ./...                          全部测试包编译通过
go test ./internal/account/ -run 'TestSession|TestValidate' -count=1   ok
gofmt -w  <改动的 13 个文件>                      exit 0
```

未执行：`./test/load/run-p0.ps1` 全量复测。原因是本机沙箱里 Docker 不可用（`docker version` → `open //./pipe/dockerDesktopLinuxEngine: Access is denied`），压测容器起不来；需要在装有 Docker Desktop 的普通终端里跑一次：

```powershell
cd backend
go test ./...
cd ..
./test/load/run-p0.ps1
```

对照基线看四个数：

| 指标 | 基线 | 目标 |
| --- | --- | --- |
| auth hot 300 失败率 | 89.50% | < 1%（503 归零） |
| anon cold 300 失败率 | 70.90% | 0 |
| 写 like 落库 P99 | 33.8s | < 500ms |
| duplicate_like | Redis 热度 11 / 通知 11 | 热度 1 / 通知 1 |

如果 P0 之后 auth hot 300 仍上不去，再按 P1 → P4 的顺序加，每次只改一层，避免把收益混在一起。

## 5. 不要做的事

- 不要调大 `max_connections` 或 buffer pool 来「解决」这个问题：103/400 已经说明不是连接数上限。
- 不要给鉴权加长 TTL 又不做失效：撤销会话会失效，这是安全问题。
- 不要用 `SetMaxOpenConns` 很小（比如 8）来「保护」MySQL：300 并发下会变成等连接，P95 直接恶化。64 是按单进程 16 核估的起点，用 `MYSQL_MAX_OPEN_CONNS` 扫一遍。
- 不要把 SSE 的 `WriteTimeout` 一起设上（见 P0 说明）。

## 6. 待确认

1. 生产是否多实例？如果多实例，`MYSQL_MAX_OPEN_CONNS=64` 要按实例数分摊，总量控制在 MySQL 舒适区。
2. 13307 的 refused 是否只在 Docker 发布端口下出现？可以在压测机上把 MySQL 换成原生实例对比一次，确认端口代理的贡献。
3. 是否需要「点赞最终一致」而不是同步落库？如果业务允许，P2 的同步插入也可以换成 outbox，但需要接受秒级可见性。
4. 接口语义变了：重复点赞由 `500 {"error":"user has liked this video"}` 改为 `200 {"message":"like success","already_liked":true}`，重复取消点赞同理。前端如果靠 500 判断「已点赞」，需要同步改成读 `already_liked`。
5. 重复点赞不再给作者重复发通知（只有首次插入成功才发）。这是 p0bench 的校验口径（`notifications == expectedRows == 1`），也是业务上更合理的行为，但如果产品要求「每次点击都发通知」需要另行确认。

