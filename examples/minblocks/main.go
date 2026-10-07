// 最少区块数门槛（TxQuery.MinBlocks）完整示例：门槛语义、按完整固定范围
// 计数、与标识筛选和时间窗口的组合、范围在第一页固定、负门槛与续查改变
// 门槛的 ErrInvalidArgument、无人达到门槛时的成功空页。
//
// 运行：go run ./examples/minblocks
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
	// 规格示例：高度一至三的交易依次为 [m,m,n]、[n,p]、[m,q]。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"m", "m", "n"}})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"n", "p"}})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"m", "q"}})
	fmt.Printf("链顶高度 tip=%d\n\n", index.Tip)

	// 门槛 2：m 出现在区块 1、3，n 出现在区块 1、2，都达到门槛；
	// p、q 各只出现在一个区块，不返回。达到门槛后保留每一次匹配出现：
	// m 的三次出现与 n 的两次出现共 5 条记录、3 个匹配区块。
	// 每页 2 条，分三页读完；统计是整个固定范围的统计，逐页一致。
	query := indexroom.TxQuery{MinBlocks: 2, PageSize: 2}
	page1, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("门槛 2、每页 2 条：第1页", page1)
	query.Cursor = page1.NextCursor
	page2, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("第2页（沿用同一门槛与原样游标）", page2)
	query.Cursor = page2.NextCursor
	page3, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("第3页（最后一页）", page3)
	fmt.Printf("最后一页后续游标为空：%v\n\n", page3.NextCursor == "")

	// 是否达到门槛按第一页固定下来的完整高度范围判断：范围只含前两个
	// 区块时，m 只出现在一个区块，只有 n 达到门槛。
	restricted, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 2, MinBlocks: 2})
	if err != nil {
		panic(err)
	}
	printPage("高度范围 [1,2]、门槛 2（只有 n 达到门槛）", restricted)
	fmt.Println()

	// MinBlocks 为零（未设置）时不启用门槛，沿用原查询结果：p、q 也返回。
	zero, err := index.QueryTxs(indexroom.TxQuery{PageSize: 10})
	if err != nil {
		panic(err)
	}
	printPage("未设置门槛（MinBlocks=0），同一链条", zero)
	fmt.Println()

	// 门槛与标识筛选共同生效：只有先通过 TxIDs 筛选的出现才参与计数。
	// 筛选 [m,q] 时 m 仍出现在两个区块达到门槛，q 只出现在一个区块。
	filtered, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"m", "q"}, MinBlocks: 2})
	if err != nil {
		panic(err)
	}
	printPage("筛选 [m,q]、门槛 2（q 不达到门槛）", filtered)
	fmt.Println()

	// 范围在第一页固定：先取第一页，再追加高度 4 [p,q,q]。此后 p 出现在
	// 区块 2、4，q 出现在区块 3、4——但只对新查询而言。进行中的翻页仍
	// 只看固定范围 1..3，新追加的区块不会帮助 p、q 在当前查询里达到门槛。
	pinned, err := index.QueryTxs(indexroom.TxQuery{MinBlocks: 2, PageSize: 3})
	if err != nil {
		panic(err)
	}
	mustAppend(index, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"p", "q", "q"}})
	fmt.Printf("已追加高度 4 [p,q,q]，当前链顶 tip=%d\n", index.Tip)
	continued, err := index.QueryTxs(indexroom.TxQuery{MinBlocks: 2, PageSize: 3, Cursor: pinned.NextCursor})
	if err != nil {
		panic(err)
	}
	printPage("追加后继续翻页（仍只看固定范围 1..3）", continued)
	fresh, err := index.QueryTxs(indexroom.TxQuery{MinBlocks: 2, PageSize: 20})
	if err != nil {
		panic(err)
	}
	printPage("空游标重新查询（p、q 现在都达到门槛）", fresh)
	fmt.Println()

	// 参数类错误（ErrInvalidArgument）：负门槛、续查改变门槛，都没有可用页。
	negPage, negErr := index.QueryTxs(indexroom.TxQuery{MinBlocks: -1})
	fmt.Printf("负门槛：ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(negErr, indexroom.ErrInvalidArgument), len(negPage.Hits))
	changedPage, changedErr := index.QueryTxs(indexroom.TxQuery{MinBlocks: 1, PageSize: 3, Cursor: pinned.NextCursor})
	fmt.Printf("续查把门槛从 2 改为 1：ErrInvalidArgument=%v 可用命中数=%d\n\n",
		errors.Is(changedErr, indexroom.ErrInvalidArgument), len(changedPage.Hits))

	// 合法门槛无人达到时不是错误：成功返回空页、零统计、空游标。
	none, err := index.QueryTxs(indexroom.TxQuery{MinBlocks: 5})
	if err != nil {
		panic(err)
	}
	fmt.Printf("门槛 5 无人达到：err=%v 命中=%d TotalMatches=%d MatchedBlocks=%d 游标为空=%v\n\n",
		err, len(none.Hits), none.TotalMatches, none.MatchedBlocks, none.NextCursor == "")

	demonstrateTimeWindow()
}

// demonstrateTimeWindow 在独立索引上展示门槛与时间窗口的关系：只有同时
// 落在高度范围、时间窗口内且通过标识筛选的区块才参与计数。启用窗口后，
// 缺失时间的区块不帮助标识达到门槛；不启用窗口时，缺失时间照常参与计数。
func demonstrateTimeWindow() {
	fmt.Println("---- 门槛与时间窗口共同生效 ----")
	// 高度 1 时间 100、高度 2 缺失时间、高度 3 时间 105，交易同主示例。
	timed := indexroom.New()
	mustAppend(timed, indexroom.Block{Height: 1, Hash: "w1", Parent: "genesis", Txs: []string{"m", "m", "n"}, Time: unix(100)})
	mustAppend(timed, indexroom.Block{Height: 2, Hash: "w2", Parent: "w1", Txs: []string{"n", "p"}})
	mustAppend(timed, indexroom.Block{Height: 3, Hash: "w3", Parent: "w2", Txs: []string{"m", "q"}, Time: unix(105)})

	// 窗口 [100,110) 内只有高度 1、3 参与计数：m 仍在两个区块达到门槛；
	// n 的第二次出现在缺失时间的高度 2，不帮助它达到门槛。
	inWindow, err := timed.QueryTxs(indexroom.TxQuery{
		MinBlocks: 2, TimeStart: unix(100), TimeEnd: unix(110),
	})
	if err != nil {
		panic(err)
	}
	printPage("窗口 [100,110)、门槛 2（高度 2 缺失时间不参与计数）", inWindow)

	// 不启用时间窗口时，缺失时间的高度 2 照常参与计数，n 达到门槛。
	noWindow, err := timed.QueryTxs(indexroom.TxQuery{MinBlocks: 2})
	if err != nil {
		panic(err)
	}
	printPage("不启用窗口、门槛 2（缺失时间照常参与计数）", noWindow)
}
