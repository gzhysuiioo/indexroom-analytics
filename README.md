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
- `Index.Append` / `Index.Reorg`：摄取区块与重组，负时间整体拒绝且不改变已有链；哈希、父哈希或任一交易标识含非法 UTF-8 字节时同样整体拒绝，错误信息指出区块高度与字段（交易标识另指出从 0 开始的位置），保证快照可无损导出与恢复。`Append` 在链顶前进后仍可再次提交内容完全相同的旧区块（成功且无副作用），完整规则见下方[重复提交区块指南（Append）](#重复提交区块指南append)；`Reorg` 的分支范围、丢弃高度口径、失败原子性与完整用法见下方[重组指南（Reorg）](#重组指南reorg)。
- `Index.QueryTxs`：既有分页交易查询，围绕一个或多个交易标识读取主链上的每次出现；默认按高度、再按块内位置升序返回，`Order` 设为 `OrderDesc` 时整体倒序（高度与块内位置均从大到小）；`TimeStart`/`TimeEnd` 可同时给出非负 Unix 秒，叠加 `[Start, End)` 半开时间窗口筛选（含起点、排除终点，与 `QueryTimeStats` 一致；缺失区块时间不命中，真实零秒按窗口判断），两者都不传表示不启用时间筛选；固定高度范围内的区块时间变化会使旧游标返回 `ErrQueryChanged`。完整翻页用法见下方[分页交易查询指南](#分页交易查询指南querytxs)。
- `Index.QueryTimeStats`：按 `[Start, End)` 半开窗口与 `StepSeconds` 分段统计交易出现次数、不同标识数、含匹配交易的区块数，并给出整窗口去重汇总与缺失时间区块数；非法参数返回 `ErrInvalidArgument`。完整用法见下方[按时间窗口统计交易指南](#按时间窗口统计交易指南querytimestats)。
- `Index.Export` / `Index.Restore`：快照版本 1（区块不含 `timestamp`）与版本 2（任一块有时间时为每块输出必填 `timestamp`，缺失为 `null`）。`Restore` 保证**数据一致**并把再导出规范化为本功能的固定文本，不保留输入的字段顺序、空白与转义写法；它用一份完整快照整体替换主链，非法输入返回 `ErrInvalidSnapshot` 且不改变现有链；底层读取失败（包括与完整快照字节一起送达的故障）不是 `ErrInvalidSnapshot`，而是包装为 `indexroom: read snapshot: ...` 并保留原始读取错误，同样不改变现有链。完整用法见下方[快照导出与恢复指南（Export/Restore）](#快照导出与恢复指南exportrestore)。

## 分页交易查询指南（QueryTxs）

`Index.QueryTxs(query TxQuery) (TxPage, error)` 围绕一个或多个交易标识读取这些标识在主链上的**每一次出现**。默认按"高度、再按块内位置"升序；`Order` 设为 `OrderDesc` 时在整个固定范围内倒序——高度从大到小，同一区块内的位置也从大到小。次序只由高度与原有块内位置决定，区块时间是否缺失、相同或随高度下降都不影响次序。查询只读，可与摄取并发调用，每次调用都看到一个完整的链状态。

- `TxQuery`：`From`/`To` 为闭区间高度，`TxIDs` 为筛选标识集合，`PageSize` 为每页条数（0 取 `DefaultPageSize=100`，最大 `MaxPageSize=1000`），`Order` 为读取方向（零值 `OrderAsc` 升序，`OrderDesc` 倒序），`TimeStart`/`TimeEnd` 为可选时间窗口（两个 `*int64`，同时给出才启用，半开 `[Start, End)`，单位为非负 Unix 秒），`Cursor` 为续查游标。
- `TxHit`：一次命中，字段为 `Height`、`BlockHash`、`TxID`、`Position`（块内从 0 开始的位置；**倒序不会重新编号**）。
- `TxPage`：本页 `Hits`，以及对整个固定范围的统计 `TotalMatches`（匹配出现总次数）、`MatchedBlocks`（含匹配交易的区块数）、`ToHeight`（第一页固定下来的实际上界）和 `NextCursor`。统计是整个固定范围的统计，**不随方向或页大小改变**。

### 如何取得第一页、继续读取、何时结束

1. **第一页**：`Cursor` 传空字符串即首次查询。`From` 为 0 时默认从高度 1 开始；`To` 为 0 时取首次查询看到的链顶。需要先看靠近链顶的记录时，把 `Order` 设为 `OrderDesc`（见下方[倒序读取](#倒序读取orderdesc)）。
2. **继续读取**：把上一页返回的 `NextCursor` 原样填回 `TxQuery.Cursor` 再次调用。游标是服务返回的**不透明字符串**，不要解析或拼接。续查时高度范围、`TxIDs`、读取方向 `Order` 以及是否启用时间窗口和窗口本身必须与第一页等价或一致；`PageSize` 可以逐页调整。
3. **结束**：返回页的 `NextCursor` 为空即最后一页。范围内没有匹配交易时不是错误，而是成功返回一个空页（`Hits` 为空、无游标）。

重复出现的交易**不会合并**：同一标识在不同区块、或同一区块内出现多次，就返回多条 `TxHit`。筛选按字符串精确匹配，`TxIDs` 的**顺序与重复项不影响匹配**（`["a","a"]` 与 `["a"]` 等价）；但大小写与首尾空白仍按原字符串区分（`"A"`、`" a "` 都不会匹配 `"a"`）。

### 范围在第一页固定

- 第一页若显式给出高于当前链顶的 `To`，返回的 `ToHeight` 是夹到链顶后的值；**后续请求仍要传原先请求的 `To`，不能用返回值替换**（例如首查 `To=10`、链顶只有 3 时 `ToHeight=3`，续查仍须传 `To=10`）。
- 范围固定后，之后新追加的区块对本次翻页不可见：第一页之后再追加含匹配标识的区块，继续翻页仍只读取原来固定的范围，`TotalMatches`、`MatchedBlocks` 也不增加。需要新数据就以空游标重新开始一次查询。

### 游标失效：区分 ErrQueryChanged 与 ErrInvalidArgument

- **`ErrQueryChanged`（链数据变了）**：固定范围内任一区块的内容发生改变——即使区块哈希不变、**只改变了时间**——或链顶退到固定结束高度以下，续查都会返回该错误，且**没有可用的页结果**。此时只能**从空游标重新开始第一页**，绝不能把新结果拼接到旧结果后面。
- **`ErrInvalidArgument`（请求本身不合法）**：续查更改高度范围或筛选集合、**续查方向与游标不一致**（正序游标配 `OrderDesc`，或倒序游标配 `OrderAsc`/不传 `Order`）、**续查改变时间筛选的启用状态或窗口任一边界**、只给时间窗口一个边界、时间边界为负、起点不小于终点、传入非法 `Order` 值、游标损坏、或把游标交给另一个索引实例（游标带实例签名，跨实例无效）。它与链数据变化无关，用 `errors.Is` 与 `ErrQueryChanged` 区分；请先修正请求再重试。方向或窗口不一致的续查同样**不返回任何可用页结果**。

### 倒序读取（OrderDesc）

- **选择倒序**：第一页把 `TxQuery.Order` 设为 `indexroom.OrderDesc`。不设置（零值 `OrderAsc`）时行为与过去完全一致，原有正序调用与有效游标都不受影响。
- **整体次序**：倒序作用于**整个固定范围**，不是只翻转当前页。命中按高度从大到小返回，同一区块内的 `Position` 也从大到小；逐页拼接后仍符合这一整体次序。每条命中的 `Position` 仍是它在区块内从 0 开始的真实位置，**不会因为倒序重新编号**；同一标识的每次出现也**不会合并**。
- **继续翻页**：续查必须沿用第一页的读取方向，继续把 `NextCursor` 原样传回；`PageSize` 仍可逐页调整，未读记录既不重复也不遗漏。
- **切换方向**：不能拿一个方向的游标去续查另一个方向——会得到 `ErrInvalidArgument`。要改读另一方向，**以空游标重新发起一次第一页查询**（此时会按当时的链顶重新固定范围）。
- **范围固定与统计**：第一页照旧固定实际上界，随后追加的更高区块不能插进进行中的倒序翻页；要读取这些新记录同样需要空游标重查。`TotalMatches`、`MatchedBlocks`、`ToHeight` 仍是整个固定范围的统计，与方向、页大小无关。
- **空结果**：空链、起始高度超过链顶、或筛选无命中时，倒序也成功返回空页与空的后续游标。

例如高度 1 至 3 的交易依次是 `[a,b,a]`、`[a,c]`、`[b,a]`，筛选 `a`、每页 2 条时：第一页依次给出**高度 3 位置 1、高度 2 位置 0**，第二页依次给出**高度 1 位置 2、高度 1 位置 0**，随后结束翻页；两页的 `TotalMatches` 都是 4、`MatchedBlocks` 都是 3。

### 时间窗口筛选（TimeStart/TimeEnd）

- **启用方式**：`TxQuery.TimeStart` 与 `TxQuery.TimeEnd` 同时给出非 `nil` 的 `*int64`，即按**半开窗口 `[Start, End)`** 筛选区块时间（非负 Unix 秒，**包含起点、排除终点**），语义与 `QueryTimeStats` 一致。两个字段都不传（均为 `nil`）时**不启用**时间筛选，行为与过去完全一致，原有调用与有效游标不受影响。
- **未启用与起点为零是两回事**：调用方必须能区分"没有时间筛选"（两个指针都为 `nil`）与"窗口起点恰好为零"（`TimeStart` 指向 0、`TimeEnd` 指向更大的值）。只给一个边界（只给起点或只给终点）是 `ErrInvalidArgument`。
- **缺失时间不命中，真实零秒按窗口判断**：启用窗口后，`Time` 为 `nil`（没有时间）的区块**永不命中**；时间为 0 是真实时间戳，按数值与窗口比较（例如窗口 `[0,1)` 会保留它）。这与摄取、快照中"缺失时间不是 0"的区分一致。
- **三类条件共同生效**：一次出现必须**同时**落在高度范围、时间窗口内、且标识通过 `TxIDs` 筛选才被保留。时间窗口只决定哪些出现被保留，**不改变读取次序**——结果仍按高度与原有块内位置正序或倒序读取，不能改成按时间排序；同一标识的重复出现既**不合并也不重新编号**，`Position` 仍是块内从 0 开始的真实位置。时间允许随高度下降、允许相等，均按实际值判断。
- **统计口径**：`TotalMatches` 与 `MatchedBlocks` 描述**固定高度范围内通过全部条件**（标识 + 时间窗口）的出现次数与区块数，每页保持一致；`ToHeight` 仍表示第一页固定下来的高度上界，与是否启用窗口无关。合法窗口没有命中时不是错误：成功返回空页（`Hits` 为空、无后续游标）。
- **参数错误**：只提供一个边界、起点或终点为负、起点不小于终点，都返回 `ErrInvalidArgument` 且**不提供可用页**。
- **继续翻页沿用同一窗口**：续查必须沿用"是否启用时间筛选"及完全相同的窗口；在启用与不启用之间切换、或改变任一边界，都返回 `ErrInvalidArgument` 且没有可用页，调用方需**从空游标重新查询**。`PageSize` 仍可逐页调整。
- **范围变化判定不变**：固定范围内**任一**区块内容变化（包括原本因时间不在窗口内而未命中的区块、缺失时间变成已知时间等）续查仍返回 `ErrQueryChanged` 且没有可用页，必须从空游标重新开始，不能把新结果接到旧结果后面。

例如高度一至四的时间依次为 105、缺失、100、110，交易依次为 `[a,b,a]`、`[a]`、`[a]`、`[a]`。筛选 `a` 与窗口 `[100,110)`、正序每页两条时：第一页返回**高度一的位置零和位置二**（时间 105 在窗口内；同一区块两次出现都保留），第二页返回**高度三的位置零**（时间 100 在窗口内）；高度二缺失时间不命中、高度四时间 110 正好等于被排除的终点。总出现次数 `TotalMatches=3`、匹配区块数 `MatchedBlocks=2`，高度上界 `ToHeight` 仍为 4。

### 完整示例

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/querytxs/main.go`](examples/querytxs/main.go)：

```bash
go run ./examples/querytxs
```

场景：三个连续区块的交易列表依次为 `[a,b,a]`、`[a]`、`[b]`，筛选标识 `a`，每页 2 条。第一页返回高度 1 中的两次 `a`（位置 0、2），第二页返回高度 2 中的一次 `a`（位置 0）；两页的 `TotalMatches` 都是 3、`MatchedBlocks` 都是 2，最后一页没有后续游标。

```go
// 分页交易查询（Index.QueryTxs）完整示例：第一页、继续翻页、范围固定、
// 筛选语义、空页、倒序读取、时间窗口筛选、ErrInvalidArgument 与
// ErrQueryChanged 的处理。
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

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间或时间窗口边界。
func unix(sec int64) *int64 { return &sec }

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

	demonstrateDescending()
	demonstrateTimeWindow()
}

// demonstrateTimeWindow 在独立索引上展示可选区块时间窗口：时间筛选与高度范围、
// 交易标识筛选共同生效；次序仍由高度与块内位置决定；缺失时间不命中、真实零秒
// 按窗口判断；续查必须沿用同一窗口，改变启用状态或任一边界都是
// ErrInvalidArgument；固定范围内原本因时间未命中的区块变化仍是 ErrQueryChanged。
func demonstrateTimeWindow() {
	fmt.Println("---- 时间窗口筛选（TimeStart/TimeEnd: [Start, End)）----")
	// 规格示例：高度 1 至 4 的时间依次为 105、缺失、100、110，交易依次为
	// [a,b,a]、[a]、[a]、[a]。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "w1", Parent: "genesis", Txs: []string{"a", "b", "a"}, Time: unix(105)})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "w2", Parent: "w1", Txs: []string{"a"}})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "w3", Parent: "w2", Txs: []string{"a"}, Time: unix(100)})
	mustAppend(index, indexroom.Block{Height: 4, Hash: "w4", Parent: "w3", Txs: []string{"a"}, Time: unix(110)})

	// 启用窗口：TimeStart 与 TimeEnd 同时给出，含起点、排除终点 [100,110)。
	// 高度 2 没有时间不命中；高度 4 时间正好等于终点 110 被排除。
	// 两个字段都留 nil 表示不启用筛选，这与起点恰好为零的窗口不同。
	query := indexroom.TxQuery{
		TxIDs: []string{"a"}, TimeStart: unix(100), TimeEnd: unix(110), PageSize: 2,
	}
	page1, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("窗口 [100,110) 筛选 a，每页 2 条：第1页", page1)

	// 继续翻页：是否启用窗口及窗口本身必须与第一页一致；PageSize 仍可调整。
	query.Cursor = page1.NextCursor
	query.PageSize = 10
	page2, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("同窗口继续（本页改每页 10 条）", page2)
	fmt.Println("  两页 TotalMatches 都是 3（高度1两次、高度3一次）、MatchedBlocks 都是 2、ToHeight 仍是 4")
	fmt.Println("  次序只按高度与块内位置：高度1时间105 仍排在高度3时间100 之前，没有改成按时间排序")
	fmt.Println()

	// 真实零秒是有效时间，缺失时间不是：窗口 [0,1) 只保留高度 1 那块
	// 真实时间为零的区块。
	zeroIndex := indexroom.New()
	mustAppend(zeroIndex, indexroom.Block{Height: 1, Hash: "z1", Parent: "genesis", Txs: []string{"a"}, Time: unix(0)})
	mustAppend(zeroIndex, indexroom.Block{Height: 2, Hash: "z2", Parent: "z1", Txs: []string{"a"}})
	zeroWin, err := zeroIndex.QueryTxs(indexroom.TxQuery{TimeStart: unix(0), TimeEnd: unix(1)})
	if err != nil {
		panic(err)
	}
	fmt.Printf("窗口 [0,1)：命中 %d 条（真实零秒命中，缺失时间不命中）；", zeroWin.TotalMatches)
	disabled, err := zeroIndex.QueryTxs(indexroom.TxQuery{})
	if err != nil {
		panic(err)
	}
	fmt.Printf("不启用窗口命中 %d 条（缺失时间也保留）\n", disabled.TotalMatches)

	// 合法窗口没有命中时仍是成功的空页，没有后续游标。
	empty, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, TimeStart: unix(1000), TimeEnd: unix(2000)})
	if err != nil {
		panic(err)
	}
	fmt.Printf("窗口 [1000,2000) 无命中：err=%v 命中=%d 有后续游标=%v\n\n",
		err, len(empty.Hits), empty.NextCursor != "")

	// 非法窗口：只给一个边界、边界为负、起点不小于终点，都是
	// ErrInvalidArgument，且没有可用页结果。
	invalid := []struct {
		name  string
		query indexroom.TxQuery
	}{
		{"只给起点", indexroom.TxQuery{TimeStart: unix(100)}},
		{"只给终点", indexroom.TxQuery{TimeEnd: unix(110)}},
		{"负起点", indexroom.TxQuery{TimeStart: unix(-1), TimeEnd: unix(110)}},
		{"负终点", indexroom.TxQuery{TimeStart: unix(0), TimeEnd: unix(-1)}},
		{"起点不小于终点", indexroom.TxQuery{TimeStart: unix(110), TimeEnd: unix(110)}},
	}
	for _, tc := range invalid {
		page, err := index.QueryTxs(tc.query)
		fmt.Printf("%s：ErrInvalidArgument=%v 可用命中数=%d\n",
			tc.name, errors.Is(err, indexroom.ErrInvalidArgument), len(page.Hits))
	}
	fmt.Println()

	// 续查改变窗口（含从启用改为不启用）返回 ErrInvalidArgument，没有可用页；
	// 要改窗口必须从空游标重新查询。
	first, err := index.QueryTxs(indexroom.TxQuery{
		TxIDs: []string{"a"}, TimeStart: unix(100), TimeEnd: unix(110), PageSize: 1,
	})
	if err != nil {
		panic(err)
	}
	changedEnd, err := index.QueryTxs(indexroom.TxQuery{
		TxIDs: []string{"a"}, TimeStart: unix(100), TimeEnd: unix(111),
		PageSize: 1, Cursor: first.NextCursor,
	})
	fmt.Printf("续查改终点：ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(err, indexroom.ErrInvalidArgument), len(changedEnd.Hits))
	disabledCont, err := index.QueryTxs(indexroom.TxQuery{
		TxIDs: []string{"a"}, PageSize: 1, Cursor: first.NextCursor,
	})
	fmt.Printf("续查去掉窗口：ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(err, indexroom.ErrInvalidArgument), len(disabledCont.Hits))

	// 固定范围内任一区块内容变化，包括原本因时间而未命中的区块（这里给
	// 缺失时间的高度 2 补上窗口外时间 120），续查仍是 ErrQueryChanged。
	if _, err := index.Reorg([]indexroom.Block{
		{Height: 2, Hash: "w2", Parent: "w1", Txs: []string{"a"}, Time: unix(120)},
		{Height: 3, Hash: "w3", Parent: "w2", Txs: []string{"a"}, Time: unix(100)},
		{Height: 4, Hash: "w4", Parent: "w3", Txs: []string{"a"}, Time: unix(110)},
	}); err != nil {
		panic(err)
	}
	changedPage, err := index.QueryTxs(indexroom.TxQuery{
		TxIDs: []string{"a"}, TimeStart: unix(100), TimeEnd: unix(110),
		PageSize: 1, Cursor: first.NextCursor,
	})
	fmt.Printf("原本未命中的高度2内容变化：ErrQueryChanged=%v 可用命中数=%d\n",
		errors.Is(err, indexroom.ErrQueryChanged), len(changedPage.Hits))
}

// demonstrateDescending 在独立索引上展示倒序读取：如何选择倒序、跨页继续、
// 范围在第一页固定、方向混用被拒绝，以及切换方向时需要重新发起查询。
func demonstrateDescending() {
	fmt.Println("---- 倒序读取（Order: indexroom.OrderDesc）----")
	// 规格示例：高度 1 至 3 的交易依次是 [a,b,a]、[a,c]、[b,a]，筛选 a。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "g1", Parent: "genesis", Txs: []string{"a", "b", "a"}})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "g2", Parent: "g1", Txs: []string{"a", "c"}})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "g3", Parent: "g2", Txs: []string{"b", "a"}})

	// 选择倒序：TxQuery.Order 传 indexroom.OrderDesc；不传（零值）仍是正序。
	query := indexroom.TxQuery{From: 1, To: 3, TxIDs: []string{"a"}, PageSize: 2, Order: indexroom.OrderDesc}
	page1, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("倒序第1页（从靠近链顶的出现开始）", page1)

	// 继续翻页：Order 必须沿用第一页的倒序，PageSize 仍可调整，游标原样传回。
	query.Cursor = page1.NextCursor
	page2, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("倒序第2页（继续向低高度读取，随后结束翻页）", page2)

	// 第一页固定实际上界：之后追加的更高区块不能插进正在进行的倒序翻页。
	mustAppend(index, indexroom.Block{Height: 4, Hash: "g4", Parent: "g3", Txs: []string{"a"}})
	fmt.Printf("已追加高度 4，当前链顶 tip=%d\n", index.Tip)
	query.Cursor = page1.NextCursor
	replayed, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	fmt.Printf("倒序续查仍只看固定范围 1..3：本页 %d 条、ToHeight=%d、TotalMatches=%d\n\n",
		len(replayed.Hits), replayed.ToHeight, replayed.TotalMatches)

	// 要读取新记录，以空游标重新查询；想换方向也一样，必须重新发起第一页。
	freshDesc, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, PageSize: 2, Order: indexroom.OrderDesc})
	if err != nil {
		panic(err)
	}
	printPage("空游标重新倒序查询（第一页固定到新链顶 4）", freshDesc)

	// 方向必须与游标一致：拿正序游标请求倒序、或拿倒序游标请求正序，
	// 都返回 ErrInvalidArgument，且没有可用页结果。
	asc, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, PageSize: 2})
	if err != nil {
		panic(err)
	}
	desc, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, PageSize: 2, Order: indexroom.OrderDesc})
	if err != nil {
		panic(err)
	}
	wrong1, err1 := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, PageSize: 2, Order: indexroom.OrderDesc, Cursor: asc.NextCursor})
	wrong2, err2 := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}, PageSize: 2, Cursor: desc.NextCursor})
	fmt.Printf("正序游标配倒序请求：ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(err1, indexroom.ErrInvalidArgument), len(wrong1.Hits))
	fmt.Printf("倒序游标配正序请求：ErrInvalidArgument=%v 可用命中数=%d\n\n",
		errors.Is(err2, indexroom.ErrInvalidArgument), len(wrong2.Hits))

	// 空链、起始高度超过链顶、筛选无命中：倒序同样成功返回空页与空游标。
	empty := indexroom.New()
	emptyPage, err := empty.QueryTxs(indexroom.TxQuery{Order: indexroom.OrderDesc})
	if err != nil {
		panic(err)
	}
	noHit, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"zzz"}, Order: indexroom.OrderDesc})
	if err != nil {
		panic(err)
	}
	fmt.Printf("空链倒序：err=%v 命中=%d 游标为空=%v；筛选无命中倒序：命中=%d TotalMatches=%d\n",
		err, len(emptyPage.Hits), emptyPage.NextCursor == "", len(noHit.Hits), noHit.TotalMatches)
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
---- 倒序读取（Order: indexroom.OrderDesc）----
倒序第1页（从靠近链顶的出现开始）：
  命中 height=3 block=g3 tx="a" position=1
  命中 height=2 block=g2 tx="a" position=0
  TotalMatches=4 MatchedBlocks=3 ToHeight=3 有后续游标=true
