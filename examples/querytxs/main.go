// Command querytxs 是 Index.QueryTxs 分页交易查询的离线示例：
// 如何取第一页、如何继续翻页、何时结束、范围如何固定，以及游标
// 失效时 ErrQueryChanged 与 ErrInvalidArgument 的区别。
//
// 运行：go run ./examples/querytxs
package main

import (
	"errors"
	"fmt"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func main() {
	firstPage()
	continuePaging()
	doNotRewriteTo()
	rangeStaysPinnedAsChainGrows()
	filterSemantics()
	defaultsAndEmptyPage()
	changedInsideRange()
	tipBelowPinnedBound()
	invalidArguments()
}

// mustAppend 追加一个区块，失败即终止示例。
func mustAppend(index *indexroom.Index, height int64, hash, parent string, txs ...string) {
	block := indexroom.Block{Height: height, Hash: hash, Parent: parent, Txs: txs}
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

// demoChain 构建三个连续区块：
//
//	高度 1: [a, b, a]
//	高度 2: [a]
//	高度 3: [b]
func demoChain() *indexroom.Index {
	index := indexroom.New()
	mustAppend(index, 1, "h1", "genesis", "a", "b", "a")
	mustAppend(index, 2, "h2", "h1", "a")
	mustAppend(index, 3, "h3", "h2", "b")
	return index
}

func cursorMark(cursor string) string {
	if cursor == "" {
		return "<空>"
	}
	return "<非空不透明字符串>"
}

func printPage(label string, page indexroom.TxPage) {
	fmt.Println(label)
	fmt.Printf("  err=<nil> ToHeight=%d TotalMatches=%d MatchedBlocks=%d NextCursor=%s\n",
		page.ToHeight, page.TotalMatches, page.MatchedBlocks, cursorMark(page.NextCursor))
	fmt.Print("  命中:")
	if len(page.Hits) == 0 {
		fmt.Print(" <无>")
	}
	for _, hit := range page.Hits {
		fmt.Printf(" (height=%d hash=%s tx=%q position=%d)",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
	fmt.Println()
}

func printError(label string, err error, target error) {
	fmt.Printf("%s\n  err=%v\n  errors.Is(err, %v) = %v\n", label, err, target, errors.Is(err, target))
}

func firstPage() {
	fmt.Println("=== 1. 首次查询：Cursor 留空，取得第一页 ===")
	index := demoChain()
	// 当前链顶只有高度 3，请求却把结束高度写成 10。
	page, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}
	printPage("请求 To=10、筛选 a、每页 2 条、空游标：", page)
}

func continuePaging() {
	fmt.Println("=== 2. 带上一页返回的游标继续读取，直到 NextCursor 为空 ===")
	index := demoChain()
	first, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}
	// 继续请求仍写原来的 To=10；游标原样透传，当作不透明字符串使用。
	second, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
		Cursor:   first.NextCursor,
	})
	if err != nil {
		panic(err)
	}
	printPage("第二页（游标来自第一页）：", second)
	fmt.Println("  NextCursor 为空，翻页结束；不能再继续请求。")
}

func doNotRewriteTo() {
	fmt.Println("=== 3. 后续请求保留原结束高度 10，不能用返回的 ToHeight=3 替换 ===")
	index := demoChain()
	first, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}
	_, err = index.QueryTxs(indexroom.TxQuery{
		To:       first.ToHeight, // 错误示范：把钳制后的 3 当作新请求的 To
		TxIDs:    []string{"a"},
		PageSize: 2,
		Cursor:   first.NextCursor,
	})
	printError("误用 To=3 继续同一个游标：", err, indexroom.ErrInvalidArgument)
}

func rangeStaysPinnedAsChainGrows() {
	fmt.Println("=== 4. 第一页之后链顶增长，继续翻页仍只读原固定范围 ===")
	index := demoChain()
	first, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}
	// 第一页之后再追加一个含 a 的高度 4 区块。
	mustAppend(index, 4, "h4", "h3", "a")
	second, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
		Cursor:   first.NextCursor,
	})
	if err != nil {
		panic(err)
	}
	printPage("追加高度 4（含 a）后继续翻页：", second)
}

