# Level 4 复刻验收结果清单

## 1. 验收目标

目标：

```text
能关掉文档，从零复刻 medium 版核心模块：
账号鉴权、视频发布、Feed 分页、点赞评论、关注关系、热榜和 Worker 消费。
```

证明方式：

```text
能还原目录结构、接口设计、核心表、缓存 key、MQ topic 和关键 Service 编排。
```

这份文档就是最终验收清单。

## 2. 目录结构

需要能从零还原下面这套后端目录：

```text
backend/
  cmd/
    main.go
    worker/
      main.go
  configs/
    config.yaml
  internal/
    auth/
      jwt.go
    config/
      loadconfig.go
    db/
      db.go
    http/
      router.go
    middleware/
      jwt/
        jwt.go
      redis/
        redis.go
      rabbitmq/
        rabbitmq.go
        likeMQ.go
        commentMQ.go
        socialMQ.go
        popularityMQ.go
        timelineMQ.go
      ratelimit/
        ratelimit.go
    account/
      entity.go
      repo.go
      service.go
      handler.go
    video/
      video_entity.go
      video_repo.go
      video_service.go
      video_handler.go
      like_entity.go
      like_repo.go
      like_service.go
      like_handler.go
      comment_entity.go
      comment_repo.go
      comment_service.go
      comment_handler.go
      tag_entity.go
      popularity_cache.go
    feed/
      entity.go
      repo.go
      service.go
      handler.go
    social/
      entity.go
      repo.go
      service.go
      handler.go
    worker/
      outboxworker.go
      likeworker.go
      commentworker.go
      socialworker.go
      popularityworker.go
```

## 3. 接口设计

## 3.1 账号鉴权

```text
POST /account/register
POST /account/login
POST /account/refresh
POST /account/logout
POST /account/changePassword
POST /account/rename
POST /account/findByID
POST /account/findByUsername
POST /account/getProfile
```

## 3.2 视频

```text
POST /video/uploadVideo
POST /video/uploadCover
POST /video/publish
POST /video/getDetail
POST /video/listByAuthorID
```

## 3.3 Feed

```text
POST /feed/listLatest
POST /feed/listLikesCount
POST /feed/listByPopularity
POST /feed/listByFollowing
POST /feed/listByTag
```

## 3.4 点赞

```text
POST /like/like
POST /like/unlike
POST /like/isLiked
POST /like/listMyLikedVideos
```

## 3.5 评论

```text
POST /comment/publish
POST /comment/delete
POST /comment/listAll
```

## 3.6 关注

```text
POST /social/follow
POST /social/unfollow
POST /social/getAllFollowers
POST /social/getAllVloggers
POST /social/getCounts
```

## 3.7 通知可选

```text
GET  /notification/stream
POST /notification/list
POST /notification/markRead
POST /notification/unreadCount
```

## 4. 核心表

## 4.1 accounts

```text
id
username
password
token
refresh_token
avatar_url
bio
```

要点：

- password 存 bcrypt hash
- token 用于可撤销 JWT
- refresh_token 用于刷新 access token

## 4.2 videos

```text
id
author_id
username
title
description
play_url
cover_url
create_time
likes_count
popularity
```

要点：

- 视频文件不存数据库
- 数据库只保存 play_url 和 cover_url
- likes_count 是点赞数
- popularity 是热度分

## 4.3 likes

```text
id
video_id
account_id
created_at
unique(video_id, account_id)
```

要点：

- `video_id + account_id` 唯一索引用于防止重复点赞

## 4.4 comments

```text
id
username
video_id
author_id
content
create_time
```

## 4.5 socials

```text
id
follower_id
vlogger_id
unique(follower_id, vlogger_id)
```

要点：

- follower_id 表示关注者
- vlogger_id 表示被关注者

## 4.6 tags

```text
id
name
unique(name)
```

## 4.7 video_tags

```text
id
video_id
tag_id
```

## 4.8 outbox_msgs

```text
id
video_id
event_type
status
create_time
```

要点：

- 发布视频时写 pending 消息
- 用于后续异步更新 Feed 时间线

## 4.9 notifications 可选

```text
id
recipient_id
sender_id
type
target_id
content
is_read
created_at
```

## 5. Redis 缓存 key

## 5.1 登录态

```text
account:<accountID>             access token
account:<accountID>:refresh     refresh token
refresh:<refreshToken>          accountID
```

用途：

- JWTAuth 先查 Redis
- Redis miss 或不可用时回源 MySQL
- logout / changePassword 删除这些 key

## 5.2 视频详情

