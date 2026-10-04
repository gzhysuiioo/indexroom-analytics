// 快照导出与恢复（Index.Export / Index.Restore）完整示例：用现有导出功能
// 取得一份较短新链的快照（含缺失时间与零时间区块），整体替换预先准备的
// 较长旧链，对照恢复前后的链顶、交易查询与再次导出；演示最后一个区块父
// 哈希错误的非法快照与读取输入失败的区别（都不改变现有链），以及恢复
// 失败前与成功后分页游标的行为。
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

// export 把当前主链导出为一份快照并返回其字节内容。
func export(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// printQuery 打印一次交易查询的命中情况（单页足够放下全部命中）。
func printQuery(index *indexroom.Index, txID string) {
	page, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{txID}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询 %q：命中 %d 条", txID, len(page.Hits))
	for _, hit := range page.Hits {
		fmt.Printf(" height=%d", hit.Height)
	}
	fmt.Println()
}

// errDisk 模拟读取输入时发生的底层错误。
var errDisk = errors.New("disk read failed")

// failingReader 的 Read 总是返回 errDisk，模拟损坏的输入来源。
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errDisk }

func main() {
	// 预先准备的较长旧链：5 个区块，都没有时间；标识 old-a 出现在高度 1、3、5。
	old := indexroom.New()
	mustAppend(old, indexroom.Block{Height: 1, Hash: "o1", Parent: "genesis", Txs: []string{"old-a"}})
	mustAppend(old, indexroom.Block{Height: 2, Hash: "o2", Parent: "o1", Txs: []string{"old-b"}})
	mustAppend(old, indexroom.Block{Height: 3, Hash: "o3", Parent: "o2", Txs: []string{"old-a"}})
	mustAppend(old, indexroom.Block{Height: 4, Hash: "o4", Parent: "o3", Txs: []string{"old-c"}})
	mustAppend(old, indexroom.Block{Height: 5, Hash: "o5", Parent: "o4", Txs: []string{"old-a"}})

	fmt.Println("[1] 恢复前的旧链")
	fmt.Printf("  链顶 tip=%d\n", old.Tip)
	printQuery(old, "old-a")
	beforeExport := export(old)
	fmt.Printf("  导出（version=1，所有区块都没有时间）：%s\n", beforeExport)

	// 在任何恢复之前取得一个分页游标：每页 2 条，第 1 页读到高度 1、3。
	page1, err := old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"old-a"}, PageSize: 2})
	if err != nil {
		panic(err)
	}
	cursor := page1.NextCursor
	fmt.Printf("  翻页第 1 页命中 %d 条，取得续查游标（固定范围上界 ToHeight=%d）\n\n",
		len(page1.Hits), page1.ToHeight)

	// 较短的新链：3 个区块。高度 1 时间为 0（真实零时间），高度 2 没有时间，
	// 高度 3 时间为 1700000000。任一块有时间即导出为版本 2，每块都带 timestamp。
	fresh := indexroom.New()
	mustAppend(fresh, indexroom.Block{Height: 1, Hash: "n1", Parent: "genesis", Txs: []string{"new-a"}, Time: unix(0)})
	mustAppend(fresh, indexroom.Block{Height: 2, Hash: "n2", Parent: "n1", Txs: []string{"new-b"}})
	mustAppend(fresh, indexroom.Block{Height: 3, Hash: "n3", Parent: "n2", Txs: []string{"new-a"}, Time: unix(1700000000)})
	snapshot := export(fresh)
	fmt.Println("[2] 用现有导出功能取得较短新链的快照")
	fmt.Printf("  新链链顶 tip=%d\n", fresh.Tip)
	fmt.Printf("  快照（version=2）：%s\n", snapshot)
	fmt.Println("  高度 1 的 timestamp 为 0（真实零时间），高度 2 为 null（缺失），二者不能互换")
	fmt.Println()

	// 非法快照：把最后一个区块（高度 3）的父哈希改错。即使前两个区块完全
	// 合法，恢复也被整体拒绝，前面的区块不会被部分导入。
	bad := strings.Replace(snapshot, `"parent":"n2"`, `"parent":"nX"`, 1)
	fmt.Println("[3] 恢复一份最后一个区块父哈希错误的快照（被拒绝）")
	err = old.Restore(strings.NewReader(bad))
	fmt.Printf("  Restore 返回：%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v\n", errors.Is(err, indexroom.ErrInvalidSnapshot))
	fmt.Printf("  拒绝后链顶 tip=%d（不变）\n", old.Tip)
	printQuery(old, "old-a")
	fmt.Printf("  再次导出与恢复前一致：%v\n\n", export(old) == beforeExport)

	// 恢复失败前取得的游标仍可用于原链，继续读到高度 5 的 old-a。
	page2, err := old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"old-a"}, PageSize: 2, Cursor: cursor})
	if err != nil {
		panic(err)
	}
	fmt.Println("[4] 恢复失败前取得的游标仍可用于原链")
	fmt.Printf("  续查第 2 页：err=%v 命中 %d 条", err, len(page2.Hits))
	for _, hit := range page2.Hits {
		fmt.Printf(" height=%d", hit.Height)
	}
	fmt.Println()
	fmt.Println()

	// 读取输入发生的错误与非法快照不同：原样保留底层错误，可用 errors.Is
	// 命中调用方自己的错误；它同样不改变现有链。
	fmt.Println("[5] 读取输入失败（不是非法快照）")
	err = old.Restore(failingReader{})
	fmt.Printf("  Restore 返回：%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidSnapshot)=%v，errors.Is(err, errDisk)=%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), errors.Is(err, errDisk))
	fmt.Printf("  链仍未改变：tip=%d\n\n", old.Tip)

	// 成功恢复：快照整体替换主链，链顶从 5 降到快照末尾高度 3。
	fmt.Println("[6] 把新链快照恢复到旧链索引（成功）")
	if err := old.Restore(strings.NewReader(snapshot)); err != nil {
		panic(err)
	}
	fmt.Printf("  恢复后链顶 tip=%d（快照末尾高度，旧链多出的高度 4、5 已消失）\n", old.Tip)
	printQuery(old, "old-a")
	printQuery(old, "new-a")
	fmt.Printf("  高度 1 时间=%d（真实零时间），高度 2 时间缺失=%v\n",
		*old.Blocks[1].Time, old.Blocks[2].Time == nil)
	afterExport := export(old)
	fmt.Printf("  再次导出：%s\n", afterExport)
	fmt.Printf("  再次导出与恢复的快照逐字节一致：%v\n\n", afterExport == snapshot)

	// 恢复成功后，旧游标按固定范围规则失效：范围内数据已变，且链顶 3 低于
	// 固定上界 5，续查返回 ErrQueryChanged，应从空游标重新开始。
	fmt.Println("[7] 恢复成功后旧游标失效，应重新开始查询")
	_, err = old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"old-a"}, PageSize: 2, Cursor: cursor})
	fmt.Printf("  旧游标续查：ErrQueryChanged=%v\n", errors.Is(err, indexroom.ErrQueryChanged))
	restart, err := old.QueryTxs(indexroom.TxQuery{TxIDs: []string{"new-a"}, PageSize: 10})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  从空游标重新开始查询 %q：命中 %d 条", "new-a", len(restart.Hits))
	for _, hit := range restart.Hits {
		fmt.Printf(" height=%d", hit.Height)
	}
	fmt.Println()
	fmt.Println()

	// 输入必须是恰好一份快照对象：结尾允许空白，拼接两份文档会被拒绝。
	fmt.Println("[8] 输入必须是恰好一份完整快照")
	err = old.Restore(strings.NewReader(snapshot + "  \n"))
	fmt.Printf("  快照后跟随空白：err=%v（接受）\n", err)
	err = old.Restore(strings.NewReader(snapshot + "\n" + snapshot))
	fmt.Printf("  拼接两份快照：ErrInvalidSnapshot=%v，原因：%v\n",
		errors.Is(err, indexroom.ErrInvalidSnapshot), err)
}
