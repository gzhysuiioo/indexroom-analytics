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
- `Index.QueryTimeStats`：按 `[Start, End)` 半开窗口与 `StepSeconds` 分段统计交易出现次数、不同标识数、含匹配交易的区块数，并给出整窗口去重汇总与缺失时间区块数；非法参数返回 `ErrInvalidArgument`。完整用法见下方[按时间窗口统计交易指南](#按时间窗口统计交易指南querytimestats)。
- `Index.Export` / `Index.Restore`：快照版本 1（全无时间时保持原字节）与版本 2（任一块有时间时为每块输出必填 `timestamp`，缺失为 `null`）；`Restore` 用快照整体替换主链，非法快照与读取错误都不改变现有链。恢复到已有索引的完整用法见下方[快照恢复指南](#快照恢复指南export--restore)。

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

## 按时间窗口统计交易指南（QueryTimeStats）

`Index.QueryTimeStats(query TimeStatsQuery) (TimeStats, error)` 按**区块时间**把主链上高度范围内的匹配交易归入等长时间段，返回每段计数与整窗口汇总。查询只读，可与摄取并发调用，每次调用都看到一个完整的链状态。

- `TimeStatsQuery`：`From`/`To` 为闭区间高度（语义与 `TxQuery` 相同），`TxIDs` 为筛选标识集合，`Start`/`End` 为时间窗口边界（Unix 秒），`StepSeconds` 为每段长度（秒）。
- `TimeBucket`：一个分段，字段为 `Start`/`End`（段边界）、`TxCount`（段内匹配交易出现次数）、`DistinctTxIDs`（段内不同匹配标识数）、`Blocks`（时间落入该段且含匹配交易的区块数）。
- `TimeStats`：`Buckets`（按时间升序，**始终覆盖整个请求窗口**）、`Totals`（整窗口汇总）、`FromHeight`/`ToHeight`（本次调用解析后的闭区间高度边界）、`MissingTimeBlocks`（范围内无时间的区块数）。

### 时间边界与分段规则

- 时间是非负 Unix 秒；**窗口与每段都包含起点、排除终点**，即 `[Start, End)`。
- 分段从请求起点 `Start` 开始，每段长 `StepSeconds`，**末段截到请求终点 `End` 为止**，因此末段可能不足一个完整步长。
- 区块时间正好等于某段起点时归入该段；正好等于窗口终点 `End` 时被排除。
- **时间为 0 是真实时间戳，与"没有时间"（`Time` 为 `nil`）严格不同**：前者按数值参与归段（可能因落在窗口之外而不计入任何段），后者只计入 `MissingTimeBlocks`。
- 区块时间**允许随高度下降**，仍按实际时间归入相应段；没有匹配交易的段也会返回，各项计数为零。

### 计数口径

- `TxCount` 按出现次数计：同一区块内重复出现的标识各计一次。
- `DistinctTxIDs` 在段内去重；`Blocks` 按区块计：**同一块中匹配多次仍只贡献一个区块**。
- `Totals.DistinctTxIDs` 在**整个窗口上重新去重**，不能把各段数值相加——同一标识出现在多个时间段时会被重复计算（下方示例中各段为 2+2+1+0=5，整窗口实际为 4）。`Totals.TxCount` 与 `Totals.Blocks` 则等于各段之和（每笔出现、每个区块只属于一个段）。
- `MissingTimeBlocks` 统计解析后高度范围内所有无时间的区块，**不看交易是否匹配筛选**。

### 高度范围与时间窗口同时生效

- `From`/`To` 为闭区间，两端都包含在内；只有同时落在高度范围与时间窗口内的区块才参与计数。
- `From` 为 0 表示从高度 1 开始；`To` 为 0 或超过链顶时，取**本次调用**看到的链顶，返回的 `ToHeight` 是夹取后的值。
- 空链或起始高度超过链顶都是**合法请求**：仍成功返回覆盖整个窗口的零计数分段（空链时 `ToHeight` 为 0）。

### 筛选语义

`TxIDs` 沿用分页查询的精确匹配规则：按原字符串精确比较（大小写与首尾空白有区分），**集合的顺序和重复项不影响结果**（`["a","a"]` 与 `["a"]` 等价），**空集合表示不筛选**。筛选只改变匹配计数；`MissingTimeBlocks` 仍按选定高度范围统计，不因交易是否匹配而减少。

### 参数错误与无匹配

以下请求返回 `ErrInvalidArgument`，且**没有可用的统计结果**：负高度、倒置的显式高度范围（显式 `To` 小于 `From`）、`Start` 或 `End` 为负、`Start` 不小于 `End`、`StepSeconds` 非正，以及分段数超过 `MaxTimeBuckets=10000` 的请求。用 `errors.Is(err, indexroom.ErrInvalidArgument)` 识别。注意区分：**没有匹配数据不是错误**，而是正常返回零计数结果。

### 完整示例

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/timestats/main.go`](examples/timestats/main.go)：

```bash
go run ./examples/timestats
```

场景：七个连续区块，交易与时间各不相同——高度 2 的块内标识 `a` 重复出现，`a` 还出现在另一时间段（高度 3）；高度 4 没有时间；高度 1 时间为 0（在窗口之前）；高度 5 的时间随高度下降；高度 6 的时间正好等于窗口终点。查询窗口 `[100, 320)` 按 60 秒分段，末段 `[280, 320)` 不足一个完整步长。

```go
// 按时间窗口统计交易（Index.QueryTimeStats）完整示例：分段边界、整窗口去重
// 汇总、缺失时间区块、高度范围、筛选语义、空链与 ErrInvalidArgument 的处理。
//
// 运行：go run ./examples/timestats
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

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间。
func unix(sec int64) *int64 { return &sec }

func printStats(title string, stats indexroom.TimeStats) {
	fmt.Println(title + "：")
	for _, b := range stats.Buckets {
		fmt.Printf("  段 [%d, %d)：交易出现=%d 不同标识=%d 含匹配区块=%d\n",
			b.Start, b.End, b.TxCount, b.DistinctTxIDs, b.Blocks)
	}
	fmt.Printf("  整窗口汇总：交易出现=%d 不同标识=%d 含匹配区块=%d\n",
		stats.Totals.TxCount, stats.Totals.DistinctTxIDs, stats.Totals.Blocks)
	fmt.Printf("  解析后高度范围=[%d, %d] 缺失时间区块=%d\n",
		stats.FromHeight, stats.ToHeight, stats.MissingTimeBlocks)
}

func main() {
	// 七个连续区块，覆盖各种时间情形：
	//   高度 1：时间为 0（真实时间戳，不是缺失），落在窗口之前；
	//   高度 2：时间 100，正好在窗口起点，块内标识 a 重复出现；
	//   高度 3：时间 160，正好在段边界上，a 在另一时间段再次出现；
	//   高度 4：没有时间；
	//   高度 5：时间 150，随高度下降，仍按实际时间归入第一段；
	//   高度 6：时间 320，正好是窗口终点，被排除；
	//   高度 7：时间 225，落入第三段。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"x"}, Time: unix(0)})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "b", "a"}, Time: unix(100)})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "c"}, Time: unix(160)})
	mustAppend(index, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}})
	mustAppend(index, indexroom.Block{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"b"}, Time: unix(150)})
	mustAppend(index, indexroom.Block{Height: 6, Hash: "h6", Parent: "h5", Txs: []string{"a"}, Time: unix(320)})
	mustAppend(index, indexroom.Block{Height: 7, Hash: "h7", Parent: "h6", Txs: []string{"d"}, Time: unix(225)})
	fmt.Printf("链顶高度 tip=%d\n\n", index.Tip)

	// 窗口 [100, 320) 按 60 秒分段：分段从起点 100 开始，末段 [280, 320)
	// 不足一个完整步长，截到窗口终点为止。窗口与每段都包含起点、排除终点。
	// From 留空表示从高度 1 开始，To 留空表示取本次调用看到的链顶。
	window := indexroom.TimeStatsQuery{Start: 100, End: 320, StepSeconds: 60}
	stats, err := index.QueryTimeStats(window)
	if err != nil {
		panic(err)
	}
	printStats("不筛选，窗口 [100, 320) 步长 60", stats)
	// 高度 1 时间为 0，落在窗口之前，不计入任何段，也不算缺失时间；
	// 高度 6 时间等于窗口终点 320，被排除；高度 4 没有时间，只计入缺失数。
	fmt.Println("  （高度 1 时间为 0 在窗口之前、高度 6 时间在窗口终点，均不入段；高度 4 无时间）")
	// 各段不同标识之和是 2+2+1+0=5，但整窗口汇总为 4：标识 a 同时出现在
	// 第一段和第二段，整窗口需要重新去重，不能把各段数值相加。
	// 高度 2 一块内 a 匹配两次，交易出现计 2 次，含匹配区块只计 1 个。
	fmt.Println()

	// 筛选标识 a：集合的顺序和重复项不影响结果，[a,a] 与 [a] 等价。
	// 筛选改变匹配计数，但缺失时间区块数仍按选定高度范围统计，不减少。
	filtered := window
	filtered.TxIDs = []string{"a", "a"}
	fstats, err := index.QueryTimeStats(filtered)
	if err != nil {
		panic(err)
	}
	printStats("筛选 [a,a]（等价于 [a]），同一窗口", fstats)
	fmt.Println()

	// 高度范围与时间窗口同时生效：From/To 为闭区间，只统计范围内区块。
	ranged := window
	ranged.From, ranged.To = 3, 5
	rstats, err := index.QueryTimeStats(ranged)
	if err != nil {
		panic(err)
	}
	printStats("限定高度 [3, 5]，同一窗口", rstats)
	fmt.Println()

	// To 为 0 或超过链顶时，都取本次调用看到的链顶。
	clamped := window
	clamped.To = 99
	cstats, err := index.QueryTimeStats(clamped)
	if err != nil {
		panic(err)
	}
	fmt.Printf("请求 To=99（链顶只有 %d）：解析后 ToHeight=%d\n\n", index.Tip, cstats.ToHeight)

	// 没有匹配的交易标识不是错误，而是正常返回零计数结果。
	none := window
	none.TxIDs = []string{"zzz"}
	nstats, err := index.QueryTimeStats(none)
	fmt.Printf("筛选 [zzz] 无匹配：err=%v 整窗口交易出现=%d\n\n", err, nstats.Totals.TxCount)

	// 空链上的合法请求同样成功，返回覆盖整个窗口的零计数分段。
	empty := indexroom.New()
	estats, err := empty.QueryTimeStats(window)
	if err != nil {
		panic(err)
	}
	printStats("空链，同一窗口", estats)
	fmt.Println()

	// 起始高度超过链顶也是合法请求，同样返回整窗口零计数分段。
	above := window
	above.From = 100
	astats, err := index.QueryTimeStats(above)
	if err != nil {
		panic(err)
	}
	fmt.Printf("From=100 超过链顶 %d：err=%v 段数=%d 解析后高度范围=[%d, %d] 整窗口交易出现=%d\n\n",
		index.Tip, err, len(astats.Buckets), astats.FromHeight, astats.ToHeight, astats.Totals.TxCount)

	// 参数类错误（ErrInvalidArgument）：没有可用的统计结果，用 errors.Is 识别。
	invalid := []struct {
		name  string
		query indexroom.TimeStatsQuery
	}{
		{"负起始高度", indexroom.TimeStatsQuery{From: -1, Start: 100, End: 320, StepSeconds: 60}},
		{"负结束高度", indexroom.TimeStatsQuery{To: -2, Start: 100, End: 320, StepSeconds: 60}},
		{"显式高度范围倒置", indexroom.TimeStatsQuery{From: 5, To: 3, Start: 100, End: 320, StepSeconds: 60}},
		{"负起点时间", indexroom.TimeStatsQuery{Start: -1, End: 320, StepSeconds: 60}},
		{"负终点时间", indexroom.TimeStatsQuery{Start: 100, End: -5, StepSeconds: 60}},
		{"起点时间不小于终点", indexroom.TimeStatsQuery{Start: 320, End: 320, StepSeconds: 60}},
		{"非正步长", indexroom.TimeStatsQuery{Start: 100, End: 320, StepSeconds: 0}},
		{"分段超过一万段", indexroom.TimeStatsQuery{Start: 0, End: 20000, StepSeconds: 1}},
	}
	for _, tc := range invalid {
		_, err := index.QueryTimeStats(tc.query)
		fmt.Printf("%s：ErrInvalidArgument=%v\n", tc.name, errors.Is(err, indexroom.ErrInvalidArgument))
	}
}
```

对应输出：

```text
链顶高度 tip=7

