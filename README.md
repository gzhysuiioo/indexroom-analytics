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

A `select` request may also carry an optional `excludeInstanceIds` array of
strings naming instances this one request must not choose — for example when
the caller already tried one instance for this attempt and wants another
healthy target. The exclusion scopes only that request: excluded instances
stay registered with their health records and remain eligible for later
requests submitted without the list. Ids are trimmed before matching,
duplicates count as one, and unknown ids are ignored; an omitted field or an
empty array selects exactly as before. An explicit `null`, a non-array value,
a non-string element or a blank-after-trim id is `invalid` with a reason
naming the `excludeInstanceIds` problem, checked before the revision
comparison. The rotation still continues just after the last actually rotated
id — excluding that id does not reset the position — and a session whose
bound instance is excluded falls back to the filtered rotation, rebinding
only its own key on success. When every healthy instance is excluded the
result is `no_healthy` with a reason saying the exclusion removed them all;
the failure fabricates no target and moves neither the cursor nor any
binding.

A `release_session` request carries `service`, `expectedRevision` and a
**required** `sessionKey` and drops that one session binding in that one
service, so the key's next selection joins the normal rotation again. A
present binding is removed and the result carries `changed:true`; releasing a
key that has no binding still succeeds, but the result omits `changed`. The
state of the formerly bound instance is irrelevant: removed, never-confirmed-
healthy or currently unhealthy instances, and even an empty instance list, do
not block removing the binding. A release chooses no target — its result never
contains `instanceId`, `address` or a health `sequence` — and it moves or
resets neither the rotation cursor nor any other session's binding (including
another session bound to the same instance and the same key in another
service); it never alters the instance list, revision or health records. The
key's next selection follows the first-binding rule, rotating from just after
the last actually rotated position and rebinding on success. The service name
and session key are trimmed first; a missing, `null`, non-string or
blank-after-trim key is `invalid` (the reason names the `sessionKey`
problem), field validity precedes the revision check, a revision mismatch is
`conflict`, and an unknown service at `expectedRevision` 0 is `not_found` —
each failure states the current revision and a reason, preserves every binding
and the rotation position, and later requests in the batch still run.

## 离线健康上报（`health` 请求）中文说明

`health` 请求在 `register` 命令的同一批 `requests` 中记录一条离线健康观察，不做任何网络探测。规则如下：

- `expectedRevision` 必须等于该服务当前的注册修订号；健康上报**不增加**修订号，上报前后修订号不变。
- `sequence` 是**单个实例**的正整数序号，每个实例独立计数，不是整批请求共用的计数。合法范围为 `1..9223372036854775807`（有符号 64 位整数），按提交的原始数值判定：`1.5`、字符串、`true` 等非整数令牌，以及超出 int64 范围的整数（如 `9223372036854775808`）都返回 `invalid`，原因指出 `sequence` 的数值问题而不是让整条请求变成解码错误；该单项失败不影响批次后续请求的逐项结果。即使在 2^53 之上，相邻序号（如 `9007199254740992` 与 `9007199254740993`）仍被精确区分，输出的序号与提交值逐位一致，不做浮点舍入。
- 序号大于当前记录时更新健康状态；小于当前记录时返回 `stale`；等于当前记录时，只有健康状态和整理后的原因都相同才成功且不产生变更，否则返回 `conflict`。
- 实例刚注册时健康状态为 `unknown`、序号为 0、无原因。
- 不健康观察（`"healthy":false`）必须提供 `reason`，且去除两端空白后仍非空；健康观察（`"healthy":true`）会清空原因。原因先去除两端空白再存储，整理后的结果参与相同序号的重复判断。
- 成功结果中 `changed` 未出现表示没有变更（例如重复上报），不能把 `"ok":true` 等同于记录已更新。
- 失败的请求不会覆盖先前记录；批次中后续请求仍按输入顺序继续执行。
- 每次调用都从空注册表开始，示例所需的注册和观察必须放在同一批请求中。
- 注册替换与观察的关系：替换实例列表时，实例标识和地址都没变的实例保留健康记录；地址改变或删除后重新加入的实例回到 `unknown`、序号 0、无原因，新地址只有在当前修订号下上报健康结果后才能被选择。旧修订号的观察即使序号更大也返回 `conflict`，不能把旧地址的健康结果带到新修订号。
- 错误分类：字段无效（如 `service` 为空、`expectedRevision` 为负数或超出本程序整数范围（32 位 `0..2147483647`、64 位 `0..9223372036854775807`）、`sequence` 不是整数、非正或超出 `1..9223372036854775807`、不健康但原因为空白）返回 `invalid`；修订号不符返回 `conflict`；修订号匹配但服务或实例不存在返回 `not_found`。字段检查先于修订号判断。

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