倒序第2页（继续向低高度读取，随后结束翻页）：
  命中 height=1 block=g1 tx="a" position=2
  命中 height=1 block=g1 tx="a" position=0
  TotalMatches=4 MatchedBlocks=3 ToHeight=3 有后续游标=false
已追加高度 4，当前链顶 tip=4
倒序续查仍只看固定范围 1..3：本页 2 条、ToHeight=3、TotalMatches=4

空游标重新倒序查询（第一页固定到新链顶 4）：
  命中 height=4 block=g4 tx="a" position=0
  命中 height=3 block=g3 tx="a" position=1
  TotalMatches=5 MatchedBlocks=4 ToHeight=4 有后续游标=true
正序游标配倒序请求：ErrInvalidArgument=true 可用命中数=0
倒序游标配正序请求：ErrInvalidArgument=true 可用命中数=0

空链倒序：err=<nil> 命中=0 游标为空=true；筛选无命中倒序：命中=0 TotalMatches=0
---- 时间窗口筛选（TimeStart/TimeEnd: [Start, End)）----
窗口 [100,110) 筛选 a，每页 2 条：第1页：
  命中 height=1 block=w1 tx="a" position=0
  命中 height=1 block=w1 tx="a" position=2
  TotalMatches=3 MatchedBlocks=2 ToHeight=4 有后续游标=true
同窗口继续（本页改每页 10 条）：
  命中 height=3 block=w3 tx="a" position=0
  TotalMatches=3 MatchedBlocks=2 ToHeight=4 有后续游标=false
  两页 TotalMatches 都是 3（高度1两次、高度3一次）、MatchedBlocks 都是 2、ToHeight 仍是 4
  次序只按高度与块内位置：高度1时间105 仍排在高度3时间100 之前，没有改成按时间排序

窗口 [0,1)：命中 1 条（真实零秒命中，缺失时间不命中）；不启用窗口命中 2 条（缺失时间也保留）
窗口 [1000,2000) 无命中：err=<nil> 命中=0 有后续游标=false

只给起点：ErrInvalidArgument=true 可用命中数=0
只给终点：ErrInvalidArgument=true 可用命中数=0
负起点：ErrInvalidArgument=true 可用命中数=0
负终点：ErrInvalidArgument=true 可用命中数=0
起点不小于终点：ErrInvalidArgument=true 可用命中数=0

续查改终点：ErrInvalidArgument=true 可用命中数=0
续查去掉窗口：ErrInvalidArgument=true 可用命中数=0
原本未命中的高度2内容变化：ErrQueryChanged=true 可用命中数=0
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

## 重复提交区块指南（Append）

`Index.Append(block Block) error` 只接受两种请求：**追加一个接在当前链顶之后的新区块**，或**再次提交一个与已索引区块内容完全相同的旧区块**（成功但什么都不改）。这两种情况要区分清楚——链顶已经前进后，旧区块仍可原样重发；但同一高度上内容不同的区块会被拒绝，不能靠再次 `Append` 覆盖。

### 再次提交已有区块：内容完全相同即成功，且不限于链顶

- 当提交区块的高度在主链上**已经存在**时，`Append` 把它与该高度的现存区块做**逐项内容比较**：高度、哈希、父哈希、按原次序排列的交易标识，以及**时间是否提供和具体数值**全部一致，才视为相同。
- 内容完全相同时调用**成功返回（`err == nil`）但是一次空操作**：链顶不推进、不覆盖任何字段、**不会新增交易出现记录**（`QueryTxs` 的命中次数与块内位置不变）。因此网络重试、超时后重发等场景可以直接重放同一个区块，无需先查询链状态。
- 这种重发**并不限于当前链顶的区块**：链顶前进到更高高度后，重新提交任意一个更早高度上的同内容区块，同样成功且无副作用。
- 比较口径的两个等价/不等价细节：
  - **交易列表未提供（`Txs` 为 `nil`）与空列表（长度为 0）等价**，互相重发仍算相同；
  - **时间未提供（`Time` 为 `nil`）与真实的零秒（`Time` 指向 0）不同**，只改这一点也不算相同。
- 交易标识按**原始内容**逐项、按原次序比较：块内的**重复项、大小写、首尾空白都属于原始内容**，不能为了判断相同而排序、去重或改写（例如 `[a,b,a]` 与 `[a,a,b]` 不同，`"a"` 与 `" A "` 不同）。

### 追加新区块：必须接在当前链顶之后