不筛选，窗口 [100, 320) 步长 60：
  段 [100, 160)：交易出现=4 不同标识=2 含匹配区块=2
  段 [160, 220)：交易出现=2 不同标识=2 含匹配区块=1
  段 [220, 280)：交易出现=1 不同标识=1 含匹配区块=1
  段 [280, 320)：交易出现=0 不同标识=0 含匹配区块=0
  整窗口汇总：交易出现=7 不同标识=4 含匹配区块=4
  解析后高度范围=[1, 7] 缺失时间区块=1
  （高度 1 时间为 0 在窗口之前、高度 6 时间在窗口终点，均不入段；高度 4 无时间）

筛选 [a,a]（等价于 [a]），同一窗口：
  段 [100, 160)：交易出现=2 不同标识=1 含匹配区块=1
  段 [160, 220)：交易出现=1 不同标识=1 含匹配区块=1
  段 [220, 280)：交易出现=0 不同标识=0 含匹配区块=0
  段 [280, 320)：交易出现=0 不同标识=0 含匹配区块=0
  整窗口汇总：交易出现=3 不同标识=1 含匹配区块=2
  解析后高度范围=[1, 7] 缺失时间区块=1

限定高度 [3, 5]，同一窗口：
  段 [100, 160)：交易出现=1 不同标识=1 含匹配区块=1
  段 [160, 220)：交易出现=2 不同标识=2 含匹配区块=1
  段 [220, 280)：交易出现=0 不同标识=0 含匹配区块=0
  段 [280, 320)：交易出现=0 不同标识=0 含匹配区块=0
  整窗口汇总：交易出现=3 不同标识=3 含匹配区块=2
  解析后高度范围=[3, 5] 缺失时间区块=1

