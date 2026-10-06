// 快照恢复的“输入结束”语义示例：同一份小快照，在三种输入结束方式下对照
// Index.Restore 的结果分类——
//
//  1. 完整合法快照在底层直接返回裸 io.EOF 时正常结束：恢复成功，主链被快照
//     整体替换，恢复前取得的游标按既有规则返回 ErrQueryChanged；
//  2. 截断的不完整 JSON 同样以裸 io.EOF 正常结束：流没有故障，但内容不是
//     一份完整快照，返回 ErrInvalidSnapshot，链与恢复前游标保持原状；
//  3. 完整快照字节与读取故障在同一次读取中一起到达（io.ErrUnexpectedEOF、
//     包装过的 io.EOF、errors.Join(io.EOF, 自定义故障)）：故障不能被字节
//     抵消，即使 errors.Is 能匹配 io.EOF，也属于读取失败而不是成功。
//
// 随后演示故障与内容问题（未知字段）的优先级：同一批已报告故障的字节即使
// 同时暴露内容问题，也返回读取失败；但内容问题先在一次无故障读取中被发现
// 时，立即以 ErrInvalidSnapshot 结束，不会继续读取去寻找后续故障。版本 1、
// 版本 2 对以上规则行为一致。
//
// 运行：go run ./examples/restorereader
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// errStorage 模拟与 EOF 无关的底层存储读取故障，调用方用 errors.Is 识别它。
var errStorage = errors.New("simulated storage fault")

// snapV1 与 snapV2 是同一份两区块小快照的两个版本布局，也是所有情形围绕的
// 同一份数据；文本本身就是规范化紧凑写法。
const snapV1 = `{"version":1,"tip":2,"blocks":` +
	`[{"height":1,"hash":"n1","parent":"g","txs":["pay"]},` +
	`{"height":2,"hash":"n2","parent":"n1","txs":["pay","new-tx"]}]}`

const snapV2 = `{"version":2,"tip":2,"blocks":` +
	`[{"height":1,"hash":"n1","parent":"g","txs":["pay"],"timestamp":100},` +
	`{"height":2,"hash":"n2","parent":"n1","txs":["pay","new-tx"],"timestamp":null}]}`

// contentBad 是一份含未知顶层字段的文档；它在“故障优先”与“先发现先拒绝”
// 两个对照情形里使用完全相同的字节。
const contentBad = `{"version":1,"tip":0,"blocks":[],"extra":0}`

// queryIDs 同时覆盖旧链独有、新链独有与新旧共有的交易标识，一次查询即可
// 看出链是否被替换。
var queryIDs = []string{"old-tx", "new-tx", "pay"}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// freshChain 构造每个情形共用的三区块旧链（全部无时间，导出为版本 1）。
func freshChain() *indexroom.Index {
	idx := indexroom.New()
	must(idx.Append(indexroom.Block{Height: 1, Hash: "o1", Parent: "g", Txs: []string{"old-tx"}}))
	must(idx.Append(indexroom.Block{Height: 2, Hash: "o2", Parent: "o1", Txs: []string{"old-tx"}}))
	must(idx.Append(indexroom.Block{Height: 3, Hash: "o3", Parent: "o2", Txs: []string{"old-tx", "tail"}}))
	return idx
}

func exportString(idx *indexroom.Index) string {
	var buf bytes.Buffer
	must(idx.Export(&buf))
	return buf.String()
}

// queryPage 在固定范围 [1, 3]、每页 2 条下查询；cursor 为空取首页，否则
// 续查恢复前取得的游标。
func queryPage(idx *indexroom.Index, cursor string) indexroom.TxPage {
	page, err := idx.QueryTxs(indexroom.TxQuery{
		From: 1, To: 3, TxIDs: queryIDs, PageSize: 2, Cursor: cursor,
	})
	must(err)
	return page
}

