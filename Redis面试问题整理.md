# Redis 面试问题整理

## 1. 这个项目里 Redis 用在哪里

这个项目里 Redis 不是只做普通缓存，主要有这些用途：

- 登录态 token 缓存
- refresh token 反查用户 ID
- 视频详情缓存
- Feed 全局时间线 ZSet
- 热榜分钟窗口 ZSet
- 热榜快照 ZSet
- 分布式锁，防止缓存击穿
- 接口限流

重点源码位置：

- `backend/internal/middleware/jwt/jwt.go`
- `backend/internal/account/service.go`
- `backend/internal/video/video_service.go`
- `backend/internal/feed/service.go`
- `backend/internal/video/popularity_cache.go`
- `backend/internal/middleware/ratelimit/ratelimit.go`

## 2. 项目相关 Redis 高频问题

### 2.1 为什么 JWT 还要存 Redis

普通 JWT 一旦签发，只要没有过期，服务端通常很难主动让它失效。

这个项目为了实现“可撤销 token”，登录后会把 access token 存到 Redis：

```text
account:<id> -> access token
```

请求进入 JWT 中间件后，会比较：

```text
请求里的 token == Redis 或 MySQL 里当前保存的 token
```

如果用户登出、改密码、改名，服务端会删除或更新 Redis/MySQL 中的 token，这样旧 token 就失效了。

### 2.2 Redis 挂了怎么办

这个项目里 Redis 不是所有业务的强依赖。

以 JWT 鉴权为例：

```text
先查 Redis
Redis 不可用或未命中
-> 回源 MySQL 查 account.token
-> 校验成功后再回填 Redis
```

所以 Redis 挂掉时，核心鉴权仍然可以通过 MySQL 兜底，只是性能会下降。

### 2.3 视频详情缓存怎么做

视频详情缓存 key：

```text
video:detail:id=<videoID>
```

流程：

```text
查 Redis
-> 命中直接返回
-> miss 后尝试加锁
-> 拿到锁的请求查 MySQL
-> 写回 Redis，TTL 5 分钟
-> 其他请求短暂等待缓存回填
```

这个设计主要是为了防止热点视频详情缓存过期后，大量请求同时打到 MySQL。

### 2.4 Feed 最新流为什么用 Redis ZSet

最新流需要按发布时间倒序取视频，Redis ZSet 很适合做这种排序集合。

项目里的全局时间线大致是：

```text
key: feed:global_timeline
member: videoID
score: 视频发布时间戳
```

查询时按 score 倒序取出视频 ID，再批量查视频详情。

### 2.5 热榜为什么用 Redis ZSet

热榜需要按热度分数排序，ZSet 很适合：

```text
member: videoID
score: 热度分数
```

项目里使用分钟级窗口 key：

```text
hot:video:1m:<yyyyMMddHHmm>
```

点赞、评论等行为会带来热度增量，写入当前分钟窗口。

查询热榜时，会聚合最近多个分钟窗口，生成快照 key：

```text
hot:video:merge:1m:<as_of>
```

### 2.6 为什么热榜要用快照分页

如果热度实时变化，用户翻页时排序可能发生变化，容易出现：

- 重复数据
- 漏数据
- 跳页

所以项目使用：

```text
as_of + offset
```

第一次请求确定一个快照时间，后续分页都基于同一个快照读取，这样分页结果更稳定。

### 2.7 项目如何防止缓存击穿

缓存击穿是某个热点 key 过期后，大量请求同时打到数据库。

项目在视频详情缓存里使用了：

- Redis 锁
- 短暂等待缓存回填
- 回源后写回 Redis

核心思想：

```text
只允许一个请求回源 MySQL，其他请求等待缓存回填
```

### 2.8 项目里的 Redis key 设计

常见 key：

```text
account:<id>
account:<id>:refresh
refresh:<refreshToken>
video:detail:id=<videoID>
feed:global_timeline
hot:video:1m:<yyyyMMddHHmm>
hot:video:merge:1m:<as_of>
```

好的 key 设计一般要满足：

- 业务前缀清楚
- 包含必要维度
- 避免过长
- 避免 BigKey
- 方便定位和排查问题

## 3. 项目 Redis 面试回答模板

如果面试官问“你项目里 Redis 怎么用的”，可以这样答：

```text
这个项目里 Redis 主要有三类用途。

第一类是登录态缓存。登录后把 access token 存成 account:<id>，refresh token 也有对应 key。JWT 中间件校验时先查 Redis，Redis 不可用再回源 MySQL，所以可以实现 token 撤销，也能做降级兜底。

第二类是视频和 Feed 缓存。视频详情用 video:detail:id=<id>，先查 Redis，miss 后加锁回源 MySQL，再写回缓存，避免热点详情击穿数据库。最新 Feed 用 ZSet 存全局时间线，member 是 videoID，score 是发布时间。

第三类是热榜。点赞评论产生热度增量，写入分钟级 ZSet，比如 hot:video:1m:<time>。查询热榜时聚合最近窗口生成快照，用 as_of 和 offset 做稳定分页，避免热度实时变化导致翻页重复或漏数据。
```

## 4. Redis 通用八股问题

### 4.1 Redis 为什么快

常见原因：

- 基于内存
- 单线程执行命令，避免大量锁竞争
- IO 多路复用
- 数据结构高效
- C 语言实现，网络模型成熟

