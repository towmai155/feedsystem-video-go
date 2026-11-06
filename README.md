# feedsystem_video_go

基于 Go + Vue 3 的短视频 Feed 系统，含账号、视频、点赞、评论、关注、Feed 流，支持 Redis 缓存与 RabbitMQ 异步 Worker（API 与 Worker 可拆分部署）。

## 功能

| 模块 | 功能 |
|------|------|
| 账号 | 注册/登录/改名/改密/登出，头像上传，个人简介，Refresh Token 双 Token 鉴权 |
| 视频 | 上传/发布/删除，按作者查看，详情（三级缓存），#话题标签 |
| 点赞 | 点赞/取消/是否已赞/已赞列表，SSE 实时通知 |
| 评论 | 发布/删除/列表，@提及 通知 |
| 关注 | 关注/取关/粉丝列表/关注列表/粉丝计数，SSE 实时通知 |
| Feed | 最新/点赞榜/热度榜/关注流/话题标签流，冷热分离+游标分页，虚拟滚动 |
| 私信 | 发送/对话列表 |
| 通知 | SSE 实时推送，未读计数，已读标记 |

## Docker Compose 一键启动

```bash
docker compose up -d --build
```

访问：
- 前端：`http://localhost:5173`
- 后端 API：`http://localhost:8080`
- RabbitMQ 管理台：`http://localhost:15672`（`admin` / `password123`）

默认 `.env` 自动生成 JWT 密钥。生产环境请修改 `JWT_SECRET`。

## 测试数据

启动后内置 100 个测试用户（`user001` ~ `user100`，密码均为 `123456`），`user001` 已发布视频并拥有粉丝/点赞数据。

## 本地开发

```bash
# 启动依赖
docker compose up -d mysql redis rabbitmq

# 后端
cd backend
CONFIG_PATH=configs/config.compose-local.yaml go run ./cmd

# Worker
CONFIG_PATH=configs/config.compose-local.yaml go run ./cmd/worker

# 前端
cd frontend
npm install && npm run dev
```

## 登录态与升级

登录态以 MySQL 为唯一依据。每次登录生成新的 `session_id`；Access Token 包含 `sid` 和唯一 `jti`，有效期 15 分钟。业务接口和通知接口校验 JWT 后，再按用户主键检查数据库中的会话与当前 Token，不读取或回填 Redis 登录态。

刷新时通过索引查询 Refresh Token，并在事务中锁住对应账户，再次校验会话与数据库中的 7 天过期时间。当前 Access Token 剩余有效期超过 1 分钟时直接复用，临近到期或已过期时才签发新 Token；多个页面或 API 实例并发刷新会复用同一个结果。刷新始终读取锁定后的最新用户名，改名与刷新交错不会让有效会话被误判为失效。条件更新同时匹配用户、会话和 Refresh Token，避免登出或重新登录后被旧请求覆盖。改密与撤销会话在同一个事务内完成，旧登出请求只影响其所属会话。

数据库暂时不可用时，鉴权和刷新返回 `503`，前端保留凭证供重试；无效、过期或撤销的凭证返回 `401`。前端收到业务接口的 `401` 后最多恢复并重试两次，只有刷新凭证被拒绝（或没有刷新凭证）时才清空匹配的登录状态，避免改名造成的 Token 替换误触发退出。SSE 使用同一鉴权规则，本进程登出或重新登录会主动关闭旧会话连接；其他进程的撤销由每秒复查发现，每次发送通知前也会复查，并在 Access Token 到期时关闭连接。

启动时 AutoMigrate 会增加 `session_id`、`refresh_expires_at` 和 Refresh Token 查询索引。升级前的凭证缺少会话信息，用户需要重新登录一次。多 API 实例必须配置相同的 `JWT_SECRET`。

## 验证

前端测试需要 Node.js 22.12 或更高的受支持版本；Docker 构建使用 Node.js 24。

```bash
cd backend
go test ./...

# MySQL 集成测试使用独立随机测试库，账号需有创建和删除测试库的权限。
# 未提供连接串时，集成测试会跳过，普通单元测试仍会运行。
FEEDSYSTEM_TEST_MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/?parseTime=true' go test ./... -count=1

cd ../frontend
npm ci
npm test
npm run build
```