func hitsText(page indexroom.TxPage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ToHeight=%d TotalMatches=%d 命中：", page.ToHeight, page.TotalMatches)
	if len(page.Hits) == 0 {
		b.WriteString("<无>")
		return b.String()
	}
	for i, hit := range page.Hits {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d/%s@%d", hit.Height, hit.TxID, hit.Position)
	}
	return b.String()
}

// oneShotReader 在第一次 Read 时把全部 data 连同 err 一起返回，之后一律
// 返回裸 io.EOF，模拟底层“字节与故障同批到达、随后流正常结束”。
type oneShotReader struct {
	done bool
	data []byte
	err  error
}

func (r *oneShotReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	return n, r.err
}

// scriptChunk 是一次 Read 固定返回的字节与错误。
type scriptChunk struct {
	data string
	err  error
}

// scriptReader 每个 chunk 恰好对应一次 Read，chunk 用完后返回裸 io.EOF；
// faultReads 统计真正交付了故障的读取次数，用来证明恢复没有向前多读。
type scriptReader struct {
	chunks     []scriptChunk
	faultReads int
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	if chunk.err != nil {
		r.faultReads++
	}
	n := copy(p, chunk.data)
	return n, chunk.err
}

// wrappedEOF 是包装过的 io.EOF：errors.Is 命中 io.EOF，但它不是底层直接
// 返回的那个裸哨兵。
type wrappedEOF struct{}

func (wrappedEOF) Error() string { return "storage layer reported end: " + io.EOF.Error() }
func (wrappedEOF) Unwrap() error { return io.EOF }

// reportRestore 打印恢复结果与错误分类：成功、ErrInvalidSnapshot，还是保留
// 原始读取错误的读取失败。
func reportRestore(err error) {
	fmt.Printf("  Restore 返回 err=%v\n", oneLineErr(err))
	if err == nil {
		fmt.Println("  分类：恢复成功（err == nil）")
		return
	}
	fmt.Printf("  errors.Is：ErrInvalidSnapshot=%v、io.EOF=%v、io.ErrUnexpectedEOF=%v、原始存储故障=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, errStorage))
	fmt.Printf("  错误信息以 \"indexroom: read snapshot:\" 开头：%v\n",
		strings.HasPrefix(err.Error(), "indexroom: read snapshot:"))
}

func oneLineErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), "\n", " | "))
}