- 高度尚未索引时，区块高度必须恰好是**当前链顶加一**、父哈希必须等于当前链顶区块的哈希、区块哈希不能与已索引哈希重复，否则拒绝（空索引上第一个高度 1 区块的父哈希只是链起点标识，无需已索引）。
- **同一高度、同一哈希并不等于同一内容**：即使哈希相同，父哈希、交易列表次序或时间任一不一致，该高度已存在时仍以 `height already indexed with different content` 拒绝。`Append` 没有"覆盖已有高度"的用法。
- 需要替换已索引的链内容（含只改某个旧区块的交易或时间）时，不要反复 `Append`，应使用[重组指南（Reorg）](#重组指南reorg)；要用一份完整链整体替换（含高度 1）时，使用[快照导出与恢复指南（Export/Restore）](#快照导出与恢复指南exportrestore)。

### 拒绝错误不是 ErrInvalidArgument

`Append` 的普通拒绝（高度不接链顶、父哈希不匹配、同高度内容不同、哈希重复、负时间、非法 UTF-8 等）返回的是**普通非空错误**，不是查询参数错误 `ErrInvalidArgument`——后者只由 `QueryTxs`/`QueryTimeStats` 等查询接口用于标识非法查询参数。摄取调用方只需按"错误非空即本次提交未生效、索引未改变"处理，修正输入后重新提交即可；不要用 `errors.Is(err, ErrInvalidArgument)` 来判断一次 `Append` 是否被接受。

### 完整示例

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/resubmit/main.go`](examples/resubmit/main.go)：

```bash
go run ./examples/resubmit
```

场景：从空索引摄取两个连续区块——高度 1 的交易按序为 `[a,b,a]` 且**不提供时间**，高度 2 **不提供交易列表**并带**零秒时间**。随后再次提交高度 1（链顶已前进，它不是链顶区块），再以**显式空列表**再次提交高度 2：两次都成功返回，链顶始终为 2，`a` 仍只有高度 1 块内位置 0、2 的两次出现，没有新增记录。之后演示两种被拒绝的请求——仅把高度 1 的交易次序改为 `[a,a,b]`、仅把它的缺失时间改为零秒——程序打印拒绝原因，并核对原区块内容与 `a` 的查询结果仍保持原状（拒绝是预期分支，不会让程序提前退出）。最后演示不同内容不能覆盖已有高度 2，而接在链顶之后的高度 3 可以正常追加。

```go
// 区块摄取的重复提交（Index.Append）完整示例：区分“再次提交已有区块”与
// “追加新区块”。链顶前进之后，内容完全相同的旧区块仍可再次提交并成功返回
// （不推进链顶、不新增交易出现记录，且不限于当前链顶区块）；只要高度、哈希、
// 父哈希、按原次序排列的交易标识、时间是否提供及具体数值中有任一不同，就被
// 拒绝，已有高度的内容不能通过再次 Append 覆盖。示例还展示 Append 的普通
// 拒绝错误不是查询参数错误 ErrInvalidArgument，调用方按非空错误处理即可。
//
// 运行：go run ./examples/resubmit
package main

import (
	"errors"
	"fmt"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间。
func unix(sec int64) *int64 { return &sec }

// timeText 把区块时间渲染成便于阅读的文本：缺失时间为 "nil（缺失）"，
// 否则为实际 Unix 秒。
func timeText(b indexroom.Block) string {
	if b.Time == nil {
		return "nil（缺失）"
	}
	return fmt.Sprintf("%d", *b.Time)
}

// showBlock 打印某个高度上已索引区块的原始内容：交易列表（含重复项与次序）、
// 时间是否提供及取值。拒绝发生后用它核对内容仍保持原状。
func showBlock(index *indexroom.Index, height int64) {
	b := index.Blocks[height]
	fmt.Printf("  高度 %d 现存内容：hash=%s parent=%s txs=%q 时间=%s\n",
		height, b.Hash, b.Parent, b.Txs, timeText(b))
}

// showA 查询标识 a 在高度 [1,2] 的每次出现，打印块内位置与汇总计数。
func showA(index *indexroom.Index) {
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 2, TxIDs: []string{"a"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询 a：TotalMatches=%d MatchedBlocks=%d，命中",
		page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf(" height=%d/position=%d", hit.Height, hit.Position)
	}
	fmt.Println()
}

// tryAppend 提交一个区块并报告结果：成功时给出当前链顶；被拒绝时打印原因，
// 并说明它不是 ErrInvalidArgument。被拒绝是预期内的分支，不能让程序退出。
func tryAppend(title string, index *indexroom.Index, block indexroom.Block) {
	err := index.Append(block)
	fmt.Printf("%s：err=%v\n", title, err)
	if err != nil {
		fmt.Printf("  errors.Is(err, ErrInvalidArgument)=%v（Append 的普通拒绝不是查询参数错误，按非空错误处理即可）\n",
			errors.Is(err, indexroom.ErrInvalidArgument))
		return
	}
	fmt.Printf("  提交成功，当前链顶 tip=%d\n", index.Tip)
}

func main() {
	// 1. 从空索引摄取两个连续区块：
	//    高度 1：交易按序为 [a,b,a]（a 在块内出现两次），不提供时间；
	//    高度 2：不提供交易列表（Txs 为 nil，与空列表等价），带真实零秒时间。
	index := indexroom.New()
	if err := index.Append(indexroom.Block{
		Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"},
	}); err != nil {
		panic(err)
	}
	if err := index.Append(indexroom.Block{
		Height: 2, Hash: "h2", Parent: "h1", Txs: nil, Time: unix(0),
	}); err != nil {
		panic(err)
	}
	fmt.Printf("两个区块摄取完成，链顶 tip=%d\n", index.Tip)
	showBlock(index, 1)
	showBlock(index, 2)
	showA(index)
	fmt.Println()

	// 2. 链顶已经前进到高度 2。再次提交“旧”区块（高度 1）：内容与已索引
	//    区块逐项相同（同一高度、哈希、父哈希、按原次序的交易标识、时间缺失），
	//    成功返回但不推进链顶，也不新增任何交易出现记录。重复提交不限于
	//    当前链顶区块。
	tryAppend("再次提交高度 1（内容完全相同，且它不是当前链顶区块）", index,
		indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}})
	fmt.Printf("  链顶仍为 tip=%d（没有推进）\n", index.Tip)
	showA(index)
	fmt.Println()

	// 3. 再次提交高度 2，这次显式给出空交易列表：空列表与“未提供交易列表”
	//    在内容上等价，时间仍是真实零秒，所以仍是内容完全相同，成功且无副作用。
	tryAppend("再次提交高度 2（本次显式给空交易列表，与未提供等价；时间仍为零秒）", index,
		indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{}, Time: unix(0)})
	fmt.Printf("  链顶仍为 tip=%d；高度 2 现存交易列表长度=%d\n", index.Tip, len(index.Blocks[2].Txs))
	showA(index)
	fmt.Println()

	// 4. 被拒绝之一：只把高度 1 的交易次序从 [a,b,a] 改成 [a,a,b]。
	//    高度、哈希、父哈希、时间都相同也不够——交易标识必须按原次序逐项相同，
	//    不能为了判断相同而排序或去重。拒绝不改变任何已索引内容。
	tryAppend("再次提交高度 1，仅把交易次序改为 [a,a,b]", index,
		indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "a", "b"}})
	showBlock(index, 1)
	showA(index)
	fmt.Println()

	// 5. 被拒绝之二：只把高度 1 的缺失时间改成真实零秒。
	//    “未提供时间”（Time 为 nil）与时间为 0 是两种不同内容。
	zeroTime := indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}, Time: unix(0)}
	tryAppend("再次提交高度 1，仅把缺失时间改为零秒", index, zeroTime)
	showBlock(index, 1)
	showA(index)
	fmt.Println()

	// 6. 新区块必须接在当前链顶之后；已有高度不能靠再次 Append 覆盖。
	//    上面两次被拒绝的高度 1 仍保持原内容，随后可以正常追加高度 3。
	tryAppend("试图在已有高度 2 上追加一个不同内容的区块（覆盖已有高度）", index,
		indexroom.Block{Height: 2, Hash: "h2x", Parent: "h1", Txs: []string{"a"}, Time: unix(0)})
	tryAppend("追加接在链顶之后的新高度 3（正常前进）", index,
		indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: unix(30)})
	showBlock(index, 1)
	showA(index)
}
```

对应输出（`go run ./examples/resubmit` 的实际输出，每次运行逐字一致）：

```text
两个区块摄取完成，链顶 tip=2
  高度 1 现存内容：hash=h1 parent=genesis txs=["a" "b" "a"] 时间=nil（缺失）
  高度 2 现存内容：hash=h2 parent=h1 txs=[] 时间=0
  查询 a：TotalMatches=2 MatchedBlocks=1，命中 height=1/position=0 height=1/position=2

再次提交高度 1（内容完全相同，且它不是当前链顶区块）：err=<nil>
  提交成功，当前链顶 tip=2
  链顶仍为 tip=2（没有推进）
  查询 a：TotalMatches=2 MatchedBlocks=1，命中 height=1/position=0 height=1/position=2

再次提交高度 2（本次显式给空交易列表，与未提供等价；时间仍为零秒）：err=<nil>
  提交成功，当前链顶 tip=2
  链顶仍为 tip=2；高度 2 现存交易列表长度=0
  查询 a：TotalMatches=2 MatchedBlocks=1，命中 height=1/position=0 height=1/position=2

再次提交高度 1，仅把交易次序改为 [a,a,b]：err=height already indexed with different content
  errors.Is(err, ErrInvalidArgument)=false（Append 的普通拒绝不是查询参数错误，按非空错误处理即可）
  高度 1 现存内容：hash=h1 parent=genesis txs=["a" "b" "a"] 时间=nil（缺失）
  查询 a：TotalMatches=2 MatchedBlocks=1，命中 height=1/position=0 height=1/position=2

再次提交高度 1，仅把缺失时间改为零秒：err=height already indexed with different content
  errors.Is(err, ErrInvalidArgument)=false（Append 的普通拒绝不是查询参数错误，按非空错误处理即可）
  高度 1 现存内容：hash=h1 parent=genesis txs=["a" "b" "a"] 时间=nil（缺失）
  查询 a：TotalMatches=2 MatchedBlocks=1，命中 height=1/position=0 height=1/position=2

试图在已有高度 2 上追加一个不同内容的区块（覆盖已有高度）：err=height already indexed with different content
  errors.Is(err, ErrInvalidArgument)=false（Append 的普通拒绝不是查询参数错误，按非空错误处理即可）
追加接在链顶之后的新高度 3（正常前进）：err=<nil>
  提交成功，当前链顶 tip=3
  高度 1 现存内容：hash=h1 parent=genesis txs=["a" "b" "a"] 时间=nil（缺失）
  查询 a：TotalMatches=2 MatchedBlocks=1，命中 height=1/position=0 height=1/position=2
```

## 重组指南（Reorg）

`Index.Reorg(blocks []Block) (dropped []int64, err error)` 用提交的**分支**整体替换主链在某个已索引父区块之后的全部内容，并返回旧内容发生变化或被删除的高度（升序）。调用与查询、摄取、快照可并发，每次调用看到的都是一个完整的链状态；重组失败不会留下半截分支。

### 分支范围：父哈希必须已索引，父之后整体替换

准备 `Reorg` 输入时按下面的边界组织分支，这是最容易理解错的一点：

- **第一个新区块的父哈希（`blocks[0].Parent`）必须对应当前主链中已经索引的某个区块**。设该父区块高度为 `a`，则分支第一个区块的高度必须是 `a+1`。
- **分支内部高度连续、逐个连接**：从第二个区块起，每个区块高度必须是前一块高度加一，父哈希必须等于前一块的哈希；分支末尾区块的高度就是重组后的**新链顶**。
- **父区块及其之前的主链原样保留；父区块高度之后的内容由提交的分支整体替换**——无论主链原来在这段有多少区块，重组后这段恰好是分支里的区块。分支比旧链短时，多出的旧区块被删除，链顶随之降到分支末尾。
- 因此**不能只提交“变化的区块”然后指望旧链余下部分自动保留**：若新链顶之后还有旧区块要留下，就必须把它们（或其在新链上的对应区块）一并放进分支。要替换从高度 `a+1` 开始的一段，就提交从高度 `a+1` 到目标新链顶的完整后缀。
- **与首块摄取的区别**：空索引 `Append` 第一个区块（高度 1）时，它的 `Parent` 只是命名链起点的标识，**不需要也不可能**已索引。`Reorg` 没有这个豁免——即使分支首块高度写成 1，它的父哈希仍必须能在当前主链中找到，否则返回 `branch parent is unknown`。首块摄取允许未索引起点，**不代表**重组也能拿这个标识当父哈希去替换高度 1。想用全新内容替换整条链（含高度 1），应使用 [`Restore` 整体替换](#快照导出与恢复指南exportrestore)。

分支内哈希还需满足：哈希非空、分支内不重复；**父区块及其以下保留区间的哈希不能被分支复用**（冲突返回 `hash conflicts with a retained block`）；被替换后缀里的旧哈希则允许在分支中重新出现，甚至可以出现在与原来不同的高度。负时间、非法 UTF-8 等校验与 `Append` 一致。

### 丢弃高度（dropped）报的是什么

成功时返回的 `dropped` 是一个**按高度升序**的列表，口径是**父区块之后、旧内容发生变化或被删除的高度**：

- 旧链在 `(父高度, 旧链顶]` 内、新分支也覆盖到的高度，逐块比较该高度上的**旧区块与分支区块**（高度、哈希、父哈希、按序的交易列表、时间的有无与取值全部相同才算相同）；不同就把该高度列入。
- 旧链高出新链顶的高度（分支更短、旧区块被删除）全部列入。
- 提交的区块与旧块**完全相同**的高度不列入；重新提交当前已生效的同一条分支，返回空列表。

所以它**既不是“全部提交高度”，也不是“消失的哈希数量”**：分支里与旧块相同的高度不报；反过来，某个旧哈希即使在分支别的高度重新出现，原高度内容变了仍要报。特别注意**缺失时间与真实零秒是两种不同的区块内容**（`Time == nil` 与 `Time` 指向 0），只改这一点的高度也必须列入。

### 失败是整体拒绝，没有部分生效

整支分支在应用之前完整校验：空分支、父哈希未索引、首块高度不是父高度加一、分支高度不连续、某块父哈希不等于前一块哈希、哈希为空或分支内重复、复用保留区间哈希、负时间、非法 UTF-8 等，任何一项不过（哪怕错误出现在**最后一个区块**）都会：

- 返回非 nil 错误，并返回**空的**丢弃高度列表；
- 前面看似合法的区块**不会提前生效**：原链顶、各高度区块内容、哈希→高度对应关系全部保持原状（可用再次 `Export` 逐字节比对验证）。

本功能没有为重组错误导出专用哨兵，调用方把任何非 nil 返回当作“本次分支被整体拒绝、状态未变”处理即可，修正分支后重新提交。重组成功后，之前分页查询取得的游标仍按[既有固定范围规则](#游标失效区分-errquerychanged-与-errinvalidargument)校验：固定范围内区块变化（即使只改时间）或链顶降到固定上界以下，续查返回 `ErrQueryChanged`，此时应从空游标重新开始。

### 完整示例：缩短重组与父链接错误的整体拒绝

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/reorg/main.go`](examples/reorg/main.go)：

