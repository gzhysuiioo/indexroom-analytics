# 链上索引与交易分析服务

## 用途

区块与交易摄取、事件解码与规范化、可组合查询与聚合、索引重建与一致性校验、分析指标与快照。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `indexroom/`，命令入口位于 `cmd/indexroom/`。

```bash
go run ./cmd/indexroom demo
go run ./cmd/indexroom version
go run ./cmd/indexroom help
go test ./...
```

`register` reads service registrations as JSON from standard input and maintains
an in-memory service instance registry (each invocation starts empty):

```bash
echo '{"requests":[{"service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"host:8080"}]}]}' \
  | go run ./cmd/indexroom register
```

Each request fully replaces that service's instance list. New services require
`expectedRevision` 0; existing services require the current revision. Output is
JSON with per-item results and the final sorted service list; exit status is 0
only when every registration succeeds. If the JSON result cannot be written to
standard output (whether or not the requests themselves succeeded, and
including the top-level error object for malformed input), the command exits 1
and reports the write failure on standard error instead; the partially written
standard output is left as-is with no second JSON document appended.

The same `requests` array also accepts health observations and target
selection, processed strictly in input order (a selection sees only state
committed by earlier items and performs no network I/O):

```bash
echo '{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1}
]}' | go run ./cmd/indexroom register
```

A `health` request records an offline healthy/unhealthy observation and never
bumps the registration revision. A `select` request chooses one healthy target
instance; on success its result adds `instanceId`, `address` and the instance's
current health `sequence` alongside `service`, `ok` and `revision`.

Only `healthy` instances are eligible (`unknown` and `unhealthy` are never
chosen). Each service rotates independently through its healthy instances by
ascending instance id: the first success takes the smallest id, later
selections continue just after the previously chosen id and wrap to the
smallest past the end; a single healthy instance may be chosen repeatedly.
Registration replacements and health changes alter the eligible set
immediately without resetting the rotation — even a removed last-chosen
instance simply leaves its position, and recovered or newly joined instances
participate in id order. Failed selections (`invalid`, `conflict`,
`not_found`, `no_healthy`) and duplicate registrations or health reports never
advance the cursor; a selection never changes a revision or a health record.

Field validation precedes the revision check: a blank `service` or a
missing/negative `expectedRevision` is `invalid`. Then a revision mismatch is
`conflict` (an unknown service is at revision 0), an unknown service with a
matching revision is `not_found`, and a service with no healthy instance is
`no_healthy`. Every failure states the reason and current revision and leaves
the rotation position intact.

A `select` request may carry an optional `sessionKey` string to pin the
request to a per-service session. The key's first successful selection rotates
normally and remembers the chosen instance; later requests with the same key
reuse that instance — its current address and latest accepted health
`sequence` — while it stays registered and healthy, without moving the
rotation. If the bound instance was removed or is currently `unknown` or
`unhealthy`, the request falls back to the normal rotation and rebinds only on
success. Keys are trimmed before use (keys equal after trimming share one
session), bindings are independent per service, and an explicit `null`, a
non-string or a blank-after-trim key is `invalid` with a reason naming the
`sessionKey` problem. Failed selections never create or rewrite a binding.

## 离线健康上报（`health` 请求）中文说明

`health` 请求在 `register` 命令的同一批 `requests` 中记录一条离线健康观察，不做任何网络探测。规则如下：

- `expectedRevision` 必须等于该服务当前的注册修订号；健康上报**不增加**修订号，上报前后修订号不变。
- `sequence` 是**单个实例**的正整数序号，每个实例独立计数，不是整批请求共用的计数。
- 序号大于当前记录时更新健康状态；小于当前记录时返回 `stale`；等于当前记录时，只有健康状态和整理后的原因都相同才成功且不产生变更，否则返回 `conflict`。
- 实例刚注册时健康状态为 `unknown`、序号为 0、无原因。
- 不健康观察（`"healthy":false`）必须提供 `reason`，且去除两端空白后仍非空；健康观察（`"healthy":true`）会清空原因。原因先去除两端空白再存储，整理后的结果参与相同序号的重复判断。
- 成功结果中 `changed` 未出现表示没有变更（例如重复上报），不能把 `"ok":true` 等同于记录已更新。
- 失败的请求不会覆盖先前记录；批次中后续请求仍按输入顺序继续执行。
- 每次调用都从空注册表开始，示例所需的注册和观察必须放在同一批请求中。
- 注册替换与观察的关系：替换实例列表时，实例标识和地址都没变的实例保留健康记录；地址改变或删除后重新加入的实例回到 `unknown`、序号 0、无原因，新地址只有在当前修订号下上报健康结果后才能被选择。旧修订号的观察即使序号更大也返回 `conflict`，不能把旧地址的健康结果带到新修订号。
- 错误分类：字段无效（如 `service` 为空、`sequence` 非正整数、不健康但原因为空白）返回 `invalid`；修订号不符返回 `conflict`；修订号匹配但服务或实例不存在返回 `not_found`。字段检查先于修订号判断。

