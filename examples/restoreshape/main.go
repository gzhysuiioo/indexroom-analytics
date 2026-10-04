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
