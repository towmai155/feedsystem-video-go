# Level 2 + Level 3 面试模拟稿

这份稿子按面试节奏整理：先用 Level 2 讲清项目整体，再用 Level 3 回答追问。回答时尽量落到源码入口、数据结构和降级方案。

## Level 2：项目整体怎么讲

### 问：你这个项目整体架构是什么？

答：

这个项目可以看成一个短视频后端系统，整体是：

```text
API 进程 + Worker 进程 + MySQL + Redis + RabbitMQ
```

API 进程负责接收前端请求，比如注册登录、发布视频、点赞评论、关注、拉取 feed 流。

Worker 进程负责处理异步任务，比如消费 RabbitMQ 里的点赞、评论、关注、时间线更新消息，做统计更新、缓存更新、通知推送等。

MySQL 是主存储，保存账号、视频、点赞、评论、关注关系等核心数据。

Redis 用来做缓存、排行榜、时间线、限流和 token 缓存。

RabbitMQ 用来做异步解耦，比如用户点赞后，不一定所有统计和通知都在接口里同步完成，可以发消息给 worker 异步处理。

源码入口：

```text
backend/cmd/main.go
backend/cmd/worker/main.go
backend/internal/http/router.go
```

### 问：三层架构是怎么分的？

答：

项目里每个业务模块基本是三层：

```text
handler -> service -> repo
```

`handler` 负责 HTTP 接口，解析 JSON、校验参数、返回响应。

`service` 负责业务逻辑，比如登录时校验密码、生成 token；feed 里决定查 Redis 还是查 MySQL、生成游标、组装返回数据。

`repo` 负责数据库访问，比如 GORM 的 `Create`、`Find`、`Where`、`Update`。

比如 feed 模块：

```text
backend/internal/feed/handler.go
backend/internal/feed/service.go
backend/internal/feed/repo.go
```

调用链是：

```text
前端请求
-> handler 解析参数
-> service 执行业务逻辑
-> repo 查询数据库
-> service 组装响应
-> handler 返回 JSON
```

### 问：Redis 在项目里怎么用？

答：

Redis 主要有几类用途。

第一类是 token 缓存。登录后 access token 和 refresh token 会写 Redis，JWT 中间件校验时优先查 Redis，Redis 没有再查 MySQL。

源码入口：

```text
backend/internal/account/service.go
backend/internal/middleware/jwt/jwt.go
```

第二类是 feed 时间线缓存。最新视频流用 Redis ZSET：

```text
key: feed:global_timeline
member: 视频 ID
score: 视频发布时间戳
```

源码入口：

```text
backend/internal/feed/service.go
FeedService.ListLatest
```

第三类是视频实体缓存。`GetVideoByIDs` 里用了：

```text
本地缓存 -> Redis -> MySQL
```

第四类是热榜。热门视频用 Redis ZSET 记录一段时间内的视频热度，再用 `ZUnionStore` 合并最近 60 分钟的数据，形成热门榜快照。

源码入口：

```text
backend/internal/feed/service.go
FeedService.ListByPopularity
```

### 问：RabbitMQ 异步的价值是什么？

答：

RabbitMQ 主要用来把“用户请求”和“后续处理”拆开。

比如点赞、评论、关注这些动作，API 可以先完成核心操作，然后把事件发到 MQ。Worker 消费消息后再做统计更新、热度更新、通知推送等。

好处是：

```text
接口响应更快
业务模块解耦
高峰期可以靠队列削峰
失败任务可以重试
```

源码入口：

```text
backend/internal/middleware/rabbitmq
backend/internal/worker
```

### 问：Outbox 是干什么的？

答：

Outbox 是为了解决：

```text
数据库写成功了，但 MQ 消息发送失败
```

比如发布视频时，如果先写 MySQL，再发 MQ，中间 MQ 挂了，就会出现视频已经发布，但时间线更新消息没发出去。

Outbox 的做法是：

```text
在同一个数据库事务里，写业务数据 + 写 outbox 消息
```

然后 worker 定时扫描 outbox 表，把未发送的消息投递到 RabbitMQ，成功后再标记已发送。

源码入口：

```text
backend/internal/video/video_entity.go
backend/internal/worker/outboxworker.go
```

### 问：游标分页是什么？

答：

feed 流不用普通 `page=1&page_size=10`，而是用游标分页。

比如最新视频流用 `latest_time`：

```text
第一页不传 latest_time，查最新 10 条
后端返回 next_time
下一页前端带 next_time
后端查这个时间之前的视频
```

按点赞数或热度排序时，会用组合游标：

```text
likes_count_before + id_before
popularity + create_time + id
```

因为点赞数、热度可能重复，所以需要加 ID 保证位置唯一，避免重复或漏数据。

源码入口：

```text
backend/internal/feed/entity.go
backend/internal/feed/handler.go
backend/internal/feed/service.go
backend/internal/feed/repo.go
```

### 问：什么是推拉结合？

答：

这个项目里 feed 和通知可以理解成推拉结合。

“拉”是用户主动请求接口，比如：

