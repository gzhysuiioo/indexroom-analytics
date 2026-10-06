// 快照字符串标识的编码规则示例：恢复一份两区块的小快照，区块哈希、父哈希与
// 每个交易标识都经历同一套编码检查（版本 1、版本 2 规则相同）。手写快照里，
// 中文、表情符号与真正的 U+FFFD 直接写出；同一个字符也可以写成等价的
// \uXXXX 转义（包括表情符号的成对代理转义）——恢复后二者是同一个标识，
// 查询按解码后的值精确匹配，同一交易的多次出现与块内位置全部保留；再次导出
// 会把转义改写成字面字符，文本变化不代表数据损坏。
//
// 随后在保留现有链的前提下分别尝试两类非法快照：其一是字符串中含非法 UTF-8
// 原始字节（0xFF），其二是 Unicode 转义中出现孤立高代理项（\uD83D 后没有
// 紧接着的低代理项）。两类都以 ErrInvalidSnapshot 被整体拒绝，错误指出所属
// 高度、字段（交易标识还指出从零开始的位置）；前面的合法区块不会部分生效，
// 拒绝后链顶与原交易查询结果保持原状。这类拒绝不同于底层读取输入失败。
//
// 运行：go run ./examples/snapshotencoding
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// u 拼出 JSON 的“反斜杠-u”十六进制转义文本（共六个字符，如 u("7532")
// 解码后即“甲”），在运行期拼接，让手写快照里同一字符同时有“字面写出”和
// “转义写出”两种写法可对照。
func u(hex4 string) string { return `\u` + hex4 }

// 下面两份手写快照刻意混用等价文本写法：
//   - 区块 1 的哈希、父哈希与 txs[0] 中的“甲”直接写出；txs[1] 的同一个字
//     写成 u("7532")（“甲”的反斜杠-u 转义）；
//   - 区块 2 的哈希、父哈希全部写成反斜杠-u 转义（父哈希解码后必须等于
//     区块 1 的字面哈希，父链接同样按解码后的值匹配）；
//   - txs[1] 的表情符号直接写出，txs[2] 用成对代理转义
//     u("d83d")+u("de00") 表示同一个字符；
//   - txs[3] 是字面写出的真正 U+FFFD（�），txs[4] 写成 u("fffd")。
//
// “付款-甲”共出现三次：区块 1 位置 0、1 与区块 2 位置 0。
var goodV2 = `{"version":2,"tip":2,"blocks":[` +
	`{"height":1,"hash":"区块-甲","parent":"起点-甲","txs":["付款-甲","付款-` + u("7532") + `"],"timestamp":100},` +
	`{"height":2,"hash":"` + u("533a") + u("5757") + `-` + u("4e59") + `","parent":"` + u("533a") + u("5757") + `-` + u("7532") + `",` +
	`"txs":["付款-甲","付款-😀","付款-` + u("d83d") + u("de00") + `","替换符-�","替换符-` + u("fffd") + `"],"timestamp":200}` +
	`]}`