补充：

Redis 6 之后引入了多线程 IO，但命令执行主流程仍然主要是单线程模型。

### 4.2 Redis 常见数据结构

| 数据结构 | 常见用途 |
| --- | --- |
| String | 缓存、计数器、token |
| Hash | 对象字段缓存 |
| List | 消息列表、简单队列 |
| Set | 去重、共同关注 |
| ZSet | 排行榜、延迟队列、Feed |
| Bitmap | 签到、活跃状态 |
| HyperLogLog | UV 统计 |
| Stream | 消息队列 |

这个项目里主要用：

- String
- ZSet

### 4.3 缓存穿透、击穿、雪崩

缓存穿透：

```text
查询一个缓存和数据库都不存在的数据，每次请求都打到数据库。
```

解决方案：

- 参数校验
- 缓存空值
- 布隆过滤器

缓存击穿：

```text
热点 key 过期，大量请求同时打到数据库。
```

解决方案：

- 互斥锁
- singleflight
- 热点 key 不过期
- 提前异步刷新

缓存雪崩：

```text
大量 key 同时过期，或者 Redis 整体宕机。
```

解决方案：

- TTL 加随机值
- 多级缓存
- 限流降级
- Redis 高可用

### 4.4 Redis 持久化方式

RDB：

```text
定时生成内存快照。恢复快，占用小，但可能丢失最近一段时间的数据。
```

AOF：

```text
记录写命令。数据更完整，但文件更大，恢复可能较慢。
```

实际生产中常见：

```text
RDB + AOF 混合持久化
```

### 4.5 Redis 内存淘汰策略

常见策略：

| 策略 | 含义 |
| --- | --- |
| noeviction | 不淘汰，写入时报错 |
| allkeys-lru | 所有 key 中淘汰最近最少使用 |
| volatile-lru | 只淘汰设置了过期时间的 key |
| allkeys-lfu | 所有 key 中淘汰最不常用 |
| volatile-lfu | 只淘汰设置了过期时间的 key |
| allkeys-random | 所有 key 中随机淘汰 |
| volatile-random | 从设置了过期时间的 key 中随机淘汰 |
| volatile-ttl | 优先淘汰快过期的 key |

缓存场景常用：

```text
allkeys-lru
allkeys-lfu
```

### 4.6 Redis 如何做分布式锁

基础加锁命令：

```text
SET lock_key unique_value NX EX 10
```

要点：

- `NX` 表示不存在时才设置
- `EX` 设置过期时间，避免死锁
- value 要唯一，标识锁的拥有者
- 释放锁时要先判断 value 是否是自己的
- 判断和删除最好用 Lua 脚本保证原子性

不能简单直接 `DEL lock_key`，否则可能误删别人的锁。

### 4.7 Redis 和数据库如何保持一致

常见方案：

```text
先更新数据库，再删除缓存
```

为什么通常删除缓存，而不是更新缓存：

- 更新缓存可能出现并发覆盖
- 缓存数据可能是复杂聚合结果
- 删除后下次读取再回源更简单可靠

进一步增强方式：

- 延迟双删
- 消息队列异步删除
- 订阅 binlog
- 设置较短 TTL 兜底

### 4.8 BigKey 是什么

BigKey 是 value 很大的 key，比如：

- 一个 String 有几 MB
- 一个 Hash/Set/ZSet 有几十万成员

问题：

- 阻塞 Redis
- 网络传输慢
- 删除慢
- 主从同步压力大
- 内存倾斜

解决方案：

- 拆分 key
- 分页读取
- 用 `UNLINK` 异步删除
- 避免一次取全量

### 4.9 HotKey 是什么

HotKey 是访问特别集中的 key。

问题：

- 单点访问压力大
- Redis 实例负载不均
- 可能拖慢整体服务

解决方案：

- 本地缓存
- 多副本 key
- 读写分离
- 限流
- 热点 key 预热
- 热点 key 不过期或异步刷新

### 4.10 Redis 主从、哨兵、Cluster

主从复制：

```text
用于读写分离和数据备份，但不能自动故障转移。
```

哨兵 Sentinel：

```text
监控主节点，主节点挂了可以自动选举新主。
```

Cluster：

```text
数据分片，支持水平扩展。
```

### 4.11 Redis 事务

Redis 事务常用命令：

```text
MULTI
EXEC
DISCARD
WATCH
```

特点：

- 命令按顺序执行
- 不像 MySQL 那样支持完整回滚
- `WATCH` 可以实现乐观锁

### 4.12 Redis 过期删除策略

Redis 的过期删除主要有两种：

惰性删除：

```text
访问 key 时发现过期，再删除。
```

定期删除：

```text
后台定期抽样检查并删除过期 key。
```

同时 Redis 还会结合内存淘汰策略控制内存。

## 5. 面试建议

如果被问 Redis，不要只背概念。可以先用项目开场：

```text
我这个项目里 Redis 主要用在 token 校验、视频详情缓存、Feed 时间线和热榜。比如视频详情缓存用了 Redis key + 互斥锁防击穿，Feed 用 ZSet 做按时间排序，热榜用分钟窗口 ZSet 聚合并用快照分页。
```

然后再展开通用八股，比如缓存击穿、ZSet、分布式锁、数据一致性。

这样回答会比单纯背“Redis 为什么快”更像真实项目经验。