### 完整示例

下面用同一个实例 `i1` 依次展示：不健康观察被接受、带两端空白的相同原因重复上报、旧序号被拒绝、更大序号恢复健康，最后基于已接受的恢复结果选择目标：

```bash
echo '{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":10,"healthy":false,"reason":"心跳超时"},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":10,"healthy":false,"reason":"  心跳超时  "},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":5,"healthy":false,"reason":"误报重发"},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":11,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1}
]}' | go run ./cmd/indexroom register
```

输出（逐项说明见后）：

```json
{
  "results": [
    {"service":"svc","ok":true,"changed":true,"revision":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":10},
    {"service":"svc","ok":true,"revision":1,"sequence":10},
    {"service":"svc","ok":false,"revision":1,"error":"stale","reason":"sequence 5 is older than the current sequence 10","sequence":10},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":11},
    {"service":"svc","ok":true,"revision":1,"sequence":11,"instanceId":"i1","address":"h1:8080"}
  ],
  "services": [
    {"service":"svc","revision":1,"instances":[
      {"id":"i1","address":"h1:8080","health":"healthy","sequence":11}
    ]}
  ]
}
```

逐项说明：

1. 注册成功，服务修订号变为 1；实例 `i1` 初始为 `unknown`、序号 0。
2. 序号 10 的不健康观察被接受（`changed:true`），原因记录为 `心跳超时`；修订号仍为 1，健康上报不增加修订号。
3. 相同序号 10、相同健康状态，原因去除两端空白后也是 `心跳超时`，属于重复上报：成功但**没有** `changed` 字段，表示记录未变。
4. 序号 5 小于当前序号 10，返回 `stale`，`sequence` 报告当前序号 10；先前记录未被覆盖。
5. 更大序号 11 恢复健康被接受，原因被清空；尽管第 4 项失败，本项仍按输入顺序正常生效。
6. 目标选择使用已接受的恢复结果，选中 `i1`，返回其地址和最新健康序号 11。

末尾的 `services` 列表显示每个实例的最终健康状态和最新序号（`i1` 为 `healthy`、序号 11、无原因）。本批次含有失败项（第 4 项），进程退出状态为 1，但后续成功项仍然生效；只有全部请求成功时退出状态才为 0。

## 会话保持（`select` 请求的 `sessionKey`）中文说明

`select` 请求可以携带可选的字符串 `sessionKey`，把请求绑定到该服务下的一个会话：

- 会话键第一次成功选择时按当时的轮询位置选出健康实例并记住绑定，轮询位置照常推进；之后复用绑定时直接返回该实例当前的地址和最新已接受的健康序号，**不推进**轮询位置。因此带会话键和不带会话键的请求交错时，普通选择仍从最近一次实际轮询选中的实例之后继续。
- 绑定按服务独立：不同服务使用相同会话键互不影响；未提供 `sessionKey` 的请求继续按原有规则轮询。
- 判断绑定是否可用以本次选择处理时的状态为准：绑定的实例已被删除，或当前健康状态为 `unknown`/`unhealthy` 时，本次按轮询规则重新选择，仅在成功时替换绑定；实例曾经不健康但在本次请求前已恢复的，可以继续复用。同一实例标识更换地址后健康记录会重置，必须先按既有规则重新上报健康，才能返回当前地址。
- 会话键先去除两端空白，整理后相同的键视为同一会话；显式提供 `null`、非字符串或纯空白字符串时返回 `invalid`，原因中指明 `sessionKey` 问题。字段检查仍先于修订号判断。
- 失败项（`invalid`、`conflict`、`not_found`、`no_healthy`）不创建或改写绑定，也不移动轮询位置；批次中后续请求照常处理。会话选择不改变注册修订号或健康记录。

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
