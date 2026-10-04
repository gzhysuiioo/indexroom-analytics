// 快照恢复（Index.Export / Index.Restore）完整示例：用导出的较短新链快照
// 整体替换较长旧链，展示替换前后的链顶、交易查询与再次导出；再展示最后一个
// 区块父哈希错误的快照被整体拒绝（ErrInvalidSnapshot）、链与游标保持原状，
// 以及读取输入失败与非法快照的区分、恢复成功后旧游标返回 ErrQueryChanged。
//
// 运行：go run ./examples/restore
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func mustAppend(index *indexroom.Index, block indexroom.Block) {
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间。
func unix(sec int64) *int64 { return &sec }

// querySet 同时包含旧链独有标识、新链独有标识与两链共有的标识，
// 让一次查询就能看出哪些交易被替换。
var querySet = []string{"alpha", "OLD-4", "beta"}

func exportString(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// dumpState 打印当前链顶、固定筛选下的交易查询首页和再次导出内容，
// 每次调用都看到一个完整的链状态。
func dumpState(index *indexroom.Index) {
	fmt.Printf("  链顶 tip=%d\n", index.Tip)
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 4, TxIDs: querySet})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询高度 [1, 4] 内标识 %v（空游标首页，ToHeight=%d）：TotalMatches=%d MatchedBlocks=%d\n",
		querySet, page.ToHeight, page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf("    命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
	fmt.Printf("  再次导出：%s\n", exportString(index))
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

func main() {
	// 1. 预先准备一条较长的旧链（4 个区块，全部没有时间，导出为版本 1）。
	target := indexroom.New()
	mustAppend(target, indexroom.Block{Height: 1, Hash: "s1", Parent: "genesis", Txs: []string{"alpha", "OLD-1"}})
	mustAppend(target, indexroom.Block{Height: 2, Hash: "s2", Parent: "s1", Txs: []string{"alpha"}})
	mustAppend(target, indexroom.Block{Height: 3, Hash: "s3", Parent: "s2", Txs: []string{"OLD-3"}})
	mustAppend(target, indexroom.Block{Height: 4, Hash: "s4", Parent: "s3", Txs: []string{"OLD-4", "alpha"}})
	fmt.Println("操作 1：准备较长旧链（4 个区块）后的状态")
	oldExport := exportString(target)
	dumpState(target)
	fmt.Println()

	// 2. 用现有导出功能从另一条较短的新链取得快照（2 个区块）。
	//    高度 1 时间为 0（真实时间戳），高度 2 没有时间；只要有一个区块带时间，
	//    导出即为版本 2，每块都带 timestamp：0 与 null 同时出现。
	source := indexroom.New()
	mustAppend(source, indexroom.Block{Height: 1, Hash: "n1", Parent: "genesis", Txs: []string{"alpha"}, Time: unix(0)})
	mustAppend(source, indexroom.Block{Height: 2, Hash: "n2", Parent: "n1", Txs: []string{"beta"}})
	snapshot := exportString(source)
	fmt.Printf("操作 2：从较短新链（2 个区块）Export 出快照\n  %s\n\n", snapshot)

	// 3. 在旧链上取得两个分页游标（同一固定范围 [1, 4]、同一筛选，每页 2 条），
	//    分别留给“恢复被拒绝”和“恢复成功”之后续查。
	query := indexroom.TxQuery{From: 1, To: 4, TxIDs: querySet, PageSize: 2}
	firstA, err := target.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	firstB, err := target.QueryTxs(query)
	if err != nil {
		panic(err)
	}
	cursorAfterReject := firstA.NextCursor
	cursorAfterRestore := firstB.NextCursor
	fmt.Printf("操作 3：旧链固定范围 [1, 4] 首页（每页 2 条）命中：")
	for _, hit := range firstA.Hits {
		fmt.Printf("%d/%s ", hit.Height, hit.TxID)
	}
	fmt.Printf("剩余记录由游标续查（游标不透明，不展示内容）\n\n")

	// 4a. 拒绝情形之一：把两份快照文档拼在一起不是合法输入，
	//     只允许一份完整 JSON 文档（结尾可以有空白）。
	err = target.Restore(strings.NewReader(snapshot + snapshot))
	fmt.Printf("操作 4a：拼接两份文档后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v，链顶仍为 tip=%d\n\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), target.Tip)

	// 4b. 拒绝情形之二：最后一个区块的父哈希被改错。前一个区块完全合法，
	//     但整份快照必须在应用前全部校验通过，因此不会有任何部分导入。
	broken := strings.Replace(snapshot, `"parent":"n1"`, `"parent":"WRONG-PARENT"`, 1)
	err = target.Restore(strings.NewReader(broken))
	fmt.Printf("操作 4b：最后一个区块父哈希错误后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v（具体原因已在错误信息中指出）\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Println("拒绝后的状态（应与操作 1 完全一致，不能出现新链区块）：")
	dumpState(target)
	fmt.Printf("  导出内容与恢复前逐字节一致：%v\n", exportString(target) == oldExport)
	// 恢复失败前取得的游标仍可用于原链，继续读出旧链高度 4 的两条记录。
	cont, err := target.QueryTxs(indexroom.TxQuery{
		From: 1, To: 4, TxIDs: querySet, PageSize: 2, Cursor: cursorAfterReject,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  旧游标续查成功（err=%v），本页命中：", err)
	for _, hit := range cont.Hits {
		fmt.Printf("%d/%s ", hit.Height, hit.TxID)
	}
	fmt.Println()
	fmt.Println()

	// 4c. 区分读取输入发生的错误：读到一半底层 reader 故障。
	//     原始读取错误仍可用 errors.Is 识别，它不是 ErrInvalidSnapshot；
	//     这种情况同样不改变现有链。
	readProblem := errors.New("simulated storage read failure")
	err = target.Restore(&failAfterReader{data: []byte(snapshot[:len(snapshot)/2]), err: readProblem})
	fmt.Printf("操作 4c：读取途中故障后 Restore：err=%v\n", err)
	fmt.Printf("  errors.Is(err, 读取错误)=%v errors.Is(err, ErrInvalidSnapshot)=%v 链顶仍为 tip=%d\n\n",
		errors.Is(err, readProblem), errors.Is(err, indexroom.ErrInvalidSnapshot), target.Tip)

	// 5. 成功恢复：输入是一份完整 JSON 快照，结尾允许空白（这里追加一个换行）。
	//    Restore 用快照整体替换主链，不与旧链合并。
	err = target.Restore(strings.NewReader(snapshot + "\n"))
	fmt.Printf("操作 5：用合法短链快照（结尾带换行）Restore：err=%v\n", err)
	fmt.Println("成功恢复后的状态（旧链多出的高度 3、4 与被替换的交易应全部消失）：")
	dumpState(target)
	fmt.Printf("  再次导出与操作 2 的快照逐字节一致：%v\n", exportString(target) == snapshot)
	// 旧游标遵循既有固定范围规则：固定上界为 4，链顶已降到 2，续查返回
	// ErrQueryChanged 而不是部分数据；应从空游标重新开始查询。
	_, err = target.QueryTxs(indexroom.TxQuery{
		From: 1, To: 4, TxIDs: querySet, PageSize: 2, Cursor: cursorAfterRestore,
	})
	fmt.Printf("  旧游标续查：errors.Is(err, ErrQueryChanged)=%v errors.Is(err, ErrInvalidArgument)=%v\n",
		errors.Is(err, indexroom.ErrQueryChanged), errors.Is(err, indexroom.ErrInvalidArgument))
	restart, err := target.QueryTxs(indexroom.TxQuery{From: 1, To: 4, TxIDs: querySet, PageSize: 2})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  从空游标重新开始：ToHeight=%d 命中：", restart.ToHeight)
	for _, hit := range restart.Hits {
		fmt.Printf("%d/%s ", hit.Height, hit.TxID)
	}
	fmt.Printf("有后续游标=%v\n", restart.NextCursor != "")
}
