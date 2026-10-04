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
- `Index.Append` / `Index.Reorg`：摄取区块与重组，负时间整体拒绝且不改变已有链；哈希、父哈希或任一交易标识含非法 UTF-8 字节时同样整体拒绝，错误信息指出区块高度与字段（交易标识另指出从 0 开始的位置），保证快照可无损导出与恢复。
- `Index.QueryTxs`：既有分页交易查询，围绕一个或多个交易标识读取主链上的每次出现；固定高度范围内的区块时间变化会使旧游标返回 `ErrQueryChanged`。完整翻页用法见下方[分页交易查询指南](#分页交易查询指南querytxs)。
- `Index.QueryTimeStats`：按 `[Start, End)` 半开窗口与 `StepSeconds` 分段统计交易出现次数、不同标识数、含匹配交易的区块数，并给出整窗口去重汇总与缺失时间区块数；非法参数返回 `ErrInvalidArgument`。完整用法见下方[时间窗口统计指南](#时间窗口统计指南querytimestats)。
- `Index.Export` / `Index.Restore`：快照版本 1（全无时间时保持原字节）与版本 2（任一块有时间时为每块输出必填 `timestamp`，缺失为 `null`）。

## 分页交易查询指南（QueryTxs）

`Index.QueryTxs(query TxQuery) (TxPage, error)` 围绕一个或多个交易标识，按"高度、再按块内位置"的顺序读取这些标识在主链上的**每一次出现**。查询只读，可与摄取并发调用，每次调用都看到一个完整的链状态。

- `TxQuery`：`From`/`To` 为闭区间高度，`TxIDs` 为筛选标识集合，`PageSize` 为每页条数（0 取 `DefaultPageSize=100`，最大 `MaxPageSize=1000`），`Cursor` 为续查游标。
- `TxHit`：一次命中，字段为 `Height`、`BlockHash`、`TxID`、`Position`（块内从 0 开始的位置）。
- `TxPage`：本页 `Hits`，以及对整个固定范围的统计 `TotalMatches`（匹配出现总次数）、`MatchedBlocks`（含匹配交易的区块数）、`ToHeight`（第一页固定下来的实际上界）和 `NextCursor`。

### 如何取得第一页、继续读取、何时结束

1. **第一页**：`Cursor` 传空字符串即首次查询。`From` 为 0 时默认从高度 1 开始；`To` 为 0 时取首次查询看到的链顶。
2. **继续读取**：把上一页返回的 `NextCursor` 原样填回 `TxQuery.Cursor` 再次调用。游标是服务返回的**不透明字符串**，不要解析或拼接。续查时高度范围与 `TxIDs` 必须与第一页等价；`PageSize` 可以逐页调整。
3. **结束**：返回页的 `NextCursor` 为空即最后一页。范围内没有匹配交易时不是错误，而是成功返回一个空页（`Hits` 为空、无游标）。

重复出现的交易**不会合并**：同一标识在不同区块、或同一区块内出现多次，就返回多条 `TxHit`。筛选按字符串精确匹配，`TxIDs` 的**顺序与重复项不影响匹配**（`["a","a"]` 与 `["a"]` 等价）；但大小写与首尾空白仍按原字符串区分（`"A"`、`" a "` 都不会匹配 `"a"`）。

### 范围在第一页固定

- 第一页若显式给出高于当前链顶的 `To`，返回的 `ToHeight` 是夹到链顶后的值；**后续请求仍要传原先请求的 `To`，不能用返回值替换**（例如首查 `To=10`、链顶只有 3 时 `ToHeight=3`，续查仍须传 `To=10`）。
- 范围固定后，之后新追加的区块对本次翻页不可见：第一页之后再追加含匹配标识的区块，继续翻页仍只读取原来固定的范围，`TotalMatches`、`MatchedBlocks` 也不增加。需要新数据就以空游标重新开始一次查询。

### 游标失效：区分 ErrQueryChanged 与 ErrInvalidArgument

- **`ErrQueryChanged`（链数据变了）**：固定范围内任一区块的内容发生改变——即使区块哈希不变、**只改变了时间**——或链顶退到固定结束高度以下，续查都会返回该错误，且**没有可用的页结果**。此时只能**从空游标重新开始第一页**，绝不能把新结果拼接到旧结果后面。
- **`ErrInvalidArgument`（请求本身不合法）**：续查更改高度范围或筛选集合、游标损坏、或把游标交给另一个索引实例（游标带实例签名，跨实例无效）。它与链数据变化无关，用 `errors.Is` 与 `ErrQueryChanged` 区分；请先修正请求再重试。

### 完整示例

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/querytxs/main.go`](examples/querytxs/main.go)：

```bash
go run ./examples/querytxs
```

场景：三个连续区块的交易列表依次为 `[a,b,a]`、`[a]`、`[b]`，筛选标识 `a`，每页 2 条。第一页返回高度 1 中的两次 `a`（位置 0、2），第二页返回高度 2 中的一次 `a`（位置 0）；两页的 `TotalMatches` 都是 3、`MatchedBlocks` 都是 2，最后一页没有后续游标。

```go
// 分页交易查询（Index.QueryTxs）完整示例：第一页、继续翻页、范围固定、
// 筛选语义、空页、ErrInvalidArgument 与 ErrQueryChanged 的处理。
//
// 运行：go run ./examples/querytxs
package main

import (
	"errors"
	"fmt"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func mustAppend(index *indexroom.Index, block indexroom.Block) {
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

func printPage(title string, page indexroom.TxPage) {
	fmt.Println(title + "：")
	for _, hit := range page.Hits {
		fmt.Printf("  命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
	fmt.Printf("  TotalMatches=%d MatchedBlocks=%d ToHeight=%d 有后续游标=%v\n",
		page.TotalMatches, page.MatchedBlocks, page.ToHeight, page.NextCursor != "")
}

func main() {
	// 三个连续区块，交易列表依次为 [a,b,a]、[a]、[b]。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b"}})
	fmt.Printf("链顶高度 tip=%d\n\n", index.Tip)

	// 取得第一页：Cursor 留空表示首次查询，From 留空默认从高度 1 开始。
	// 显式 To=10 高于当前链顶 3，第一页把查询范围固定到实际链顶 3。
	query := indexroom.TxQuery{To: 10, TxIDs: []string{"a"}, PageSize: 2}
	page1, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("第1页（请求 To=10，链顶只有 3）", page1)

	// 翻页时每页条数可以调整，其余条件必须与首次查询等价。
	sized := indexroom.TxQuery{To: 10, TxIDs: []string{"a"}, PageSize: 1}
	sizedPage1, err := index.QueryTxs(sized)
	if err != nil {
		panic(err)
	}
	printPage("同一范围每页 1 条时的第1页", sizedPage1)
	sized.Cursor = sizedPage1.NextCursor
	sized.PageSize = 3 // 下一页改成每页 3 条
	sizedPage2, err := index.QueryTxs(sized)
	if err != nil {
		panic(err)
	}
	printPage("改为每页 3 条后继续", sizedPage2)

	// 第一页之后链上又追加一个含 a 的高度 4 区块。
	mustAppend(index, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}})
	fmt.Printf("已追加高度 4，当前链顶 tip=%d\n\n", index.Tip)

	// 继续读取：沿用服务返回的不透明游标，To 仍传最初请求的 10，
	// 不能用第一页返回的 ToHeight=3 替换，否则属于另一次查询。
	query.Cursor = page1.NextCursor
	page2, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("第2页（追加高度 4 之后继续翻页）", page2)
	fmt.Printf("最后一页没有后续游标：%v\n\n", page2.NextCursor == "")

	// 重复出现的交易不会合并；筛选标识的顺序与重复项不影响匹配；
	// 大小写与首尾空白仍按原字符串做精确区分。
	dupA, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{"a", "a"}})
	if err != nil {
		panic(err)
	}
	setAB1, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{"a", "b"}})
	if err != nil {
		panic(err)
	}
	setAB2, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{"b", "a", "b"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("筛选 [a,a] 命中 %d 条；[a,b] 命中 %d 条，乱序且重复的 [b,a,b] 同样命中 %d 条\n",
		dupA.TotalMatches, setAB1.TotalMatches, setAB2.TotalMatches)
	upper, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{"A"}})
	if err != nil {
		panic(err)
	}
	spaced, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{" a "}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("筛选 %q 命中 %d 条，筛选 %q 命中 %d 条（都是成功的空页）\n\n",
		"A", len(upper.Hits), " a ", len(spaced.Hits))

	// To=0 表示取首次查询看到的链顶；范围内没有匹配交易时成功返回空页。
	tipBound, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	fmt.Printf("To=0 固定到首次查询看到的链顶：ToHeight=%d（当前 tip=%d）\n",
		tipBound.ToHeight, index.Tip)
	empty, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{"zzz"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("无匹配交易：err=%v 命中 %d 条 有后续游标=%v\n\n",
		err, len(empty.Hits), empty.NextCursor != "")

	// 参数类错误（ErrInvalidArgument）：与链数据变化无关。
	fresh, err := index.QueryTxs(indexroom.TxQuery{To: 3, TxIDs: []string{"a"}, PageSize: 2})
	if err != nil {
		panic(err)
	}
	goodCursor := fresh.NextCursor // 不透明字符串，只应原样传回
	continueWith := func(to int64, from int64, txIDs []string, cursor string) error {
		_, err := index.QueryTxs(indexroom.TxQuery{From: from, To: to, TxIDs: txIDs, PageSize: 2, Cursor: cursor})
		return err
	}
	fmt.Printf("续查更改结束高度：ErrInvalidArgument=%v\n",
		errors.Is(continueWith(10, 0, []string{"a"}, goodCursor), indexroom.ErrInvalidArgument))
	fmt.Printf("续查更改起始高度：ErrInvalidArgument=%v\n",
		errors.Is(continueWith(3, 2, []string{"a"}, goodCursor), indexroom.ErrInvalidArgument))
	fmt.Printf("续查更改筛选集合：ErrInvalidArgument=%v\n",
		errors.Is(continueWith(3, 0, []string{"b"}, goodCursor), indexroom.ErrInvalidArgument))
	fmt.Printf("游标损坏：ErrInvalidArgument=%v\n",
		errors.Is(continueWith(3, 0, []string{"a"}, "q1.damaged"), indexroom.ErrInvalidArgument))
	other := indexroom.New()
	_, err = other.QueryTxs(indexroom.TxQuery{To: 3, TxIDs: []string{"a"}, PageSize: 2, Cursor: goodCursor})
	fmt.Printf("游标交给另一个索引实例：ErrInvalidArgument=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidArgument))

	// 固定范围内任一区块内容改变（即使只改时间）：续查返回 ErrQueryChanged，
	// 且没有可用的页结果。分支必须覆盖受影响高度之后的所有区块。
	changedTime := int64(1700000000)
	dropped, err := index.Reorg([]indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: &changedTime},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("只改高度 2 的时间，重组替换高度 %v，当前 tip=%d\n", dropped, index.Tip)
	changedPage, err := index.QueryTxs(indexroom.TxQuery{To: 3, TxIDs: []string{"a"}, PageSize: 2, Cursor: goodCursor})
	fmt.Printf("续查：ErrQueryChanged=%v ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(err, indexroom.ErrQueryChanged),
		errors.Is(err, indexroom.ErrInvalidArgument), len(changedPage.Hits))
	// 正确做法：从空游标重新开始，不能把新结果接到旧结果后面。
	restart, err := index.QueryTxs(indexroom.TxQuery{To: 3, TxIDs: []string{"a"}, PageSize: 2})
	if err != nil {
		panic(err)
	}
	printPage("从空游标重新开始后的第1页", restart)

	// 链顶退到固定结束高度以下，同样是 ErrQueryChanged。
	pinned, err := index.QueryTxs(indexroom.TxQuery{To: 10, TxIDs: []string{"a"}, PageSize: 2})
	if err != nil {
		panic(err)
	}
	dropped, err = index.Reorg([]indexroom.Block{{Height: 2, Hash: "h2c", Parent: "h1", Txs: []string{"a"}}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("链顶回退，重组丢弃高度 %v，当前 tip=%d\n", dropped, index.Tip)
	_, err = index.QueryTxs(indexroom.TxQuery{To: 10, TxIDs: []string{"a"}, PageSize: 2, Cursor: pinned.NextCursor})
	fmt.Printf("续查：ErrQueryChanged=%v（固定结束高度为 %d）\n",
		errors.Is(err, indexroom.ErrQueryChanged), pinned.ToHeight)
	restartTip, err := index.QueryTxs(indexroom.TxQuery{To: 10, TxIDs: []string{"a"}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	printPage("再次从空游标重新开始", restartTip)
}
```

对应输出：

```text
链顶高度 tip=3

第1页（请求 To=10，链顶只有 3）：
  命中 height=1 block=h1 tx="a" position=0
  命中 height=1 block=h1 tx="a" position=2
  TotalMatches=3 MatchedBlocks=2 ToHeight=3 有后续游标=true
同一范围每页 1 条时的第1页：
  命中 height=1 block=h1 tx="a" position=0
  TotalMatches=3 MatchedBlocks=2 ToHeight=3 有后续游标=true
改为每页 3 条后继续：
  命中 height=1 block=h1 tx="a" position=2
  命中 height=2 block=h2 tx="a" position=0
  TotalMatches=3 MatchedBlocks=2 ToHeight=3 有后续游标=false
已追加高度 4，当前链顶 tip=4

第2页（追加高度 4 之后继续翻页）：
  命中 height=2 block=h2 tx="a" position=0
  TotalMatches=3 MatchedBlocks=2 ToHeight=3 有后续游标=false
最后一页没有后续游标：true

筛选 [a,a] 命中 3 条；[a,b] 命中 5 条，乱序且重复的 [b,a,b] 同样命中 5 条
筛选 "A" 命中 0 条，筛选 " a " 命中 0 条（都是成功的空页）

To=0 固定到首次查询看到的链顶：ToHeight=4（当前 tip=4）
无匹配交易：err=<nil> 命中 0 条 有后续游标=false

续查更改结束高度：ErrInvalidArgument=true
续查更改起始高度：ErrInvalidArgument=true
续查更改筛选集合：ErrInvalidArgument=true
游标损坏：ErrInvalidArgument=true
游标交给另一个索引实例：ErrInvalidArgument=true

只改高度 2 的时间，重组替换高度 [2]，当前 tip=4
续查：ErrQueryChanged=true ErrInvalidArgument=false 可用命中数=0
从空游标重新开始后的第1页：
  命中 height=1 block=h1 tx="a" position=0
  命中 height=1 block=h1 tx="a" position=2
  TotalMatches=3 MatchedBlocks=2 ToHeight=3 有后续游标=true
链顶回退，重组丢弃高度 [2 3 4]，当前 tip=2
续查：ErrQueryChanged=true（固定结束高度为 4）
再次从空游标重新开始：
  命中 height=1 block=h1 tx="a" position=0
  命中 height=1 block=h1 tx="a" position=2
  命中 height=2 block=h2c tx="a" position=0
  TotalMatches=3 MatchedBlocks=2 ToHeight=2 有后续游标=false
```

## 时间窗口统计指南（QueryTimeStats）

`Index.QueryTimeStats(query TimeStatsQuery) (TimeStats, error)` 把高度范围内的匹配交易按**区块时间**归入等长分段，返回每段计数与整窗口汇总。查询只读，可与摄取并发调用，每次调用都看到一个完整的链状态。

- `TimeStatsQuery`：`From`/`To` 为闭区间高度，`TxIDs` 为筛选标识集合，`Start`/`End` 为时间窗口边界（Unix 秒），`StepSeconds` 为每段秒数。
- `TimeBucket`：一个分段，字段为 `Start`、`End`、`TxCount`（匹配交易出现次数）、`DistinctTxIDs`（段内不同标识数）、`Blocks`（含匹配交易的区块数）。
- `TimeStats`：`Buckets` 按时间升序，`Totals` 为整窗口汇总，`FromHeight`/`ToHeight` 为解析后的高度边界，`MissingTimeBlocks` 为缺失时间区块数。

### 高度范围与时间窗口同时生效

- 高度两端包含在内：`From` 为 0 表示从高度 1 开始；`To` 为 0 或超过链顶时，取**本次调用**看到的链顶，解析结果体现在 `FromHeight`/`ToHeight`。
- 只有高度在范围内、**且**区块时间落在窗口内的交易才计数，两个条件缺一不可。
- 空链或起始高度超过链顶都是合法请求：仍返回覆盖整个窗口的分段，所有计数为零。

### 时间边界：包含起点、排除终点

- 时间是非负 Unix 秒；窗口与每段都是半开区间 `[Start, End)`：包含起点、排除终点。落在段边界上的时间属于以它为起点的那一段。
- 分段从请求的 `Start` 开始，每段长 `StepSeconds`，末段在 `End` 处截断，可能不足一个完整步长。
- 时间为 0 是合法时间，落入包含它的第一段；**没有时间（`Time` 为 `nil`）与时间为 0 不同**——没有时间的区块不进任何分段，只计入 `MissingTimeBlocks`。
- 区块时间允许随高度下降：仍按实际时间归入相应段，与高度顺序无关。
- 没有匹配交易的段也会返回，所有计数为零；分段永远覆盖整个请求窗口。

### 计数与去重

- `TxCount` 统计出现次数：同一标识在同一区块出现多次就计多次。
- `Blocks` 统计含匹配交易的区块数：同一块中匹配多次仍只贡献一个区块。
- 每段 `DistinctTxIDs` 在段内去重；整窗口 `Totals.DistinctTxIDs` 在整个窗口上**重新去重**，不能把各段数值相加（同一标识出现在多个段时会被重复计算）。
- `MissingTimeBlocks` 统计解析后高度范围内没有时间的区块数，**不按交易筛选**：即使区块内没有匹配交易也照常计入。

### 筛选语义

`TxIDs` 沿用分页查询的精确匹配规则：按字符串精确匹配，集合的顺序与重复项不影响结果（`["a","a"]` 与 `["a"]` 等价），空集合（`nil` 或空切片）表示不筛选。筛选只改变匹配计数（`TxCount`、`DistinctTxIDs`、`Blocks`），不改变 `MissingTimeBlocks`。

### 参数错误与无匹配

以下请求都返回 `ErrInvalidArgument`，且没有可用的统计结果：负高度、显式高度范围倒置（`To` 非 0 且小于 `From`）、时间为负、起点不小于终点、非正步长、分段数超过 `MaxTimeBuckets=10000`。用 `errors.Is` 识别该错误；无匹配数据属于正常结果，不是错误。

### 完整示例

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/timestats/main.go`](examples/timestats/main.go)：

```bash
go run ./examples/timestats
```

场景：六个区块，窗口 `[0, 2500)` 按 600 秒分段。高度 1 时间为 0 且块内 `a` 重复（`[a,b,a]`，计入第一段，出现 3 次、不同标识 2 个）；高度 2 时间 600 恰好落在段边界上（属于第二段，`a` 在另一时间段再次出现）；高度 4 时间 2500 等于窗口终点（被排除）；高度 5 没有时间（只计入缺失时间区块数）；高度 6 时间 1700 低于高度 4 的时间（仍按实际时间归入 `[1200, 1800)`）。末段 `[2400, 2500)` 不足一个完整步长，`[1800, 2400)` 没有匹配交易仍返回零计数。整窗口不同标识数为 3（`a`、`b`、`c` 重新去重），而各段数值相加会得到 6，二者不可混用。

```go
// 时间窗口统计（Index.QueryTimeStats）完整示例：分段边界、块内与跨段
// 重复标识、缺失时间区块、筛选语义、高度范围解析、空链与参数错误。
//
// 运行：go run ./examples/timestats
package main

import (
	"errors"
	"fmt"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func unix(sec int64) *int64 { return &sec }

func mustAppend(index *indexroom.Index, block indexroom.Block) {
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

func mustStats(index *indexroom.Index, query indexroom.TimeStatsQuery) indexroom.TimeStats {
	stats, err := index.QueryTimeStats(query)
	if err != nil {
		panic(err)
	}
	return stats
}

func printStats(title string, stats indexroom.TimeStats) {
	fmt.Println(title + "：")
	fmt.Printf("  高度范围 [%d, %d] 缺失时间区块数=%d\n",
		stats.FromHeight, stats.ToHeight, stats.MissingTimeBlocks)
	for _, bucket := range stats.Buckets {
		fmt.Printf("  段 [%d, %d)：TxCount=%d DistinctTxIDs=%d Blocks=%d\n",
			bucket.Start, bucket.End, bucket.TxCount, bucket.DistinctTxIDs, bucket.Blocks)
	}
	fmt.Printf("  整窗口汇总：TxCount=%d DistinctTxIDs=%d Blocks=%d\n",
		stats.Totals.TxCount, stats.Totals.DistinctTxIDs, stats.Totals.Blocks)
}

func main() {
	// 六个区块，各覆盖一种情形：
	//   高度 1：时间为 0（合法时间，与没有时间不同），块内标识 a 重复出现；
	//   高度 2：时间恰好落在段边界 600 上，a 在另一个时间段再次出现；
	//   高度 3：普通分段内部；
	//   高度 4：时间等于窗口终点 2500，被排除（终点不包含）；
	//   高度 5：没有时间，不进任何分段，只计入缺失时间区块数；
	//   高度 6：时间 1700 低于高度 4 的 2500，时间随高度下降，
	//           仍按实际时间归入 [1200, 1800)。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}, Time: unix(0)})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "c"}, Time: unix(600)})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b"}, Time: unix(1500)})
	mustAppend(index, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}, Time: unix(2500)})
	mustAppend(index, indexroom.Block{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"a", "b"}})
	mustAppend(index, indexroom.Block{Height: 6, Hash: "h6", Parent: "h5", Txs: []string{"c", "c"}, Time: unix(1700)})
	fmt.Printf("链顶高度 tip=%d\n\n", index.Tip)

	// 窗口 [0, 2500) 按 600 秒分段：包含起点、排除终点，分段从 0 开始，
	// 末段 [2400, 2500) 不足一个完整步长。From/To 留空表示整个链。
	query := indexroom.TimeStatsQuery{Start: 0, End: 2500, StepSeconds: 600}
	printStats("不筛选，全高度范围", mustStats(index, query))

	// 筛选标识 a：只改变匹配计数；缺失时间区块数仍按高度范围统计，
	// 不因高度 5 的区块是否含 a 而改变。集合的顺序与重复项不影响结果。
	filtered := mustStats(index, indexroom.TimeStatsQuery{Start: 0, End: 2500, StepSeconds: 600, TxIDs: []string{"a"}})
	printStats("筛选标识 [a]", filtered)
	filteredDup := mustStats(index, indexroom.TimeStatsQuery{Start: 0, End: 2500, StepSeconds: 600, TxIDs: []string{"a", "a"}})
	fmt.Printf("筛选 [a,a] 与 [a] 的整窗口汇总相同：%v\n\n", filteredDup.Totals == filtered.Totals)

	// 高度范围两端包含在内；显式 To 超过链顶时夹到本次调用看到的链顶。
	printStats("限定高度 [2, 4]", mustStats(index, indexroom.TimeStatsQuery{From: 2, To: 4, Start: 0, End: 2500, StepSeconds: 600}))
	clamped := mustStats(index, indexroom.TimeStatsQuery{To: 100, Start: 0, End: 2500, StepSeconds: 600})
	fmt.Printf("显式 To=100 超过链顶：解析为高度范围 [%d, %d]\n\n", clamped.FromHeight, clamped.ToHeight)

	// 空链或起始高度超过链顶都是合法请求：仍返回覆盖整个窗口的零计数分段。
	empty := indexroom.New()
	printStats("空链", mustStats(empty, query))
	printStats("起始高度 100 超过链顶", mustStats(index, indexroom.TimeStatsQuery{From: 100, Start: 0, End: 2500, StepSeconds: 600}))

	// 无匹配数据属于正常结果，不是错误。
	noMatch, err := index.QueryTimeStats(indexroom.TimeStatsQuery{Start: 0, End: 2500, StepSeconds: 600, TxIDs: []string{"zzz"}})
	fmt.Printf("筛选无匹配标识：err=%v TxCount=%d\n\n", err, noMatch.Totals.TxCount)

	// 参数错误都返回 ErrInvalidArgument，且没有可用的统计结果。
	check := func(name string, query indexroom.TimeStatsQuery) {
		_, err := index.QueryTimeStats(query)
		fmt.Printf("%s：ErrInvalidArgument=%v\n", name, errors.Is(err, indexroom.ErrInvalidArgument))
	}
	check("负起始高度", indexroom.TimeStatsQuery{From: -1, Start: 0, End: 2500, StepSeconds: 600})
	check("显式高度范围倒置", indexroom.TimeStatsQuery{From: 5, To: 2, Start: 0, End: 2500, StepSeconds: 600})
	check("负时间", indexroom.TimeStatsQuery{Start: -1, End: 2500, StepSeconds: 600})
	check("起点不小于终点", indexroom.TimeStatsQuery{Start: 2500, End: 2500, StepSeconds: 600})
	check("非正步长", indexroom.TimeStatsQuery{Start: 0, End: 2500, StepSeconds: 0})
	check("分段超过一万段", indexroom.TimeStatsQuery{Start: 0, End: 10001, StepSeconds: 1})
}
```

对应输出：

```text
链顶高度 tip=6

不筛选，全高度范围：
  高度范围 [1, 6] 缺失时间区块数=1
  段 [0, 600)：TxCount=3 DistinctTxIDs=2 Blocks=1
  段 [600, 1200)：TxCount=2 DistinctTxIDs=2 Blocks=1
  段 [1200, 1800)：TxCount=3 DistinctTxIDs=2 Blocks=2
  段 [1800, 2400)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [2400, 2500)：TxCount=0 DistinctTxIDs=0 Blocks=0
  整窗口汇总：TxCount=8 DistinctTxIDs=3 Blocks=4
筛选标识 [a]：
  高度范围 [1, 6] 缺失时间区块数=1
  段 [0, 600)：TxCount=2 DistinctTxIDs=1 Blocks=1
  段 [600, 1200)：TxCount=1 DistinctTxIDs=1 Blocks=1
  段 [1200, 1800)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [1800, 2400)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [2400, 2500)：TxCount=0 DistinctTxIDs=0 Blocks=0
  整窗口汇总：TxCount=3 DistinctTxIDs=1 Blocks=2
筛选 [a,a] 与 [a] 的整窗口汇总相同：true

限定高度 [2, 4]：
  高度范围 [2, 4] 缺失时间区块数=0
  段 [0, 600)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [600, 1200)：TxCount=2 DistinctTxIDs=2 Blocks=1
  段 [1200, 1800)：TxCount=1 DistinctTxIDs=1 Blocks=1
  段 [1800, 2400)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [2400, 2500)：TxCount=0 DistinctTxIDs=0 Blocks=0
  整窗口汇总：TxCount=3 DistinctTxIDs=3 Blocks=2
显式 To=100 超过链顶：解析为高度范围 [1, 6]

空链：
  高度范围 [1, 0] 缺失时间区块数=0
  段 [0, 600)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [600, 1200)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [1200, 1800)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [1800, 2400)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [2400, 2500)：TxCount=0 DistinctTxIDs=0 Blocks=0
  整窗口汇总：TxCount=0 DistinctTxIDs=0 Blocks=0
起始高度 100 超过链顶：
  高度范围 [100, 6] 缺失时间区块数=0
  段 [0, 600)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [600, 1200)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [1200, 1800)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [1800, 2400)：TxCount=0 DistinctTxIDs=0 Blocks=0
  段 [2400, 2500)：TxCount=0 DistinctTxIDs=0 Blocks=0
  整窗口汇总：TxCount=0 DistinctTxIDs=0 Blocks=0
筛选无匹配标识：err=<nil> TxCount=0

负起始高度：ErrInvalidArgument=true
显式高度范围倒置：ErrInvalidArgument=true
负时间：ErrInvalidArgument=true
起点不小于终点：ErrInvalidArgument=true
非正步长：ErrInvalidArgument=true
分段超过一万段：ErrInvalidArgument=true
```

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