### 完整示例

下面围绕一个服务 `svc` 和两个健康实例（`i1`/`h1:8080`、`i2`/`h2:8080`）展示：会话首次选择参与轮询、之后复用绑定而不推动轮询、普通选择穿插进来后仍从最近一次实际轮询位置继续；中途绑定实例变为不健康后回退轮询并改写绑定，原实例恢复健康后会话不会自动迁回；最后穿插一个纯空白会话键的失败项。

**每次调用 `register` 命令都从空注册表开始**，因此首次注册、健康上报、建立会话与后续全部选择必须放进同一份 `requests` 数组、用同一条命令提交；拆成多次独立调用时，上一次调用内存中的绑定和轮询位置不会保留，不能声称会话仍然绑定：

```bash
echo '{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[
    {"id":"i1","address":"h1:8080"},
    {"id":"i2","address":"h2:8080"}
  ]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  user-42  "},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":false,"reason":"连接失败"},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"   "},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":3,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  user-42  "},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"}
]}' | go run ./cmd/indexroom register
```

输出（逐项说明见后）：

```json
{
  "results": [
    {"service":"svc","ok":true,"changed":true,"revision":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i1","address":"h1:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i2","address":"h2:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i1","address":"h1:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i1","address":"h1:8080"},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":2},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i2","address":"h2:8080"},
    {"service":"svc","ok":false,"revision":1,"error":"invalid","reason":"sessionKey must not be empty"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i2","address":"h2:8080"},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":3},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i2","address":"h2:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":3,"instanceId":"i1","address":"h1:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i2","address":"h2:8080"}
  ],
  "services": [
    {"service":"svc","revision":1,"instances":[
      {"id":"i1","address":"h1:8080","health":"healthy","sequence":3},
      {"id":"i2","address":"h2:8080","health":"healthy","sequence":1}
    ]}
  ]
}
```

逐项说明：

1. 注册服务 `svc`，含 `i1`、`i2` 两个实例，`changed:true`，修订号为 1；两个实例初始都是 `unknown`、序号 0。
2. `i1` 的健康观察（序号 1）被接受，变为 `healthy`；健康上报不增加修订号。
3. `i2` 的健康观察（序号 1）被接受，同样变为 `healthy`，修订号仍为 1。
4. 会话键 `"  user-42  "` 去除两端空白后为 `user-42`。该键第一次出现、尚无绑定，因此**参与正常轮询**：按实例标识升序选出最小的 `i1`，返回地址 `h1:8080` 和最新健康序号 1；轮询位置随这次实际轮询停在 `i1`，并记住绑定 `user-42 → i1`。
5. 不带 `sessionKey` 的普通选择从最近一次实际轮询选中的 `i1` 之后继续，选中 `i2`（`h2:8080`、序号 1），轮询位置推进到 `i2`。
6. 再次使用会话键 `user-42`：直接复用绑定的 `i1`（`h1:8080`、序号 1），**不推动轮询位置**，轮询位置仍停在第 5 项实际选中的 `i2`。
7. 又一次普通选择：从最近一次实际轮询位置 `i2` 之后继续，越过末尾回到最小的 `i1`。这说明第 6 项的复用既没有把轮询向前推、也没有向后拉。
8. `i1` 收到更大序号 2 的不健康观察，原因为非空的 `连接失败`，`changed:true`；修订号仍为 1。
9. 同一 `user-42` 再选择时，绑定的 `i1` 当前为 `unhealthy`，不能复用：回退到正常轮询，从最近一次实际轮询位置（第 7 项选中的 `i1`）之后继续；当前健康集合只剩 `i2`，于是选中 `i2`（`h2:8080`、序号 1）。成功后绑定**改写**为 `user-42 → i2`，轮询位置也随这次实际轮询推进到 `i2`。
10. 提交纯空白会话键 `"   "`：整理后为空，返回 `invalid`，原因 `sessionKey must not be empty` 明确指出 `sessionKey` 问题，`revision` 报告当前修订号 1。失败项没有 `instanceId`、`address`、`sequence` 等目标实例字段，也**不创建或改写绑定、不移动轮询位置**；批次后续请求继续执行。
11. `user-42` 再选择：仍然复用 `i2`（`h2:8080`、序号 1），证明第 10 项失败没有动到绑定。
12. `i1` 收到更大序号 3 的健康观察，恢复为 `healthy`，`changed:true`，原因被清空；修订号仍为 1。
13. 使用带两端空白的 `"  user-42  "`（整理后仍是第 4 项建立的同一个会话）：返回的还是 `i2`、地址 `h2:8080`、序号 1。原绑定实例 `i1` 虽然已经恢复健康，会话**不会自动迁回**。
14. 普通选择：从最近一次实际轮询位置（第 9 项选中的 `i2`）之后继续，越过末尾回到 `i1`；此时 `i1` 已恢复，返回其当前地址 `h1:8080` 和最新健康序号 3，轮询位置推进到 `i1`。
15. `user-42` 最后再选择：依旧复用 `i2`、序号 1。普通轮询在第 14 项经过 `i1` 不会改写会话绑定。

