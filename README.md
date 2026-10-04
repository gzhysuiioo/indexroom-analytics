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
- `Index.Export` / `Index.Restore`：快照版本 1（全无时间时保持原字节）与版本 2（任一块有时间时为每块输出必填 `timestamp`，缺失为 `null`）。`Restore` 用一份完整快照整体替换主链，非法输入返回 `ErrInvalidSnapshot` 且不改变现有链。完整用法见下方[快照导出与恢复指南（Export/Restore）](#快照导出与恢复指南exportrestore)。

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

## 快照导出与恢复指南（Export/Restore）

`Index.Export(w)` 把当前主链写成一份 JSON 快照（`version`、`tip`、按高度升序的 `blocks`）；`Index.Restore(r)` 读取一份快照并用它**整体替换主链**。两个接口都只看到一个完整的链状态，且与查询、摄取可并发使用：恢复在读取和校验阶段不持锁，索引全程可查询、可追加。

### Restore 是整体替换，不是合并

- 恢复成功后，索引中**恰好**是快照里的链：高度从 1 连续到快照最后一个区块，旧链的任何高度、哈希都不残留。
- 用较短的快照恢复后，链顶直接降到快照的末尾高度：旧链多出的区块被删除，被替换高度上的旧交易也一并消失，它们**不再出现在 `QueryTxs`/`QueryTimeStats` 的结果中，也不再出现在再次 `Export` 的内容里**。恢复普遍保证的是**数据一致**：再次导出的文档与输入快照描述同一条链（按解码后的值比较）；**逐字节一致只在输入是未经修改的本功能导出内容、且恢复后链内容未再改动时成立**，手写或重新排版过的快照不享受这个保证，详见下文[数据一致不等于文本一致](#数据一致不等于文本一致)。
- 快照第一个区块的 `parent` 只命名链起点，不需要在索引中存在；其余每个区块必须指向前一区块。

### 输入必须是一份完整快照

- `Restore` 的输入必须是**恰好一份**完整的 JSON 快照对象；对象结尾之后允许有空白（空格、制表符、换行）。
- 在一个对象后面拼接第二份文档（或任何其他非空白内容）会被拒绝，不能把两份快照当作一次输入。
- 全部区块在应用之前整体校验：高度从 1 连续递增、哈希非空且不重复、父链接依次相连等。即使错误出现在**最后一个区块**，前面的合法区块也不会被部分导入。

### 数据一致不等于文本一致

恢复按**解码后的 JSON 值**重建主链。合法快照可以自由选择不改变值的文本写法：对象字段的书写顺序、对象与数组内外的排版空白、字符串的等价转义写法（如把“甲”写成 `\u7532`）都不影响恢复结果；但区块顺序与交易列表顺序不是可自由排版的部分，始终按输入中的原序保留。再次 `Export` 输出的是唯一的**规范文本**（固定字段顺序、无多余空白、字符串采用统一转义写法），不会原样保留输入的文本形式。因此要区分两种一致性：

- **数据一致（普遍保证）**：恢复后再导出，区块高度、哈希、父链接、每笔交易标识（含空标识）、块内位置、重复出现、交易顺序，以及缺失时间与实际时间，都与输入快照**解码后的值**精确一致；`QueryTxs`/`QueryTimeStats` 同样按解码后的原值精确匹配。
- **文本一致（有条件的保证）**：只有输入是**未经修改的本功能导出内容**、且恢复后链内容没有再被改动时，再次导出才与输入逐字节一致——同一条链无论经历过什么恢复、重组历史，导出的字节永远相同。对手写或重新排版过的快照，应比较解码后的数据而不是字节。
- **允许换写法不等于放松校验**：字段仍必须合法且不得重复（同一对象内出现两个 `hash` 照样拒绝），未知字段仍被拒绝；一份输入仍只能是恰好一份快照，对象后追加第二份文档会以 `ErrInvalidSnapshot` 拒绝。任何拒绝都不改变已有主链。

下文的[文本写法示例](#文本写法示例不同字段顺序排版与转义)用一份交易列表为 `[甲, "", 甲]` 的单区块快照具体展示这些区别。

### 版本与时间语义：缺失时间不是 0

- **版本 1**：区块没有 `timestamp` 字段。恢复后这些区块的 `Block.Time` 为 `nil`，即**没有时间**；这不是"自动补 0"，再导出时仍是不带 `timestamp` 的版本 1（输入若为未经修改的本功能导出内容，则同时逐字节一致，见上节）。
- **版本 2**：每个区块都必须带 `timestamp`：`null` 表示**缺失时间**（恢复为 `nil`），数字 `0` 表示**实际时间为零**（Unix 秒 0，恢复为指向 0 的非空指针）。`null` 与 `0` 不能互换，二者在查询指纹中也被严格区分。
- 导出自动选版：所有区块都没有时间时保持版本 1；**只要任一区块带时间（包括时间为 0）**，整份文档就是版本 2，每个区块都输出 `timestamp`，缺失处为 `null`。

### 区分非法快照与读取输入的错误

- **快照非法**：JSON 损坏或截断、对象后有多余数据、版本未知、字段缺失/类型错误/未知/重复、高度不连续、哈希为空或重复、父链接断裂、版本 2 时间戳缺失或既非 `null` 也非非负整数等，统一返回可用 `errors.Is(err, indexroom.ErrInvalidSnapshot)` 识别的错误，具体原因附在错误信息中（例如指出断裂发生在哪个高度）。
- **读取输入失败**：底层 `io.Reader` 在读到完整文档前返回的错误（网络中断、存储故障等）**不是** `ErrInvalidSnapshot`，而是包装为 `indexroom: read snapshot: ...` 返回，**原始读取错误仍可用 `errors.Is` 识别**。
- 两种失败都不会改变现有链：链顶、区块、哈希以及此前发出的分页游标全部保持原状，可以继续使用恢复前那份数据。

### 恢复与分页游标

- **恢复失败**（快照被拒或读取出错）：链没有变化，失败前取得的游标仍可用于原链，按[分页查询的既有规则](#游标失效区分-errquerychanged-与-errinvalidargument)继续翻页。
- **恢复成功**：旧游标不是整体作废，而是遵循同一套固定范围规则继续校验——固定范围内的区块内容变了（即使只是时间变化），或链顶降到该查询固定上界以下，续查返回 `ErrQueryChanged` 且没有可用页结果；此时应**从空游标重新开始查询**，不要把新页拼到旧结果后面。若恢复后的链在固定范围内与首页完全相同且链顶仍覆盖该范围，游标仍可正常续查。

### 完整示例：整体替换、拒绝情形与游标

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/restore/main.go`](examples/restore/main.go)；字段顺序、排版与转义等文本写法问题见其后的[文本写法示例](#文本写法示例不同字段顺序排版与转义)：

```bash
go run ./examples/restore
```

场景：先准备一条 4 个区块的较长旧链（全部无时间，导出为版本 1）；再用 `Export` 从另一条 2 个区块的较短新链取得版本 2 快照——高度 1 时间为 0、高度 2 缺失时间（`timestamp` 分别为 `0` 和 `null`）。程序先展示拼接两份文档与**最后一个区块父哈希错误**两种输入被 `ErrInvalidSnapshot` 拒绝、链与游标保持原状（并演示读取途中故障保留原始读取错误）；再用合法快照恢复，输出恢复前后的链顶、交易查询与再次导出内容，可以直接看到旧链多出的高度 3、4 和被替换的交易已经消失，最后演示成功恢复后旧游标返回 `ErrQueryChanged`。

```go
// 快照恢复（Index.Export / Index.Restore）完整示例：用导出的较短新链快照
// 整体替换较长旧链，展示替换前后的链顶、交易查询与再次导出；再展示最后一个
// 区块父哈希错误的快照被整体拒绝（ErrInvalidSnapshot）、链与游标保持原状，
// 以及读取输入失败与非法快照的区分、恢复成功后旧游标返回 ErrQueryChanged。
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

// querySet 同时包含旧链独有标识、新链独有标识与两链共有的标识，
// 让一次查询就能看出哪些交易被替换。
var querySet = []string{"alpha", "OLD-4", "beta"}

func exportString(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// dumpState 打印当前链顶、固定筛选下的交易查询首页和再次导出内容，
// 每次调用都看到一个完整的链状态。
func dumpState(index *indexroom.Index) {
	fmt.Printf("  链顶 tip=%d\n", index.Tip)
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 4, TxIDs: querySet})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询高度 [1, 4] 内标识 %v（空游标首页，ToHeight=%d）：TotalMatches=%d MatchedBlocks=%d\n",
		querySet, page.ToHeight, page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf("    命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
	fmt.Printf("  再次导出：%s\n", exportString(index))
}

// failAfterReader 先送出 data，随后始终返回 err，模拟读取快照途中存储故障。
type failAfterReader struct {
	data []byte
	err  error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func main() {
	// 1. 预先准备一条较长的旧链（4 个区块，全部没有时间，导出为版本 1）。
	target := indexroom.New()
	mustAppend(target, indexroom.Block{Height: 1, Hash: "s1", Parent: "genesis", Txs: []string{"alpha", "OLD-1"}})
	mustAppend(target, indexroom.Block{Height: 2, Hash: "s2", Parent: "s1", Txs: []string{"alpha"}})
	mustAppend(target, indexroom.Block{Height: 3, Hash: "s3", Parent: "s2", Txs: []string{"OLD-3"}})
	mustAppend(target, indexroom.Block{Height: 4, Hash: "s4", Parent: "s3", Txs: []string{"OLD-4", "alpha"}})
	fmt.Println("操作 1：准备较长旧链（4 个区块）后的状态")
	oldExport := exportString(target)
	dumpState(target)
	fmt.Println()

	// 2. 用现有导出功能从另一条较短的新链取得快照（2 个区块）。
	//    高度 1 时间为 0（真实时间戳），高度 2 没有时间；只要有一个区块带时间，
	//    导出即为版本 2，每块都带 timestamp：0 与 null 同时出现。
	source := indexroom.New()
	mustAppend(source, indexroom.Block{Height: 1, Hash: "n1", Parent: "genesis", Txs: []string{"alpha"}, Time: unix(0)})
	mustAppend(source, indexroom.Block{Height: 2, Hash: "n2", Parent: "n1", Txs: []string{"beta"}})
	snapshot := exportString(source)
	fmt.Printf("操作 2：从较短新链（2 个区块）Export 出快照\n  %s\n\n", snapshot)

	// 3. 在旧链上取得两个分页游标（同一固定范围 [1, 4]、同一筛选，每页 2 条），
	//    分别留给“恢复被拒绝”和“恢复成功”之后续查。
	query := indexroom.TxQuery{From: 1, To: 4, TxIDs: querySet, PageSize: 2}
	firstA, err := target.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	firstB, err := target.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	cursorAfterReject := firstA.NextCursor
	cursorAfterRestore := firstB.NextCursor
	fmt.Printf("操作 3：旧链固定范围 [1, 4] 首页（每页 2 条）命中：")
	for _, hit := range firstA.Hits {
		fmt.Printf("%d/%s ", hit.Height, hit.TxID)
	}
	fmt.Printf("剩余记录由游标续查（游标不透明，不展示内容）\n\n")

	// 4a. 拒绝情形之一：把两份快照文档拼在一起不是合法输入，
	//     只允许一份完整 JSON 文档（结尾可以有空白）。
	err = target.Restore(strings.NewReader(snapshot + snapshot))
	fmt.Printf("操作 4a：拼接两份文档后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v，链顶仍为 tip=%d\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), target.Tip)

	// 4b. 拒绝情形之二：最后一个区块的父哈希被改错。前一个区块完全合法，
	//     但整份快照必须在应用前全部校验通过，因此不会有任何部分导入。
	broken := strings.Replace(snapshot, `"parent":"n1"`, `"parent":"WRONG-PARENT"`, 1)
	err = target.Restore(strings.NewReader(broken))
	fmt.Printf("操作 4b：最后一个区块父哈希错误后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v（具体原因已在错误信息中指出）\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Println("拒绝后的状态（应与操作 1 完全一致，不能出现新链区块）：")
	dumpState(target)
	fmt.Printf("  导出内容与恢复前逐字节一致：%v\n", exportString(target) == oldExport)
	// 恢复失败前取得的游标仍可用于原链，继续读出旧链高度 4 的两条记录。
	cont, err := target.QueryTxs(indexroom.TxQuery{
		From: 1, To: 4, TxIDs: querySet, PageSize: 2, Cursor: cursorAfterReject,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  旧游标续查成功（err=%v），本页命中：", err)
	for _, hit := range cont.Hits {
		fmt.Printf("%d/%s ", hit.Height, hit.TxID)
	}
	fmt.Println()
	fmt.Println()

	// 4c. 区分读取输入发生的错误：读到一半底层 reader 故障。
	//     原始读取错误仍可用 errors.Is 识别，它不是 ErrInvalidSnapshot；
	//     这种情况同样不改变现有链。
	readProblem := errors.New("simulated storage read failure")
	err = target.Restore(&failAfterReader{data: []byte(snapshot[:len(snapshot)/2]), err: readProblem})
	fmt.Printf("操作 4c：读取途中故障后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, 读取错误)=%v errors.Is(err, ErrInvalidSnapshot)=%v 链顶仍为 tip=%d\n\n",
		errors.Is(err, readProblem), errors.Is(err, indexroom.ErrInvalidSnapshot), target.Tip)

	// 5. 成功恢复：输入是一份完整 JSON 快照，结尾允许空白（这里追加一个换行）。
	//    Restore 用快照整体替换主链，不与旧链合并。
	err = target.Restore(strings.NewReader(snapshot + "\n"))
	fmt.Printf("操作 5：用合法短链快照（结尾带换行）Restore：err=%v\n", err)
	fmt.Println("成功恢复后的状态（旧链多出的高度 3、4 与被替换的交易应全部消失）：")
	dumpState(target)
	fmt.Printf("  再次导出与操作 2 的快照逐字节一致：%v\n", exportString(target) == snapshot)
	// 旧游标遵循既有固定范围规则：固定上界为 4，链顶已降到 2，续查返回
	// ErrQueryChanged 而不是部分数据；应从空游标重新开始查询。
	_, err = target.QueryTxs(indexroom.TxQuery{
		From: 1, To: 4, TxIDs: querySet, PageSize: 2, Cursor: cursorAfterRestore,
	})
	fmt.Printf("  旧游标续查：errors.Is(err, ErrQueryChanged)=%v errors.Is(err, ErrInvalidArgument)=%v\n",
		errors.Is(err, indexroom.ErrQueryChanged), errors.Is(err, indexroom.ErrInvalidArgument))
	restart, err := target.QueryTxs(indexroom.TxQuery{From: 1, To: 4, TxIDs: querySet, PageSize: 2})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  从空游标重新开始：ToHeight=%d 命中：", restart.ToHeight)
	for _, hit := range restart.Hits {
		fmt.Printf("%d/%s ", hit.Height, hit.TxID)
	}
	fmt.Printf("有后续游标=%v\n", restart.NextCursor != "")
}
```

对应输出（`go run ./examples/restore` 的实际输出，每次运行逐字一致）：

```text
操作 1：准备较长旧链（4 个区块）后的状态
  链顶 tip=4
  查询高度 [1, 4] 内标识 [alpha OLD-4 beta]（空游标首页，ToHeight=4）：TotalMatches=4 MatchedBlocks=3
    命中 height=1 block=s1 tx="alpha" position=0
    命中 height=2 block=s2 tx="alpha" position=0
    命中 height=4 block=s4 tx="OLD-4" position=0
    命中 height=4 block=s4 tx="alpha" position=1
  再次导出：{"version":1,"tip":4,"blocks":[{"height":1,"hash":"s1","parent":"genesis","txs":["alpha","OLD-1"]},{"height":2,"hash":"s2","parent":"s1","txs":["alpha"]},{"height":3,"hash":"s3","parent":"s2","txs":["OLD-3"]},{"height":4,"hash":"s4","parent":"s3","txs":["OLD-4","alpha"]}]}

操作 2：从较短新链（2 个区块）Export 出快照
  {"version":2,"tip":2,"blocks":[{"height":1,"hash":"n1","parent":"genesis","txs":["alpha"],"timestamp":0},{"height":2,"hash":"n2","parent":"n1","txs":["beta"],"timestamp":null}]}

操作 3：旧链固定范围 [1, 4] 首页（每页 2 条）命中：1/alpha 2/alpha 剩余记录由游标续查（游标不透明，不展示内容）

操作 4a：拼接两份文档后 Restore：err=indexroom: invalid snapshot: trailing data after the snapshot object
  errors.Is(err, ErrInvalidSnapshot)=true，链顶仍为 tip=4

操作 4b：最后一个区块父哈希错误后 Restore：err=indexroom: invalid snapshot: block at height 2 does not link to its parent
  errors.Is(err, ErrInvalidSnapshot)=true（具体原因已在错误信息中指出）
拒绝后的状态（应与操作 1 完全一致，不能出现新链区块）：
  链顶 tip=4
  查询高度 [1, 4] 内标识 [alpha OLD-4 beta]（空游标首页，ToHeight=4）：TotalMatches=4 MatchedBlocks=3
    命中 height=1 block=s1 tx="alpha" position=0
    命中 height=2 block=s2 tx="alpha" position=0
    命中 height=4 block=s4 tx="OLD-4" position=0
    命中 height=4 block=s4 tx="alpha" position=1
  再次导出：{"version":1,"tip":4,"blocks":[{"height":1,"hash":"s1","parent":"genesis","txs":["alpha","OLD-1"]},{"height":2,"hash":"s2","parent":"s1","txs":["alpha"]},{"height":3,"hash":"s3","parent":"s2","txs":["OLD-3"]},{"height":4,"hash":"s4","parent":"s3","txs":["OLD-4","alpha"]}]}
  导出内容与恢复前逐字节一致：true
  旧游标续查成功（err=<nil>），本页命中：4/OLD-4 4/alpha 

操作 4c：读取途中故障后 Restore：err=indexroom: read snapshot: field "blocks": simulated storage read failure
  errors.Is(err, 读取错误)=true errors.Is(err, ErrInvalidSnapshot)=false 链顶仍为 tip=4

操作 5：用合法短链快照（结尾带换行）Restore：err=<nil>
成功恢复后的状态（旧链多出的高度 3、4 与被替换的交易应全部消失）：
  链顶 tip=2
  查询高度 [1, 4] 内标识 [alpha OLD-4 beta]（空游标首页，ToHeight=2）：TotalMatches=2 MatchedBlocks=2
    命中 height=1 block=n1 tx="alpha" position=0
    命中 height=2 block=n2 tx="beta" position=0
  再次导出：{"version":2,"tip":2,"blocks":[{"height":1,"hash":"n1","parent":"genesis","txs":["alpha"],"timestamp":0},{"height":2,"hash":"n2","parent":"n1","txs":["beta"],"timestamp":null}]}
  再次导出与操作 2 的快照逐字节一致：true
  旧游标续查：errors.Is(err, ErrQueryChanged)=true errors.Is(err, ErrInvalidArgument)=false
  从空游标重新开始：ToHeight=2 命中：1/alpha 2/beta 有后续游标=false
```

### 文本写法示例：不同字段顺序、排版与转义

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/restoreshape/main.go`](examples/restoreshape/main.go)：

```bash
go run ./examples/restoreshape
```

场景：同一个单区块、交易列表为 `[甲, "", 甲]` 的版本 2 快照，分别以 `timestamp` 为 `null` 和数字 `0` 的两份**手写输入**恢复。两份输入都打乱了顶层与区块对象的字段顺序、加入排版空白，并把块内位置 0 的“甲”写成等价的 Unicode 转义 `\u7532`（位置 2 仍写字面“甲”）。程序依次展示：输入文本、恢复后的查询结果与再次导出文本；在同一个包含零秒的窗口 `[0, 60)` 上对比缺失时间与零秒的统计区别；再用本功能自己导出的规范字节演示逐字节一致保证成立的条件；最后演示重复字段、对象后追加第二份文档仍被 `ErrInvalidSnapshot` 拒绝且已有主链保持原状。

```go
// 快照文本写法示例（Index.Restore / Export / QueryTxs / QueryTimeStats）：
// 同一份单区块快照在输入里换用不同的对象字段顺序、排版空白与 Unicode 转义，
// 恢复后数据（交易标识、空标识、重复出现、块内位置、顺序）按解码后的值精确
// 保留，但再次导出的是规范文本，不与输入逐字节一致；再用 timestamp 分别为
// null 与 0 的两份输入演示版本随“区块是否带时间”变化，以及同一时间窗口下
// 缺失时间区块与零秒区块的统计区别。最后演示等价写法不等于放松校验：重复
// 字段、在对象后追加第二份文档仍被 ErrInvalidSnapshot 拒绝，已有主链保持原状。
//
// 运行：go run ./examples/restoreshape
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// inputMissing 是手写的版本 2 快照：顶层字段与区块字段都打乱了顺序，
// 排版加入了空格与换行，第一笔“甲”写成等价的 Unicode 转义（\u7532，
// 即 U+7532），第三笔“甲”仍是字面字符；交易列表为 [甲, "", 甲]（中间是空标识）。
// timestamp 为 null：表示这个区块缺失时间。
const inputMissing = `{
  "version" : 2 ,
  "tip" : 1,
  "blocks" : [
    {
      "txs" : [ "\u7532",  "" , "甲" ],
      "parent" : "genesis",
      "timestamp" : null,
      "hash" : "b1",
      "height" : 1
    }
  ]
}`

// inputZero 描述同一条链、同一批交易，但排版紧凑、字段顺序再次不同，
// 唯一的语义差别是 timestamp 为数字 0：真实的零秒时间，而不是缺失时间。
const inputZero = `{"blocks":[{"timestamp":0,"txs":["甲","","甲"],"height":1,"hash":"b1","parent":"genesis"}],"version":2,"tip":1}`

func exportString(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// dumpRestored 展示恢复后的链顶、区块时间、交易查询结果与再次导出文本。
func dumpRestored(index *indexroom.Index) {
	block := index.Blocks[1]
	fmt.Printf("  链顶 tip=%d；高度 1 区块 hash=%s parent=%s\n", index.Tip, block.Hash, block.Parent)
	if block.Time == nil {
		fmt.Println("  区块时间：缺失（Block.Time 为 nil）")
	} else {
		fmt.Printf("  区块时间：真实时间戳 %d 秒（Block.Time 非空，指向 %d）\n", *block.Time, *block.Time)
	}

	// 不筛选：块内三笔交易全部按位置升序返回，空标识与重复出现都保留。
	all, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 1})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  不筛选查询高度 1（TotalMatches=%d）：\n", all.TotalMatches)
	for _, hit := range all.Hits {
		fmt.Printf("    position=%d tx=%q\n", hit.Position, hit.TxID)
	}

	// 精确筛选“甲”：转义写法在解码后与字面“甲”是同一个字符串，
	// 两次出现分别位于块内位置 0 和 2；空标识不匹配。
	jia, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 1, TxIDs: []string{"甲"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  精确筛选 %q：TotalMatches=%d MatchedBlocks=%d，位置", "甲", jia.TotalMatches, jia.MatchedBlocks)
	for _, hit := range jia.Hits {
		fmt.Printf(" %d", hit.Position)
	}
	fmt.Println()

	fmt.Printf("  再次导出：%s\n", exportString(index))
}

// printWindowStats 在同一个包含零秒的窗口 [0, 60) 上打印统计，
// 便于对比缺失时间区块与零秒区块的区别。
func printWindowStats(index *indexroom.Index) {
	stats, err := index.QueryTimeStats(indexroom.TimeStatsQuery{Start: 0, End: 60, StepSeconds: 60})
	if err != nil {
		panic(err)
	}
	b := stats.Buckets[0]
	fmt.Printf("  同一窗口 [0, 60)：段 [0, 60) 交易出现=%d 不同标识=%d 含交易区块=%d；"+
		"整窗口汇总 交易出现=%d 不同标识=%d 区块=%d；缺失时间区块=%d\n",
		b.TxCount, b.DistinctTxIDs, b.Blocks,
		stats.Totals.TxCount, stats.Totals.DistinctTxIDs, stats.Totals.Blocks,
		stats.MissingTimeBlocks)
}

func main() {
	// 1. 恢复 timestamp 为 null 的手写快照：恢复得到缺失时间。
	fmt.Println("输入 1（版本 2，timestamp 为 null；字段乱序、带排版空白、含 \\u7532 转义）：")
	fmt.Println(inputMissing)
	idx := indexroom.New()
	if err := idx.Restore(strings.NewReader(inputMissing)); err != nil {
		panic(err)
	}
	canonicalV1 := exportString(idx)
	fmt.Println("恢复 1 成功（err=<nil>）后的查询与再次导出：")
	dumpRestored(idx)
	fmt.Printf("  再次导出与输入文本逐字节相等：%v（文本形式不同，但数据一致）\n", canonicalV1 == inputMissing)
	// 所有区块都缺失时间：导出自动选为版本 1，区块不含 timestamp 字段。
	// 这不是“丢失了时间”——输入的 null 本来就表示没有时间。
	printWindowStats(idx)
	fmt.Println("  （缺失时间区块不进入任何时间段，只计入缺失时间区块数）")
	fmt.Println()

	// 2. 恢复同链但 timestamp 为 0 的快照：恢复得到真实的零秒时间。
	fmt.Println("输入 2（版本 2，timestamp 为 0；紧凑排版、字段顺序再次不同）：")
	fmt.Println(inputZero)
	if err := idx.Restore(strings.NewReader(inputZero)); err != nil {
		panic(err)
	}
	canonicalV2 := exportString(idx)
	fmt.Println("恢复 2 成功（err=<nil>）后的查询与再次导出：")
	dumpRestored(idx)
	fmt.Printf("  再次导出与输入文本逐字节相等：%v（排版与转义被规范化，数据不变）\n", canonicalV2 == inputZero)
	fmt.Println("  版本随区块是否带时间变化：输入 1 的 null 恢复为缺失时间，再导出是不带 timestamp 的版本 1；")
	fmt.Println("  输入 2 的 0 恢复为真实零秒，再导出仍是版本 2 且 timestamp 仍为 0。null 与 0 不可互换。")
	printWindowStats(idx)
	fmt.Println("  （零秒是真实时间戳，区块落入段 [0, 60)，三笔交易出现全部计入）")
	fmt.Println()

	// 3. 逐字节一致保证的范围：只有未经修改的本功能导出内容，在链内容不变时
	//    恢复后再次导出才逐字节一致。把手写输入换成自己导出的规范字节演示一次。
	if err := idx.Restore(strings.NewReader(canonicalV2)); err != nil {
		panic(err)
	}
	fmt.Printf("用本功能自己导出的字节恢复，链内容不变时再次导出逐字节一致：%v\n\n",
		exportString(idx) == canonicalV2)

	// 4. 允许换文本写法，不等于放松校验：字段仍须合法且不得重复。
	//    下面的区块对象写了两个 hash 字段（其余内容合法），仍被整体拒绝。
	duplicateField := `{"version":2,"tip":1,"blocks":[` +
		`{"height":1,"hash":"b1","parent":"genesis","txs":["甲","","甲"],"timestamp":0,"hash":"b1"}]}`
	err := idx.Restore(strings.NewReader(duplicateField))
	fmt.Printf("区块对象内重复 hash 字段后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v\n", errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Printf("  已有主链保持原状：tip=%d，再次导出仍为版本 2 规范文本：%v\n\n",
		idx.Tip, exportString(idx) == canonicalV2)

	// 5. 一份输入只能是恰好一份快照：对象后追加第二份文档（中间可有空白）
	//    会以 ErrInvalidSnapshot 拒绝，已恢复的主链不受影响。
	twoDocs := inputZero + "   " + inputMissing
	err = idx.Restore(strings.NewReader(twoDocs))
	fmt.Printf("对象后追加第二份文档后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v\n", errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Printf("  已有主链保持原状：tip=%d，零秒区块仍在，再次导出未改变：%v\n",
		idx.Tip, exportString(idx) == canonicalV2)
}
```

对应输出（`go run ./examples/restoreshape` 的实际输出，每次运行逐字一致）：

```text
输入 1（版本 2，timestamp 为 null；字段乱序、带排版空白、含 \u7532 转义）：
{
  "version" : 2 ,
  "tip" : 1,
  "blocks" : [
    {
      "txs" : [ "\u7532",  "" , "甲" ],
      "parent" : "genesis",
      "timestamp" : null,
      "hash" : "b1",
      "height" : 1
    }
  ]
}
恢复 1 成功（err=<nil>）后的查询与再次导出：
  链顶 tip=1；高度 1 区块 hash=b1 parent=genesis
  区块时间：缺失（Block.Time 为 nil）
  不筛选查询高度 1（TotalMatches=3）：
    position=0 tx="甲"
    position=1 tx=""
    position=2 tx="甲"
  精确筛选 "甲"：TotalMatches=2 MatchedBlocks=1，位置 0 2
  再次导出：{"version":1,"tip":1,"blocks":[{"height":1,"hash":"b1","parent":"genesis","txs":["甲","","甲"]}]}
  再次导出与输入文本逐字节相等：false（文本形式不同，但数据一致）
  同一窗口 [0, 60)：段 [0, 60) 交易出现=0 不同标识=0 含交易区块=0；整窗口汇总 交易出现=0 不同标识=0 区块=0；缺失时间区块=1
  （缺失时间区块不进入任何时间段，只计入缺失时间区块数）

输入 2（版本 2，timestamp 为 0；紧凑排版、字段顺序再次不同）：
{"blocks":[{"timestamp":0,"txs":["甲","","甲"],"height":1,"hash":"b1","parent":"genesis"}],"version":2,"tip":1}
恢复 2 成功（err=<nil>）后的查询与再次导出：
  链顶 tip=1；高度 1 区块 hash=b1 parent=genesis
  区块时间：真实时间戳 0 秒（Block.Time 非空，指向 0）
  不筛选查询高度 1（TotalMatches=3）：
    position=0 tx="甲"
    position=1 tx=""
    position=2 tx="甲"
  精确筛选 "甲"：TotalMatches=2 MatchedBlocks=1，位置 0 2
  再次导出：{"version":2,"tip":1,"blocks":[{"height":1,"hash":"b1","parent":"genesis","txs":["甲","","甲"],"timestamp":0}]}
  再次导出与输入文本逐字节相等：false（排版与转义被规范化，数据不变）
  版本随区块是否带时间变化：输入 1 的 null 恢复为缺失时间，再导出是不带 timestamp 的版本 1；
  输入 2 的 0 恢复为真实零秒，再导出仍是版本 2 且 timestamp 仍为 0。null 与 0 不可互换。
  同一窗口 [0, 60)：段 [0, 60) 交易出现=3 不同标识=2 含交易区块=1；整窗口汇总 交易出现=3 不同标识=2 区块=1；缺失时间区块=0
  （零秒是真实时间戳，区块落入段 [0, 60)，三笔交易出现全部计入）

用本功能自己导出的字节恢复，链内容不变时再次导出逐字节一致：true

区块对象内重复 hash 字段后 Restore：err=indexroom: invalid snapshot: duplicate block field "hash"
  errors.Is(err, ErrInvalidSnapshot)=true
  已有主链保持原状：tip=1，再次导出仍为版本 2 规范文本：true

对象后追加第二份文档后 Restore：err=indexroom: invalid snapshot: trailing data after the snapshot object
  errors.Is(err, ErrInvalidSnapshot)=true
  已有主链保持原状：tip=1，零秒区块仍在，再次导出未改变：true
```

输出要点：

- **文本变了，数据没变**：两份手写输入恢复后，不筛选查询都按块内位置 0、1、2 返回 `[甲, "", 甲]`——空标识、两次“甲”的重复出现与交易顺序全部保留；精确筛选“甲”命中位置 0 和 2 共 2 次，说明匹配按**解码后的原值**进行，转义写法与字面写法是同一个标识。对象字段可以换顺序，但区块与交易列表始终按原序保留。
- **版本由区块是否带时间决定，版本变化不是丢失时间**：`null` 恢复为缺失时间（`Block.Time == nil`），链上没有任何带时间的区块，再导出自动选为**版本 1**、区块不含 `timestamp`；`0` 恢复为真实的零秒时间（非空指针，指向 0），再导出仍是**版本 2** 且 `timestamp` 仍为 `0`。`null` 与 `0` 不是可互换的表示，版本 1 只是如实记录“整条链都没有时间”，并非时间被丢失。
- **同一窗口 `[0, 60)` 下的统计区别**：缺失时间区块不进入任何时间段（段内与整窗口计数全为 0），但计入 `缺失时间区块=1`；零秒区块真实落入段 `[0, 60)`，三笔交易出现贡献 `交易出现=3`、不同标识 2（“甲”去重后与空标识）、含交易区块 1，`缺失时间区块=0`。
- **逐字节一致的边界**：两份手写输入再次导出都与输入字节不同（比较结果为 `false`）；改用本功能自己导出的规范字节恢复、链内容不变时，再次导出逐字节一致（`true`）。
- **校验不放松**：区块对象内重复 `hash` 字段、对象后拼接第二份文档都返回 `ErrInvalidSnapshot`；拒绝后链顶仍为 1、零秒区块仍在，再次导出与拒绝前完全相同，已有主链保持原状。

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
