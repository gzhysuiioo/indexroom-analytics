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
only when every registration succeeds.

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

### 离线健康上报（中文使用说明）

`health` 请求沿用上面的 `register` 命令：与 `register`、`select` 混放在同一个
`requests` 数组中，按输入顺序逐项处理，不进行任何网络探测。每次调用命令都从
**空注册表**开始，因此下面示例所需的注册和观察全部放在同一批请求里。

#### 字段与序号规则

- `expectedRevision` 必须等于该服务**当前的注册修订号**（未知服务为 0）。健康上报
  只记录观察，**不会增加修订号**；修订号只有真正改变实例列表的 `register` 才会
  增加。
- `sequence` 是**单个实例各自维护的正整数序号**（≥ 1），不是整批请求共用的计数，
  也不与修订号互相比较；不同实例各有自己的序号。实例刚注册时为 `unknown`、序号 0。
- `healthy` 为布尔值，必填：
  - `healthy:false`（不健康）必须提供 `reason`，且去除两端空白后仍非空，否则
    `invalid`；保存时原因会先做两端去空白整理。
  - `healthy:true`（健康）不需要原因；即使请求里带了 `reason` 也会被清空。
- 序号判定（修订号匹配、服务和实例都存在时）：
  - **更大**序号：写入新的健康状态和原因，结果 `"changed": true`；
  - **更小**序号：返回 `stale`，结果中的 `sequence` 是当前已接受的序号；
  - **相同**序号：只有健康状态相同、且整理后的原因也完全相同，才算重复上报——
    请求成功但记录不变（结果中**不出现** `changed` 字段）；状态或原因不同则返回
    `conflict`。健康观察会清空原因，因此“带原因的不健康”与“同序号的健康”即使
    后一个请求没写原因，也不算重复。
- 成功不等于记录已更新：结果里 `ok:true` 只表示请求被接受；**只有出现
  `"changed": true` 才表示记录发生了变化**，`changed` 未出现即幂等重复、状态不变。
- 任何失败（`invalid` / `conflict` / `stale` / `not_found` / `no_healthy`）都
  **不会覆盖先前的记录**；批处理不会中断，后续请求仍按输入顺序继续执行。批次中
  只要有一项失败，进程退出状态就是 **1**，但已经成功的项目（包括失败项之后的
  成功项）仍然生效，最终 `services` 列表反映全部成功提交后的状态。

#### 完整 JSON 示例：同一实例从故障到恢复

```bash
go run ./cmd/indexroom register <<'EOF'
{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":false,"reason":"disk full"},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":false,"reason":"  disk full  "},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":false,"reason":"disk full"},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":3,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1}
]}
EOF
echo "exit=$?"
```

输出（错误原文字符串可能随版本调整，判定类型和字段不变）：

```json
{
  "results": [
    {
      "service": "svc",
      "ok": true,
      "changed": true,
      "revision": 1
    },
    {
      "service": "svc",
      "ok": true,
      "changed": true,
      "revision": 1,
      "sequence": 2
    },
    {
      "service": "svc",
      "ok": true,
      "revision": 1,
      "sequence": 2
    },
    {
      "service": "svc",
      "ok": false,
      "revision": 1,
      "error": "stale",
      "reason": "sequence 1 is older than the current sequence 2",
      "sequence": 2
    },
    {
      "service": "svc",
      "ok": true,
      "changed": true,
      "revision": 1,
      "sequence": 3
    },
    {
      "service": "svc",
      "ok": true,
      "revision": 1,
      "sequence": 3,
      "instanceId": "i1",
      "address": "h1:8080"
    }
  ],
  "services": [
    {
      "service": "svc",
      "revision": 1,
      "instances": [
        {
          "id": "i1",
          "address": "h1:8080",
          "health": "healthy",
          "sequence": 3
        }
      ]
    }
  ]
}
```

逐项说明（这也是判断观察是否生效的依据）：

1. **注册**：新服务以 `expectedRevision:0` 创建，修订号变为 1；`i1` 初始为
   `unknown`、序号 0。
