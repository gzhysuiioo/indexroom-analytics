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

Requests are processed in input order and come in three types:

- `register` — replace the service's instance list (the default when `type` is
  omitted).
- `health` — record an offline healthy/unhealthy observation for one instance;
  it never probes and never bumps the registration revision.
- `select` — pick one healthy instance for the service. Only `healthy`
  instances are eligible; `unknown` and `unhealthy` are not. Each service
  rotates through instance ids in ascending order: the first pick is the
  smallest id, and each later pick continues strictly after the previous pick,
  wrapping to the smallest after the end. A single healthy instance is picked
  repeatedly. The rotation continues from the last successful pick's id even
  when that instance has been removed or become unhealthy; failed requests,
  duplicate registrations and duplicate health reports never advance it. A
  successful select returns `instanceId`, `address` and the instance's current
  health `sequence`; it never changes the registry. Failures are `invalid`
  (bad fields), `conflict` (revision mismatch), `not_found` (service missing)
  or `no_healthy` (no healthy target available), each with the current revision.

```bash
echo '{"requests":[
  {"service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"host:8080"}]},
  {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
  {"type":"select","service":"svc","expectedRevision":1}
]}' | go run ./cmd/indexroom register
```

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