```bash
go run ./examples/reorg
```

场景分四步：先准备一条高度 1 到 4 的旧链（h1/h2/h4 有时间，h3 没有时间）；再以高度 1（h1）为父提交高度 2、3 的缩短分支——高度 2 与原区块完全相同，高度 3 保持原哈希 h3、父哈希 h2 与交易列表 `[t3]`，只把时间从缺失改为 0，分支在高度 3 结束。成功后链顶为 3、高度 4 不再存在，丢弃高度按升序为 `[3,4]`：高度 2 完全相同不报，高度 3 因“缺失时间→真实零秒”而内容不同必须报，高度 4 被删除必须报。第三步用一个**全新索引**从原来的四块链重新开始（避免与成功后的状态混淆），提交同一分支但把最后一个区块（高度 3）的父哈希改错：调用返回错误、丢弃高度为 `[]`，链顶仍为 4，区块内容、哈希对应关系与再次导出文本全部保持原状。最后一步演示首块摄取的未索引起点不能用作重组父哈希。

```go
// 重组（Index.Reorg）完整示例：从一条高度 1..4 的链出发，以已索引的
// 高度 1 为父区块（分支首块高度 2，h2 与原块完全相同；h3 保持原哈希、
// 父哈希与交易列表，只把时间从缺失改为 0）做一次缩短重组；再用一个全新的
// 索引从同样的四块链出发，演示同一分支最后一个区块父链接错误时整支分支被
// 拒绝、不报告丢弃高度、原链顶/区块内容/哈希对应关系全部保持原状；最后
// 区分“首块摄取允许未索引的链起点标识”与“重组父哈希必须已索引”。
//
// 运行：go run ./examples/reorg
package main

import (
	"bytes"
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

// timeText 把区块时间渲染成便于阅读的文本：缺失时间为 "nil（缺失）"，
// 否则为实际 Unix 秒。
func timeText(b indexroom.Block) string {
	if b.Time == nil {
		return "nil（缺失）"
	}
	return fmt.Sprintf("%d", *b.Time)
}

// heightsText 把丢弃高度列表渲染成 [3,4] 这样的文本（空列表为 []），
// 便于直接看到“按升序报告高度”，而不是 Go 默认的 [3 4]。
func heightsText(heights []int64) string {
	var sb bytes.Buffer
	sb.WriteByte('[')
	for i, h := range heights {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprint(&sb, h)
	}
	sb.WriteByte(']')
	return sb.String()
}

// dumpChain 按高度升序打印每个主链区块的完整内容（哈希、父哈希、交易、
// 时间），并给出链顶与哈希→高度对应表，用来观察操作后的主链。
func dumpChain(title string, index *indexroom.Index) {
	fmt.Println(title)
	for h := int64(1); h <= index.Tip; h++ {
		b := index.Blocks[h]
		fmt.Printf("  高度 %d：hash=%s parent=%s txs=%q 时间=%s\n",
			h, b.Hash, b.Parent, b.Txs, timeText(b))
	}
	fmt.Printf("  链顶 tip=%d；ByHash=%v\n", index.Tip, index.ByHash)
}

// exportText 返回当前主链的规范化导出文本，用于逐字节核对失败前后的链内容。
func exportText(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// newFourBlockChain 返回一条高度 1..4 的链，用于成功与失败两个场景。
// h1、h2、h4 有正常时间（链上只要有任一块带时间，再导出即为版本 2），
// h3 没有时间（这是成功重组要修改的唯一内容）。
func newFourBlockChain() *indexroom.Index {
	idx := indexroom.New()
	mustAppend(idx, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}, Time: unix(100)})
	mustAppend(idx, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: unix(200)})
	mustAppend(idx, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}})
	mustAppend(idx, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"t4"}, Time: unix(400)})
	return idx
}

// shorteningBranch 构造成功场景提交的分支：
//   - 第一个新区块（高度 2）的父哈希是 h1 —— 当前主链上已索引的高度 1；
//   - 高度 2 的块与原块完全相同（h2，时间 200，不进入丢弃列表）；
//   - 高度 3 保持原哈希 h3、父哈希 h2、交易 [t3]，只把时间从缺失改为 0；
//   - 分支在高度 3 结束，因此旧高度 4（h4）整体消失，新的链顶就是分支末尾的 h3。
func shorteningBranch() []indexroom.Block {
	return []indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: unix(200)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}, Time: unix(0)},
	}
}

// brokenBranch 构造失败场景提交的同一分支，但最后一个区块（高度 3）的
// 父哈希被改错：它应指向分支前一块的哈希 h2，却写成了未在分支中出现的
// "WRONG-PARENT"。
func brokenBranch() []indexroom.Block {
	return []indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: unix(200)},
		{Height: 3, Hash: "h3", Parent: "WRONG-PARENT", Txs: []string{"t3"}, Time: unix(0)},
	}
}

func main() {
	// 1. 成功场景：一条高度 1..4 的旧链。
	idx := newFourBlockChain()
	fmt.Println("操作 1：高度 1 到 4 的旧链")
	dumpChain("主链内容：", idx)
	fmt.Printf("  h3 的时间：%s —— 旧 h3 没有时间，这不是 0\n", timeText(idx.Blocks[3]))
	fmt.Println()

	// 2. 以高度 1（h1）为父提交缩短分支：高度 2 完全相同，高度 3 只改时间，
	//    分支在 3 结束，旧高度 4 被整体删除。Reorg 是“分支整体替换父之后
	//    全部主链内容”，不是只应用变化的块。
	dropped, err := idx.Reorg(shorteningBranch())
	if err != nil {
		panic(err)
	}
	fmt.Printf("操作 2：以高度 1（h1）为父提交高度 2、3 的缩短分支，Reorg 返回\n")
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  丢弃高度 dropped=%s（升序）\n", heightsText(dropped))
	fmt.Println("  解释：该列表报告旧内容发生变化或被删除的高度——")
	fmt.Println("  · 高度 2 提交的块与旧块完全相同，不列入；")
	fmt.Println("  · 高度 3 哈希/父哈希/交易都没变，但时间由缺失变成真实零秒，旧内容变了，必须列入；")
	fmt.Println("  · 高度 4 在分支覆盖范围之外，旧块被删除，必须列入；")
	fmt.Println("  · 它不是全部提交高度（提交的是 2、3），也不是消失的哈希数量（h4 只有一个）。")
	dumpChain("操作后的主链：", idx)
	fmt.Printf("  h3 现在的时间：%s —— 真实零秒，与缺失严格不同\n", timeText(idx.Blocks[3]))
	fmt.Printf("  高度 4 是否存在：%v；h4 是否还在哈希表：%v\n",
		idx.Blocks[4].Hash != "", idx.ByHash["h4"] != 0)
	page, err := idx.QueryTxs(indexroom.TxQuery{From: 1, To: 4})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询高度 [1, 4]：ToHeight=%d TotalMatches=%d MatchedBlocks=%d\n",
		page.ToHeight, page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf("    命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
	fmt.Println()

	// 3. 失败场景：用一个全新的索引从原来的四块链重新开始（避免与成功后的
	//    状态混淆）。同一分支，只是最后一个区块父链接错误。
	fresh := newFourBlockChain()
	before := exportText(fresh)
	fmt.Println("操作 3：新索引，仍是原来的高度 1..4 链（失败场景从这条链开始）")
	fmt.Printf("  失败前链顶 tip=%d；h3 时间=%s\n", fresh.Tip, timeText(fresh.Blocks[3]))
	badDropped, badErr := fresh.Reorg(brokenBranch())
	fmt.Printf("  提交最后一块父链接错误的分支：err=%v\n", badErr)
	fmt.Printf("  重组失败只表现为非 nil 错误（本功能未导出专用哨兵错误）\n")
	fmt.Printf("  返回的丢弃高度 dropped=%s —— 出错时不报告任何高度，前面合法的块也不会提前生效\n",
		heightsText(badDropped))
	fmt.Printf("  失败后链顶 tip=%d（应仍为 4）\n", fresh.Tip)
	fmt.Println("  失败后的主链（应与操作 1 的旧链完全一致：h3 仍无时间，h4 仍在）：")
	for h := int64(1); h <= fresh.Tip; h++ {
		b := fresh.Blocks[h]
		fmt.Printf("    高度 %d：hash=%s parent=%s txs=%q 时间=%s\n",
			h, b.Hash, b.Parent, b.Txs, timeText(b))
	}
	fmt.Printf("  ByHash=%v\n", fresh.ByHash)
	fmt.Printf("  失败前后导出逐字节一致：%v；h3 时间仍缺失=%v；h4 仍在哈希表=%v\n",
		exportText(fresh) == before, fresh.Blocks[3].Time == nil, fresh.ByHash["h4"] != 0)
	fmt.Println()

	// 4. 区分：空索引上的第一个块，父哈希只是链起点标识，不需要已索引；
	//    但 Reorg 的父哈希必须对应当前主链中已索引的区块。
	fmt.Println("操作 4：首块摄取的未索引起点 vs 重组的父哈希")
	first := indexroom.New()
	firstErr := first.Append(indexroom.Block{Height: 1, Hash: "b1", Parent: "unindexed-start", Txs: []string{"x"}})
	fmt.Printf("  空索引 Append 高度 1、parent=%q：err=%v（该标识只是链起点，无需已索引）\n",
		"unindexed-start", firstErr)
	_, reorgErr := first.Reorg([]indexroom.Block{
		{Height: 1, Hash: "r1", Parent: "unindexed-start", Txs: []string{"y"}},
	})
	fmt.Printf("  对同一个未索引标识发起 Reorg（试图从高度 1 整体替换）：err=%v\n", reorgErr)
	fmt.Println("  即：首块摄取允许用未索引的起点标识，不代表重组也能以这个标识为父哈希替换高度 1；")
	fmt.Println("  重组第一个新区块的父哈希必须是当前主链上已经索引的区块，分支从它的下一高度开始。")
}
```

对应输出（`go run ./examples/reorg` 的实际输出，每次运行逐字一致）：

```text
操作 1：高度 1 到 4 的旧链
主链内容：
  高度 1：hash=h1 parent=genesis txs=["t1"] 时间=100
  高度 2：hash=h2 parent=h1 txs=["t2"] 时间=200
  高度 3：hash=h3 parent=h2 txs=["t3"] 时间=nil（缺失）
  高度 4：hash=h4 parent=h3 txs=["t4"] 时间=400
  链顶 tip=4；ByHash=map[h1:1 h2:2 h3:3 h4:4]
  h3 的时间：nil（缺失） —— 旧 h3 没有时间，这不是 0

操作 2：以高度 1（h1）为父提交高度 2、3 的缩短分支，Reorg 返回
  err=<nil>
  丢弃高度 dropped=[3,4]（升序）
  解释：该列表报告旧内容发生变化或被删除的高度——
  · 高度 2 提交的块与旧块完全相同，不列入；
  · 高度 3 哈希/父哈希/交易都没变，但时间由缺失变成真实零秒，旧内容变了，必须列入；
  · 高度 4 在分支覆盖范围之外，旧块被删除，必须列入；
  · 它不是全部提交高度（提交的是 2、3），也不是消失的哈希数量（h4 只有一个）。
操作后的主链：
  高度 1：hash=h1 parent=genesis txs=["t1"] 时间=100
  高度 2：hash=h2 parent=h1 txs=["t2"] 时间=200
  高度 3：hash=h3 parent=h2 txs=["t3"] 时间=0
  链顶 tip=3；ByHash=map[h1:1 h2:2 h3:3]
  h3 现在的时间：0 —— 真实零秒，与缺失严格不同
  高度 4 是否存在：false；h4 是否还在哈希表：false
  查询高度 [1, 4]：ToHeight=3 TotalMatches=3 MatchedBlocks=3
    命中 height=1 block=h1 tx="t1" position=0
    命中 height=2 block=h2 tx="t2" position=0
    命中 height=3 block=h3 tx="t3" position=0

操作 3：新索引，仍是原来的高度 1..4 链（失败场景从这条链开始）
  失败前链顶 tip=4；h3 时间=nil（缺失）
  提交最后一块父链接错误的分支：err=branch parent does not match the previous block
  重组失败只表现为非 nil 错误（本功能未导出专用哨兵错误）
  返回的丢弃高度 dropped=[] —— 出错时不报告任何高度，前面合法的块也不会提前生效
  失败后链顶 tip=4（应仍为 4）
  失败后的主链（应与操作 1 的旧链完全一致：h3 仍无时间，h4 仍在）：
    高度 1：hash=h1 parent=genesis txs=["t1"] 时间=100
    高度 2：hash=h2 parent=h1 txs=["t2"] 时间=200
    高度 3：hash=h3 parent=h2 txs=["t3"] 时间=nil（缺失）
    高度 4：hash=h4 parent=h3 txs=["t4"] 时间=400
  ByHash=map[h1:1 h2:2 h3:3 h4:4]
  失败前后导出逐字节一致：true；h3 时间仍缺失=true；h4 仍在哈希表=true

操作 4：首块摄取的未索引起点 vs 重组的父哈希
  空索引 Append 高度 1、parent="unindexed-start"：err=<nil>（该标识只是链起点，无需已索引）
  对同一个未索引标识发起 Reorg（试图从高度 1 整体替换）：err=branch parent is unknown
  即：首块摄取允许用未索引的起点标识，不代表重组也能以这个标识为父哈希替换高度 1；
  重组第一个新区块的父哈希必须是当前主链上已经索引的区块，分支从它的下一高度开始。
```