2. **不健康观察被接受**：序号 2 大于当前序号 0，写入 `unhealthy` 和原因
   `disk full`，`changed:true`；修订号仍为 1，说明健康上报不增加修订号。
3. **带两端空白的相同原因重复上报**：`"  disk full  "` 整理后仍是 `disk full`，
   相同序号、相同状态、相同整理后原因，所以 `ok:true` 但**没有 `changed` 字段**
   ——请求成功而记录未变。
4. **旧序号被拒绝**：序号 1 小于已接受的序号 2，返回 `stale`，结果里的
   `sequence:2` 标明当前序号；先前的不健康记录没有被覆盖，批处理继续向下执行。
5. **更大序号恢复健康**：序号 3 写入 `healthy` 并清空原因，`changed:true`。
6. **目标选择使用已接受的恢复结果**：因为第 5 项已让 `i1` 恢复健康，第 4 项失败
   也没有影响后续执行，`select` 成功选中 `i1`，并带回地址 `h1:8080` 和该实例
   当前的健康序号 3。

最终 `services` 列表中 `i1` 为 `"health": "healthy"`、`"sequence": 3`，原因已清空
（因此 `reason` 字段省略）。由于批次中含有一个 `stale` 失败项，命令退出状态为
**1**，但其后的恢复上报和选择都已生效——不能因为退出状态非 0 就认为整批回滚。

#### 注册替换与健康记录的关系

`register` 是整表替换，但健康记录按下述规则保留或重置：

- 实例标识和地址**都没有变化**时，保留其健康状态、序号和原因（只是旁边新增、
  删除别的实例不影响它）。
- 实例**地址改变**，或先被删除、之后**重新加入**时，健康记录重置为
  `unknown`、序号 0、无原因。旧地址上的健康结果不会带到新地址。
- 新地址（或重置后的实例）**只有在当前修订号下成功上报健康结果后**，才可能被
  `select` 选中；在此之前它是 `unknown`，选择时按 `no_healthy` 处理。
- 针对**旧修订号**的健康观察一律返回 `conflict`，**即使它携带的序号更大**也一样
  拒绝；不能借大号序号把旧修订号（或旧地址）的健康结果带进当前状态。

```bash
go run ./cmd/indexroom register <<'EOF'
{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":5,"healthy":true},
  {"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
  {"type":"select","service":"svc","expectedRevision":2},
  {"type":"register","service":"svc","expectedRevision":2,"instances":[{"id":"i1","address":"h9:9090"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":99,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":3}
]}
EOF
```

要点：第 2 项后 `i1` 为 `healthy`、序号 5；第 3 项注册只新增 `i2`，`i1` 标识和
地址都没变，健康记录保留（修订号升到 2），所以第 4 项在修订号 2 下选中 `i1` 并
带回序号 5，而尚无观察的 `i2` 不会被选中；第 5 项把 `i1` 换到新地址 `h9:9090`，
修订号升到 3，`i1` 重置为 `unknown`、序号 0；第 6 项仍用旧修订号 1（序号 99 再大
也没用）返回 `conflict`；第 7 项在修订号 3 下找不到健康实例，返回 `no_healthy`，
最终列表里 `i1` 是新地址、`unknown`、序号 0。

#### health 请求的失败类型

判定顺序固定为**先检查字段，再检查修订号，最后检查存在性和序号**：

- `invalid`：字段本身无效——`service` 或 `instanceId` 去空白后为空、
  `expectedRevision` 缺失或为负数、`sequence` 缺失或不是正整数、`healthy` 缺失、
  不健康时原因去空白后为空。字段错误优先于一切，即使修订号也填错了仍报
  `invalid`。
- `conflict`：字段有效但 `expectedRevision` 与当前修订号不符（结果同时给出
  `expectedRevision` 和 `actualRevision`）；相同序号携带不同状态或原因时也是
  `conflict`。
- `not_found`：修订号匹配（未知服务的当前修订号视为 0，所以对未知服务报
  `expectedRevision:0` 会走到这一步），但服务不存在，或服务存在而该实例不存在。
- `stale`：服务、实例和修订号都匹配，但序号小于已接受的当前序号。

以上均为既有处理行为，本次只补充说明，未改变任何逻辑。

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
