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
