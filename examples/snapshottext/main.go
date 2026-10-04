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