## 快照导出与恢复指南（Export/Restore）

`Index.Export(w)` 把当前主链写成一份 JSON 快照（`version`、`tip`、按高度升序的 `blocks`）；`Index.Restore(r)` 读取一份快照并用它**整体替换主链**。两个接口都只看到一个完整的链状态，且与查询、摄取可并发使用：恢复在读取和校验阶段不持锁，索引全程可查询、可追加。

### Restore 是整体替换，不是合并

- 恢复成功后，索引中**恰好**是快照里的链：高度从 1 连续到快照最后一个区块，旧链的任何高度、哈希都不残留。
- 用较短的快照恢复后，链顶直接降到快照的末尾高度：旧链多出的区块被删除，被替换高度上的旧交易也一并消失，它们**不再出现在 `QueryTxs`/`QueryTimeStats` 的结果中，也不再出现在再次 `Export` 的内容里**。随后对恢复后的索引再次导出，得到的是同一条链的规范化文本——与输入快照**数据一致**；只有当输入本身就是本功能未经修改的导出内容、且恢复后链内容不变时，才同时与输入**逐字节一致**（见下方[数据一致不等于文本一致](#数据一致不等于文本一致)）。
- 快照第一个区块的 `parent` 只命名链起点，不需要在索引中存在；其余每个区块必须指向前一区块。

### 输入必须是一份完整快照

- `Restore` 的输入必须是**恰好一份**完整的 JSON 快照对象；对象结尾之后允许有空白（空格、制表符、换行）。
- 在一个对象后面拼接第二份文档（或任何其他非空白内容）会被拒绝，不能把两份快照当作一次输入。
- 全部区块在应用之前整体校验：高度从 1 连续递增、哈希非空且不重复、父链接依次相连等。即使错误出现在**最后一个区块**，前面的合法区块也不会被部分导入。

### 版本与时间语义：缺失时间不是 0

- **版本 1**：区块没有 `timestamp` 字段。恢复后这些区块的 `Block.Time` 为 `nil`，即**没有时间**；这不是"自动补 0"，再导出时仍是版本 1（区块不含 `timestamp`）。
- **版本 2**：每个区块都必须带 `timestamp`：`null` 表示**缺失时间**（恢复为 `nil`），数字 `0` 表示**实际时间为零**（Unix 秒 0，恢复为指向 0 的非空指针）。`null` 与 `0` 不能互换，二者在查询指纹中也被严格区分。
- 导出自动选版：所有区块都没有时间时输出版本 1；**只要任一区块带时间（包括时间为 0）**，整份文档就是版本 2，每个区块都输出 `timestamp`，缺失处为 `null`。因此一份版本 2 快照若每个 `timestamp` 都是 `null`，恢复后全链无时间，**再导出会变为版本 1**——这是版本随数据重选，不是丢失时间：输入本来就没有任何时间；反过来把缺失处的 `null` 写成 `0`，恢复得到的是真实零秒时间，再导出才保持版本 2 且 `timestamp` 为 `0`。

### 数据一致不等于文本一致

`Restore` 读取的是 JSON **数据**，不是某一种固定文本；再导出写出的是本功能自己的**规范化文本**（字段按固定顺序、无多余空白、字符串按标准库方式转义的紧凑单行文档）。所以：

- **数据一致是保证**：恢复成功后，区块与交易列表按原序保留——对象字段可以换顺序，区块数组和 `txs` 数组的元素顺序不能换；字符串的等价写法（如 `"甲"` 与 `"\u7532"`）解码后是同一个值，查询按**解码后的原值**精确匹配，空标识、重复出现与块内位置都原样保留。字符串标识完整的合法与非法写法（含真正的 U+FFFD、代理项转义规则）见下方[字符串标识的编码规则](#字符串标识的编码规则合法写法与两类拒绝)。
- **文本一致不是普遍保证**：合法快照可以采用不同的对象字段顺序、排版空白和字符串转义写法，这些纯文本形式在恢复时被解码、再导出时不会保留，因此再次导出的字节可能与输入不同。
- **逐字节一致的充分条件**：只有输入是**本功能未经修改的导出内容**，且恢复后链内容不变（没有追加、重组或再用别的快照恢复）时，再次导出才与输入逐字节一致。判断恢复结果应核对数据（链顶、区块、交易标识与位置、时间是否缺失及取值），不要把字节差异当作数据损坏。

允许更换文本写法**不放宽任何校验**：字段仍必须是 schema 规定的字段，且不能缺失、类型错误、未知或重复；只是同一份合法数据可以有多种等价文本。

### 区分恢复成功、非法快照与读取失败

`Restore` 的结果分三类，调用方按下述口径区分：

- **恢复成功**（`err == nil`）：链被快照整体替换，索引中恰好是快照里的链。
- **快照非法**：JSON 损坏或截断、对象后有多余数据、版本未知、字段缺失/类型错误/未知/重复、高度不连续、哈希为空或重复、父链接断裂、版本 2 时间戳缺失或既非 `null` 也非非负整数、**字符串标识（`hash`、`parent`、每个 `txs` 元素）含非法 UTF-8 原始字节或孤立/错序的 Unicode 代理项转义**等，统一返回可用 `errors.Is(err, indexroom.ErrInvalidSnapshot)` 识别的错误，具体原因附在错误信息中：一般问题指出所在位置（例如父链接断裂发生在哪个高度），字符串编码类错误还会指出所属高度与字段，交易标识再指出从 0 开始的位置（规则与示例见下方[字符串标识的编码规则](#字符串标识的编码规则合法写法与两类拒绝)）。
- **读取失败**：底层 `io.Reader` 报告的任何故障（网络中断、存储故障等）都**不是** `ErrInvalidSnapshot`，而是包装为以 `indexroom: read snapshot: ` 开头的错误返回，**原始读取错误保留在错误链中，可用 `errors.Is` 识别**。注意故障**包括与完整快照字节一起送达的情况**，并不限于"读到一半就出错"，详见下文。

#### 输入结束的含义：只有底层直接返回的 io.EOF 是正常结束

- **正常结束**只认底层 reader **直接返回的 `io.EOF` 本身**。完整合法快照在这种情况下恢复成功；不完整的 JSON 在正常结束后返回 `ErrInvalidSnapshot`（`unexpected end of input`）——输入确实只有这些，是内容问题，不是读取失败。
- 底层主动返回 `io.ErrUnexpectedEOF`、**包装过的 `io.EOF`**（如 `fmt.Errorf("storage at EOF: %w", io.EOF)`）、或 `errors.Join(io.EOF, 自定义读取错误)` 这类组合错误，都属于**读取失败**：即使 `errors.Is(err, io.EOF)` 能匹配，也不能据此当成正常结束、更不能当成恢复成功。

#### 同一次读取既返回字节又报告故障：故障不能被忽略

- 即便那批字节组成**完整合法的快照**，恢复仍失败并返回读取失败——流已经失败，无法证明这些字节就是完整输入。
- 若已收到故障的那批字节还暴露未知字段等内容问题，返回的仍是**读取失败**，而不是 `ErrInvalidSnapshot`。
- 这个优先关系只适用于**已经收到**的故障：如果未知字段先在一次无故障读取中被发现，恢复按非法快照结束，不会为了寻找后续可能出现的读取错误继续取数据。
- 版本 1 与版本 2 在这里遵循同一规则。

#### 失败后的状态

两种失败（非法快照、读取失败）都不会改变现有链：链顶、区块、哈希以及此前发出的分页游标全部保持原状，恢复前取得的游标仍能续查原链；只有恢复成功时链才被快照整体替换。

### 完整示例：正常结束、截断与读取故障的对照

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/restoreread/main.go`](examples/restoreread/main.go)：

```bash
go run ./examples/restoreread
```

场景：围绕同一份单区块小快照（版本 1 与版本 2 各一份）对照三种结局——正常结束、截断后正常结束、完整字节伴随读取故障；再演示故障与内容问题的优先关系。程序先准备一条 3 个区块的原链并取得一个续查游标，每次恢复尝试都打印实际错误与分类（是否 `ErrInvalidSnapshot`、是否读取失败、`errors.Is` 能识别到哪些原始错误），随后核对全部失败之后链顶、交易查询与恢复前取得的游标都保持原状，而正常结束的同一份快照把链整体替换。

```go
// 快照恢复（Index.Restore）的输入结束与读取故障完整示例：围绕同一份小快照
// 对照三种结局——正常结束（底层直接返回 io.EOF）可恢复、截断后正常结束返回
// ErrInvalidSnapshot、完整字节伴随读取故障返回读取失败。随后展示故障与内容
// 问题的优先关系（已收到的故障优先；先发现的内容问题按非法快照结束，不继续
// 读）、版本 1 与版本 2 遵循同一规则，以及失败后链顶、交易查询与恢复前取得
// 的游标全部保持原状，成功时链被快照整体替换。
//
// 运行：go run ./examples/restoreread
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// 同一份小快照的两个布局版本：一个区块、一笔交易 snap-tx。
const snapshotV1 = `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"g1","parent":"g","txs":["snap-tx"]}]}`
const snapshotV2 = `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"g1","parent":"g","txs":["snap-tx"],"timestamp":null}]}`

// unknownFieldDoc 的顶层多一个未知字段 extra，本身是一份非法快照。
const unknownFieldDoc = `{"version":1,"tip":0,"blocks":[],"extra":0}`

// oneShotReader 在一次 Read 中同时交出 data 与 err，之后永远返回正常的
// io.EOF，模拟“同一批字节与故障一起送达”的底层流。
type oneShotReader struct {
	done bool
	data []byte
	err  error
}

func (r *oneShotReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}

// scriptReader 按脚本逐段送出内容：每段可以只带字节、只带错误，或两者
// 兼有；脚本用完后以正常的 io.EOF 结束。
type scriptReader struct {
	chunks []scriptChunk
}

type scriptChunk struct {
	data string
	err  error
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	n := copy(p, chunk.data)
	return n, chunk.err
}

// isReadFailure 报告 err 是否是一次读取失败：错误信息以
// "indexroom: read snapshot: " 开头，原始读取错误保留在错误链中。
func isReadFailure(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "indexroom: read snapshot: ")
}

// newOriginalChain 返回一条三个区块的原链，交易依次为 a、b、c。
func newOriginalChain() *indexroom.Index {
	index := indexroom.New()
	for _, block := range []indexroom.Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"b"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"c"}},
	} {
		if err := index.Append(block); err != nil {
			panic(err)
		}
	}
	return index
}

// countTx 返回若干标识在主链上的命中总数。
func countTx(index *indexroom.Index, txIDs ...string) int64 {
	page, err := index.QueryTxs(indexroom.TxQuery{TxIDs: txIDs})
	if err != nil {
		panic(err)
	}
	return page.TotalMatches
}