```text
video:detail:id=<videoID>
lock:video:detail:id=<videoID>
```

用途：

- 缓存 Video JSON
- TTL 建议 5 分钟
- miss 后加锁回源 MySQL

## 5.3 Feed 最新流

```text
feed:global_timeline
```

结构：

```text
ZSET
member = videoID
score = create_time UnixMilli
```

## 5.4 关注流缓存可选

```text
feed:listByFollowing:limit=<n>:accountID=<id>:before=<timestamp>
```

## 5.5 视频实体缓存可选

```text
video:entity:<videoID>
```

## 5.6 热榜分钟窗口

```text
hot:video:1m:<yyyyMMddHHmm>
```

结构：

```text
ZSET
member = videoID
score = popularity delta
```

## 5.7 热榜快照

```text
hot:video:merge:1m:<as_of>
```

用途：

- 聚合多个分钟窗口
- 使用 as_of + offset 稳定分页

## 5.8 限流 key

```text
ratelimit:account_login:<ip>
ratelimit:account_register:<ip>
ratelimit:like_write:<accountID>
ratelimit:comment_write:<accountID>
ratelimit:social_write:<accountID>
```

## 6. MQ topic / exchange / queue

## 6.1 点赞事件

```text
exchange: like.events
queue: like.events
binding: like.*
routing key:
  like.like
  like.unlike
```

事件体：

```json
{
  "action": "like",
  "user_id": 1,
  "video_id": 10,
  "ts": 1770000000
}
```

## 6.2 评论事件

```text
exchange: comment.events
queue: comment.events
binding: comment.*
routing key:
  comment.publish
  comment.delete
```

事件体：

```json
{
  "action": "publish",
  "username": "user001",
  "author_id": 1,
  "video_id": 10,
  "content": "hello",
  "ts": 1770000000
}
```

## 6.3 关注事件

```text
exchange: social.events
queue: social.events
binding: social.*
routing key:
  social.follow
  social.unfollow
```

事件体：

```json
{
  "action": "follow",
  "follower_id": 1,
  "vlogger_id": 2,
  "ts": 1770000000
}
```

## 6.4 热度事件

```text
exchange: video.popularity.events
queue: video.popularity.events
binding: video.popularity.*
routing key:
  video.popularity.update
```

事件体：

```json
{
  "video_id": 10,
  "change": 1,
  "reason": "like",
  "ts": 1770000000
}
```

## 6.5 时间线事件

```text
exchange: video.timeline.events
queue: video.timeline.update.queue
binding: video.timeline.*
routing key:
  video.timeline.publish
```

事件体：

```json
{
  "video_id": 10,
  "create_time": 1770000000000
}
```

## 7. 关键 Service 编排

## 7.1 账号注册

```text
CreateAccount
-> bcrypt 加密密码
-> repo.CreateAccount 写 MySQL
```

## 7.2 登录

```text
Login
-> repo.FindByUsername
-> bcrypt 校验密码
-> GenerateToken 生成 access token
-> GenerateRefreshToken 生成 refresh token
-> repo.Login 写 MySQL token / refresh_token
-> Redis 写 account:<id>
-> Redis 写 account:<id>:refresh
-> Redis 写 refresh:<refreshToken>
-> 返回 token
```

## 7.3 刷新 token

```text
RefreshAccessToken
-> 读取 refresh token
-> 优先 Redis refresh:<refreshToken> 反查 accountID
-> MySQL 校验 refresh_token 是否匹配
-> 生成新 access token
-> 更新 MySQL token
-> 更新 Redis account:<id>
-> 返回新 token
```

## 7.4 登出

```text
Logout
-> repo.FindByID
-> Redis 删除 account:<id>
-> Redis 删除 account:<id>:refresh
-> Redis 删除 refresh:<oldRefreshToken>
-> MySQL 清空 token / refresh_token
```

## 7.5 JWTAuth

```text
JWTAuth
-> 读取 Authorization: Bearer token
-> ParseToken 校验签名和过期时间
-> 得到 claims.account_id / username
-> Redis 查 account:<id>
-> 比较 Redis token 和请求 token
-> Redis 不可用或 miss 时回源 MySQL
-> 校验成功后 c.Set(accountID, username)
```

## 7.6 视频发布

```text
PublishVideo
-> JWTAuth
-> 从上下文拿 authorID / username
-> 组装 Video
-> 校验 title / play_url / cover_url
-> MySQL 事务
-> tx.Create(video) 写 videos
-> tx.Create(outbox) 写 outbox_msgs pending
-> ExtractTags
-> 写 tags / video_tags
-> 返回 video
```