// goodV1 是同一份标识数据的版本 1 文本（无 timestamp），证明两个版本的
// 字符串编码规则完全相同。
var goodV1 = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"区块-甲","parent":"起点-甲","txs":["付款-甲","付款-` + u("7532") + `"]},` +
	`{"height":2,"hash":"` + u("533a") + u("5757") + `-` + u("4e59") + `","parent":"` + u("533a") + u("5757") + `-` + u("7532") + `",` +
	`"txs":["付款-甲","付款-😀","付款-` + u("d83d") + u("de00") + `","替换符-�","替换符-` + u("fffd") + `"]}` +
	`]}`

// exportString 返回当前主链再次导出的快照文本。
func exportString(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// dumpIdentifiers 打印当前链上实际保存的哈希、父哈希与每个交易标识及其
// 块内位置：这些是 JSON 解码后的值，与输入用字面字符还是 \uXXXX 转义无关。
func dumpIdentifiers(index *indexroom.Index) {
	for height := int64(1); height <= index.Tip; height++ {
		block := index.Blocks[height]
		fmt.Printf("    height=%d hash=%q parent=%q\n", height, block.Hash, block.Parent)
		for position, tx := range block.Txs {
			fmt.Printf("      txs[%d]=%q\n", position, tx)
		}
	}
}

// printQueries 对给定标识逐个做精确查询，打印总命中数与每次出现的位置。
func printQueries(index *indexroom.Index, txIDs ...string) {
	for _, txID := range txIDs {
		page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 2, TxIDs: []string{txID}, PageSize: 50})
		if err != nil {
			panic(err)
		}
		fmt.Printf("  查询 %q：TotalMatches=%d\n", txID, page.TotalMatches)
		for _, hit := range page.Hits {
			fmt.Printf("    命中 height=%d block=%q tx=%q position=%d\n",
				hit.Height, hit.BlockHash, hit.TxID, hit.Position)
		}
	}
}

// printState 打印拒绝前后用于比对的完整可观察状态：链顶、三个既有标识的
// 查询结果，以及只出现在被拒快照高度 1 里的“哨兵”标识——若它出现，就说明
// 发生了部分生效；0 次命中才是整份快照被拒绝。
func printState(index *indexroom.Index) {
	fmt.Printf("  链顶 tip=%d\n", index.Tip)
	printQueries(index, "付款-甲", "付款-😀", "替换符-�")
	printQueries(index, "BAD-SHOULD-NOT-APPEAR")
}

func main() {
	idx := indexroom.New()

	// 1. 恢复手写的版本 2 小快照。区块哈希、父哈希和每个交易标识都接受
	//    同一套编码检查；直接写出的字符与表示同一字符的合法 \uXXXX 转义
	//    （含表情符号的成对代理转义）解码后是同一个标识。
	fmt.Println("操作 1：恢复手写的两区块版本 2 小快照（字面字符与等价 Unicode 转义混用）")
	fmt.Printf("输入文本：%s\n", goodV2)
	if err := idx.Restore(strings.NewReader(goodV2)); err != nil {
		panic(err)
	}
	fmt.Println("恢复后实际保存的标识（解码后的值；字面与转义不产生两个标识）：")
	dumpIdentifiers(idx)
	fmt.Println()

	// 2. 查询按解码后的值精确匹配：同一标识的多次出现（含同一区块内的
	//    不同位置）全部命中；真正的 U+FFFD 只是一个普通合法标识。
	fmt.Println("操作 2：按解码后的值精确查询（同一标识的每次出现与块内位置都保留）")
	printQueries(idx, "付款-甲", "付款-😀", "替换符-�")
	fmt.Println()

	// 3. 同一份标识数据以版本 1 文本再恢复一次：版本 1 与版本 2 对
	//    哈希、父哈希和交易标识使用完全相同的编码规则。
	fmt.Println("操作 3：用同一份标识数据的版本 1 文本再恢复（版本 1、2 的编码规则相同）")
	if err := idx.Restore(strings.NewReader(goodV1)); err != nil {
		panic(err)
	}
	fmt.Println("恢复后实际保存的标识（与操作 1 完全相同，仅时间字段随版本缺失）：")
	dumpIdentifiers(idx)
	fmt.Println()

	// 4. 再次导出：导出按本功能的规范化方式写字面字符，输入里的反斜杠-u
	//    转义（包括 u("7532") 与成对代理转义）和 u("fffd") 都不再以转义
	//    形式出现。文本写法变了，但标识、出现次数与位置等数据一字未变，
	//    这不是数据损坏；用规范化文本再恢复再导出，则字节稳定一致。
	exported := exportString(idx)
	fmt.Println("操作 4：再次导出（转义写法被规范化；文本可变、数据不变）")
	fmt.Printf("  再次导出：%s\n", exported)
	fmt.Printf("  与含转义的输入逐字节一致=%v（预期 false：数据应按解码后的标识核对，而非字节）\n", exported == goodV1)
	if err := idx.Restore(strings.NewReader(exported)); err != nil {
		panic(err)
	}
	fmt.Printf("  规范化文本再恢复后再次导出，与上一次导出逐字节一致=%v\n\n", exportString(idx) == exported)

	// 5. 拒绝类一：后面区块（高度 2）的 hash 字符串里带非法 UTF-8 原始
	//    字节 0xFF。标准库 JSON 解码会把它静默替换成 U+FFFD，若放行，不同
	//    的坏标识会塌缩成同一个值，因此整份快照被拒绝，而不是替换后导入。
	//    高度 1 自身虽然合法，也不会部分生效：它的交易里放了哨兵标识。
	badUTF8 := `{"version":2,"tip":2,"blocks":[` +
		`{"height":1,"hash":"BAD-1","parent":"g","txs":["付款-甲","BAD-SHOULD-NOT-APPEAR"],"timestamp":1},` +
		`{"height":2,"hash":"b2-` + "\xff" + `","parent":"BAD-1","txs":["x"],"timestamp":2}` +
		`]}`
	fmt.Println("操作 5：尝试非法 UTF-8 原始字节（高度 2 的 hash 中含字节 0xFF）")
	fmt.Printf("  被拒字段的原始字节（Go 引号语法）：%q\n", "b2-\xff")
	err := idx.Restore(strings.NewReader(badUTF8))
	fmt.Printf("  Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, indexroom.ErrInvalidSnapshot)=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Println("  拒绝后的状态（链顶、原交易查询保持原状；哨兵标识 0 次命中，说明高度 1 也未部分生效）：")
	printState(idx)
	fmt.Println()

	// 6. 拒绝类二：高度 2 的 txs[1] 以高代理项转义开头，后面没有紧接着的
	//    低代理项，字符串随即结束——孤立代理项不表示任何码点。错误同样
	//    指出高度、字段与从零开始的元素位置。
	loneSurrogate := `{"version":2,"tip":2,"blocks":[` +
		`{"height":1,"hash":"BAD-1","parent":"g","txs":["付款-甲","BAD-SHOULD-NOT-APPEAR"],"timestamp":1},` +
		`{"height":2,"hash":"BAD-2","parent":"BAD-1","txs":["付款-甲","付款-\ud83d"],"timestamp":2}` +
		`]}`
	fmt.Println(`操作 6：尝试孤立的高代理项转义（高度 2 的 txs[1]：\uD83D 后没有紧跟低代理项）`)
	err = idx.Restore(strings.NewReader(loneSurrogate))
	fmt.Printf("  Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, indexroom.ErrInvalidSnapshot)=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Println("  拒绝后的状态（与操作 5 之后完全一致，仍是操作 3/4 恢复的原链）：")
	printState(idx)
}
