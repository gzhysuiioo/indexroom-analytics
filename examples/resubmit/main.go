// 区块摄取的重复提交（Index.Append）完整示例：区分“再次提交已有区块”与
// “追加新区块”。链顶前进之后，内容完全相同的旧区块仍可再次提交并成功返回
// （不推进链顶、不新增交易出现记录，且不限于当前链顶区块）；只要高度、哈希、
// 父哈希、按原次序排列的交易标识、时间是否提供及具体数值中有任一不同，就被
// 拒绝，已有高度的内容不能通过再次 Append 覆盖。示例还展示 Append 的普通
// 拒绝错误不是查询参数错误 ErrInvalidArgument，调用方按非空错误处理即可。
//
// 运行：go run ./examples/resubmit
package main

import (
	"errors"
	"fmt"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间。
func unix(sec int64) *int64 { return &sec }

// timeText 把区块时间渲染成便于阅读的文本：缺失时间为 "nil（缺失）"，
// 否则为实际 Unix 秒。
func timeText(b indexroom.Block) string {
	if b.Time == nil {
		return "nil（缺失）"
	}
	return fmt.Sprintf("%d", *b.Time)
}

// showBlock 打印某个高度上已索引区块的原始内容：交易列表（含重复项与次序）、
// 时间是否提供及取值。拒绝发生后用它核对内容仍保持原状。
func showBlock(index *indexroom.Index, height int64) {
	b := index.Blocks[height]
	fmt.Printf("  高度 %d 现存内容：hash=%s parent=%s txs=%q 时间=%s\n",
		height, b.Hash, b.Parent, b.Txs, timeText(b))
}

// showA 查询标识 a 在高度 [1,2] 的每次出现，打印块内位置与汇总计数。
func showA(index *indexroom.Index) {
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 2, TxIDs: []string{"a"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询 a：TotalMatches=%d MatchedBlocks=%d，命中",
		page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf(" height=%d/position=%d", hit.Height, hit.Position)
	}
	fmt.Println()
}

// tryAppend 提交一个区块并报告结果：成功时给出当前链顶；被拒绝时打印原因，
// 并说明它不是 ErrInvalidArgument。被拒绝是预期内的分支，不能让程序退出。
func tryAppend(title string, index *indexroom.Index, block indexroom.Block) {
	err := index.Append(block)
	fmt.Printf("%s：err=%v\n", title, err)
	if err != nil {
		fmt.Printf("  errors.Is(err, ErrInvalidArgument)=%v（Append 的普通拒绝不是查询参数错误，按非空错误处理即可）\n",
			errors.Is(err, indexroom.ErrInvalidArgument))
		return
	}
	fmt.Printf("  提交成功，当前链顶 tip=%d\n", index.Tip)
}

func main() {
	// 1. 从空索引摄取两个连续区块：
	//    高度 1：交易按序为 [a,b,a]（a 在块内出现两次），不提供时间；
	//    高度 2：不提供交易列表（Txs 为 nil，与空列表等价），带真实零秒时间。
	index := indexroom.New()
	if err := index.Append(indexroom.Block{
		Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"},
	}); err != nil {
		panic(err)
	}
	if err := index.Append(indexroom.Block{
		Height: 2, Hash: "h2", Parent: "h1", Txs: nil, Time: unix(0),
	}); err != nil {
		panic(err)
	}
	fmt.Printf("两个区块摄取完成，链顶 tip=%d\n", index.Tip)
	showBlock(index, 1)
	showBlock(index, 2)
	showA(index)
	fmt.Println()

	// 2. 链顶已经前进到高度 2。再次提交“旧”区块（高度 1）：内容与已索引
	//    区块逐项相同（同一高度、哈希、父哈希、按原次序的交易标识、时间缺失），
	//    成功返回但不推进链顶，也不新增任何交易出现记录。重复提交不限于
	//    当前链顶区块。
	tryAppend("再次提交高度 1（内容完全相同，且它不是当前链顶区块）", index,
		indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}})
	fmt.Printf("  链顶仍为 tip=%d（没有推进）\n", index.Tip)
	showA(index)
	fmt.Println()

	// 3. 再次提交高度 2，这次显式给出空交易列表：空列表与“未提供交易列表”
	//    在内容上等价，时间仍是真实零秒，所以仍是内容完全相同，成功且无副作用。
	tryAppend("再次提交高度 2（本次显式给空交易列表，与未提供等价；时间仍为零秒）", index,
		indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{}, Time: unix(0)})
	fmt.Printf("  链顶仍为 tip=%d；高度 2 现存交易列表长度=%d\n", index.Tip, len(index.Blocks[2].Txs))
	showA(index)
	fmt.Println()

	// 4. 被拒绝之一：只把高度 1 的交易次序从 [a,b,a] 改成 [a,a,b]。
	//    高度、哈希、父哈希、时间都相同也不够——交易标识必须按原次序逐项相同，
	//    不能为了判断相同而排序或去重。拒绝不改变任何已索引内容。
	tryAppend("再次提交高度 1，仅把交易次序改为 [a,a,b]", index,
		indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "a", "b"}})
	showBlock(index, 1)
	showA(index)
	fmt.Println()

	// 5. 被拒绝之二：只把高度 1 的缺失时间改成真实零秒。
	//    “未提供时间”（Time 为 nil）与时间为 0 是两种不同内容。
	zeroTime := indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}, Time: unix(0)}
	tryAppend("再次提交高度 1，仅把缺失时间改为零秒", index, zeroTime)
	showBlock(index, 1)
	showA(index)
	fmt.Println()

	// 6. 新区块必须接在当前链顶之后；已有高度不能靠再次 Append 覆盖。
	//    上面两次被拒绝的高度 1 仍保持原内容，随后可以正常追加高度 3。
	tryAppend("试图在已有高度 2 上追加一个不同内容的区块（覆盖已有高度）", index,
		indexroom.Block{Height: 2, Hash: "h2x", Parent: "h1", Txs: []string{"a"}, Time: unix(0)})
	tryAppend("追加接在链顶之后的新高度 3（正常前进）", index,
		indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: unix(30)})
	showBlock(index, 1)
	showA(index)
}
