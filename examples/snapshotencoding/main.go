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
