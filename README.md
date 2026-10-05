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
only when every registration succeeds **and** the JSON result is fully written
to standard output. If standard output rejects the result (including the
top-level error JSON for malformed input), the command writes a clear
"failed to write request results" diagnostic carrying the actual write error to
standard error, exits 1 even when every request succeeded, and never appends a
second JSON document to an already partially written standard output; this
lets a rejected request be distinguished from an undelivered result.

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
missing `expectedRevision` is `invalid`, and so is a value that is not an
integer (`1.5`, a string, `true`) or that falls outside the build's integer
range — `0..2147483647` on a 32-bit build, `0..9223372036854775807` on a
64-bit build. A negative number is therefore invalid too, and the check uses
the submitted value itself, so on a 32-bit build `4294967297` or
`-4294967295` (both of which truncate to `1` when narrowed) is still rejected
as out of range rather than matching revision 1; the reason names the
`expectedRevision` range. `2147483648` stays a valid integer on a 64-bit
build and yields a normal conflict. Then a revision mismatch is `conflict`
(an unknown service is at revision 0), an unknown service with a matching
revision is `not_found`, and a service with no healthy instance is
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
- 错误分类：字段无效（如 `service` 为空、`expectedRevision` 为负数或超出本程序整数范围（32 位 `0..2147483647`、64 位 `0..9223372036854775807`）、`sequence` 非正整数、不健康但原因为空白）返回 `invalid`；修订号不符返回 `conflict`；修订号匹配但服务或实例不存在返回 `not_found`。字段检查先于修订号判断。

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

## 服务注册与实例列表替换（`register` 请求）中文说明

`register` 请求维护一个服务的实例列表，全部在内存中离线完成、不做任何网络访问。**每次调用 `register` 命令都从空注册表开始**，因此要在一份示例里连续展示创建与多次替换，必须把它们放进同一批 `requests` 按输入顺序提交。

### 请求字段与校验规则

- `service`：服务名，去除两端空白后使用；整理后为空返回 `invalid`。
- `expectedRevision`：必填的整数，合法范围为 `0..2147483647`（32 位程序）或 `0..9223372036854775807`（64 位程序），任何负数或超范围整数都按**提交的原始数值**判定为 `invalid`，原因指出 `expectedRevision` 的范围；因此 32 位程序提交 `4294967297`（截断后恰为 1）或 `-4294967295` 不会被当成 1，而 64 位程序中的 `2147483648` 仍是合法整数。新服务必须提交 `0`，创建成功后修订号为 `1`；已有服务必须提交该服务**当前**修订号，提交其他在合法范围内的值（包括对尚不存在的服务提交非 0 值）返回 `conflict`。
- `instances`：实例对象数组，每项含 `id` 与 `address`。注册提交的是**完整实例列表**：本次列表中未包含的已有实例会被移除。
  - 实例 `id` 先去除两端空白；整理后为空、或同一服务内整理后的标识重复，都返回 `invalid`。
  - `address` 先去除两端空白，之后必须是带端口的 `host:port`：主机支持域名、IPv4 和带方括号的 IPv6（如 `h1:8080`、`10.0.0.2:8081`、`[2001:db8::1]:9000`，裸 IPv6 必须加方括号）；端口必须是 1 到 65535 的十进制整数；地址内部不能含空白或控制字符。
- **字段检查先于修订号判断**：任一字段不合法都返回 `invalid`，即使同一项的修订号也不匹配，仍报 `invalid`；只有字段全部合法、修订号却不符时才报 `conflict`。

### 修订号、变更判定与空列表

- 只有实例列表**内容改变**（整理后的标识集合不同，或任一标识对应的地址不同）才把修订号加 1；仅调整实例的提交顺序不产生变更。
- 重复提交整理后标识和地址都相同的列表会成功，但结果中**不出现 `changed`**，修订号保持原值。
- `instances: []` 是合法的空数组，可以清空实例列表；服务仍然存在（修订号匹配的查询仍能找到它），修订号照常按内容是否改变计算。`instances: null` 不能当成空数组，返回原因为 `instances must be an array` 的 `invalid`。
- 每个失败项都带有失败原因（`reason`）和处理该项时的当前修订号（`revision`），失败不会覆盖原有列表；批次中的后续请求仍按输入顺序继续处理，因此一次失败后改用正确修订号提交即可成功更新。批次中只要含失败项，进程退出状态即为 1；全部成功才为 0。