```text
/feed/listLatest
/feed/listByFollowing
/feed/listByPopularity
```

后端查 Redis 或 MySQL，返回一页视频。

“推”是系统在事件发生后，通过 MQ 和 worker 更新缓存、时间线或通知。比如发布视频后，worker 可以异步更新 timeline；点赞、评论、关注事件可以异步触发通知。

所以整体是：

```text
读的时候拉取列表
写的时候通过 MQ 推动缓存、时间线、通知更新
```

### 问：热榜是怎么设计的？

答：

热门视频不是每次请求都实时扫 MySQL 排序，而是用 Redis ZSET 做热榜。

项目会按时间窗口记录热度，比如每分钟一个 key：

```text
hot:video:1m:{minute}
```

热门接口查询时，把最近 60 分钟的 ZSET 合并：

```text
ZUnionStore
```

然后用：

```text
ZRevRange
```

按分数从高到低取视频 ID。

这里还用了 `as_of + offset` 做稳定分页：

```text
as_of 表示这次热门榜快照时间
offset 表示下一页从第几个位置继续取
```

源码入口：

```text
backend/internal/feed/service.go
FeedService.ListByPopularity
```

## Level 3：抗追问模拟

### 问：你项目里怎么防缓存击穿？

答：

项目里主要用 `singleflight` 防缓存击穿。

比如 feed 最新流的 Redis key 是：

```text
feed:global_timeline
```

入口在：

```text
backend/internal/feed/service.go
FeedService.ListLatest
```

`ListLatest` 一开始会查 Redis：

```text
ZRangeWithScores("feed:global_timeline", 0, 0)
```

如果发现 Redis 时间线是空的，就需要从 MySQL 查最新 1000 条视频重建缓存。

这里用了：

```go
f.requestGroup.Do(sfKey, func() (interface{}, error) {
    dbVideos, err := f.repo.ListLatest(ctx, 1000, time.Time{})
    ...
})
```

`requestGroup` 是：

```go
singleflight.Group
```

同一个 `sfKey` 的并发请求，只会有一个请求执行查库和重建 Redis，其他请求等待并复用结果。

它防的是：

```text
热点 key 消失后，大量请求同时打到 MySQL
```

### 问：singleflight 的 key 是哪里来的？Do 是什么？

答：

key 是项目自己定义的，不是包里自带的。

比如：

```go
sfKey := f.rediscache.Key("sf:fallback:global_timeline_rebuild")
```

`singleflight` 只根据这个字符串判断是不是同一个任务。

`Do` 是 `singleflight.Group` 的方法：

```go
f.requestGroup.Do(sfKey, func() (interface{}, error) {
    ...
})
```

它内部大概维护：

```go
map[string]*call
```

第一个请求进来时，把 key 登记进去并执行函数。后面相同 key 的请求发现任务已经在执行，就等待前一个请求完成，然后拿同一份结果。

### 问：缓存雪崩是什么？项目怎么缓解？

答：

缓存雪崩是大量缓存同一时间失效，或者 Redis 整体不可用，导致大量请求同时打到数据库。

项目里有几种缓解方式。

第一，部分 Redis 操作加了超时：

```go
context.WithTimeout(ctx, 50*time.Millisecond)
```

Redis 慢或不可用时，请求不会一直卡住。

第二，视频实体查询是三级缓存：

```text
本地缓存 -> Redis -> MySQL
```

入口：

```text
FeedService.GetVideoByIDs
```

第三，缓存没命中时用 `singleflight` 或 Redis lock，防止大家一起重建缓存。

第四，Redis 不可用时，部分接口可以降级到 MySQL。比如热门流 Redis 失败后走：

```go
f.repo.ListByPopularity(...)
```

但我也会说明：项目只能缓解雪崩，不是完全解决。更完整的方案还需要 Redis 高可用、热点 key 预热、过期时间随机化、接口限流和降级返回。

### 问：冷热分离是什么？你项目哪里用了？

答：

冷热分离就是：

```text
最近经常访问的数据放 Redis
较老、访问少的数据查 MySQL
```

项目里最新 feed 用了冷热分离。

入口：

```text
FeedService.ListLatest
```

Redis 的 key 是：

```text
feed:global_timeline
```

代码里会取 Redis 里最旧一条的时间作为 `watermark`：

```go
watermark := int64(zsetTail[0].Score)
```

如果用户请求的 `latest_time` 比 `watermark` 还老，说明 Redis 没这段数据了，就走 MySQL：

```go
f.repo.ListLatest(ctx, limit, latestBefore)
```

如果请求的是较新的时间段，就走 Redis：

```text
ZRevRangeByScore
```

这样 Redis 不需要保存无限历史，老数据仍然可以从数据库查。

### 问：RabbitMQ 挂了怎么办？

答：

项目里 RabbitMQ 是可选的，API 启动时如果 MQ 初始化失败，不会直接让服务挂掉，而是降级。

入口：

```text
backend/internal/http/router.go
```

比如：

```go
popularityMQ, err := rabbitmq.NewPopularityMQ(rmq)
if err != nil {
    log.Printf("PopularityMQ init failed (mq disabled): %v", err)
    popularityMQ = nil
}
```

