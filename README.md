# 链上索引与交易分析服务

## 用途

区块与交易摄取、事件解码与规范化、可组合查询与聚合、索引重建与一致性校验、分析指标与快照。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `indexroom/`，命令入口位于 `cmd/indexroom/`。

```bash
go run ./cmd/indexroom demo
go run ./cmd/indexroom version
echo '{"requests":[]}' | go run ./cmd/indexroom register
go test ./...
```

## 离线服务注册

`register` 从标准输入读取一个 JSON 对象，按 `requests` 数组顺序逐条登记服务实例，每次调用从空注册表开始：

```json
{
  "requests": [
    {
      "service": "orders",
      "expectedRevision": 0,
      "instances": [
        {"id": "a", "address": "orders-1.internal:8080"},
        {"id": "b", "address": "[2001:db8::1]:9000"}
      ]
    }
  ]
}
```

- 一次登记完整替换该服务的实例列表；其他服务不受影响，空列表保留服务记录。
- 新服务版本为零，仅接受 `expectedRevision` 为 0 的请求，创建后版本为一；已有服务要求期望版本等于当前版本，列表变化使版本加一，内容相同（忽略实例顺序）则版本不变。
- 先校验内容（服务名、实例 id 与地址去空白后非空，同一服务内 id 唯一，地址为域名/IPv4 的 `host:port` 或带方括号的 `[IPv6]:port`，端口 1–65535），再判定版本冲突；失败登记不产生任何副作用。
- 输出每条结果与最终服务列表（服务按名排序、实例按 id 排序）。全部成功退出码为 0，任一登记失败为 1；输入不是合法 JSON 或 `requests` 不是数组时输出错误并以 2 退出，不处理任何登记。


## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