### 替换对健康记录的影响

实例替换按实例标识匹配，与上文《离线健康上报（`health` 请求）中文说明》一节直接相关：

- 实例**标识和地址都没变**：保留其健康记录（状态、序号、原因均不变）。
- 同一标识的**地址改变**，或实例先被移除再重新加入，或全新加入的实例：健康状态回到 `unknown`、序号为 0、无原因。旧地址上的健康状态不能沿用到新地址，必须在**新的注册修订号**下重新上报 `health`，该实例才可能被 `select` 选中。
- 被移除的实例连同其健康记录一起消失。

因此每次替换后对照新旧列表即可判断：标识和地址都保留的实例无需重新上报；地址变化与新加入的实例都需要按当前修订号重新上报健康。

### 完整示例

下面用同一批请求依次展示：首次创建、两次健康上报、内容未变的重复提交（含空白整理）、仅调整顺序、字段非法与修订号冲突两次失败，以及失败之后用正确修订号完成的实际替换：

```bash
echo '{"requests":[
  {"type":"register","service":"svc-a","expectedRevision":0,"instances":[
    {"id":"i1","address":"h1:8080"},
    {"id":"i2","address":"10.0.0.2:8081"},
    {"id":"i3","address":"[2001:db8::1]:9000"}
  ]},
  {"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc-a","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":false,"reason":"磁盘故障"},
  {"type":"register","service":"  svc-a  ","expectedRevision":1,"instances":[
    {"id":"  i1  ","address":"  h1:8080  "},
    {"id":" i2 ","address":" 10.0.0.2:8081 "},
    {"id":" i3 ","address":"[2001:db8::1]:9000"}
  ]},
  {"type":"register","service":"svc-a","expectedRevision":1,"instances":[
    {"id":"i3","address":"[2001:db8::1]:9000"},
    {"id":"i1","address":"h1:8080"},
    {"id":"i2","address":"10.0.0.2:8081"}
  ]},
  {"type":"register","service":"svc-a","expectedRevision":5,"instances":[
    {"id":"i1","address":"h1:70000"}
  ]},
  {"type":"register","service":"svc-a","expectedRevision":2,"instances":[
    {"id":"i1","address":"h1:8080"}
  ]},
  {"type":"register","service":"svc-a","expectedRevision":1,"instances":[
    {"id":"i1","address":"h1:8080"},
    {"id":"i2","address":"10.0.0.2:9090"},
    {"id":"i4","address":"h4.example.com:8080"}
  ]}
]}' | go run ./cmd/indexroom register
```

输出（逐项说明见后）：

```json
{
  "results": [
    {"service":"svc-a","ok":true,"changed":true,"revision":1},
    {"service":"svc-a","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc-a","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc-a","ok":true,"revision":1},
    {"service":"svc-a","ok":true,"revision":1},
    {"service":"svc-a","ok":false,"revision":1,"error":"invalid","reason":"instance address \"h1:70000\" port 70000 is out of range (1-65535)"},
    {"service":"svc-a","ok":false,"revision":1,"error":"conflict","reason":"service \"svc-a\" is at revision 1, not 2","expectedRevision":2,"actualRevision":1},
    {"service":"svc-a","ok":true,"changed":true,"revision":2}
  ],
  "services": [
    {"service":"svc-a","revision":2,"instances":[
      {"id":"i1","address":"h1:8080","health":"healthy","sequence":1},
      {"id":"i2","address":"10.0.0.2:9090","health":"unknown","sequence":0},
      {"id":"i4","address":"h4.example.com:8080","health":"unknown","sequence":0}
    ]}
  ]
}
```

逐项说明：

