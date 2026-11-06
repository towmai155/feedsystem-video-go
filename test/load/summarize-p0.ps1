param([string]$ResultDirectory)
$ErrorActionPreference = 'Stop'
if (-not $ResultDirectory) {
    $ResultDirectory = (Get-ChildItem (Join-Path $PSScriptRoot 'results') -Directory |
        Sort-Object Name -Descending | Select-Object -First 1).FullName
}
$data = Get-Content -LiteralPath (Join-Path $ResultDirectory 'results.json') -Raw | ConvertFrom-Json
if (-not $data.finished) { throw 'Wait for the run to finish before summarizing.' }
$hostData = Get-Content -LiteralPath (Join-Path $ResultDirectory 'host.json') -Raw | ConvertFrom-Json
$dockerData = Get-Content -LiteralPath (Join-Path $ResultDirectory 'docker-info.json') -Raw | ConvertFrom-Json
$native = @()
if (Test-Path (Join-Path $ResultDirectory 'native-resources.csv')) {
    $native = @(Import-Csv -LiteralPath (Join-Path $ResultDirectory 'native-resources.csv'))
}
$lines = [Collections.Generic.List[string]]::new()
$lines.Add('# P0 压测报告')
$lines.Add('')
$lines.Add(('执行时间：{0} 至 {1}。' -f $data.started,$data.finished))
$lines.Add('')
$lines.Add('## 环境与口径')
$lines.Add('')
$lines.Add(('- Windows 本机 {0} 逻辑处理器，内存 {1:N1} GiB；Docker 分配 {2} CPU、{3:N1} GiB 内存。' -f $hostData.NumberOfLogicalProcessors,($hostData.TotalPhysicalMemory/1GB),$dockerData.NCPU,($dockerData.MemTotal/1GB)))
if ($data.environment.external_processes) {
    $lines.Add('- Windows 压测器；API/Worker 在外部启动的隔离测试容器中运行，配置见 compose.resolved.yml。MySQL 8、Redis 7、RabbitMQ 3.12 使用独立容器和端口。')
} else {
    $lines.Add('- 真实编译的单实例 API、独立 Worker；压测器与服务同机。MySQL 8、Redis 7、RabbitMQ 3.12 使用独立容器和端口。')
}
$lines.Add('- 测试 MySQL max_connections=400；容器配置见 compose.p0.yml，业务实现和连接池配置以本次工作区快照及采样为准。')
$lines.Add('- 6,000 个压测账号、另一个作者账号、50,000 条视频。测试会话提前生成，鉴权和限流照常执行；不计入登录/密码校验性能。')
$lines.Add(('- {0} 并发闭环客户端；每组读取持续 {1} 秒。每组互动发送 {2:N0} 次独立账号请求，是有限批次突发测试，不代表长期持续写入能力。' -f ($data.environment.levels -join '/'),$data.environment.read_seconds,$data.environment.write_requests_per_stage))
$lines.Add('- 每轮按同一顺序执行，缓存会逐步预热；热点详情失效场景每秒删除一次测试热点缓存。数据时间戳间隔一秒，未覆盖相同时间戳的视频分页。')
$lines.Add('- 有效 RPS 仅计 HTTP 200 且响应校验通过的请求；延迟统计包含失败请求，在途请求完成时间计入窗口。未设置业务 SLA，不能据此声称生产容量或最大稳定并发。')
$lines.Add('- 落库延迟由压测期间及结束后每 250ms 轮询观察，包含观察间隔和查询时间；最终记录、计数、Redis 热度、通知另行核对。')
if ($data.checks.reliability) { $lines.Add('- 额外故障阶段：专用 RabbitMQ 停机期间分别执行 20 次点赞、20 次评论；它们不使用普通写入阶段的请求数设置。') }
$lines.Add('')
$total = ($data.results | Measure-Object requests -Sum).Sum
$valid = ($data.results | Measure-Object ok -Sum).Sum
$lines.Add(('共发出 {0:N0} 次被统计请求，其中有效响应 {1:N0} 次；预热及分页检查不包含在此计数中。重复点赞正确性场景单独解释。' -f $total,$valid))
$lines.Add('')
$lines.Add('## 读取结果')
$lines.Add('')
$lines.Add('| 场景 | 并发 | 请求数 | 有效 RPS | P95 ms | P99 ms | 失败数 | 失败率 |')
$lines.Add('|---|---:|---:|---:|---:|---:|---:|---:|')
$names = @{
    feed_anon_hot='匿名 Feed 首页'; feed_auth_hot='登录 Feed 首页'; feed_anon_cold='匿名 Feed 冷数据';
    detail_hot='热点视频详情'; detail_distributed='分散视频详情'; detail_expiring='热点详情每秒失效'
}
foreach ($r in $data.results | Where-Object { -not $_.verification }) {
    $lines.Add(('| {0} | {1} | {2} | {3:N1} | {4:N1} | {5:N1} | {6} | {7:P2} |' -f $names[$r.name],$r.concurrency,$r.requests,$r.good_rps,$r.p95_ms,$r.p99_ms,($r.requests-$r.ok),(1-$r.ok/$r.requests)))
}
$lines.Add('')
$lines.Add('## 写入与最终一致性')
$lines.Add('')
$lines.Add('| 场景 | 并发 | HTTP 成功/请求 | HTTP 有效 RPS | HTTP P99 ms | 落库观察 P99 ms | 实际记录 | DB 热度 | Redis 热度 | 通知 | 核对 |')
$lines.Add('|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|')
foreach ($r in $data.results | Where-Object verification) {
    $v=$r.verification
    $label=if($r.name -like '*comment*'){'评论'}elseif($r.name -like 'duplicate*'){'同账号重复点赞'}else{'点赞'}
    $check=if($v.passed){'通过'}else{'失败'}
    $lines.Add(('| {0} | {1} | {2}/{3} | {4:N1} | {5:N1} | {6:N1} | {7} | {8} | {9} | {10} | {11} |' -f $label,$r.concurrency,$r.ok,$r.requests,$r.good_rps,$r.p99_ms,$r.e2e_p99_ms,$v.actual_rows,$v.video_popularity,$v.redis_popularity_sum,$v.notifications,$check))
}
$lines.Add('')
$lines.Add('## 观察到的问题')
$lines.Add('')
foreach ($r in $data.results | Where-Object { $_.requests -ne $_.ok }) {
    $lines.Add(('- {0}，并发 {1}：{2}/{3} 次失败；样例：`{4}`。' -f $r.name,$r.concurrency,($r.requests-$r.ok),$r.requests,($r.error_samples -join '；')))
}
foreach ($r in $data.results | Where-Object { $_.verification -and -not $_.verification.passed }) {
    $v=$r.verification
    $lines.Add(('- {0}：期望唯一记录 {1}，实际记录 {2}，点赞计数 {3}，数据库热度 {4}，Redis 热度 {5}，通知 {6}，缺失已接受请求 {7}。' -f $r.name,$v.expected_unique_rows,$v.actual_rows,$v.video_likes_count,$v.video_popularity,$v.redis_popularity_sum,$v.notifications,$v.missing_accepted_rows))
}
$lines.Add('- 重复点赞是并发正确性用例，需同时核对唯一记录、数据库计数、Redis 热度及通知；HTTP 200 本身不证明幂等性。')
$lines.Add('- 运行完成不等于全部通过；以本次 HTTP 错误、verification.passed 和分页检查为准。历史基线结论不自动适用于本次运行。')
if(Test-Path (Join-Path $ResultDirectory 'post-run-reconciliation.txt')) {
    $lines.Add('- 运行结束后重新启动原测试 MySQL/RabbitMQ 只读复核：视频 50006 仍有 245 条评论、232 条评论通知，13 个评论作者缺少对应通知；RabbitMQ 通过 rabbitmqctl 即时查询确认所有队列 ready/unacked 均为 0，排除了管理界面统计滞后和未消费完。证据见 post-run-reconciliation.txt、post-run-queues.txt。')
    $lines.Add('- 日志显示 notificationworker.go:114 查询视频作者时发生连接拒绝；当前代码忽略 Scan 的错误，authorID 为 0 时返回 nil，随后消息被 ACK。这解释了已成功评论的通知缺失，应修复错误传播与重试。重复点赞则在数据库消费者去重前已分别发布热度和通知事件，产生额外副作用。')
}
$lines.Add('')
$lines.Add('## 资源与链路观测')
$lines.Add('')
$lines.Add('| 场景 | 并发 | MySQL Questions 增量 | 采样最大连接数 | 采样最大 MQ 积压 |')
$lines.Add('|---|---:|---:|---:|---:|')
foreach ($r in $data.results) {
    $first=$r.samples[0];$last=$r.samples[-1]
    $questions=[long]$last.mysql.Questions-[long]$first.mysql.Questions
    $maxConnections=($r.samples | ForEach-Object {[long]$_.mysql.Threads_connected} | Measure-Object -Maximum).Maximum
    $maxQueue=($r.samples | ForEach-Object {($_.queues | ForEach-Object {$_.messages_ready+$_.messages_unacknowledged} | Measure-Object -Sum).Sum} | Measure-Object -Maximum).Maximum
    if($r.verification){$maxQueue=[math]::Max([long]$maxQueue,[long]$r.verification.max_queue_backlog_during_settle)}
    $lines.Add(('| {0} | {1} | {2} | {3} | {4} |' -f $r.name,$r.concurrency,$questions,$maxConnections,$maxQueue))
}
$lines.Add('')
foreach($name in @('api','worker','p0bench')) {
    $rows=@($native | Where-Object Name -eq $name)
    if($rows.Count -gt 0){
        $peak=($rows | ForEach-Object {[long]$_.WorkingSet64} | Measure-Object -Maximum).Maximum
        $cpu=($rows | ForEach-Object {[double]$_.CPU} | Measure-Object -Maximum).Maximum-($rows | ForEach-Object {[double]$_.CPU} | Measure-Object -Minimum).Minimum
        $lines.Add(('- {0}：采样 RSS 峰值 {1:N1} MiB，采样区间累计 CPU 时间增量 {2:N1} CPU 秒（可同时使用多个核心）。' -f $name,($peak/1MB),$cpu))
    }
}
$lines.Add('- Questions 包含监控查询；队列统计可能滞后且采样会遗漏瞬时峰值。Docker resources 文件是每组结束后的快照。')
foreach ($r in $data.results) {
    $pools = @($r.samples | ForEach-Object { $_.api_pool } | Where-Object { $null -ne $_ })
    if ($pools.Count -gt 0) {
        $peak = ($pools | Measure-Object OpenConnections -Maximum).Maximum
        $waitDelta = [long]$pools[-1].WaitCount - [long]$pools[0].WaitCount
        $lines.Add(('- {0}，并发 {1}：API 连接上限 {2}，采样连接峰值 {3}，采样区间连接池等待次数增量 {4}。' -f $r.name,$r.concurrency,$pools[-1].MaxOpenConnections,$peak,$waitDelta))
    }
}
$lines.Add('')
$lines.Add('## 分页检查')
$lines.Add('')
$lines.Add('```json')
$lines.Add(($data.checks | Select-Object pagination_before,pagination_after | ConvertTo-Json -Depth 5))
$lines.Add('```')
if ($data.checks.reliability) {
    $lines.Add('')
    $lines.Add('## 故障恢复与重复投递')
    $lines.Add('')
    $lines.Add('```json')
    $lines.Add(($data.checks.reliability | ConvertTo-Json -Depth 5))
    $lines.Add('```')
    $lines.Add(('最终待发送本地事件：{0}。' -f $data.checks.final_snapshot.outbox_pending))
}
$lines.Add('')
$lines.Add('## 复现与原始证据')
$lines.Add('')
$project = [string]$data.environment.compose_project
if (-not $project) { $project = 'feedsystem-p0' }
if ($data.environment.external_processes) {
    $reproduce = "./test/load/run-p0-linux.ps1 -ProjectName '$project-new' -Seconds $($data.environment.read_seconds) -Levels '$($data.environment.levels -join ',')'"
} else {
    $cases = [string]($data.environment.cases -join ',')
    if (-not $cases) { $cases = 'feed_anon_hot,feed_auth_hot,feed_anon_cold,detail_hot,detail_distributed,detail_expiring' }
    $reproduce = "./test/load/run-p0.ps1 -ProjectName '$project-new' -Seconds $($data.environment.read_seconds) -Levels '$($data.environment.levels -join ',')' -Cases '$cases' -WriteRequests $($data.environment.write_requests_per_stage)"
}
if ($data.environment.read_only) { $reproduce += ' -ReadOnly' }
if ($data.environment.fault_checks) { $reproduce += ' -FaultChecks' }
$lines.Add(('- 复现：`{0}`。使用新的专用项目名保留已有夹具，运行前确保共享测试端口空闲。' -f $reproduce))
$lines.Add(('- 本次结果目录：`{0}`。详细数据见 `results.json`，应用日志见 `api.log` / `worker.log`，进程资源见 `native-resources.csv`。' -f $ResultDirectory))
$lines.Add('- 基准提交见 revision.txt，运行时已跟踪文件的改动见 working-tree.patch，二进制哈希见 sha256.json；不要把同时存在的多项修复效果归因于单个参数。')
$lines | Set-Content -Encoding utf8 -LiteralPath (Join-Path $ResultDirectory 'SUMMARY.zh-CN.md')
Write-Output (Join-Path $ResultDirectory 'SUMMARY.zh-CN.md')