末尾的 `services` 列表与逐项结果一一对应：`i1` 为 `healthy`、序号 3，`i2` 为 `healthy`、序号 1；`i1` 的不健康原因已被第 12 项的健康观察清空，因此列表中没有原因字段，实例按标识升序排列。整批选择期间注册修订号始终是 1，健康序号只由第 2、3、8、12 项 `health` 请求推进，`select` 不改变注册修订号或任何健康记录；成功的选择结果没有 `changed` 字段，失败项（第 10 项）没有目标实例字段。本批次含有失败项，进程退出状态为 1，即使其后第 11–15 项全部成功也一样。输出只包含程序实际公开的字段：会话绑定和轮询位置没有查询接口，只能像本示例这样通过后续选择的返回来观察。

## 选择时排除实例（`select` 请求的 `excludeInstanceIds`）中文说明

`select` 请求可以携带可选的字符串数组 `excludeInstanceIds`，用于本次请求已经尝试过一个实例、希望继续选择其他健康目标的情况。规则如下：

- 排除只影响这一项请求：被排除的实例仍保留注册状态和原有健康记录，后续未提交排除列表的普通请求仍可选择它。未提供该字段或提供空数组 `[]` 时，沿用现有选择行为。
- 列表必须是字符串数组。标识先去除两端空白，再与注册后的实例标识对应；重复标识按同一个标识处理，尚未注册的标识可以忽略。显式 `null`、非数组、数组中的非字符串或整理后为空的标识，都使该项返回 `invalid`，原因明确指出 `excludeInstanceIds` 的问题；字段校验仍先于修订号判断，列表里其他合法标识不能使失败请求部分生效。
- 选择只能返回当前健康且未被本次排除的实例。沿用按实例标识升序轮询的规则，从最近一次实际轮询选中的标识之后继续，末尾没有候选时才回到最小标识；排除最后选中的实例也不能重置轮询位置。例如当前位置在 `a`，`a`、`b`、`c` 都健康，本次排除 `b` 应选中 `c`，随后不带排除列表的普通选择应得到 `a`，再下一次得到 `b`。成功结果继续返回所选实例当前地址和最新健康序号，注册修订号不因筛选而变化。
- 带 `sessionKey` 的选择同样服从这份列表：绑定实例健康且未被排除时照常复用，不推进轮询；绑定实例被排除时，按当前轮询位置选择其他健康目标，仅成功后替换该键的绑定并推进轮询，其他会话保留原有绑定。
- 服务存在且修订号匹配，但筛选后没有健康目标时仍返回 `no_healthy`；如果原本有健康实例而全部被排除，原因明确说明这一点。失败不返回实例标识、地址或健康序号，不改写任何绑定和轮询位置，批次后续请求继续按各自的排除范围处理。

### 完整示例

下面用三个健康实例（`a`/`b`/`c`）展示：排除只影响当次请求、轮询位置不因排除而重置、会话绑定实例被排除时回退轮询并改写绑定，以及非法列表与全部排除两种失败形态：

```bash
echo '{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[
    {"id":"a","address":"h1:8080"},
    {"id":"b","address":"h2:8080"},
    {"id":"c","address":"h3:8080"}
  ]},
  {"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["b"]},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42","excludeInstanceIds":["b"]},
  {"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":null},
  {"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["a","b","c"]},
  {"type":"select","service":"svc","expectedRevision":1}
]}' | go run ./cmd/indexroom register
```

输出（逐项说明见后）：

