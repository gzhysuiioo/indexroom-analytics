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

## 服务注册与实例列表替换（`register` 请求）中文说明

`register` 请求（省略 `type` 时默认就是 `register`）用来创建服务或**整体替换**一个服务的实例列表。每次想更新实例列表时，都要把整理后的完整列表随当前修订号重新提交。规则如下：

- **每次调用都从空注册表开始**：注册表只存在于单次命令进程内，不跨调用保存。本文每个示例都是一次独立调用，前置创建必须和后续更新放在同一批 `requests` 中。
- **修订号（`expectedRevision`）必填且是非负整数**：服务不存在时只能用 `0` 创建，创建成功后修订号为 `1`；服务已存在时必须提交它的当前修订号，提交错误修订号返回 `conflict`，失败结果中带有 `expectedRevision`、`actualRevision` 和当前修订号。
- **提交的是完整实例列表**：列表中未包含的已有实例会被移除。只有整理后的内容（实例标识与地址构成的集合）发生变化，修订号才加 1；**仅调整实例顺序不算变更**。
- **重复提交是幂等的**：提交整理后标识和地址都与当前相同的列表会成功，但结果中**不出现** `changed`，修订号保持原值；不要把 `"ok":true` 当成发生了变更。
- **空数组可以清空列表**：`"instances":[]` 会移除全部实例，但服务仍然存在（仍可按当前修订号重新注册或上报；没有健康实例时 `select` 返回 `no_healthy`），修订号同样按内容是否改变计算。`null` **不能**当成空数组，`"instances":null` 返回 `invalid`。
- **字段整理**：服务名、每个实例的标识 `id` 和地址 `address` 都会先去除两端空白再比较和存储；整理后服务名或标识为空、或同一服务内出现重复标识，整项被拒绝（`invalid`）。
- **地址格式**：必须带端口，形如 `host:port`；主机支持域名、IPv4 和带方括号的 IPv6（裸 IPv6 必须写成 `[2001:db8::1]:443` 这种形式）；端口必须是 1 到 65535 的十进制整数；地址内部不能含有空白或控制字符。
- **错误判断先后**：字段不合法一律返回 `invalid`，且**先于**修订号冲突判断（即使同时提交了错误的修订号，也仍是 `invalid`）；只有字段全部合法、仅修订号不符时才返回 `conflict`。失败结果都带有原因和当前修订号（未知服务为 0），且**不会覆盖原列表**；批次中后续请求仍按输入顺序继续处理。批次只要含有失败项，进程退出状态就是 1，但此前和此后成功的项都已生效。
- **对健康记录的影响**：替换列表时，实例标识和地址**都没变**的实例保留原有健康记录（状态、序号、原因）；地址改变的实例、以及新加入（含先删除再重新加入）的实例都回到 `unknown`、序号 0、无原因。旧地址上的健康状态不能沿用到新地址，这些实例需要在**新的注册修订号**下重新上报健康（规则见下一节《离线健康上报》）。

### 完整示例

下面用同一批请求依次展示：首次创建、仅调整顺序、重复提交相同列表、实际替换（含移除与新增）、一次字段失败、失败后继续成功更新、空数组清空、再次提交空数组。整条命令完全离线，可直接复制运行：

```bash
echo '{"requests":[
  {"service":" orders ","expectedRevision":0,"instances":[{"id":" i1 ","address":"a.example.com:8080"},{"id":"i2","address":"10.0.0.2:9000"}]},
  {"service":"orders","expectedRevision":1,"instances":[{"id":"i2","address":"10.0.0.2:9000"},{"id":"i1","address":" a.example.com:8080 "}]},
  {"service":"orders","expectedRevision":1,"instances":[{"id":"i1","address":"a.example.com:8080"},{"id":"i2","address":"10.0.0.2:9000"}]},
  {"service":"orders","expectedRevision":1,"instances":[{"id":"i1","address":"a.example.com:8080"},{"id":"i3","address":"[2001:db8::1]:443"}]},
  {"service":"orders","expectedRevision":1,"instances":[{"id":"i3","address":"bad address:443"}]},
  {"service":"orders","expectedRevision":2,"instances":[{"id":"i3","address":"[2001:db8::1]:443"}]},
  {"service":"orders","expectedRevision":3,"instances":[]},
  {"service":"orders","expectedRevision":4,"instances":[]}
]}' | go run ./cmd/indexroom register
```

输出（逐项说明见后）：

```json
{
  "results": [
    {"service":"orders","ok":true,"changed":true,"revision":1},
    {"service":"orders","ok":true,"revision":1},
    {"service":"orders","ok":true,"revision":1},
    {"service":"orders","ok":true,"changed":true,"revision":2},
    {"service":"orders","ok":false,"revision":2,"error":"invalid","reason":"instance address \"bad address:443\" must not contain whitespace or control characters"},
    {"service":"orders","ok":true,"changed":true,"revision":3},
    {"service":"orders","ok":true,"changed":true,"revision":4},
    {"service":"orders","ok":true,"revision":4}
  ],
  "services": [
    {"service":"orders","revision":4,"instances":[]}
  ]
}
```

逐项说明：

