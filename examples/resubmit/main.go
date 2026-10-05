// 重复提交已有区块（Index.Append 的幂等重发）完整示例：从空索引摄取两个
// 连续区块——高度 1 的交易按序为 [a,b,a] 且没有时间，高度 2 未提供交易列表
// 并带真实的零秒时间；随后原样重发高度 1、以空列表重发高度 2，展示成功返回、
// 链顶不前进、交易出现记录不增加；再展示两种被拒绝的重发（交易顺序改为
// [a,a,b]、缺失时间改为零秒），输出拒绝原因，并核对拒绝后原区块内容与
// 交易查询结果保持原状。预期拒绝只打印错误，不会提前退出。
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

// dumpChain 按高度升序打印每个主链区块的完整内容，并给出链顶。
func dumpChain(index *indexroom.Index) {
	for h := int64(1); h <= index.Tip; h++ {
		b := index.Blocks[h]
		fmt.Printf("  高度 %d：hash=%s parent=%s txs=%q 时间=%s\n",
			h, b.Hash, b.Parent, b.Txs, timeText(b))
	}
	fmt.Printf("  链顶 tip=%d\n", index.Tip)
}

// queryA 查询标识 a 在主链上的每一次出现，打印高度与块内位置。
func queryA(index *indexroom.Index) {
	page, err := index.QueryTxs(indexroom.TxQuery{TxIDs: []string{"a"}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询标识 a：TotalMatches=%d MatchedBlocks=%d\n",
		page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf("    命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
}

func main() {
	// 1. 从空索引摄取两个连续区块：
	//    高度 1 的交易按序为 [a,b,a]（a 在块内出现两次），没有时间；
	//    高度 2 未提供交易列表（Txs 为 nil），带真实的零秒时间。
	index := indexroom.New()
	block1 := indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}}
	block2 := indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Time: unix(0)}
	for _, b := range []indexroom.Block{block1, block2} {
		if err := index.Append(b); err != nil {
			panic(err)
		}
	}
	fmt.Println("操作 1：摄取高度 1（txs=[a,b,a]，无时间）与高度 2（未提供交易列表，时间 0）")
	dumpChain(index)
	fmt.Println()

	// 2. 再次提交已有区块：内容完全相同即成功返回，不推进链顶，也不新增
	//    交易出现记录。重发不限于当前链顶——这里先重发高度 1。交易列表
	//    未提供与空列表等价，因此高度 2 用空列表重发也算内容相同。
	err := index.Append(block1)
	fmt.Printf("操作 2：原样重发高度 1（非链顶区块）：err=%v\n", err)
	resubmit2 := block2
	resubmit2.Txs = []string{} // 空列表与未提供等价
	err = index.Append(resubmit2)
	fmt.Printf("         以空列表重发高度 2：err=%v\n", err)
	fmt.Println("重发后的主链与查询（链顶不前进，a 仍只有原来的两次出现）：")
	dumpChain(index)
	queryA(index)
	fmt.Println()

	// 3. 拒绝情形之一：同一高度、同一哈希，但交易顺序改为 [a,a,b]。
	//    交易标识的顺序、重复项、大小写与首尾空白都属于原始内容，
	//    不会为了判断相同而排序、去重或改写。Append 的普通拒绝错误
	//    不是 ErrInvalidArgument，调用方按非空错误处理即可。
	reordered := block1
	reordered.Txs = []string{"a", "a", "b"}
	err = index.Append(reordered)
	fmt.Printf("操作 3：重发高度 1 但交易顺序改为 [a,a,b]：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidArgument)=%v（Append 的拒绝是普通错误，按非空错误处理）\n",
		errors.Is(err, indexroom.ErrInvalidArgument))

	// 4. 拒绝情形之二：只把缺失时间改为真实的零秒。时间是否提供与具体
	//    数值都属于区块内容，缺失与零秒是两种不同内容。
	timed := block1
	timed.Time = unix(0)
	err = index.Append(timed)
	fmt.Printf("操作 4：重发高度 1 但补上零秒时间：err=%v\n", err)
	fmt.Printf("  errors.Is(err, ErrInvalidArgument)=%v\n",
		errors.Is(err, indexroom.ErrInvalidArgument))
	fmt.Println()

	// 5. 两次预期拒绝都不改变已有链：原区块内容与交易查询结果保持原状。
	fmt.Println("操作 5：两次拒绝后的主链与查询（应与操作 2 之后完全一致）：")
	dumpChain(index)
	queryA(index)
}