这表示：

```text
MQ 不可用时，同步 API 仍然能启动和处理核心请求
异步功能比如热度更新、通知推送可能降级或延后
```

如果是必须保证最终投递的消息，就靠 Outbox 先落 MySQL，再由 worker 后续补发。

### 问：消息重复消费怎么办？

答：

MQ 里消息重复是正常情况，系统不能假设消息只消费一次。

需要用幂等消费来处理。

幂等的意思是：

```text
同一条消息处理一次和处理多次，最终结果一样
```

比如点赞事件，如果重复消费，不能让点赞数重复加很多次。常见做法是：

```text
先查点赞记录是否存在
存在就不重复插入或重复加计数
```

取消点赞也是类似：

```text
只有原来存在点赞记录，才删除并减少计数
```

源码入口可以看：

```text
backend/internal/worker/likeworker.go
backend/internal/worker/commentworker.go
backend/internal/worker/socialworker.go
backend/internal/worker/notificationworker.go
```

幂等通常落在：

```text
repo 的唯一约束
数据库条件更新
消费端先查再写
消息处理成功后 ack
失败时 nack/retry
```

### 问：JWT 怎么吊销？纯 JWT 不是没法主动失效吗？

答：

对，纯 JWT 一旦签发，在过期前通常不能主动失效。

项目里用的是：

```text
JWT + 服务端保存 token
```

登录时 service 会生成 access token 和 refresh token：

```text
AccountService.Login
auth.GenerateToken
auth.GenerateRefreshToken
```

然后把 token 存到数据库和 Redis：

```go
accountRepository.Login(...)
cache.SetBytes("account:%d", accessToken)
```

校验时，JWT 中间件不只是解析 token，还会检查当前 token 是否和服务端保存的一致。

入口：

```text
backend/internal/middleware/jwt/jwt.go
JWTAuth
check
```

流程是：

```text
解析 Authorization Bearer token
-> auth.ParseToken 拿到 account_id
-> 先查 Redis account:{id}
-> Redis 没有再查 MySQL account.Token
-> 如果请求 token 和服务端保存 token 不一致，就拒绝
```

退出登录时：

```text
AccountService.Logout
```

会清空数据库 token，并删除 Redis token。

所以 JWT 吊销靠的是：

```text
服务端 token 状态校验
```

### 问：refresh token 是怎么处理的？

答：

access token 有效期短，`auth.GenerateToken` 里是 15 分钟。

refresh token 是随机字符串：

```text
auth.GenerateRefreshToken
```

登录后会保存到数据库和 Redis。

刷新入口是：

```text
AccountService.RefreshAccessToken
```

它会先查 Redis：

```text
refresh:{refreshToken} -> accountID
```

如果 Redis 查不到，就降级查数据库账号里的 refresh token。

找到后重新生成 access token：

```go
auth.GenerateToken(account.ID, account.Username)
```

然后更新数据库和 Redis。

### 问：热门榜怎么保证分页稳定？

答：

热门榜用了 Redis ZSET 和 `as_of + offset`。

入口：

```text
FeedService.ListByPopularity
```

它会取一个分钟级快照时间：

```go
asOf := time.Now().UTC().Truncate(time.Minute)
```

然后合并最近 60 分钟的热度 ZSET：

```text
ZUnionStore
```

合并结果放到：

```text
hot:video:merge:1m:{as_of}
```

然后用：

```text
ZRevRange(dest, offset, offset+limit-1)
```

取一页。

后端返回：

```text
as_of
next_offset
```

前端下一页带回来。这样即使实时热度还在变化，用户翻页时也是基于同一个 `as_of` 快照分页，不容易重复、漏数据或列表乱跳。

### 问：接口怎么降级？

答：

项目里降级主要体现在几个地方。

Redis 不可用：

```text
从 Redis 查不到，就查 MySQL
```

比如热门榜 Redis 失败后走：

```go
repo.ListByPopularity
```

MQ 不可用：

```text
API 继续运行，异步功能禁用或延后
```

比如 router 里 MQ 初始化失败会设成 `nil`。

token 缓存失效：

```text
JWT 校验先查 Redis，查不到再查 MySQL
```

这些降级策略的目标是：

```text
优先保证核心功能可用
允许非核心功能延迟或弱一致
```

## 面试结尾总结

可以这样收尾：

```text
这个项目不是只做 CRUD，它是一个 API + Worker 的短视频后端系统。
MySQL 存核心数据，Redis 承担缓存、时间线、排行榜和限流，RabbitMQ 做异步解耦和削峰。
代码上按 handler/service/repo 三层拆分。
feed 使用游标分页保证列表稳定，热榜用 Redis ZSET + 时间窗口合并，发布和互动事件通过 MQ + Outbox 保证异步处理的可靠性。
高并发场景下，用 singleflight、Redis lock、本地缓存、MySQL fallback、JWT 服务端状态校验等方式处理缓存击穿、消息重复、MQ 故障和 token 吊销问题。
```