func main() {
	storageFault := errors.New("simulated storage failure")

	// 操作 1：准备原链（3 个区块，交易依次为 a、b、c），并取得一个每页 1 条
	// 的续查游标，留给各种失败之后核对：失败不能改变链，也不能弄坏旧游标。
	index := newOriginalChain()
	first, err := index.QueryTxs(indexroom.TxQuery{PageSize: 1})
	if err != nil {
		panic(err)
	}
	cursor := first.NextCursor
	fmt.Println("操作 1：原链与游标")
	fmt.Printf("  链顶 tip=%d；首页命中 高度%d/%s，续查游标已保存（不透明，不展示内容）\n\n",
		index.Tip, first.Hits[0].Height, first.Hits[0].TxID)

	// 操作 2：同一份被截断的字节，两种结束方式对应两种分类。
	// 正常结束（底层直接返回 io.EOF）说明输入确实只有这些——内容不完整，
	// 是 ErrInvalidSnapshot；底层主动报告 io.ErrUnexpectedEOF 则是读取失败。
	half := snapshotV1[:len(snapshotV1)/2]
	fmt.Println("操作 2：截断后的同一批字节，两种结束方式")
	err = index.Restore(strings.NewReader(half))
	fmt.Printf("  正常结束（strings.Reader 直接返回 io.EOF）：\n    err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err))
	err = index.Restore(&oneShotReader{data: []byte(half), err: io.ErrUnexpectedEOF})
	fmt.Printf("  同一批字节伴随 io.ErrUnexpectedEOF：\n    err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v errors.Is(io.ErrUnexpectedEOF)=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err),
		errors.Is(err, io.ErrUnexpectedEOF))

	// 操作 3：完整合法的字节与故障在同一次读取中送达——故障不能被忽略，
	// 即使那批字节组成完整快照，恢复仍然失败。版本 1、2 遵循同一规则。
	joined := errors.Join(io.EOF, storageFault)
	fmt.Println("操作 3：完整快照字节伴随 errors.Join(io.EOF, 存储故障)（同一次读取送达）")
	for _, tc := range []struct{ name, doc string }{
		{"版本 1", snapshotV1},
		{"版本 2", snapshotV2},
	} {
		err = index.Restore(&oneShotReader{data: []byte(tc.doc), err: joined})
		fmt.Printf("  %s：err=%v\n", tc.name, err)
		fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v errors.Is(io.EOF)=%v errors.Is(存储故障)=%v\n",
			errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err),
			errors.Is(err, io.EOF), errors.Is(err, storageFault))
	}
	fmt.Println("  errors.Is 能匹配 io.EOF 不等于正常结束：只有底层直接返回的 io.EOF 才是")
	fmt.Println()

	// 操作 4：包装过的 io.EOF 同样是读取失败，不能当成正常结束。
	wrapped := fmt.Errorf("storage at EOF: %w", io.EOF)
	err = index.Restore(&oneShotReader{data: []byte(snapshotV1), err: wrapped})
	fmt.Println("操作 4：完整快照字节伴随包装过的 io.EOF（fmt.Errorf 的 %w 包装）")
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  ErrInvalidSnapshot=%v 读取失败=%v errors.Is(io.EOF)=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err), errors.Is(err, io.EOF))

	// 操作 5：已收到故障的那批字节同时暴露内容问题（这里是未知字段）时，
	// 仍返回读取失败，而不是 ErrInvalidSnapshot——流已失败，无法证明这些
	// 字节就是完整输入。对照：同样的字节正常结束时才按非法快照拒绝。
	fmt.Println("操作 5：含未知字段的字节与故障同批送达")
	err = index.Restore(&oneShotReader{data: []byte(unknownFieldDoc), err: storageFault})
	fmt.Printf("  伴随故障：err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v errors.Is(存储故障)=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err), errors.Is(err, storageFault))
	err = index.Restore(strings.NewReader(unknownFieldDoc))
	fmt.Printf("  对照：同一文本正常结束：err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err))

	// 操作 6：优先关系只适用于已经收到的故障。未知字段在一次无故障读取中
	// 先被发现时，恢复按非法快照结束，不会为了寻找后续可能出现的读取错误
	// 继续取数据——排在其后的故障根本不会被读到。
	fmt.Println("操作 6：未知字段先在无故障读取中被发现（故障排在其后）")
	err = index.Restore(&scriptReader{chunks: []scriptChunk{
		{data: unknownFieldDoc}, // 无故障读取，未知字段在此被发现
		{err: storageFault},     // 这段故障永远不会被读到
	}})
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  ErrInvalidSnapshot=%v errors.Is(存储故障)=%v（没有为找故障继续读）\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), errors.Is(err, storageFault))

	// 操作 7：以上失败都不改变现有链：链顶与交易查询保持原状，恢复前取得
	// 的游标仍能续查原链（不是 ErrQueryChanged）。
	fmt.Println("操作 7：全部失败之后核对原链")
	fmt.Printf("  链顶仍为 tip=%d；旧交易 a/b/c 命中=%d，快照交易 snap-tx 命中=%d\n",
		index.Tip, countTx(index, "a", "b", "c"), countTx(index, "snap-tx"))
	fmt.Printf("  恢复前取得的游标续查：")
	query := indexroom.TxQuery{PageSize: 1, Cursor: cursor}
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			panic(err)
		}
		for _, hit := range page.Hits {
			fmt.Printf("高度%d/%s ", hit.Height, hit.TxID)
		}
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	fmt.Println("（游标照常读出原链其余记录）")
	fmt.Println()

	// 操作 8：同一份快照以正常结束送达时恢复成功，链被快照整体替换：
	// 旧交易 a/b/c 消失，快照交易 snap-tx 出现；版本 2 同样如此。
	fmt.Println("操作 8：同一份快照正常结束（底层直接返回 io.EOF）")
	err = index.Restore(strings.NewReader(snapshotV1))
	fmt.Printf("  恢复版本 1：err=%v，链顶 tip=%d；a/b/c 命中=%d，snap-tx 命中=%d\n",
		err, index.Tip, countTx(index, "a", "b", "c"), countTx(index, "snap-tx"))
	err = index.Restore(strings.NewReader(snapshotV2))
	fmt.Printf("  恢复版本 2：err=%v，链顶 tip=%d；a/b/c 命中=%d，snap-tx 命中=%d\n",
		err, index.Tip, countTx(index, "a", "b", "c"), countTx(index, "snap-tx"))
}
```

对应输出（`go run ./examples/restoreread` 的实际输出，每次运行逐字一致）：

```text
操作 1：原链与游标
  链顶 tip=3；首页命中 高度1/a，续查游标已保存（不透明，不展示内容）

操作 2：截断后的同一批字节，两种结束方式
  正常结束（strings.Reader 直接返回 io.EOF）：
    err=indexroom: invalid snapshot: unexpected end of input
    ErrInvalidSnapshot=true 读取失败=false
  同一批字节伴随 io.ErrUnexpectedEOF：
    err=indexroom: read snapshot: field "blocks": unexpected EOF
    ErrInvalidSnapshot=false 读取失败=true errors.Is(io.ErrUnexpectedEOF)=true

操作 3：完整快照字节伴随 errors.Join(io.EOF, 存储故障)（同一次读取送达）
  版本 1：err=indexroom: read snapshot: EOF
simulated storage failure
    ErrInvalidSnapshot=false 读取失败=true errors.Is(io.EOF)=true errors.Is(存储故障)=true
  版本 2：err=indexroom: read snapshot: EOF
simulated storage failure
    ErrInvalidSnapshot=false 读取失败=true errors.Is(io.EOF)=true errors.Is(存储故障)=true
  errors.Is 能匹配 io.EOF 不等于正常结束：只有底层直接返回的 io.EOF 才是

操作 4：完整快照字节伴随包装过的 io.EOF（fmt.Errorf 的 %w 包装）
  err=indexroom: read snapshot: storage at EOF: EOF
  ErrInvalidSnapshot=false 读取失败=true errors.Is(io.EOF)=true

操作 5：含未知字段的字节与故障同批送达
  伴随故障：err=indexroom: read snapshot: simulated storage failure
    ErrInvalidSnapshot=false 读取失败=true errors.Is(存储故障)=true
  对照：同一文本正常结束：err=indexroom: invalid snapshot: unknown field "extra"
    ErrInvalidSnapshot=true 读取失败=false

操作 6：未知字段先在无故障读取中被发现（故障排在其后）
  err=indexroom: invalid snapshot: unknown field "extra"
  ErrInvalidSnapshot=true errors.Is(存储故障)=false（没有为找故障继续读）

操作 7：全部失败之后核对原链
  链顶仍为 tip=3；旧交易 a/b/c 命中=3，快照交易 snap-tx 命中=0
  恢复前取得的游标续查：高度2/b 高度3/c （游标照常读出原链其余记录）

操作 8：同一份快照正常结束（底层直接返回 io.EOF）
  恢复版本 1：err=<nil>，链顶 tip=1；a/b/c 命中=0，snap-tx 命中=1
  恢复版本 2：err=<nil>，链顶 tip=1；a/b/c 命中=0，snap-tx 命中=1
```

（操作 3 的错误信息占两行，是因为 `errors.Join` 组合错误的 `Error()` 用换行连接各成员；原始读取错误仍在错误链中，可正常 `errors.Is`。）

### 字符串标识的编码规则：合法写法与两类拒绝

快照里每个字符串标识——区块的 `hash`、`parent`，以及 `txs` 数组中的每个交易标识——恢复时都按**同一套规则**解码并检查；**版本 1 与版本 2 的字符串规则完全相同**（版本 2 只是每个区块多一个 `timestamp`）。

**哪些写法合法**

- 字符串内容必须是合法 UTF-8：直接写出的中文、表情符号、空字符串都是合法标识。
- 反斜杠加 `u` 与四位十六进制是 JSON 的 Unicode 转义，给出一个 Unicode 码位：基本多文种平面内的字符可直接转义（如“中”写作 `\u4e2d`）；表情符号这类码位在该平面之外，必须用**一对代理项转义**写出——高代理项（`\uD800`–`\uDBFF`）后**紧接**低代理项（`\uDC00`–`\uDFFF`），顺序不能反（如表情符号“😀”写作一对转义 `\uD83D\uDE00`）。
- 同一字符的**字面写法与合法转义写法解码后是同一个值**：恢复得到同一个标识，`QueryTxs` 按解码后的值精确匹配，所以一个直接写出的字符与表示同一字符的转义不会变成两个标识；同一交易在块内出现几次、各在什么位置，都按原样保留（空标识也不例外）。
- **真正的 U+FFFD（替换字符，通常显示为“�”）是一个合法的普通字符**：直接写出或用它的合法转义 `\ufffd` 写出都接受。它与下面“坏内容被解码器替换成 U+FFFD”完全是两回事。
- 再次导出采用规范化写法（非 ASCII 字符直接写出、不保留输入的转义风格），因此同一份数据重新导出时可能改变转义写法；这只是文本差异，不是数据损坏（见上文[数据一致不等于文本一致](#数据一致不等于文本一致)）。

**两类非法编码：拒绝，而不是悄悄替换**

- **非法 UTF-8 原始字节**：字符串里出现不符合 UTF-8 编码规则的字节（例如孤立的 `0xFF`）。
- **孤立或顺序错误的代理项转义**：`\uXXXX` 落在代理项区间却没有组成合法的代理项对——只有高代理项而没有紧接着的低代理项、单独出现的低代理项、低代理项排在高代理项前面等。
- **为什么是拒绝而不是替换**：标准库 JSON 解码器对这两类缺陷会“容错”——把坏字节和孤立代理项**悄悄改写成 U+FFFD**。若照此导入，两个本来不同的标识会塌缩成同一个值，破坏查询的精确匹配。`Restore` 因此在解码之外对原始文本再做一次检查，发现缺陷即整体拒绝：**非法内容不会被替换成 U+FFFD 后导入**。再次强调：真正写入的 U+FFFD（包括 `\ufffd` 写法）始终是合法字符，被拒绝的只是“编码不合法的输入”。
- **整体校验、没有部分生效**：全部区块在应用之前完整校验。编码缺陷即使出现在后面的区块（例如高度 2），前面看似合法的区块（高度 1）也不会被部分导入；拒绝后链顶、区块内容与原交易查询结果全部保持原状。

**错误识别与定位**

- 这类拒绝可用 `errors.Is(err, indexroom.ErrInvalidSnapshot)` 识别；错误信息指出所属高度与字段，形如 `block at height 2: field "hash" has invalid UTF-8 or unpaired surrogate escapes`；交易标识还会指出**从 0 开始的位置**，形如 `block at height 2: field "txs" element 0 ...`。
- 不要把它与底层读取失败混为一谈：`io.Reader` 报告故障时（包括与完整快照字节一起送达的故障），错误包装为 `indexroom: read snapshot: ...`，`errors.Is` 命中的是**原始读取错误**，而不是 `ErrInvalidSnapshot`（见上文[区分恢复成功、非法快照与读取失败](#区分恢复成功非法快照与读取失败)）。

### 编码规则完整示例：恢复小快照、等价写法与两类拒绝

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/snapshotencoding/main.go`](examples/snapshotencoding/main.go)：

```bash
go run ./examples/snapshotencoding
```

程序围绕恢复一份小快照组织，与上文的快照规则一一对应：

1. 操作 1–2：恢复一份手写的版本 1 两区块快照，区块哈希、父哈希与交易标识混用中文字面字符、表情符号代理项对、真正的 U+FFFD（字面与 `\ufffd` 两种写法）与空标识；输出链上实际保存的标识与查询命中（高度、块内位置），可以看到字面字符与等价转义得到**同一个**标识、同一交易的两次出现和位置都保留，而“反斜杠+u+十六进制”的转义文本本身并不是标识；再次导出改变转义写法但数据一致。
2. 操作 3：同样的规则在版本 2（含 `timestamp`）上再演示一次。
3. 操作 4–6：先保留一条可查询的现有链；操作 5 展示标准库解码器会把两类坏输入悄悄改写成 U+FFFD（`Restore` 不采用这种行为）；操作 6 分别尝试含非法 UTF-8 字节、含孤立/错序代理项的快照（覆盖 `hash`、`parent`、`txs element`，且缺陷都在高度 2），打印实际错误类别与定位，并核对拒绝后链顶仍是 1、原交易查询结果不变、快照高度 1 的合法内容没有部分生效。
4. 操作 7：读取途中故障返回的是读取错误而不是 `ErrInvalidSnapshot`，与上面的编码拒绝对照。