func filterSemantics() {
	fmt.Println("=== 5. 筛选集合：重复项与顺序不影响匹配，大小写与空白按原字符串区分 ===")
	index := demoChain()

	duplicated, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a", "a"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("筛选 [\"a\", \"a\"]：TotalMatches=%d（重复出现的交易各自计数，不合并）\n", duplicated.TotalMatches)

	reordered, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"b", "a", "a", "b"}})
	if err != nil {
		panic(err)
	}
	deduped, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a", "b"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("筛选 [\"b\", \"a\", \"a\", \"b\"]：TotalMatches=%d MatchedBlocks=%d\n",
		reordered.TotalMatches, reordered.MatchedBlocks)
	fmt.Printf("筛选 [\"a\", \"b\"]：     TotalMatches=%d MatchedBlocks=%d（同一集合，结果相同）\n",
		deduped.TotalMatches, deduped.MatchedBlocks)

	upper, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"A"}})
	if err != nil {
		panic(err)
	}
	padded, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{" a"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("筛选 \"A\"：TotalMatches=%d；筛选 \" a\"：TotalMatches=%d（均为精确匹配）\n",
		upper.TotalMatches, padded.TotalMatches)
}

func defaultsAndEmptyPage() {
	fmt.Println("=== 6. 默认高度范围与没有匹配交易 ===")
	index := demoChain()

	all, err := index.QueryTxs(indexroom.TxQuery{})
	if err != nil {
		panic(err)
	}
	printPage("空 TxQuery（From=0 即从 1 开始；To=0 即首次查询看到的链顶 3）：", all)

	none, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"zzz"}})
	if err != nil {
		panic(err)
	}
	printPage("筛选不存在的标识：成功返回空页", none)
}

func changedInsideRange() {
	fmt.Println("=== 7. 固定范围内区块变化（即使只改时间）：ErrQueryChanged ===")
	index := demoChain()
	first, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}

	// 高度 2 的区块哈希、父哈希、交易列表都不变，只补一个时间戳；
	// 高度 3 原样重放。固定范围 [1,3] 的指纹因此改变。
	when := int64(42)
	dropped, err := index.Reorg([]indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: &when},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("Reorg 只给高度 2 加上时间戳：dropped=%v err=%v\n", dropped, err)

	page, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
		Cursor:   first.NextCursor,
	})
	printError("继续旧游标：", err, indexroom.ErrQueryChanged)
	fmt.Printf("  没有可用的页结果：len(Hits)=%d TotalMatches=%d\n", len(page.Hits), page.TotalMatches)

	fresh, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}
	printPage("从空游标重新开始全新查询（不得把新结果接到旧结果后面）：", fresh)
}

func tipBelowPinnedBound() {
	fmt.Println("=== 8. 链顶退到固定结束高度以下：ErrQueryChanged ===")
	index := demoChain()
	first, err := index.QueryTxs(indexroom.TxQuery{PageSize: 2})
	if err != nil {
		panic(err)
	}
	// 重组后只剩高度 1、2，链顶低于固定结束高度 3。
	dropped, err := index.Reorg([]indexroom.Block{
		{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"a"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("Reorg 把链顶收缩到高度 2：dropped=%v err=%v\n", dropped, err)
	_, err = index.QueryTxs(indexroom.TxQuery{PageSize: 2, Cursor: first.NextCursor})
	printError("继续旧游标：", err, indexroom.ErrQueryChanged)
}

func invalidArguments() {
	fmt.Println("=== 9. 游标损坏、跨实例或条件改变：ErrInvalidArgument（与链数据变化区分） ===")
	index := demoChain()
	first, err := index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
	})
	if err != nil {
		panic(err)
	}

	// 9.1 翻页时改变筛选集合。
	_, err = index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"b"},
		PageSize: 2,
		Cursor:   first.NextCursor,
	})
	printError("继续时把筛选从 a 改成 b：", err, indexroom.ErrInvalidArgument)

	// 9.2 游标损坏（翻转中间一个字符，签名即失效）。
	raw := []byte(first.NextCursor)
	mid := len(raw) / 2
	if raw[mid] == 'A' {
		raw[mid] = 'B'
	} else {
		raw[mid] = 'A'
	}
	_, err = index.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
		Cursor:   string(raw),
	})
	printError("使用损坏的游标：", err, indexroom.ErrInvalidArgument)

	// 9.3 游标交给另一个索引实例，即使数据完全相同。
	other := demoChain()
	_, err = other.QueryTxs(indexroom.TxQuery{
		To:       10,
		TxIDs:    []string{"a"},
		PageSize: 2,
		Cursor:   first.NextCursor,
	})
	printError("游标交给另一个索引实例：", err, indexroom.ErrInvalidArgument)
}
