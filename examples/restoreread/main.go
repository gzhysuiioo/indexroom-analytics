// 快照恢复（Index.Restore）的输入结束与读取故障完整示例：围绕同一份小快照
// 对照三种结局——正常结束（底层直接返回 io.EOF）可恢复、截断后正常结束返回
// ErrInvalidSnapshot、完整字节伴随读取故障返回读取失败。随后展示故障与内容
// 问题的优先关系（已收到的故障优先；先发现的内容问题按非法快照结束，不继续
// 读）、版本 1 与版本 2 遵循同一规则，以及失败后链顶、交易查询与恢复前取得
// 的游标全部保持原状，成功时链被快照整体替换。
//
// 运行：go run ./examples/restoreread
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// 同一份小快照的两个布局版本：一个区块、一笔交易 snap-tx。
const snapshotV1 = `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"g1","parent":"g","txs":["snap-tx"]}]}`
const snapshotV2 = `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"g1","parent":"g","txs":["snap-tx"],"timestamp":null}]}`

// unknownFieldDoc 的顶层多一个未知字段 extra，本身是一份非法快照。
const unknownFieldDoc = `{"version":1,"tip":0,"blocks":[],"extra":0}`

// oneShotReader 在一次 Read 中同时交出 data 与 err，之后永远返回正常的
// io.EOF，模拟“同一批字节与故障一起送达”的底层流。
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
	n := copy(p, r.data)
	return n, r.err
}

// scriptReader 按脚本逐段送出内容：每段可以只带字节、只带错误，或两者
// 兼有；脚本用完后以正常的 io.EOF 结束。
type scriptReader struct {
	chunks []scriptChunk
}

type scriptChunk struct {
	data string
	err  error
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	n := copy(p, chunk.data)
	return n, chunk.err
}

// isReadFailure 报告 err 是否是一次读取失败：错误信息以
// "indexroom: read snapshot: " 开头，原始读取错误保留在错误链中。
func isReadFailure(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "indexroom: read snapshot: ")
}

// newOriginalChain 返回一条三个区块的原链，交易依次为 a、b、c。
func newOriginalChain() *indexroom.Index {
	index := indexroom.New()
	for _, block := range []indexroom.Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"b"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"c"}},
	} {
		if err := index.Append(block); err != nil {
			panic(err)
		}
	}
	return index
}

// countTx 返回若干标识在主链上的命中总数。
func countTx(index *indexroom.Index, txIDs ...string) int64 {
	page, err := index.QueryTxs(indexroom.TxQuery{TxIDs: txIDs})
	if err != nil {
		panic(err)
	}
	return page.TotalMatches
}