```go
// 快照字符串标识的编码规则示例：围绕恢复一份小快照，展示区块哈希、父哈希和
// 每个交易标识都要经过同样的编码检查（版本 1 与版本 2 规则相同）——
// 合法中文、表情符号与真正的 U+FFFD（替换字符）都被接受；同一个字符直接
// 写出与写成等价的合法 Unicode 转义（见下方 esc 辅助函数），恢复后得到
// 同一个标识，查询按解码后的值精确匹配，重复出现与块内位置原样保留；再次
// 导出可以改变转义写法，这种文本变化不代表数据损坏。
//
// 随后在一条保留的、可查询的现有链上，分别尝试两类编码非法的快照：
//   - 字符串里含非法 UTF-8 原始字节；
//   - Unicode 转义里出现孤立或顺序错误的代理项（例如只有高代理项、
//     没有紧接着的低代理项）。
//
// 两类输入都以 ErrInvalidSnapshot 整体拒绝：错误指出所属高度与字段，交易
// 标识还指出从 0 开始的位置；缺陷即使出现在后面的区块，前面看似合法的区块
// 也不会部分生效，链顶与原交易查询保持原状。这与底层 io.Reader 读取失败是
// 两类错误，示例最后加以区分。
//
// 运行：go run ./examples/snapshotencoding
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// esc 拼出一个 JSON Unicode 转义片段：esc("4e2d") == 反斜杠+u+4e2d。
// 用函数拼接而不是直接在源码里写出该转义，快照文本里到底是“字面字符”还是
// “转义写法”就可以由代码明确控制、也便于读者逐字核对。
func esc(hex string) string { return "\\" + "u" + hex }

// exportString 返回当前主链再次导出的快照文本。
func exportString(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// showQuery 查询一个标识在主链上的每次出现，打印命中的高度与块内位置。
// 查询按解码后的原值精确匹配：调用方不需要、也不应该关心快照里用的是
// 字面字符还是反斜杠 u 开头的转义。
func showQuery(index *indexroom.Index, id string) {
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: index.Tip, TxIDs: []string{id}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询标识 %q：TotalMatches=%d，命中", id, page.TotalMatches)
	for _, hit := range page.Hits {
		fmt.Printf(" 高度%d/位置%d", hit.Height, hit.Position)
	}
	if page.TotalMatches == 0 {
		fmt.Print("（无命中）")
	}
	fmt.Println()
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

// v1Doc 用给定的第二个区块 JSON 片段拼出一份两区块版本 1 快照；
// 第一个区块始终合法，用来观察“缺陷在后面的区块时前面的区块不会部分生效”。
func v1Doc(block2 string) string {
	return `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"new-1","parent":"genesis","txs":["新交易"]},` +
		block2 + `]}`
}

// v2Doc 与 v1Doc 对应，但拼出版本 2 快照（每块带 timestamp），
// 用来证明版本 1 与版本 2 的字符串编码规则完全相同。
func v2Doc(block2 string) string {
	return `{"version":2,"tip":2,"blocks":[` +
		`{"height":1,"hash":"new-1","parent":"genesis","txs":["新交易"],"timestamp":null},` +
		block2 + `]}`
}

func main() {
	// 操作 1：恢复一份手写的版本 1 小快照。哈希、父哈希、交易标识都使用
	// 非 ASCII 字符，并刻意混用“直接写出的字符”与“等价的 Unicode 转义”：
	//   - 区块 1 的哈希直接写“中文”，区块 2 的父哈希用 esc 写成等价转义，
	//     二者解码后都是“中文”，父链接因此成立；
	//   - 区块 2 的哈希用一对代理项转义写出，解码后即表情符号；
	//   - 区块 1 的交易依次为：转义写出的“tx-表情”、直接写出的真正
	//     U+FFFD（替换字符）、用 esc 转义写出的 U+FFFD、空标识、再次直接
	//     写出的“tx-表情”——同一标识出现两次，位置 0 和 4 都要保留；
	//   - 区块 2 还有一笔直接写出的中文交易“记账”。
	inputV1 := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"中文","parent":"起点","txs":["tx-` + esc("d83d") + esc("de00") +
		`","�","` + esc("fffd") + `","","tx-😀"]},` +
		`{"height":2,"hash":"` + esc("d83d") + esc("de00") +
		`","parent":"` + esc("4e2d") + esc("6587") + `","txs":["记账"]}]}`
	fmt.Println("操作 1：恢复手写版本 1 快照（中文、表情符号、U+FFFD，字面与 Unicode 转义混用）")
	fmt.Printf("  输入文本：%s\n", inputV1)
	idx := indexroom.New()
	if err := idx.Restore(strings.NewReader(inputV1)); err != nil {
		panic(err)
	}
	fmt.Println("  恢复后链上实际保存的标识（Go 解码后的原值）：")
	fmt.Printf("    高度1 hash=%q parent=%q txs=%q\n",
		idx.Blocks[1].Hash, idx.Blocks[1].Parent, idx.Blocks[1].Txs)
	fmt.Printf("    高度2 hash=%q parent=%q txs=%q\n",
		idx.Blocks[2].Hash, idx.Blocks[2].Parent, idx.Blocks[2].Txs)
	fmt.Println("  字面字符与等价转义不会变成两个不同标识；重复出现与块内位置原样保留：")
	showQuery(idx, "tx-😀") // 位置 0（转义写出）与位置 4（直接写出）各命中一次
	showQuery(idx, "�")    // 两个真正的 U+FFFD 标识，位置 1 和 2
	showQuery(idx, "记账")
	// 反证：把“反斜杠、u、四位十六进制”当成普通字符组成的标识并不存在——
	// 快照里的转义在解码时已经还原成字符，不会把转义文本本身存进链里。
	showQuery(idx, "tx-"+esc("d83d")+esc("de00"))
	fmt.Println()

	// 操作 2：再次导出。导出使用规范化文本（紧凑、无多余空白），非 ASCII
	// 字符直接写出，输入里的转义写法不会保留——所以字节可能与输入不同；
	// 但解码后的数据（标识、次序、位置）完全一致，这不是数据损坏。
	exported := exportString(idx)
	fmt.Println("操作 2：再次导出（转义写法改变，但数据一致）")
	fmt.Printf("  再次导出：%s\n", exported)
	fmt.Printf("  与输入逐字节一致：%v（输入用了 Unicode 转义，导出直接写出字符）\n",
		exported == inputV1)
	fmt.Print("  数据仍一致的核对：")
	showQuery(idx, "tx-😀")
	fmt.Println()

	// 操作 3：版本 2 适用同样的编码规则。这份快照每块带 timestamp：
	// U+FFFD 用转义写出，中文直接写出，时间为真实零秒。
	inputV2 := `{"version":2,"tip":1,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["` + esc("fffd") + `","付款"],"timestamp":0}]}`
	fmt.Println("操作 3：恢复版本 2 快照（编码规则与版本 1 相同，timestamp 照常保留）")
	v2 := indexroom.New()
	if err := v2.Restore(strings.NewReader(inputV2)); err != nil {
		panic(err)
	}
	fmt.Printf("  恢复后 txs=%q；再次导出：%s\n", v2.Blocks[1].Txs, exportString(v2))
	showQuery(v2, "�")
	showQuery(v2, "付款")
	fmt.Println()

	// 操作 4：准备一条保留的、可查询的现有链，随后所有非法恢复都针对它。
	// 链上只有高度 1：交易“付款”（位置 0）与表情符号（位置 1）。
	target := indexroom.New()
	if err := target.Append(indexroom.Block{
		Height: 1, Hash: "keep-1", Parent: "genesis", Txs: []string{"付款", "😀"},
	}); err != nil {
		panic(err)
	}
	before := exportString(target)
	fmt.Println("操作 4：现有链（非法恢复的目标，恢复前后都应保持原状）")
	fmt.Printf("  %s\n", before)
	showQuery(target, "付款")
	fmt.Println()

	// 为什么不能把坏内容替换成 U+FFFD 后导入：标准库 JSON 解码器本身会
	// 悄悄这样做——非法字节和孤立代理项都被改写成 U+FFFD，两个本来不同
	// 的输入会塌缩成同一个标识。Restore 因此在解码之外额外检查原始文本，
	// 直接拒绝，而不是依赖这种替换。
	badByteContent := "x" + string([]byte{0xff})
	var rewrittenByte string
	_ = json.Unmarshal([]byte("\""+badByteContent+"\""), &rewrittenByte)
	loneSurJSON := `"b` + esc("d83d") + `"`
	var rewrittenSurrogate string
	_ = json.Unmarshal([]byte(loneSurJSON), &rewrittenSurrogate)
	fmt.Println("操作 5：标准库解码器会怎样“容错”（Restore 不采用这种行为）")
	fmt.Printf("  含非法字节的 JSON 字符串（Go 引号形式 %s，坏字节显示为 \\xff）直接解码会变成 %q\n",
		strconv.Quote(badByteContent), rewrittenByte)
	fmt.Printf("  含孤立高代理项的 JSON 文本 %s 直接解码会变成 %q\n", loneSurJSON, rewrittenSurrogate)
	fmt.Println("  坏字节/孤立代理项都被悄悄替换成 U+FFFD，会让不同标识无法区分；")
	fmt.Println("  真正写入的 U+FFFD（包括它的合法转义写法）是正常字符，与这种替换无关。")
	fmt.Println()

	// 操作 6：两类编码非法的快照。每份快照的第一个区块都完全合法，缺陷都
	// 放在高度 2：非法 UTF-8 原始字节（交易标识、哈希各一例），以及孤立/
	// 错序代理项（交易标识、父哈希，版本 1 与版本 2 各一例）。
	// 真正的 U+FFFD 合法，非法内容却不能替换成 U+FFFD 导入——所以这些
	// 输入一律 ErrInvalidSnapshot，且高度 1 的合法区块也不会部分生效。
	badCases := []struct {
		name string
		doc  string
	}{
		{
			"v1 高度2 交易标识0 含非法 UTF-8 字节",
			v1Doc(`{"height":2,"hash":"new-2","parent":"new-1","txs":["x` + string([]byte{0xff}) + `"]}`),
		},
		{
			"v1 高度2 哈希含非法 UTF-8 字节",
			v1Doc(`{"height":2,"hash":"h` + string([]byte{0xff}) + `","parent":"new-1","txs":[]}`),
		},
		{"v1 高度2 交易标识0 只有高代理项（缺少紧跟的低代理项）",
			v1Doc(`{"height":2,"hash":"new-2","parent":"new-1","txs":["` + esc("d800") + `"]}`)},
		{"v1 高度2 父哈希是孤立的低代理项",
			v1Doc(`{"height":2,"hash":"new-2","parent":"` + esc("dc00") + `","txs":[]}`)},
		{"v2 高度2 交易标识0 代理项对顺序颠倒（低代理项在前）",
			v2Doc(`{"height":2,"hash":"new-2","parent":"new-1","txs":["` + esc("de00") + esc("d800") + `"],"timestamp":null}`)},
	}
	fmt.Println("操作 6：两类编码非法的快照都被整体拒绝（错误定位到高度、字段与位置）")
	for _, tc := range badCases {
		err := target.Restore(strings.NewReader(tc.doc))
		unchanged := target.Tip == 1 && exportString(target) == before
		fmt.Printf("  %s：\n    err=%v\n", tc.name, err)
		fmt.Printf("    errors.Is(err, ErrInvalidSnapshot)=%v；拒绝后链顶仍为1且导出未变=%v\n",
			errors.Is(err, indexroom.ErrInvalidSnapshot), unchanged)
	}
	fmt.Println()
	fmt.Println("  拒绝后的现有链核对（原交易、位置都在；快照里的新交易没有出现）：")
	showQuery(target, "付款")
	showQuery(target, "😀")
	showQuery(target, "新交易") // 快照高度 1 的交易：整个快照被拒，无部分生效
	fmt.Printf("  链顶 tip=%d；再次导出与恢复前逐字节一致：%v\n\n",
		target.Tip, exportString(target) == before)

	// 操作 7：区分读取输入失败。读到一半底层 reader 故障不是非法快照：
	// 原始读取错误仍可用 errors.Is 识别，ErrInvalidSnapshot 为 false；
	// 现有链同样保持原状。
	readFault := errors.New("simulated storage read failure")
	half := before[:len(before)/2]
	err := target.Restore(&failAfterReader{data: []byte(half), err: readFault})
	fmt.Println("操作 7：读取途中存储故障（与非法快照区分）")
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  errors.Is(err, 读取错误)=%v errors.Is(err, ErrInvalidSnapshot)=%v；链顶仍为 tip=%d，导出未变=%v\n",
		errors.Is(err, readFault), errors.Is(err, indexroom.ErrInvalidSnapshot),
		target.Tip, exportString(target) == before)
}
```

对应输出（`go run ./examples/snapshotencoding` 的实际输出，每次运行逐字一致）：

```text
操作 1：恢复手写版本 1 快照（中文、表情符号、U+FFFD，字面与 Unicode 转义混用）
  输入文本：{"version":1,"tip":2,"blocks":[{"height":1,"hash":"中文","parent":"起点","txs":["tx-\ud83d\ude00","�","\ufffd","","tx-😀"]},{"height":2,"hash":"\ud83d\ude00","parent":"\u4e2d\u6587","txs":["记账"]}]}
  恢复后链上实际保存的标识（Go 解码后的原值）：
    高度1 hash="中文" parent="起点" txs=["tx-😀" "�" "�" "" "tx-😀"]
    高度2 hash="😀" parent="中文" txs=["记账"]
  字面字符与等价转义不会变成两个不同标识；重复出现与块内位置原样保留：
  查询标识 "tx-😀"：TotalMatches=2，命中 高度1/位置0 高度1/位置4
  查询标识 "�"：TotalMatches=2，命中 高度1/位置1 高度1/位置2
  查询标识 "记账"：TotalMatches=1，命中 高度2/位置0
  查询标识 "tx-\\ud83d\\ude00"：TotalMatches=0，命中（无命中）

操作 2：再次导出（转义写法改变，但数据一致）
  再次导出：{"version":1,"tip":2,"blocks":[{"height":1,"hash":"中文","parent":"起点","txs":["tx-😀","�","�","","tx-😀"]},{"height":2,"hash":"😀","parent":"中文","txs":["记账"]}]}
  与输入逐字节一致：false（输入用了 Unicode 转义，导出直接写出字符）
  数据仍一致的核对：  查询标识 "tx-😀"：TotalMatches=2，命中 高度1/位置0 高度1/位置4