```json
{
  "results": [
    {"service":"svc","ok":true,"changed":true,"revision":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"a","address":"h1:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"c","address":"h3:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"a","address":"h1:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"b","address":"h2:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"c","address":"h3:8080"},
    {"service":"svc","ok":false,"revision":1,"error":"invalid","reason":"excludeInstanceIds must be an array of strings, not null"},
    {"service":"svc","ok":false,"revision":1,"error":"no_healthy","reason":"service \"svc\" has no healthy instance available: all healthy instances are excluded by excludeInstanceIds"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"a","address":"h1:8080"}
  ],
  "services": [
    {"service":"svc","revision":1,"instances":[
      {"id":"a","address":"h1:8080","health":"healthy","sequence":1},
      {"id":"b","address":"h2:8080","health":"healthy","sequence":1},
      {"id":"c","address":"h3:8080","health":"healthy","sequence":1}
    ]}
  ]
}
```

逐项说明：

1. 注册服务 `svc`，含 `a`、`b`、`c` 三个实例，修订号为 1。
2–4. 三个实例各接受序号 1 的健康观察，全部 `healthy`；健康上报不增加修订号。
5. 普通选择从最小标识开始，选中 `a`，轮询位置停在 `a`。
6. 本次排除 `b`：从位置 `a` 之后继续，候选只剩 `a`、`c`，于是选中 `c`，轮询位置推进到 `c`。`b` 的注册状态和健康记录不受影响。
7. 不带排除列表的普通选择越过末尾回到 `a`，证明第 6 项的排除没有重置轮询位置，也没有移除 `b`。
8. 会话键 `user-42` 首次选择参与正常轮询，从位置 `a` 之后选中 `b`，记住绑定 `user-42 → b`。
9. 同一会话本次排除 `b`：绑定实例被排除，不能复用，按当前轮询位置（`b` 之后）在未被排除的 `a`、`c` 中选中 `c`；成功后该键的绑定改写为 `user-42 → c`，轮询位置推进到 `c`，其他会话的绑定不受影响。
10. `excludeInstanceIds:null` 不是数组，返回 `invalid`，原因明确指出 `excludeInstanceIds` 问题，`revision` 报告当前修订号 1；失败项没有目标实例字段，也不移动轮询位置、不改写绑定。
11. 排除 `a`、`b`、`c`：三个实例原本都健康但全部被排除，返回 `no_healthy`，原因明确说明所有健康实例都被 `excludeInstanceIds` 排除；失败同样不移动轮询位置。
12. 普通选择从最近一次实际轮询位置 `c` 之后继续，越过末尾回到 `a`；第 10、11 项的失败没有改变任何状态。

末尾的 `services` 列表显示三个实例仍为 `healthy`、序号 1，注册修订号始终为 1：排除只是当次请求的筛选，不改变注册和健康记录。本批次含失败项（第 10、11 项），进程退出状态为 1。

## 解除单个会话绑定（`release_session` 请求）中文说明

`release_session` 请求在同一批 `requests` 中解除**指定服务下指定会话键**的绑定，让该键的下一次选择重新参与轮询。它与已有请求按输入顺序处理，只使用本次调用的内存状态，不进行任何网络访问。

### 请求字段与校验规则

- `service`：服务名，先去除两端空白；整理后为空返回 `invalid`。解除只作用于该服务，其他服务下的同名会话键不受影响。
- `expectedRevision`：必填整数，合法性要求与其他请求完全一致（32 位程序 `0..2147483647`、64 位程序 `0..9223372036854775807`）。
- `sessionKey`：**必填**。先去除两端空白；缺失、显式 `null`、非字符串或整理后为空都返回 `invalid`，原因明确指出 `sessionKey` 问题（整理后相同的键是同一个会话）。
- **字段检查先于修订号判断**：字段全部合法后，修订号不匹配返回 `conflict`（携带 `expectedRevision` 与 `actualRevision`）；未知服务在 `expectedRevision` 为 0 时返回 `not_found`。失败报告处理该项时的当前修订号和具体原因，保留全部绑定和轮询位置，批次后续请求继续执行。

### 成功语义与不变量

- 该服务下该键**存在绑定时**：移除它，成功结果含 `service`、当前 `revision` 与 `changed:true`。
- **没有绑定时**（从未绑定、或已经解除过）：仍然成功，但结果中**不出现 `changed`**。
- 绑定指向的实例已被删除、尚未确认健康（`unknown`）、已不健康（`unhealthy`），以及服务实例列表为空，**都不能阻止**解除已有绑定——绑定只是记住的一个实例标识。
- 解除**不选择目标**：结果永远不出现 `instanceId`、`address` 或健康 `sequence`。
- 解除本身**不移动也不重置**轮询位置，不改变实例列表、注册修订号和健康记录。其他会话即使绑定同一实例也继续保留自己的绑定。
- 解除之后同一键的选择沿用**首次建立绑定的规则**：从最近一次实际轮询位置之后选取健康实例，成功后建立新绑定。例如健康实例依次为 `i1`、`i2`、`i3`，会话先绑定 `i1`，随后普通选择得到 `i2`，解除后该会话再次选择应得到 `i3`。