1. 新服务 `svc-a` 以 `expectedRevision` 0 首次创建，三个实例分别使用域名、IPv4 和带方括号的 IPv6 地址；`changed:true`，修订号为 1，各实例初始为 `unknown`、序号 0。
2. `i1` 的健康观察（`healthy`、序号 1）被接受；健康上报不增加注册修订号，修订号仍为 1。
3. `i2` 的不健康观察（序号 1、原因 `磁盘故障`）被接受，修订号仍为 1。
4. 重复提交完整列表：服务名、实例标识和地址两端的空白被去除，整理后与当前列表完全相同。成功但**没有 `changed`**，修订号保持 1。
5. 仅把实例顺序换成 `i3`、`i1`、`i2`，内容未变：成功且没有 `changed`，修订号保持 1。
6. 端口 70000 超出 1–65535，字段非法：虽然 `expectedRevision` 5 也与当前修订号不符，字段检查先于修订号判断，结果为 `invalid`，`revision` 报告当前修订号 1；原列表不被覆盖。
7. 字段全部合法，但 `expectedRevision` 2 与当前修订号 1 不符：结果为 `conflict`，并给出 `expectedRevision` 2 与 `actualRevision` 1；原列表同样不被覆盖。
8. 改用正确的当前修订号 1 提交实际替换：`i1` 地址不变、`i2` 地址改为 `10.0.0.2:9090`、`i3` 因未包含而被移除、`i4` 新加入。内容改变，`changed:true`，修订号增至 2。尽管第 6、7 项失败，本项仍按输入顺序正常生效。

末尾的 `services` 列表是全部请求处理完后的最终快照，与逐项结果一一对应：

- `i1` 标识与地址都未变，保留第 2 项的健康记录（`healthy`、序号 1），无需在修订号 2 下重新上报；
- `i2` 标识相同但地址改变，健康记录重置为 `unknown`、序号 0，第 3 项的 `磁盘故障` 不会沿用到新地址，需要重新上报健康；
- `i4` 是新加入的实例，为 `unknown`、序号 0，需要重新上报健康；
- `i3` 已被移除，不出现在列表中；实例按标识升序排列，服务修订号为 2。

本批次含有失败项（第 6、7 项），进程退出状态为 1，但失败项之后的替换仍然生效；只有全部请求成功时退出状态才为 0。

### 空数组与 `null` 示例

下面展示空数组清空实例、服务仍然存在，以及 `null` 不能代替空数组：

```bash
echo '{"requests":[
  {"type":"register","service":"svc-b","expectedRevision":0,"instances":[
    {"id":"j1","address":"h1:8080"}
  ]},
  {"type":"register","service":"svc-b","expectedRevision":1,"instances":[]},
  {"type":"register","service":"svc-b","expectedRevision":2,"instances":[]},
  {"type":"select","service":"svc-b","expectedRevision":2},
  {"type":"register","service":"svc-b","expectedRevision":2,"instances":null},
  {"type":"register","service":"svc-b","expectedRevision":2,"instances":[
    {"id":"j1","address":"h1:8080"}
  ]}
]}' | go run ./cmd/indexroom register
```

输出：

```json
{
  "results": [
    {"service":"svc-b","ok":true,"changed":true,"revision":1},
    {"service":"svc-b","ok":true,"changed":true,"revision":2},
    {"service":"svc-b","ok":true,"revision":2},
    {"service":"svc-b","ok":false,"revision":2,"error":"no_healthy","reason":"service \"svc-b\" has no healthy instance available"},
    {"service":"svc-b","ok":false,"revision":2,"error":"invalid","reason":"instances must be an array"},
    {"service":"svc-b","ok":true,"changed":true,"revision":3}
  ],
  "services": [
    {"service":"svc-b","revision":3,"instances":[
      {"id":"j1","address":"h1:8080","health":"unknown","sequence":0}
    ]}
  ]
}
```

逐项说明：

1. 创建 `svc-b`，含实例 `j1`，修订号为 1。
2. 提交空数组：实例被清空，内容改变，`changed:true`，修订号增至 2；服务本身仍存在。
3. 再次提交空数组：整理后内容相同，成功但没有 `changed`，修订号保持 2。
4. 空列表上做 `select`：服务存在且修订号匹配，但没有健康实例，返回 `no_healthy`（若服务随空列表被删除，这里会是 `not_found`，可借此确认服务仍在）。
5. `instances:null` 不是数组，返回 `invalid`；空列表不被覆盖，修订号仍为 2，后续请求继续处理。
6. 用当前修订号 2 重新加入 `j1`：内容改变，`changed:true`，修订号增至 3；`j1` 作为新内容初始为 `unknown`、序号 0。

末尾 `services` 中 `svc-b` 修订号为 3、仅含 `j1`。本批次含失败项（第 4、5 项），退出状态为 1。

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