竞态检测使用 `go test -race ./...`。Windows + MinGW 若遇到运行库加载问题，可使用 `go test -race -ldflags='-linkmode=external -extldflags=-static' ./...`；MySQL 集成测试仍需设置上述连接串。

## 接口清单

### 账号 `/account`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/register` | 否 | 注册（限流 5次/时/IP） |
| POST | `/login` | 否 | 登录，返回 access_token + refresh_token |
| POST | `/refresh` | 否 | 刷新 access_token（用 refresh_token） |
| POST | `/changePassword` | 否 | 改密码（需旧密码） |
| POST | `/findByID` | 否 | 按 ID 查用户 |
| POST | `/findByUsername` | 否 | 按用户名查 |
| POST | `/getProfile` | 否 | 用户主页（视频数/获赞/粉丝数） |
| POST | `/logout` | JWT | 登出（同时失效双 token） |
| POST | `/rename` | JWT | 改名 |
| POST | `/uploadAvatar` | JWT | 上传头像（jpg/png/webp，≤10MB） |
| POST | `/updateProfile` | JWT | 更新简介/头像 |

### 视频 `/video`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/publish` | JWT | 发布视频（自动提取 #话题） |
| POST | `/uploadVideo` | JWT | 上传视频文件（mp4，≤200MB） |
| POST | `/uploadCover` | JWT | 上传封面（jpg/png/webp，≤10MB） |
| POST | `/listByAuthorID` | 否 | 按作者查视频 |
| POST | `/getDetail` | 否 | 视频详情（三级缓存） |

### 点赞 `/like`
| POST | `/like` | JWT | 点赞 |
| POST | `/unlike` | JWT | 取消点赞 |
| POST | `/isLiked` | JWT | 是否已赞 |
| POST | `/listMyLikedVideos` | JWT | 我赞过的视频 |

### 评论 `/comment`
| POST | `/listAll` | 否 | 评论列表（分页200，按时间升序） |
| POST | `/publish` | JWT | 发布评论（支持 @username 提及） |
| POST | `/delete` | JWT | 删除评论 |

### 关注 `/social`
| POST | `/follow` | JWT | 关注 |
| POST | `/unfollow` | JWT | 取关 |
| POST | `/getAllFollowers` | JWT | 粉丝列表（含粉丝数） |
| POST | `/getAllVloggers` | JWT | 关注列表（含关注数） |
| POST | `/getCounts` | JWT | 粉丝/关注计数 |

### Feed `/feed`
| POST | `/listLatest` | 软鉴权 | 最新视频（游标分页） |
| POST | `/listLikesCount` | 软鉴权 | 点赞排行（复合游标） |
| POST | `/listByPopularity` | 软鉴权 | 热度榜（快照分页） |
| POST | `/listByFollowing` | JWT | 关注流 |
| POST | `/listByTag` | 软鉴权 | 按 #话题 浏览 |

### 通知 `/notification`
| GET | `/stream?token=` | 是 | SSE 实时推送 |
| POST | `/list` | 是 | 通知列表 |
| POST | `/markRead` | 是 | 标记已读（传 id 单条，不传全标） |
| POST | `/unreadCount` | 是 | 未读计数 |

### 私信 `/message`
| POST | `/send` | JWT | 发送私信 |
| POST | `/list` | JWT | 对话列表 |

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `JWT_SECRET` | `feedsystem-dev-secret-key` | JWT 签名密钥，生产须改 |
| `MYSQL_ROOT_PASSWORD` | `123456` | MySQL root 密码 |
| `REDIS_PASSWORD` | `123456` | Redis 密码 |
| `RABBITMQ_USER` / `RABBITMQ_PASS` | `admin` / `password123` | RabbitMQ 账号 |
| `MYSQL_MAX_OPEN_CONNS` | `64` | 主连接池上限（每进程） |
| `MYSQL_SESSION_MAX_OPEN_CONNS` | `16` | 会话校验专用小池上限，与主池隔离，写突发时鉴权不断连接 |
| `WRITE_GATE_PER_VIDEO` | `16` | 单个视频同时进行中的互动写（每进程） |
| `WRITE_GATE_GLOBAL` | `32` | 全部互动写的同时进行上限（每进程），须小于等于连接池预算 |

详见 `.env.example`。