### 完整示例

```bash
echo '{"requests":[
  {"type":"register","service":"svc","expectedRevision":0,"instances":[
    {"id":"i1","address":"h1:8080"},
    {"id":"i2","address":"h2:8080"},
    {"id":"i3","address":"h3:8080"}
  ]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"health","service":"svc","instanceId":"i3","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  user-42  "},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"  user-42  "},
  {"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
  {"type":"select","service":"svc","expectedRevision":1},
  {"type":"release_session","service":"svc","expectedRevision":9,"sessionKey":"user-42"},
  {"type":"release_session","service":"ghost","expectedRevision":0,"sessionKey":"user-42"},
  {"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"   "},
  {"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"}
]}' | go run ./cmd/indexroom register
```

输出（逐项说明见后）：

```json
{
  "results": [
    {"service":"svc","ok":true,"changed":true,"revision":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"changed":true,"revision":1,"sequence":1},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i1","address":"h1:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i2","address":"h2:8080"},
    {"service":"svc","ok":true,"changed":true,"revision":1},
    {"service":"svc","ok":true,"revision":1},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i3","address":"h3:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i3","address":"h3:8080"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i1","address":"h1:8080"},
    {"service":"svc","ok":false,"revision":1,"error":"conflict","reason":"service \"svc\" is at revision 1, not 9","expectedRevision":9,"actualRevision":1},
    {"service":"ghost","ok":false,"revision":0,"error":"not_found","reason":"service \"ghost\" does not exist"},
    {"service":"svc","ok":false,"revision":1,"error":"invalid","reason":"sessionKey must not be empty"},
    {"service":"svc","ok":true,"revision":1,"sequence":1,"instanceId":"i3","address":"h3:8080"}
  ],
  "services": [
    {"service":"svc","revision":1,"instances":[
      {"id":"i1","address":"h1:8080","health":"healthy","sequence":1},
      {"id":"i2","address":"h2:8080","health":"healthy","sequence":1},
      {"id":"i3","address":"h3:8080","health":"healthy","sequence":1}
    ]}
  ]
}
```

逐项说明：

1. 注册服务 `svc`，含三个实例，修订号为 1。
2–4. 三个实例各接受序号 1 的健康观察，全部 `healthy`；健康上报不增加修订号。
5. 会话键 `"  user-42  "` 整理为 `user-42`，首次选择参与正常轮询得到 `i1`，轮询位置停在 `i1`，并记住绑定 `user-42 → i1`。
6. 不带键的普通选择从 `i1` 之后继续，得到 `i2`，轮询位置推进到 `i2`。
7. 解除 `svc` 下整理后为 `user-42` 的绑定：绑定存在，`changed:true`；结果只有服务名和当前修订号，没有任何目标实例字段。轮询位置仍是 `i2`。
8. 再次解除同一个键：绑定已不存在，仍然成功，但**没有 `changed`**。
9. 该键的下一次选择按首次绑定规则，从最近一次实际轮询位置 `i2` 之后选取，得到 `i3`，成功后建立新绑定 `user-42 → i3`。
10. 再次使用该键：复用新绑定的 `i3`，不推动轮询。
11. 普通选择越过末尾回到最小的 `i1`，证明解除和复用都没有重置或移动轮询位置。
12. 字段合法但 `expectedRevision` 9 与当前修订号 1 不符：`conflict`，携带两个修订号；绑定不受影响。
13. 未知服务在 `expectedRevision` 为 0 时返回 `not_found`，`revision` 为 0。
14. 纯空白会话键返回 `invalid`，原因 `sessionKey must not be empty` 明确指出 `sessionKey` 问题，`revision` 报告当前修订号 1；失败项没有目标字段，也不改变任何绑定和轮询位置。
15. 第 12–14 项失败之后，会话绑定仍是第 9 项建立的 `i3`：返回 `i3`、地址 `h3:8080`、序号 1。

末尾的 `services` 快照与处理前的健康状态一一对应，证明解除不改变实例列表、修订号和健康记录。本批次含失败项（第 12–14 项），进程退出状态为 1；解除与选择都不进行网络访问，注册、健康观察、绑定建立与解除必须放在同一批 `requests` 中提交。

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