## 7.7 视频详情

```text
GetDetail
-> key = video:detail:id=<id>
-> Redis 查缓存
-> 命中直接返回
-> miss 后尝试 lock
-> 拿到锁则 repo.GetByID 查 MySQL
-> 写回 Redis，TTL 5 分钟
-> 未拿到锁则短暂等待缓存回填
-> 兜底查 MySQL
```

## 7.8 Outbox 时间线

```text
StartOutboxPoller
-> 定时扫描 outbox_msgs status=pending
-> 投递 timeline MQ
-> 成功后删除 outbox 消息

TimelineConsumer
-> 消费 video.timeline.publish
-> Redis ZAdd feed:global_timeline
-> member = videoID
-> score = create_time
```

## 7.9 Feed 最新流

```text
ListLatest
-> 优先 Redis ZRevRangeByScore feed:global_timeline
-> 拿 videoID 列表
-> 批量查 videos
-> 批量查当前用户点赞状态
-> 组装 FeedVideoItem
-> Redis 为空或冷数据则 repo.ListLatest 回源 MySQL
-> 返回 next_time / has_more
```

## 7.10 关注流

```text
ListByFollowing
-> 当前用户 accountID
-> 查询 follower_id = accountID 的 vlogger_id
-> 查 author_id IN vlogger_id 的 videos
-> 按 create_time desc 排序
-> 补 is_liked
-> 返回分页结果
```

## 7.11 点赞

```text
Like
-> 校验 video exists
-> 校验未点赞
-> 发布 like MQ
-> 发布 popularity MQ
-> 两个 MQ 都成功则返回
-> like MQ 失败则同步写 likes 表，并更新 likes_count / popularity
-> popularity MQ 失败则直接写 Redis 热度窗口
```

## 7.12 取消点赞

```text
Unlike
-> 校验 video exists
-> 校验已点赞
-> 发布 unlike MQ
-> 发布 popularity -1 MQ
-> MQ 失败则同步删除 likes 并减少计数
```

## 7.13 评论

```text
PublishComment
-> 校验 video exists
-> 校验 content 非空
-> 发布 comment MQ
-> 发布 popularity +1 MQ
-> comment MQ 失败则同步写 comments 表
-> popularity MQ 失败则直接更新 Redis 热度窗口
-> 解析 @username 生成通知
```

## 7.14 关注

```text
Follow
-> 校验不能关注自己
-> 校验用户存在
-> 发布 social.follow MQ
-> MQ 失败则同步写 socials 表
```

## 7.15 热榜

```text
UpdatePopularityCache
-> 当前分钟 key hot:video:1m:<yyyyMMddHHmm>
-> ZINCRBY videoID change
-> 设置 TTL

ListByPopularity
-> 确定 as_of
-> 取最近 60 个分钟窗口 key
-> ZUNIONSTORE 聚合到 hot:video:merge:1m:<as_of>
-> ZRevRange 按 offset 分页取 videoID
-> 批量查 videos
-> 补 is_liked
-> 返回 as_of / next_offset / has_more
-> Redis 不可用时 MySQL 按 popularity 兜底
```

## 7.16 Worker 消费

```text
Worker main
-> 连接 MySQL / Redis / RabbitMQ
-> 声明 exchange / queue / binding
-> 启动 LikeWorker
-> 启动 CommentWorker
-> 启动 SocialWorker
-> 启动 PopularityWorker
-> 监听退出信号
```

每个 Worker：

```text
Run
-> ch.Consume(queue)
-> for delivery
-> json.Unmarshal event
-> 根据 action 分发业务
-> 成功 Ack
-> 失败 Nack requeue
-> 超过重试次数进入 DLX
```

## 8. 必须能说清楚的主链路

```text
登录链路
注册链路
JWT 鉴权链路
视频发布链路
Outbox 更新时间线链路
Feed 最新流链路
关注流链路
点赞链路
评论链路
热榜链路
Worker 消费链路
```

## 9. Level 4 达标判断

如果你能做到下面这些，就基本达到 Level 4：

- 不看原项目能画出后端目录结构
- 能列出核心接口
- 能列出核心表和关键字段
- 能说清楚 Redis key 设计
- 能说清楚 MQ exchange、queue、routing key
- 能画出登录、视频发布、Feed、点赞、评论、热榜、Worker 消费链路
- 能解释为什么用 Outbox
- 能解释为什么用 Redis ZSet 做 Feed 和热榜
- 能解释 MQ 失败时如何降级
- 能从零写出 medium 版最小可运行项目
