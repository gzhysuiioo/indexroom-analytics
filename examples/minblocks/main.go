// 最少区块数筛选（Index.QueryTxs 的 TxQuery.MinBlocks）完整示例：
// 门槛为零或不传时沿用原查询结果；正数要求同一标识至少出现在第一页固定的
// 完整高度范围内的指定数量个不同区块。同一区块内多次出现只贡献一个区块；
// 一旦达标，该标识的每一次匹配出现都保留，位置仍是原来的块内位置。是否达标
// 按完整固定范围判定而非当前页，TotalMatches/MatchedBlocks 描述筛选后的完整
// 范围，不随页大小或正倒序改变。示例还展示门槛与 TxIDs、时间窗口共同生效、
// 续查必须沿用同一门槛、第一页后新追加的更高区块不帮助当前查询中的标识达标、
// 负门槛与续查改变门槛返回 ErrInvalidArgument、合法门槛无人达标时成功返回空页。
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
	fmt.Println("---- 规格短链：高度 1..3 依次为 [m,m,n]、[n,p]、[m,q]，门槛 2 ----")
	// m 出现在区块 {1,3}、n 出现在 {1,2}，都达到门槛 2；p、q 各只在一个
	// 区块出现，不达标、不返回。m 在高度 1 的块内出现两次，达标后两次都保留。
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"m", "m", "n"}})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"n", "p"}})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"m", "q"}})

	// 第一页：MinBlocks=2，每页 2 条，范围显式固定在高度 [1,3]。
	query := indexroom.TxQuery{From: 1, To: 3, MinBlocks: 2, PageSize: 2}
	page1, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("门槛 2、每页 2 条：第1页", page1)

	// 继续翻页：游标原样传回，门槛（及其他条件）必须与第一页一致。
	query.Cursor = page1.NextCursor
	page2, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("第2页（沿用同一门槛与游标）", page2)

	query.Cursor = page2.NextCursor
	page3, err := index.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	printPage("第3页", page3)
	fmt.Printf("最后一页后续游标为空：%v\n", page3.NextCursor == "")
	fmt.Println("  三次出现的 m（高度1位置0、1，高度3位置0）与两次出现的 n（高度1位置2、高度2位置0）全部保留，共 5 条、3 个匹配区块")
	fmt.Println()

	// 整范围判定与页大小、方向无关：一次取完的倒序查询给出同样的 5 条记录与
	// 5/3 统计；每条 Position 仍是原块内位置，倒序不重新编号。
	desc, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, MinBlocks: 2, Order: indexroom.OrderDesc})
	if err != nil {
		panic(err)
	}
	fmt.Print("倒序一次取完：")
	for i, hit := range desc.Hits {
		if i > 0 {
			fmt.Print("；")
		}
		fmt.Printf("高度%d %q 位置%d", hit.Height, hit.TxID, hit.Position)
	}
	fmt.Printf("\n  TotalMatches=%d MatchedBlocks=%d，与正序逐页翻页完全一致\n\n", desc.TotalMatches, desc.MatchedBlocks)

	// 高度范围只包含前两个区块时，m 在范围内只出现在高度 1（一个区块），
	// 不达标；n 在高度 1、2 各出现一次，是唯一达到门槛的标识。
	firstTwo, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 2, MinBlocks: 2})
	if err != nil {
		panic(err)
	}
	printPage("范围只含高度 [1,2]、门槛 2（只有 n 达标）", firstTwo)
	fmt.Println()

	// MinBlocks 为零或不传时不启用筛选，沿用原查询：7 次出现全部返回。
	plain, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3})
	if err != nil {
		panic(err)
	}
	fmt.Printf("不设门槛（MinBlocks=0）：命中 %d 条，TotalMatches=%d MatchedBlocks=%d\n\n",
		len(plain.Hits), plain.TotalMatches, plain.MatchedBlocks)

	// 负门槛是非法查询参数，没有可用页结果。
	bad, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, MinBlocks: -1})
	fmt.Printf("负门槛 -1：ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(err, indexroom.ErrInvalidArgument), len(bad.Hits))

	// 续查改变门槛同样是 ErrInvalidArgument：门槛在第一页钉住，要改只能
	// 从空游标重新查询。
	first, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, MinBlocks: 2, PageSize: 2})
	if err != nil {
		panic(err)
	}
	changed, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, MinBlocks: 1, PageSize: 2, Cursor: first.NextCursor})
	fmt.Printf("续查把门槛从 2 改成 1：ErrInvalidArgument=%v 可用命中数=%d\n",
		errors.Is(err, indexroom.ErrInvalidArgument), len(changed.Hits))

	// 合法门槛但没有任何标识达到：成功的空页，零统计、空游标。
	none, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 3, MinBlocks: 4})
	if err != nil {
		panic(err)
	}
	fmt.Printf("门槛 4（无人达到）：err=%v 命中=%d TotalMatches=%d MatchedBlocks=%d 有后续游标=%v\n\n",
		err, len(none.Hits), none.TotalMatches, none.MatchedBlocks, none.NextCursor != "")

	// 第一页固定完整高度范围：第一页之后追加的更高区块在本次翻页中既不可见，
	// 也不会帮助任何标识达标。To 留空表示取首查看到的链顶（此时为 3）。
	pinned, err := index.QueryTxs(indexroom.TxQuery{MinBlocks: 2, PageSize: 2})
	if err != nil {
		panic(err)
	}
	mustAppend(index, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"p"}})
	fmt.Printf("第一页后追加高度 4（含 p；p 原本只在高度 2 出现），当前链顶 tip=%d\n", index.Tip)
	cont := indexroom.TxQuery{MinBlocks: 2, PageSize: 2, Cursor: pinned.NextCursor}
	kept := append([]indexroom.TxHit{}, pinned.Hits...)
	statsHold := true
	for {
		page, err := index.QueryTxs(cont)
		if err != nil {
			panic(err)
		}
		kept = append(kept, page.Hits...)
		if page.TotalMatches != 5 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			statsHold = false
		}
		if page.NextCursor == "" {
			break
		}
		cont.Cursor = page.NextCursor
	}
	fmt.Print("沿用旧游标翻完：命中")
	for _, hit := range kept {
		fmt.Printf(" 高度%d/%q@%d", hit.Height, hit.TxID, hit.Position)
	}
	fmt.Printf("\n  共 %d 条，全部不超过高度 3，p 未出现；每页统计都是 5/3、ToHeight=3：%v\n", len(kept), statsHold)

	// 需要新区块参与达标判定时，从空游标重新查询：p 现在出现在区块 {2,4}，
	// 也达到门槛。
	restart, err := index.QueryTxs(indexroom.TxQuery{MinBlocks: 2})
	if err != nil {
		panic(err)
	}
	fmt.Printf("空游标重新查询（范围重新固定到链顶 4）：命中 %d 条，TotalMatches=%d MatchedBlocks=%d ToHeight=%d\n\n",
		len(restart.Hits), restart.TotalMatches, restart.MatchedBlocks, restart.ToHeight)

	fmt.Println("---- 门槛与 TxIDs 筛选、时间窗口共同生效 ----")
	// 高度 1 时间 100、高度 2 缺失时间、高度 3 时间 105，三个区块都含 k 与 r。
	win := indexroom.New()
	mustAppend(win, indexroom.Block{Height: 1, Hash: "w1", Parent: "genesis", Txs: []string{"k", "r"}, Time: unix(100)})
	mustAppend(win, indexroom.Block{Height: 2, Hash: "w2", Parent: "w1", Txs: []string{"k", "r"}})
	mustAppend(win, indexroom.Block{Height: 3, Hash: "w3", Parent: "w2", Txs: []string{"k", "r"}, Time: unix(105)})

	// 启用窗口 [100,110)：高度 2 缺失时间，不参与计数、其中的出现也不返回；
	// k 只靠窗口内的区块 {1,3} 计数，恰好达到门槛 2，保留这两次出现。
	// r 虽在三个区块都有，但不通过 TxIDs 筛选，根本不参与计数。
	enabled, err := win.QueryTxs(indexroom.TxQuery{
		TxIDs: []string{"k"}, TimeStart: unix(100), TimeEnd: unix(110), MinBlocks: 2,
	})
	if err != nil {
		panic(err)
	}
	printPage("窗口 [100,110)、筛选 k、门槛 2（缺失时间的高度2不帮忙）", enabled)

	// 窗口收窄为 [100,105)：高度 3 时间正好等于被排除的终点，窗口内只剩
	// 高度 1，没有人达到门槛——成功的空页、零统计、空游标。
	narrow, err := win.QueryTxs(indexroom.TxQuery{
		TxIDs: []string{"k"}, TimeStart: unix(100), TimeEnd: unix(105), MinBlocks: 2,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("窗口 [100,105)（高度2缺失时间、高度3在被排除的终点）：命中=%d TotalMatches=%d MatchedBlocks=%d 有后续游标=%v\n",
		len(narrow.Hits), narrow.TotalMatches, narrow.MatchedBlocks, narrow.NextCursor != "")

	// 不启用时间窗口时，缺失时间不会仅因没有时间而被排除：三个区块都参与
	// 计数，k 达到门槛，三次出现（含高度 2）全部保留。
	off, err := win.QueryTxs(indexroom.TxQuery{TxIDs: []string{"k"}, MinBlocks: 2})
	if err != nil {
		panic(err)
	}
	fmt.Printf("不启用窗口、筛选 k、门槛 2：命中=%d TotalMatches=%d MatchedBlocks=%d（缺失时间的高度2也参与）\n",
		len(off.Hits), off.TotalMatches, off.MatchedBlocks)
}
