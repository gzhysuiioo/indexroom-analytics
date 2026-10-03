# 链上索引与交易分析服务

## 用途

区块与交易摄取（含可选区块时间）、事件解码与规范化、可组合查询与按时间窗口分段的交易聚合、索引重建与一致性校验、分析指标与快照。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `indexroom/`，命令入口位于 `cmd/indexroom/`。

```bash
go run ./cmd/indexroom demo
go run ./cmd/indexroom version
go test ./...
```

## 主要接口

- `indexroom.Block` 的 `Time *int64` 字段携带非负 Unix 秒数；`nil` 表示未提供时间，与时间为 0 严格区分。
- `Index.Append` / `Index.Reorg`：摄取区块与重组，负时间整体拒绝且不改变已有链。区块的哈希、父哈希与每个交易标识必须是合法 UTF-8，否则在提交时整体拒绝；错误指明区块高度与字段，交易标识还给出从 0 开始的位置。合法 Unicode（中文、emoji、真正的 U+FFFD、空标识、重复值与前后空白）按原值保存，保证快照导出/恢复无损、精确查询不合并标识。
- `Index.QueryTxs`：既有分页交易查询，固定高度范围内的区块时间变化会使旧游标返回 `ErrQueryChanged`。
- `Index.QueryTimeStats`：按 `[Start, End)` 半开窗口与 `StepSeconds` 分段统计交易出现次数、不同标识数、含匹配交易的区块数，并给出整窗口去重汇总与缺失时间区块数；非法参数返回 `ErrInvalidArgument`。
- `Index.Export` / `Index.Restore`：快照版本 1（全无时间时保持原字节）与版本 2（任一块有时间时为每块输出必填 `timestamp`，缺失为 `null`）。

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
