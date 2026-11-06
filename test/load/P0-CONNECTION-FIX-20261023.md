# 数据库连接稳定性修复与验证状态

> 更新：磁盘及 Docker 已恢复，已完成完整复测，并针对热点写入增加每视频最多 4 个并行事务的排队机制。两轮 300 并发共 8,000 次写入均成功且最终核对通过；冷启动突发连接拒绝仍未解决。当前结果见 [复测报告](P0-RETEST-20261023.md)，下文阻塞记录及恢复步骤保留为历史背景。

## 当前实现

基于工作区已有的连接池、HTTP/Redis 超时修改继续完善：

- MySQL 每进程默认最多 64 条连接，保留最多 64 条空闲连接；连接生命周期 30 分钟，最大空闲 5 分钟。API 和 Worker 各自持有连接池，所有实例的连接预算之和须低于数据库上限并保留余量。
- `MYSQL_MAX_OPEN_CONNS` / `MYSQL_MAX_IDLE_CONNS` 可覆盖默认值。达到连接预算后请求等待复用连接，不无限扩张连接数。
- MySQL 配置连接/读写超时及 `interpolateParams=true`，减少普通参数查询的网络往返。保留会话数据库校验。
- 在现有 pprof 监听器增加 `GET /debug/dbstats`，公开 `sql.DB.Stats()` 的使用/空闲连接数、等待次数、等待时间与连接关闭原因，未加入业务端口。
- 新增真实 MySQL 集成用例：3 轮并发突发验证最多复用 4 条物理连接，并确认超出预算的查询发生等待。
- 压测器支持指定读取场景和只读模式，记录 API/Worker 连接池统计；可用新专用 Compose 项目复测而保留旧基线的数据。
- 删除了“已经确认临时端口耗尽”等未经验证的代码注释。连接池调整是修复候选，效果需要实测。

## 已验证

- API、Worker、压测器构建成功。
- `go test ./...` 通过普通测试；未设置测试连接串时 MySQL 集成测试按既有约定跳过。
- `go vet ./cmd/... ./internal/db ./internal/observability` 通过。
- `git diff --check` 通过。

## 复测阻塞：宿主机磁盘满

2026-10-23 复测准备期间，读取到 Windows C 盘剩余 **0 字节**。Docker 数据盘位于：

`C:\Users\mai\AppData\Local\Docker\wsl\disk\docker_data.vhdx`

新建的独立测试 MySQL 初始化失败，日志出现：

```text
[InnoDB] [FATAL] fsync() returned EIO, aborting.
mysqld got signal 6
```

因此低负载的集成测试也出现 `unexpected EOF` / `driver: bad connection`，不能把这批失败当成修复后的高并发结果。本次没有完成 300 并发复测，不能声称失败率已经降低。此证据说明本次复测环境的存储故障，不足以反推此前基线的全部错误都由磁盘满导致。

构建缓存和临时目录已位于 D 盘，清理它们无法释放 C 盘空间。没有删除用户源码、业务数据库或旧压测基线；未全局清理 Docker 数据。由于 Docker 控制请求也发生阻塞，仅取消了本次等待中的诊断/清理客户端；新测试容器的清理尚需在 Docker 恢复后完成。

## 恢复后执行

1. 释放 C 盘至少 2–5 GB，并确认 Docker 数据盘可正常写入；长期建议把 Docker 数据盘迁移到空间充足的位置，此操作涉及整套 Docker 数据。
2. 若 Docker 仍异常，恢复/重启 Docker 后移除本次初始化失败的专用测试实例（仅下列项目）：

```powershell
docker compose -p feedsystem-p0-fix -f test/load/compose.p0.yml down --volumes
```

3. 用保留鉴权和 3 秒客户端超时的场景复测：

```powershell
./test/load/run-p0.ps1 -ProjectName feedsystem-p0-fix -ReadOnly -Cases 'feed_auth_hot,feed_anon_cold' -Seconds 30 -Levels '25,100,300'
```

4. 对比原始基线的失败率、有效吞吐、P99 和新采集的连接池等待/复用指标。MySQL 集成回归还需在数据库健康后设置 `FEEDSYSTEM_TEST_MYSQL_DSN` 执行。

本次工作期间工作区还有其他业务修复同时发生；复测脚本保存工作区补丁和二进制 SHA256，后续结果应解释为对应快照的整体表现，不能仅归因于某一个参数。