请求 To=99（链顶只有 7）：解析后 ToHeight=7

筛选 [zzz] 无匹配：err=<nil> 整窗口交易出现=0

空链，同一窗口：
  段 [100, 160)：交易出现=0 不同标识=0 含匹配区块=0
  段 [160, 220)：交易出现=0 不同标识=0 含匹配区块=0
  段 [220, 280)：交易出现=0 不同标识=0 含匹配区块=0
  段 [280, 320)：交易出现=0 不同标识=0 含匹配区块=0
  整窗口汇总：交易出现=0 不同标识=0 含匹配区块=0
  解析后高度范围=[1, 0] 缺失时间区块=0

From=100 超过链顶 7：err=<nil> 段数=4 解析后高度范围=[100, 7] 整窗口交易出现=0

负起始高度：ErrInvalidArgument=true
负结束高度：ErrInvalidArgument=true
显式高度范围倒置：ErrInvalidArgument=true
负起点时间：ErrInvalidArgument=true
负终点时间：ErrInvalidArgument=true
起点时间不小于终点：ErrInvalidArgument=true
非正步长：ErrInvalidArgument=true
分段超过一万段：ErrInvalidArgument=true
```

## 快照恢复指南（Export / Restore）

`Index.Export(w io.Writer) error` 把当前主链写成一份 JSON 快照；`Index.Restore(r io.Reader) error` 读取一份快照并**整体替换**主链。两者都可与查询、摄取并发调用：导出看到的是某一完整链状态，恢复在读取与校验期间不阻塞索引，替换在校验全部通过后一次性生效。

### 恢复是整体替换，不是合并

- 恢复成功后，索引里**只有快照中的链**：链顶变为快照的 `tip`（即快照末尾区块的高度）。恢复一份较短的链时，链顶随之下降，旧链多出的区块全部消失。
- 被替换掉的区块与交易**不再出现在任何查询结果中，也不会出现在再次导出的内容里**；再次导出得到的是新链的字节，与刚恢复的快照一致。
- 替换是原子的：要么整条新链生效，要么现有链原样保留，不存在"部分导入"的中间状态。

### 输入格式：一份完整的 JSON 快照

- 输入必须是**恰好一份**快照对象，结尾允许跟随空白字符；拼接两份快照（或任何其他尾随数据）都会被拒绝。
- 快照通常由 `Export` 的字节原样提供；校验覆盖版本、字段（缺失、多余、重复、为 null 都拒绝）、高度从 1 连续递增、哈希非空且唯一、父哈希链接等，任何一处不符都整体拒绝。

### 两个版本的时间语义

- 版本 1（所有区块都没有时间时的原始布局）没有 `timestamp` 字段，恢复出的区块**没有时间**（`Time` 为 `nil`）。
- 版本 2 每个区块都有必填的 `timestamp`：`null` 表示缺失，`0` 表示真实时间为零。两者**不能互换**：把缺失写成 `0` 会让该块变成"有零时间"，把 `0` 写成 `null` 会丢失真实时间。
- 旧版快照缺失的时间**不会**在恢复时自动补零——恢复后仍然是缺失。

### 失败：非法快照与读取错误都不会改变现有链

- **非法快照**（`ErrInvalidSnapshot`，用 `errors.Is` 识别，错误信息带具体原因）：JSON 畸形或截断、对象后还有尾随数据、版本未知、字段缺失/多余/重复/为 null、高度不连续、哈希为空或重复、父哈希链接断裂、版本 2 的 `timestamp` 缺失或为负、标识含非法 UTF-8 等。即使快照前面的区块全部合法、只有最后一个区块出错，也**整体拒绝**，前面的合法区块不会被部分导入。
- **读取输入发生的错误**（如磁盘读取失败）：原样包装底层错误返回，`errors.Is` 仍能命中调用方自己的底层错误，且**不是** `ErrInvalidSnapshot`，借此可与非法快照区分。
- 两种失败都完全不改变现有链：链顶、查询结果、再次导出的内容与失败前一致，可继续放心使用原来的数据。

### 恢复与分页游标

- 恢复**失败**前取得的分页游标不受影响，仍可用于原链继续翻页。
- 恢复**成功**后，旧游标遵循既有固定范围规则：固定范围内的内容发生变化、或链顶低于固定上界时，续查返回 `ErrQueryChanged` 且没有可用页结果。此时应**从空游标重新开始查询**，不要把新结果拼接到旧结果后面。

### 完整示例

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/restore/main.go`](examples/restore/main.go)：