操作 3：恢复版本 2 快照（编码规则与版本 1 相同，timestamp 照常保留）
  恢复后 txs=["�" "付款"]；再次导出：{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":["�","付款"],"timestamp":0}]}
  查询标识 "�"：TotalMatches=1，命中 高度1/位置0
  查询标识 "付款"：TotalMatches=1，命中 高度1/位置1

操作 4：现有链（非法恢复的目标，恢复前后都应保持原状）
  {"version":1,"tip":1,"blocks":[{"height":1,"hash":"keep-1","parent":"genesis","txs":["付款","😀"]}]}
  查询标识 "付款"：TotalMatches=1，命中 高度1/位置0

操作 5：标准库解码器会怎样“容错”（Restore 不采用这种行为）
  含非法字节的 JSON 字符串（Go 引号形式 "x\xff"，坏字节显示为 \xff）直接解码会变成 "x�"
  含孤立高代理项的 JSON 文本 "b\ud83d" 直接解码会变成 "b�"
  坏字节/孤立代理项都被悄悄替换成 U+FFFD，会让不同标识无法区分；
  真正写入的 U+FFFD（包括它的合法转义写法）是正常字符，与这种替换无关。

操作 6：两类编码非法的快照都被整体拒绝（错误定位到高度、字段与位置）
  v1 高度2 交易标识0 含非法 UTF-8 字节：
    err=indexroom: invalid snapshot: block at height 2: field "txs" element 0 has invalid UTF-8 or unpaired surrogate escapes
    errors.Is(err, ErrInvalidSnapshot)=true；拒绝后链顶仍为1且导出未变=true
  v1 高度2 哈希含非法 UTF-8 字节：
    err=indexroom: invalid snapshot: block at height 2: field "hash" has invalid UTF-8 or unpaired surrogate escapes
    errors.Is(err, ErrInvalidSnapshot)=true；拒绝后链顶仍为1且导出未变=true
  v1 高度2 交易标识0 只有高代理项（缺少紧跟的低代理项）：
    err=indexroom: invalid snapshot: block at height 2: field "txs" element 0 has invalid UTF-8 or unpaired surrogate escapes
    errors.Is(err, ErrInvalidSnapshot)=true；拒绝后链顶仍为1且导出未变=true
  v1 高度2 父哈希是孤立的低代理项：
    err=indexroom: invalid snapshot: block at height 2: field "parent" has invalid UTF-8 or unpaired surrogate escapes
    errors.Is(err, ErrInvalidSnapshot)=true；拒绝后链顶仍为1且导出未变=true
  v2 高度2 交易标识0 代理项对顺序颠倒（低代理项在前）：
    err=indexroom: invalid snapshot: block at height 2: field "txs" element 0 has invalid UTF-8 or unpaired surrogate escapes
    errors.Is(err, ErrInvalidSnapshot)=true；拒绝后链顶仍为1且导出未变=true

  拒绝后的现有链核对（原交易、位置都在；快照里的新交易没有出现）：
  查询标识 "付款"：TotalMatches=1，命中 高度1/位置0
  查询标识 "😀"：TotalMatches=1，命中 高度1/位置1
  查询标识 "新交易"：TotalMatches=0，命中（无命中）
  链顶 tip=1；再次导出与恢复前逐字节一致：true

操作 7：读取途中存储故障（与非法快照区分）
  err=indexroom: read snapshot: field "blocks": simulated storage read failure
  errors.Is(err, 读取错误)=true errors.Is(err, ErrInvalidSnapshot)=false；链顶仍为 tip=1，导出未变=true
```

### 恢复与分页游标

- **恢复失败**（快照被拒或读取出错）：链没有变化，失败前取得的游标仍可用于原链，按[分页查询的既有规则](#游标失效区分-errquerychanged-与-errinvalidargument)继续翻页。
- **恢复成功**：旧游标不是整体作废，而是遵循同一套固定范围规则继续校验——固定范围内的区块内容变了（即使只是时间变化），或链顶降到该查询固定上界以下，续查返回 `ErrQueryChanged` 且没有可用页结果；此时应**从空游标重新开始查询**，不要把新页拼到旧结果后面。若恢复后的链在固定范围内与首页完全相同且链顶仍覆盖该范围，游标仍可正常续查。

### 完整示例：整体替换、非法拒绝与游标

下面的程序只使用现有公开功能，在本机离线即可运行，源码位于 [`examples/restore/main.go`](examples/restore/main.go)：

```bash
go run ./examples/restore
```

场景：先准备一条 4 个区块的较长旧链（全部无时间，导出为版本 1）；再用 `Export` 从另一条 2 个区块的较短新链取得版本 2 快照——高度 1 时间为 0、高度 2 缺失时间（`timestamp` 分别为 `0` 和 `null`）。程序先展示拼接两份文档与**最后一个区块父哈希错误**两种输入被 `ErrInvalidSnapshot` 拒绝、链与游标保持原状（并演示读取途中故障保留原始读取错误）；再用合法快照恢复，输出恢复前后的链顶、交易查询与再次导出内容，可以直接看到旧链多出的高度 3、4 和被替换的交易已经消失，最后演示成功恢复后旧游标返回 `ErrQueryChanged`。本示例的快照由本功能自己 `Export` 产生且恢复后未再修改，所以其中"再次导出与输入逐字节一致"成立——它是上面[充分条件](#数据一致不等于文本一致)的具体演示，不是对任意合法输入的保证。

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

### 补充示例：为什么再导出可能与输入文本不同

下面的程序同样只使用现有公开功能、在本机离线即可运行，源码位于 [`examples/snapshottext/main.go`](examples/snapshottext/main.go)：

```bash
go run ./examples/snapshottext
```

场景：一份**手写**的单区块版本 2 快照，交易列表为 `["甲", "", "甲"]`。输入故意采用与本功能导出不同的三种等价文本写法——打乱对象字段顺序、加入排版空白、把位置 2 的"甲"写成 Unicode 转义 `\u7532`——用来观察恢复后查询、再次导出与时间统计的实际结果：

- 恢复后查询"甲"命中块内位置 0 和 2，空标识、重复出现与交易顺序全部保留——标识按**解码后的原值**精确匹配，与输入用字面量还是 `\uXXXX` 转义无关；
- 再次导出是字段归位、无空白、无转义的规范化紧凑文本，与输入**字节不同但数据一致**；区块与交易列表仍按原序保留，换顺序的只是对象字段；
- 同一个区块把 `timestamp` 在 `null` 与 `0` 间切换：`null` 恢复为缺失时间（`Time == nil`），全链无时间，再导出降为**版本 1**（区块不含 `timestamp`）；`0` 恢复为真实零秒，再导出为**版本 2** 且 `timestamp` 仍为 `0`。在同一个包含零秒的窗口 `[0, 60)` 上，前者不计交易且缺失时间区块数为 1，后者三条交易各计一次出现、缺失时间区块数为 0——版本变化不是丢失时间，`null` 与 `0` 也不可互换；
- 最后验证"允许换写法不放宽校验"：顶层重复字段、对象后追加第二份文档都以 `ErrInvalidSnapshot` 拒绝，已有主链保持原状。

```go
// 快照的“数据一致”与“文本一致”示例：一份手写的单区块版本 2 快照，
// 交易列表为 ["甲", "", "甲"]，但故意使用与导出不同的文本写法——调整对象
// 字段顺序、加入排版空白，并把其中一次“甲”写成等价的 Unicode 转义
// （\u7532，解码后即“甲”）。恢复后按解码后的原值精确查询（两次“甲”位于块内位置 0 和 2，
// 空标识、重复出现与交易顺序都保留），再次导出则规范化为紧凑字节，与输入
// 文本不同但数据一致。同一份区块把 timestamp 在 null 与 0 之间切换：
// null（缺失时间）恢复后再导出降为版本 1，0（真实零秒）再导出为版本 2；
// 同一个包含零秒的时间窗口展示二者在统计上的区别。最后演示：允许更换文本
// 写法不代表放宽校验——重复字段与对象后追加第二份文档仍以
// ErrInvalidSnapshot 拒绝，已有主链保持原状。
//
// 运行：go run ./examples/snapshottext
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// exportString 返回当前主链再次导出的快照文本。
func exportString(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// printTxs 查询标识 jia 的每次出现并打印块内位置，同时直接核对链上保存的
// 交易列表：空标识、重复出现与原始顺序都必须原样保留。
func printTxs(index *indexroom.Index) {
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 1, TxIDs: []string{"甲"}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询“甲”：TotalMatches=%d，命中位置", page.TotalMatches)
	for _, hit := range page.Hits {
		fmt.Printf(" %d", hit.Position)
	}
	fmt.Println()
	fmt.Printf("  链上保存的交易列表（Go 解码后的原值）：%q，长度 %d\n",
		index.Blocks[1].Txs, len(index.Blocks[1].Txs))
}

// printWindow 在同一个包含零秒的窗口 [0, 60)（步长 60）上打印统计：
// 零秒区块贡献三次交易出现，缺失时间的区块只计入缺失时间区块数。
func printWindow(index *indexroom.Index) {
	stats, err := index.QueryTimeStats(indexroom.TimeStatsQuery{
		From: 1, To: 1, Start: 0, End: 60, StepSeconds: 60,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  时间窗口 [0, 60)：段 [0, 60) 交易出现=%d 不同标识=%d 含匹配区块=%d；缺失时间区块=%d\n",
		stats.Buckets[0].TxCount, stats.Buckets[0].DistinctTxIDs, stats.Buckets[0].Blocks,
		stats.MissingTimeBlocks)
}

func main() {
	// 1. 手写一份合法的版本 2 单区块快照（timestamp 为 null）。
	//    与本功能的导出文本相比，它有三处“换写法、不换数据”：
	//      - 对象字段打乱顺序（blocks 提前，块内 timestamp 提前）；
	//      - 加入缩进、换行等排版空白；
	//      - 位置 2 的“甲”写成等价的 Unicode 转义 \u7532。
	//    交易列表 ["甲", "", "甲"] 的空标识与重复出现都在数组里按原序保留。
	inputNull := `{
  "blocks": [
    {
      "timestamp": null,
      "txs": ["甲", "", "\u7532"],
      "parent": "genesis",
      "hash": "b1",
      "height": 1
    }
  ],
  "tip": 1,
  "version": 2
}`
	fmt.Println("操作 1：恢复手写快照（字段换序、排版空白、一次“甲”写成 \\u7532，timestamp 为 null）")
	fmt.Println("输入文本：")
	fmt.Println(inputNull)
	idx := indexroom.New()
	if err := idx.Restore(strings.NewReader(inputNull)); err != nil {
		panic(err)
	}
	fmt.Println("恢复后的查询结果（按解码后的原值精确匹配，与转义写法无关）：")
	printTxs(idx)
	fmt.Println()

	// 2. 再次导出：导出只保留规范化后的紧凑字节，字段顺序、空白与转义写法
	//    都不会从输入保留；同时全链缺失时间，版本从 2 降为 1（无 timestamp）。
	//    这不是“丢失时间”：输入 timestamp 本来就是 null（缺失时间）。
	exportedV1 := exportString(idx)
	fmt.Println("操作 2：再次导出（文本变了：字段归位、无空白、去转义；版本随全链无时间降为 1）")
	fmt.Printf("  再次导出：%s\n", exportedV1)
	fmt.Printf("  与输入文本逐字节一致：%v（数据一致，但文本不一致）\n", exportedV1 == inputNull)
	fmt.Printf("  恢复后区块时间：Time=%v（null 恢复为缺失时间，不是 0）\n", idx.Blocks[1].Time)
	printWindow(idx)
	fmt.Println()

	// 3. 同一个区块，只把 timestamp 从 null 改为数字 0：0 是真实的零秒时间，
	//    与缺失时间严格不同。恢复后再导出回到版本 2，timestamp 仍为 0。
	inputZero := strings.Replace(inputNull, `"timestamp": null`, `"timestamp": 0`, 1)
	fmt.Println("操作 3：同一快照把 timestamp 从 null 改为 0（真实的 Unix 零秒）")
	fmt.Printf("输入文本：%s\n", strings.ReplaceAll(inputZero, "\n", ""))
	if err := idx.Restore(strings.NewReader(inputZero)); err != nil {
		panic(err)
	}
	exportedV2 := exportString(idx)
	fmt.Printf("  再次导出：%s\n", exportedV2)
	fmt.Printf("  与输入文本逐字节一致：%v（版本 2、timestamp 仍为 0，但空白/字段顺序/转义未保留）\n",
		exportedV2 == inputZero)
	fmt.Printf("  恢复后区块时间：Time 非空=%v，*Time=%d（0 恢复为真实的零秒时间）\n",
		idx.Blocks[1].Time != nil, *idx.Blocks[1].Time)
	printTxs(idx)
	// 同一个窗口：零秒区块落入 [0, 60)，三条交易各计一次出现；
	// 缺失时间区块数为 0。对比操作 2 可知 null 与 0 不可互换。
	printWindow(idx)
	fmt.Println()

	// 4. 允许更换文本写法，仍然要求字段合法且不重复：把顶层 version 写两遍
	//    是非法快照。恢复被整体拒绝，操作 3 的主链保持原状。
	dupField := strings.Replace(inputZero, `"version": 2`, `"version": 2, "version": 2`, 1)
	before := exportString(idx)
	err := idx.Restore(strings.NewReader(dupField))
	fmt.Printf("操作 4：顶层 version 字段重复后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v；主链未改变=%v（仍为操作 3 的版本 2 快照）\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), exportString(idx) == before)

	// 5. 输入必须恰好是一份快照：在对象后追加第二份文档仍被拒绝，
	//    哪怕两份文档各自都合法；已有主链同样保持原状。
	twoDocs := exportedV2 + exportedV2
	err = idx.Restore(strings.NewReader(twoDocs))
	fmt.Printf("操作 5：对象后追加第二份文档后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v；主链未改变=%v，链顶仍为 tip=%d\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), exportString(idx) == before, idx.Tip)
}
```

对应输出（`go run ./examples/snapshottext` 的实际输出，每次运行逐字一致）：

```text
操作 1：恢复手写快照（字段换序、排版空白、一次“甲”写成 \u7532，timestamp 为 null）
输入文本：
{
  "blocks": [
    {
      "timestamp": null,
      "txs": ["甲", "", "\u7532"],
      "parent": "genesis",
      "hash": "b1",
      "height": 1
    }
  ],
  "tip": 1,
  "version": 2
}
恢复后的查询结果（按解码后的原值精确匹配，与转义写法无关）：
  查询“甲”：TotalMatches=2，命中位置 0 2
  链上保存的交易列表（Go 解码后的原值）：["甲" "" "甲"]，长度 3

操作 2：再次导出（文本变了：字段归位、无空白、去转义；版本随全链无时间降为 1）
  再次导出：{"version":1,"tip":1,"blocks":[{"height":1,"hash":"b1","parent":"genesis","txs":["甲","","甲"]}]}
  与输入文本逐字节一致：false（数据一致，但文本不一致）
  恢复后区块时间：Time=<nil>（null 恢复为缺失时间，不是 0）
  时间窗口 [0, 60)：段 [0, 60) 交易出现=0 不同标识=0 含匹配区块=0；缺失时间区块=1

操作 3：同一快照把 timestamp 从 null 改为 0（真实的 Unix 零秒）
输入文本：{  "blocks": [    {      "timestamp": 0,      "txs": ["甲", "", "\u7532"],      "parent": "genesis",      "hash": "b1",      "height": 1    }  ],  "tip": 1,  "version": 2}
  再次导出：{"version":2,"tip":1,"blocks":[{"height":1,"hash":"b1","parent":"genesis","txs":["甲","","甲"],"timestamp":0}]}
  与输入文本逐字节一致：false（版本 2、timestamp 仍为 0，但空白/字段顺序/转义未保留）
  恢复后区块时间：Time 非空=true，*Time=0（0 恢复为真实的零秒时间）
  查询“甲”：TotalMatches=2，命中位置 0 2
  链上保存的交易列表（Go 解码后的原值）：["甲" "" "甲"]，长度 3
  时间窗口 [0, 60)：段 [0, 60) 交易出现=3 不同标识=2 含匹配区块=1；缺失时间区块=0

操作 4：顶层 version 字段重复后 Restore：err=indexroom: invalid snapshot: duplicate field "version"
  errors.Is(err, ErrInvalidSnapshot)=true；主链未改变=true（仍为操作 3 的版本 2 快照）

操作 5：对象后追加第二份文档后 Restore：err=indexroom: invalid snapshot: trailing data after the snapshot object
  errors.Is(err, ErrInvalidSnapshot)=true；主链未改变=true，链顶仍为 tip=1
```

## 技术方向

blockchain-indexer, tx-indexer, onchain-analytics, tx-decoder, data-indexer, metrics, block-explorer

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
