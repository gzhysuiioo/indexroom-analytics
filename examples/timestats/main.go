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