```bash
go run ./examples/restore
```

场景：先用 `Export` 取得一份较短新链（3 个区块，含一个零时间区块和一个缺失时间区块）的快照，再把它 `Restore` 到预先准备的较长旧链（5 个区块）中，对照恢复前后的链顶、交易查询与再次导出；随后演示父哈希错误的非法快照、读取输入失败，以及恢复前后分页游标的行为。

```go
// 快照导出与恢复（Index.Export / Index.Restore）完整示例：用现有导出功能
// 取得一份较短新链的快照（含缺失时间与零时间区块），整体替换预先准备的
// 较长旧链，对照恢复前后的链顶、交易查询与再次导出；演示最后一个区块父
// 哈希错误的非法快照与读取输入失败的区别（都不改变现有链），以及恢复
// 失败前与成功后分页游标的行为。
//
// 运行：go run ./examples/restore
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func mustAppend(index *indexroom.Index, block indexroom.Block) {
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间。
func unix(sec int64) *int64 { return &sec }

// export 把当前主链导出为一份快照并返回其字节内容。
func export(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// printQuery 打印一次交易查询的命中情况（单页足够放下全部命中）。
func printQuery(index *indexroom.Index, txID string) {
	page, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{txID}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询 %q：命中 %d 条", txID, len(page.Hits))
	for _, hit := range page.Hits {
		fmt.Printf(" height=%d", hit.Height)
	}
	fmt.Println()
}

// errDisk 模拟读取输入时发生的底层错误。
var errDisk = errors.New("disk read failed")

// failingReader 的 Read 总是返回 errDisk，模拟损坏的输入来源。
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errDisk }

func main() {
	// 预先准备的较长旧链：5 个区块，都没有时间；标识 old-a 出现在高度 1、3、5。
	old := indexroom.New()
	mustAppend(old, indexroom.Block{Height: 1, Hash: "o1", Parent: "genesis", Txs: []string{"old-a"}})
	mustAppend(old, indexroom.Block{Height: 2, Hash: "o2", Parent: "o1", Txs: []string{"old-b"}})
	mustAppend(old, indexroom.Block{Height: 3, Hash: "o3", Parent: "o2", Txs: []string{"old-a"}})
	mustAppend(old, indexroom.Block{Height: 4, Hash: "o4", Parent: "o3", Txs: []string{"old-c"}})
	mustAppend(old, indexroom.Block{Height: 5, Hash: "o5", Parent: "o4", Txs: []string{"old-a"}})

	fmt.Println("[1] 恢复前的旧链")
	fmt.Printf("  链顶 tip=%d\n", old.Tip)
	printQuery(old, "old-a")
	beforeExport := export(old)
	fmt.Printf("  导出（version=1，所有区块都没有时间）：%s\n", beforeExport)

	// 在任何恢复之前取得一个分页游标：每页 2 条，第 1 页读到高度 1、3。
	page1, err := old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"old-a"}, PageSize: 2})
	if err != nil {
		panic(err)
	}
	cursor := page1.NextCursor
	fmt.Printf("  翻页第 1 页命中 %d 条，取得续查游标（固定范围上界 ToHeight=%d）\n\n",
		len(page1.Hits), page1.ToHeight)

	// 较短的新链：3 个区块。高度 1 时间为 0（真实零时间），高度 2 没有时间，
	// 高度 3 时间为 1700000000。任一块有时间即导出为版本 2，每块都带 timestamp。
	fresh := indexroom.New()
	mustAppend(fresh, indexroom.Block{Height: 1, Hash: "n1", Parent: "genesis", Txs: []string{"new-a"}, Time: unix(0)})
	mustAppend(fresh, indexroom.Block{Height: 2, Hash: "n2", Parent: "n1", Txs: []string{"new-b"}})
	mustAppend(fresh, indexroom.Block{Height: 3, Hash: "n3", Parent: "n2", Txs: []string{"new-a"}, Time: unix(1700000000)})
	snapshot := export(fresh)
	fmt.Println("[2] 用现有导出功能取得较短新链的快照")
	fmt.Printf("  新链链顶 tip=%d\n", fresh.Tip)
	fmt.Printf("  快照（version=2）：%s\n", snapshot)
	fmt.Println("  高度 1 的 timestamp 为 0（真实零时间），高度 2 为 null（缺失），二者不能互换")
	fmt.Println()

	// 非法快照：把最后一个区块（高度 3）的父哈希改错。即使前两个区块完全
	// 合法，恢复也被整体拒绝，前面的区块不会被部分导入。
	bad := strings.Replace(snapshot, `"parent":"n2"`, `"parent":"nX"`, 1)
	fmt.Println("[3] 恢复一份最后一个区块父哈希错误的快照（被拒绝）")
	err = old.Restore(strings.NewReader(bad))
	fmt.Printf("  Restore 返回：%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v\n", errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Printf("  拒绝后链顶 tip=%d（不变）\n", old.Tip)
	printQuery(old, "old-a")
	fmt.Printf("  再次导出与恢复前一致：%v\n\n", export(old) == beforeExport)

	// 恢复失败前取得的游标仍可用于原链，继续读到高度 5 的 old-a。
	page2, err := old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"old-a"}, PageSize: 2, Cursor: cursor})
	if err != nil {
		panic(err)
	}
	fmt.Println("[4] 恢复失败前取得的游标仍可用于原链")
	fmt.Printf("  续查第 2 页：err=%v 命中 %d 条", err, len(page2.Hits))
	for _, hit := range page2.Hits {
		fmt.Printf(" height=%d", hit.Height)
	}
	fmt.Println()
	fmt.Println()

	// 读取输入发生的错误与非法快照不同：原样保留底层错误，可用 errors.Is
	// 命中调用方自己的错误；它同样不改变现有链。
	fmt.Println("[5] 读取输入失败（不是非法快照）")
	err = old.Restore(failingReader{})
	fmt.Printf("  Restore 返回：%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v，errors.Is(err, errDisk)=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), errors.Is(err, errDisk))
	fmt.Printf("  链仍未改变：tip=%d\n\n", old.Tip)

	// 成功恢复：快照整体替换主链，链顶从 5 降到快照末尾高度 3。
	fmt.Println("[6] 把新链快照恢复到旧链索引（成功）")
	if err := old.Restore(strings.NewReader(snapshot)); err != nil {
		panic(err)
	}
	fmt.Printf("  恢复后链顶 tip=%d（快照末尾高度，旧链多出的高度 4、5 已消失）\n", old.Tip)
	printQuery(old, "old-a")
	printQuery(old, "new-a")
	fmt.Printf("  高度 1 时间=%d（真实零时间），高度 2 时间缺失=%v\n",
		*old.Blocks[1].Time, old.Blocks[2].Time == nil)
	afterExport := export(old)
	fmt.Printf("  再次导出：%s\n", afterExport)
	fmt.Printf("  再次导出与恢复的快照逐字节一致：%v\n\n", afterExport == snapshot)

	// 恢复成功后，旧游标按固定范围规则失效：范围内数据已变，且链顶 3 低于
	// 固定上界 5，续查返回 ErrQueryChanged，应从空游标重新开始。
	fmt.Println("[7] 恢复成功后旧游标失效，应重新开始查询")
	_, err = old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"old-a"}, PageSize: 2, Cursor: cursor})
	fmt.Printf("  旧游标续查：ErrQueryChanged=%v\n", errors.Is(err, indexroom.ErrQueryChanged))
	restart, err := old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"new-a"}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  从空游标重新开始查询 %q：命中 %d 条", "new-a", len(restart.Hits))
	for _, hit := range restart.Hits {
		fmt.Printf(" height=%d", hit.Height)
	}
	fmt.Println()
	fmt.Println()

	// 输入必须是恰好一份快照对象：结尾允许空白，拼接两份文档会被拒绝。
	fmt.Println("[8] 输入必须是恰好一份完整快照")
	err = old.Restore(strings.NewReader(snapshot + "  \n"))
	fmt.Printf("  快照后跟随空白：err=%v（接受）\n", err)
	err = old.Restore(strings.NewReader(snapshot + "\n" + snapshot))
	fmt.Printf("  拼接两份快照：ErrInvalidSnapshot=%v，原因：%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), err)
}
```