func main() {
	storageFault := errors.New("simulated storage failure")

	// 操作 1：准备原链（3 个区块，交易依次为 a、b、c），并取得一个每页 1 条
	// 的续查游标，留给各种失败之后核对：失败不能改变链，也不能弄坏旧游标。
	index := newOriginalChain()
	first, err := index.QueryTxs(indexroom.TxQuery{PageSize: 1})
	if err != nil {
		panic(err)
	}
	cursor := first.NextCursor
	fmt.Println("操作 1：原链与游标")
	fmt.Printf("  链顶 tip=%d；首页命中 高度%d/%s，续查游标已保存（不透明，不展示内容）\n\n",
		index.Tip, first.Hits[0].Height, first.Hits[0].TxID)

	// 操作 2：同一份被截断的字节，两种结束方式对应两种分类。
	// 正常结束（底层直接返回 io.EOF）说明输入确实只有这些——内容不完整，
	// 是 ErrInvalidSnapshot；底层主动报告 io.ErrUnexpectedEOF 则是读取失败。
	half := snapshotV1[:len(snapshotV1)/2]
	fmt.Println("操作 2：截断后的同一批字节，两种结束方式")
	err = index.Restore(strings.NewReader(half))
	fmt.Printf("  正常结束（strings.Reader 直接返回 io.EOF）：\n    err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err))
	err = index.Restore(&oneShotReader{data: []byte(half), err: io.ErrUnexpectedEOF})
	fmt.Printf("  同一批字节伴随 io.ErrUnexpectedEOF：\n    err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v errors.Is(io.ErrUnexpectedEOF)=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err),
		errors.Is(err, io.ErrUnexpectedEOF))

	// 操作 3：完整合法的字节与故障在同一次读取中送达——故障不能被忽略，
	// 即使那批字节组成完整快照，恢复仍然失败。版本 1、2 遵循同一规则。
	joined := errors.Join(io.EOF, storageFault)
	fmt.Println("操作 3：完整快照字节伴随 errors.Join(io.EOF, 存储故障)（同一次读取送达）")
	for _, tc := range []struct{ name, doc string }{
		{"版本 1", snapshotV1},
		{"版本 2", snapshotV2},
	} {
		err = index.Restore(&oneShotReader{data: []byte(tc.doc), err: joined})
		fmt.Printf("  %s：err=%v\n", tc.name, err)
		fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v errors.Is(io.EOF)=%v errors.Is(存储故障)=%v\n",
			errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err),
			errors.Is(err, io.EOF), errors.Is(err, storageFault))
	}
	fmt.Println("  errors.Is 能匹配 io.EOF 不等于正常结束：只有底层直接返回的 io.EOF 才是")
	fmt.Println()

	// 操作 4：包装过的 io.EOF 同样是读取失败，不能当成正常结束。
	wrapped := fmt.Errorf("storage at EOF: %w", io.EOF)
	err = index.Restore(&oneShotReader{data: []byte(snapshotV1), err: wrapped})
	fmt.Println("操作 4：完整快照字节伴随包装过的 io.EOF（fmt.Errorf 的 %w 包装）")
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  ErrInvalidSnapshot=%v 读取失败=%v errors.Is(io.EOF)=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err), errors.Is(err, io.EOF))

	// 操作 5：已收到故障的那批字节同时暴露内容问题（这里是未知字段）时，
	// 仍返回读取失败，而不是 ErrInvalidSnapshot——流已失败，无法证明这些
	// 字节就是完整输入。对照：同样的字节正常结束时才按非法快照拒绝。
	fmt.Println("操作 5：含未知字段的字节与故障同批送达")
	err = index.Restore(&oneShotReader{data: []byte(unknownFieldDoc), err: storageFault})
	fmt.Printf("  伴随故障：err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v errors.Is(存储故障)=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err), errors.Is(err, storageFault))
	err = index.Restore(strings.NewReader(unknownFieldDoc))
	fmt.Printf("  对照：同一文本正常结束：err=%v\n", err)
	fmt.Printf("    ErrInvalidSnapshot=%v 读取失败=%v\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), isReadFailure(err))

	// 操作 6：优先关系只适用于已经收到的故障。未知字段在一次无故障读取中
	// 先被发现时，恢复按非法快照结束，不会为了寻找后续可能出现的读取错误
	// 继续取数据——排在其后的故障根本不会被读到。
	fmt.Println("操作 6：未知字段先在无故障读取中被发现（故障排在其后）")
	err = index.Restore(&scriptReader{chunks: []scriptChunk{
		{data: unknownFieldDoc}, // 无故障读取，未知字段在此被发现
		{err: storageFault},     // 这段故障永远不会被读到
	}})
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  ErrInvalidSnapshot=%v errors.Is(存储故障)=%v（没有为找故障继续读）\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), errors.Is(err, storageFault))

	// 操作 7：以上失败都不改变现有链：链顶与交易查询保持原状，恢复前取得
	// 的游标仍能续查原链（不是 ErrQueryChanged）。
	fmt.Println("操作 7：全部失败之后核对原链")
	fmt.Printf("  链顶仍为 tip=%d；旧交易 a/b/c 命中=%d，快照交易 snap-tx 命中=%d\n",
		index.Tip, countTx(index, "a", "b", "c"), countTx(index, "snap-tx"))
	fmt.Printf("  恢复前取得的游标续查：")
	query := indexroom.TxQuery{PageSize: 1, Cursor: cursor}
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			panic(err)
		}
		for _, hit := range page.Hits {
			fmt.Printf("高度%d/%s ", hit.Height, hit.TxID)
		}
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	fmt.Println("（游标照常读出原链其余记录）")
	fmt.Println()

	// 操作 8：同一份快照以正常结束送达时恢复成功，链被快照整体替换：
	// 旧交易 a/b/c 消失，快照交易 snap-tx 出现；版本 2 同样如此。
	fmt.Println("操作 8：同一份快照正常结束（底层直接返回 io.EOF）")
	err = index.Restore(strings.NewReader(snapshotV1))
	fmt.Printf("  恢复版本 1：err=%v，链顶 tip=%d；a/b/c 命中=%d，snap-tx 命中=%d\n",
		err, index.Tip, countTx(index, "a", "b", "c"), countTx(index, "snap-tx"))
	err = index.Restore(strings.NewReader(snapshotV2))
	fmt.Printf("  恢复版本 2：err=%v，链顶 tip=%d；a/b/c 命中=%d，snap-tx 命中=%d\n",
		err, index.Tip, countTx(index, "a", "b", "c"), countTx(index, "snap-tx"))
}
