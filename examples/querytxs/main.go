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