对应输出：

```text
[1] 恢复前的旧链
  链顶 tip=5
  查询 "old-a"：命中 3 条 height=1 height=3 height=5
  导出（version=1，所有区块都没有时间）：{"version":1,"tip":5,"blocks":[{"height":1,"hash":"o1","parent":"genesis","txs":["old-a"]},{"height":2,"hash":"o2","parent":"o1","txs":["old-b"]},{"height":3,"hash":"o3","parent":"o2","txs":["old-a"]},{"height":4,"hash":"o4","parent":"o3","txs":["old-c"]},{"height":5,"hash":"o5","parent":"o4","txs":["old-a"]}]}
  翻页第 1 页命中 2 条，取得续查游标（固定范围上界 ToHeight=5）

[2] 用现有导出功能取得较短新链的快照
  新链链顶 tip=3
  快照（version=2）：{"version":2,"tip":3,"blocks":[{"height":1,"hash":"n1","parent":"genesis","txs":["new-a"],"timestamp":0},{"height":2,"hash":"n2","parent":"n1","txs":["new-b"],"timestamp":null},{"height":3,"hash":"n3","parent":"n2","txs":["new-a"],"timestamp":1700000000}]}
  高度 1 的 timestamp 为 0（真实零时间），高度 2 为 null（缺失），二者不能互换

[3] 恢复一份最后一个区块父哈希错误的快照（被拒绝）
  Restore 返回：indexroom: invalid snapshot: block at height 3 does not link to its parent
  errors.Is(err, ErrInvalidSnapshot)=true
  拒绝后链顶 tip=5（不变）
  查询 "old-a"：命中 3 条 height=1 height=3 height=5
  再次导出与恢复前一致：true

[4] 恢复失败前取得的游标仍可用于原链
  续查第 2 页：err=<nil> 命中 1 条 height=5

[5] 读取输入失败（不是非法快照）
  Restore 返回：indexroom: read snapshot: disk read failed
  errors.Is(err, ErrInvalidSnapshot)=false，errors.Is(err, errDisk)=true
  链仍未改变：tip=5

[6] 把新链快照恢复到旧链索引（成功）
  恢复后链顶 tip=3（快照末尾高度，旧链多出的高度 4、5 已消失）
  查询 "old-a"：命中 0 条
  查询 "new-a"：命中 2 条 height=1 height=3
  高度 1 时间=0（真实零时间），高度 2 时间缺失=true
  再次导出：{"version":2,"tip":3,"blocks":[{"height":1,"hash":"n1","parent":"genesis","txs":["new-a"],"timestamp":0},{"height":2,"hash":"n2","parent":"n1","txs":["new-b"],"timestamp":null},{"height":3,"hash":"n3","parent":"n2","txs":["new-a"],"timestamp":1700000000}]}
  再次导出与恢复的快照逐字节一致：true

[7] 恢复成功后旧游标失效，应重新开始查询
  旧游标续查：ErrQueryChanged=true
  从空游标重新开始查询 "new-a"：命中 2 条 height=1 height=3

[8] 输入必须是恰好一份完整快照
  快照后跟随空白：err=<nil>（接受）
  拼接两份快照：ErrInvalidSnapshot=true，原因：indexroom: invalid snapshot: trailing data after the snapshot object
```

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