func main() {
	fmt.Println("准备：三个区块的旧链，与同一份两区块小快照")
	base := freshChain()
	fmt.Printf("  旧链：tip=%d %s\n", base.Tip, exportString(base))
	first := queryPage(base, "")
	fmt.Printf("  旧链固定范围 [1, 3] 首页（每页 2 条）%s，其余记录由游标续查\n", hitsText(first))
	fmt.Printf("  小快照（版本 1）：%s\n", snapV1)
	fmt.Printf("  小快照（版本 2）：%s\n\n", snapV2)

	// 情形 1：完整合法快照 + 正常结束（strings.Reader 耗尽后直接返回裸
	// io.EOF）。恢复成功，链被快照整体替换。
	fmt.Println("情形 1：完整合法快照，底层直接返回裸 io.EOF（正常结束）")
	idx := freshChain()
	cursor := queryPage(idx, "").NextCursor
	err := idx.Restore(strings.NewReader(snapV1))
	reportRestore(err)
	page := queryPage(idx, "")
	fmt.Printf("  恢复后：tip=%d；空游标查询 %s\n", idx.Tip, hitsText(page))
	fmt.Printf("  再次导出：%s\n", exportString(idx))
	_, err = idx.QueryTxs(indexroom.TxQuery{
		From: 1, To: 3, TxIDs: queryIDs, PageSize: 2, Cursor: cursor,
	})
	fmt.Printf("  恢复前游标续查：ErrQueryChanged=%v（链已被整体替换，应从空游标重新查询）\n\n",
		errors.Is(err, indexroom.ErrQueryChanged))

	// 情形 2：截断 JSON + 正常结束。同一份快照只送到第一区块之后，流以裸
	// io.EOF 结束：没有读取故障，但文档不完整，属于非法快照。
	fmt.Println("情形 2：截断的不完整 JSON，底层仍直接返回裸 io.EOF（正常结束）")
	idx = freshChain()
	cursor = queryPage(idx, "").NextCursor
	half := snapV1[:strings.Index(snapV1, `{"height":2`)]
	fmt.Printf("  截断前缀：%s\n", half)
	err = idx.Restore(strings.NewReader(half))
	reportRestore(err)
	cont := queryPage(idx, cursor)
	fmt.Printf("  链顶仍为 tip=%d；恢复前游标续查原链成功：%s\n\n", idx.Tip, hitsText(cont))

	// 情形 3：完整合法字节与故障同一次读取到达。字节本身已组成完整快照，
	// 但故障已被报告，恢复仍失败；三种故障形式都不是 ErrInvalidSnapshot。
	fmt.Println("情形 3：完整快照字节与读取故障同批到达（故障不能被字节抵消）")
	cases := []struct {
		name  string
		fault error
	}{
		{"3a 底层直接返回 io.ErrUnexpectedEOF", io.ErrUnexpectedEOF},
		{"3b errors.Join(io.EOF, 自定义读取错误)", errors.Join(io.EOF, errStorage)},
		{"3c 包装过的 io.EOF（errors.Is 命中 io.EOF，但不是裸哨兵）", wrappedEOF{}},
	}
	for _, tc := range cases {
		idx = freshChain()
		err = idx.Restore(&oneShotReader{data: []byte(snapV1), err: tc.fault})
		fmt.Printf("  %s\n", tc.name)
		reportRestore(err)
		fmt.Printf("  链顶仍为 tip=%d，完整快照没有被应用\n", idx.Tip)
	}
	fmt.Println()

	// 情形 4：故障与内容问题同时暴露时的优先级。
	fmt.Println("情形 4：已收到的读取故障优先于同一批字节暴露的内容问题")
	idx = freshChain()
	err = idx.Restore(&oneShotReader{data: []byte(contentBad), err: errStorage})
	fmt.Printf("  4a 同批字节既含未知字段又报告故障（字节：%s）\n", contentBad)
	reportRestore(err)
	fmt.Printf("  链顶仍为 tip=%d（返回读取失败，而不是 ErrInvalidSnapshot）\n", idx.Tip)

	idx = freshChain()
	script := &scriptReader{chunks: []scriptChunk{
		{data: contentBad}, // 第一次读取：完整字节、无故障
		{err: errStorage},  // 下一次读取才会交付故障
	}}
	err = idx.Restore(script)
	fmt.Println("  4b 同样字节在无故障读取中先暴露未知字段（故障要下一次读取才发生）")
	reportRestore(err)
	fmt.Printf("  含故障的后续读取实际发生次数：%d（不继续读去寻找故障）；链顶仍为 tip=%d\n\n",
		script.faultReads, idx.Tip)

	// 情形 5：版本 1、版本 2 规则相同：伴随故障都是读取失败，同样字节在裸
	// io.EOF 正常结束时都能恢复成功。
	fmt.Println("情形 5：版本 1 与版本 2 遵循同一规则")
	for _, doc := range []struct {
		name string
		text string
	}{
		{"版本 1", snapV1},
		{"版本 2", snapV2},
	} {
		idx = freshChain()
		err = idx.Restore(&oneShotReader{data: []byte(doc.text), err: errors.Join(io.EOF, errStorage)})
		fmt.Printf("  %s 完整字节伴随 errors.Join(io.EOF, 故障)：err=%v；ErrInvalidSnapshot=%v；tip 仍为 %d\n",
			doc.name, oneLineErr(err), errors.Is(err, indexroom.ErrInvalidSnapshot), idx.Tip)
		err = idx.Restore(strings.NewReader(doc.text))
		fmt.Printf("  %s 同样字节以裸 io.EOF 正常结束：err=%v；恢复成功，tip=%d\n", doc.name, err, idx.Tip)
	}
}
