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