1. **首次创建**：服务名 `" orders "` 去掉两端空白后为 `orders`；新服务用 `expectedRevision:0` 创建成功，修订号为 1（`changed:true`）。标识 `" i1 "` 整理为 `i1`，地址两端的空白同样被去除；`i1` 使用域名地址，`i2` 使用 IPv4 地址，两个实例初始均为 `unknown`、序号 0。
2. **仅调整顺序**：实例顺序换成 `i2` 在前，`i1` 的地址还带两端空白，但整理后的标识与地址集合和当前完全相同，因此成功但没有 `changed`，修订号保持 1。
3. **重复提交**：与第 1 项整理后的列表完全相同，再次成功，仍没有 `changed`，修订号仍为 1。
4. **实际替换**：`i1`（标识和地址都没变）保留；`i2` 因未出现在列表中被移除；带方括号的 IPv6 实例 `i3` 新加入，为 `unknown`、序号 0。内容发生变化，`changed:true`，修订号增加到 2。
5. **字段失败**：地址 `bad address:443` 内部含有空白，返回 `invalid` 并给出原因；注意本项的 `expectedRevision` 也是过期的 1，但字段检查先于修订号判断，所以错误是 `invalid` 而不是 `conflict`。结果中的 `revision:2` 是当前修订号；第 4 项提交的列表（`i1`、`i3`）没有被覆盖。
6. **失败后继续成功更新**：按第 5 项报告的当前修订号 2 提交，只保留 `i3`（`i1` 被移除），成功替换，修订号增加到 3。说明失败项不会中断批次，后续请求仍按输入顺序处理。
7. **空数组清空**：`"instances":[]` 移除最后的 `i3`，内容改变，修订号增加到 4；但 `orders` 服务仍然存在。
8. **再次提交空数组**：内容与当前相同，成功但没有 `changed`，修订号保持 4。

`results` 是每一项请求按输入顺序得到的结论，末尾的 `services` 则是整批请求处理完后的**最终快照**（服务按名称排序、实例按标识排序），两者是“逐项过程”和“最终状态”的关系。本例最终 `services` 中只剩一个实例列表为空、修订号为 4 的 `orders`——实例清空了，服务并没有消失。本批含有失败项（第 5 项），进程退出状态为 1；其余成功项均已生效。

### 字段校验与健康记录去留示例

再用一批请求展示 `null`、重复标识、空白标识的处理，`invalid` 与 `conflict` 的先后，以及替换后健康记录哪些保留、哪些重置。其中第 6、7 项是 `health` 请求，具体规则见下一节：

```bash
echo '{"requests":[
  {"service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
  {"service":"svc","expectedRevision":1,"instances":null},
  {"service":"svc","expectedRevision":1,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i1","address":"h9:8080"}]},
  {"service":"svc","expectedRevision":5,"instances":[{"id":"  ","address":"h9:8080"}]},
  {"service":"svc","expectedRevision":5,"instances":[{"id":"i9","address":"h9:8080"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":false,"reason":"磁盘满"},
  {"service":"svc","expectedRevision":1,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:9090"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":2,"sequence":2,"healthy":true},
  {"service":"svc","expectedRevision":2,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:9090"},{"id":"i3","address":"h3:8080"}]}
]}' | go run ./cmd/indexroom register
```

输出：

```json
{
  "results": [
    {"service":"svc","ok":true,"changed":true,"revision":1},
    {"service":"svc","ok":false,"revision":1,"error":"invalid","reason":"instances must be an array"},
    {"service":"svc","ok":false,"revision":1,"error":"invalid","reason":"duplicate instance id \"i1\""},
    {"service":"svc","ok":false,"revision":1,"error":"invalid","reason":"instance id must not be empty"},
    {"service":"svc","ok":false,"revision":1,"error":"conflict","reason":"service \"svc\" is at revision 1, not 5","expectedRevision":5,"actualRevision":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":2},
    {"service":"svc","ok":true,"changed":true,"revision":2,"sequence":2},
    {"service":"svc","ok":true,"changed":true,"revision":3}
  ],
  "services": [
    {"service":"svc","revision":3,"instances":[
      {"id":"i1","address":"h1:8080","health":"healthy","sequence":2},
      {"id":"i2","address":"h2:9090","health":"unknown","sequence":0},
      {"id":"i3","address":"h3:8080","health":"unknown","sequence":0}
    ]}
  ]
}
```

逐项说明：

1. 创建 `svc`，含 `i1`、`i2`，修订号为 1。
2. `"instances":null` 不是数组，返回 `invalid`；要清空列表必须使用 `[]`。
3. 同一服务内出现两个整理后相同的标识 `i1`（即使地址不同），返回 `invalid`。
4. 标识去除两端空白后为空，返回 `invalid`；虽然 `expectedRevision:5` 也是错的，但字段检查先于修订号判断，错误仍是 `invalid`，`revision:1` 为当前修订号。
5. 字段全部合法、仅修订号不符（当前为 1，提交了 5），才返回 `conflict`；结果给出原因和双方修订号，原列表不被覆盖。
6. 给 `i1` 上报序号 1 的健康观察并被接受。
7. 给 `i2` 上报序号 1 的不健康观察（原因整理后为 `磁盘满`）；两项健康上报都不改变注册修订号，此时列表仍是第 1 项创建的内容。
8. 替换列表：`i1` 标识和地址都没变，**保留**健康记录；`i2` 地址由 `h2:8080` 变为 `h2:9090`，健康记录**重置**为 `unknown`、序号 0、无原因。内容改变，修订号增加到 2。
9. 在新修订号 2 下给 `i1` 上报序号 2 的健康观察并被接受，说明保留下来的记录可以继续使用。
10. 再加入新实例 `i3`，修订号增加到 3；`i3` 初始为 `unknown`、序号 0。

对照末尾 `services` 即可判断谁需要重新上报：`i1` 标识与地址始终未变，仍为 `healthy`、序号 2，健康记录沿用；`i2` 虽然标识还在，但地址已变，回到 `unknown`、序号 0，旧地址上的不健康记录不能沿用；`i3` 是新加入的，同样为 `unknown`、序号 0。因此在修订号 3 下，`i2` 和 `i3` 必须重新上报健康后才可能被 `select` 选中，并且观察必须提交当前修订号 3——继续用旧修订号 2 上报会得到 `conflict`。健康上报的完整规则见下一节。

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
