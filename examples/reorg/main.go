// 链上重组（Index.Reorg）完整示例：先在一条高度 1 到 4 的旧链上做一次
// 缩短重组——以已索引的高度 1 区块为父区块，提交高度 2、3：高度 2 与原区块
// 完全相同，高度 3 保持原哈希、父哈希和交易列表，只把时间从缺失改为真实的
// 0 秒；成功后新链顶为 3，高度 4 不再存在，丢弃高度按升序为 [3 4]。
// 再从一条全新的四块链出发，展示同一分支最后一个区块父链接错误时，调用返回
// 错误且不报告丢弃高度，前面合法的区块不会提前生效，原链顶、区块内容和哈希
// 对应关系全部保持原状。最后区分：首块摄取允许使用未索引的起点标识，
// 而重组不能以该标识为父哈希替换高度 1。
//
// 运行：go run ./examples/reorg
package main

import (
	"fmt"
	"reflect"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func mustAppend(index *indexroom.Index, block indexroom.Block) {
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

// unix 返回指向给定 Unix 秒的指针，用于设置区块时间。
func unix(sec int64) *int64 { return &sec }

// oldChain 构造一条高度 1 到 4 的旧链，四个区块都没有区块时间（Time 为 nil）。
func oldChain() *indexroom.Index {
	index := indexroom.New()
	mustAppend(index, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"coinbase"}})
	mustAppend(index, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"alice->bob"}})
	mustAppend(index, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"carol->dave"}})
	mustAppend(index, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"eve->frank"}})
	return index
}

func timeText(block indexroom.Block) string {
	if block.Time == nil {
		return "缺失"
	}
	return fmt.Sprintf("%d 秒", *block.Time)
}

// printHeights 打印高度 1 到 max 每个位置上的区块内容（不存在则明确标出）与链顶。
func printHeights(title string, index *indexroom.Index, max int64) {
	fmt.Println(title)
	fmt.Printf("  链顶 tip=%d\n", index.Tip)
	for height := int64(1); height <= max; height++ {
		block, ok := index.Blocks[height]
		if !ok {
			fmt.Printf("  高度 %d：不存在\n", height)
			continue
		}
		fmt.Printf("  高度 %d：hash=%s parent=%s txs=%q 时间=%s\n",
			height, block.Hash, block.Parent, block.Txs, timeText(block))
	}
}

// printHashes 逐个报告哈希当前解析到的高度，不存在则明确标出，避免遍历 map
// 造成输出顺序不稳定。
func printHashes(index *indexroom.Index, hashes ...string) {
	for _, hash := range hashes {
		if height, ok := index.ByHash[hash]; ok {
			fmt.Printf("  哈希 %s -> 高度 %d\n", hash, height)
		} else {
			fmt.Printf("  哈希 %s -> 已不在索引中\n", hash)
		}
	}
}

// printMainChain 用公开查询接口读出主链上的全部交易，直观展示链顶与旧尾段。
func printMainChain(index *indexroom.Index) {
	page, err := index.QueryTxs(indexroom.TxQuery{From: 1, To: 4})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  QueryTxs 高度 [1, 4] 看到的主链（实际 ToHeight=%d，共 %d 笔）：\n",
		page.ToHeight, page.TotalMatches)
	for _, hit := range page.Hits {
		fmt.Printf("    命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
}

func main() {
	// 1. 成功的缩短重组。先准备高度 1 到 4 的旧链（全部缺失时间）。
	index := oldChain()
	printHeights("操作 1：重组前的旧链（高度 1 到 4，全部缺失时间）", index, 4)
	fmt.Println()

	// 2. 准备 Reorg 的分支输入：
	//    - 第一个新区块高度 2，父哈希 h1 必须对应当前主链中已经索引的区块
	//      （h1 就在高度 1），分支从父区块的下一高度开始；
	//    - 高度 2 与原区块逐字段完全相同（同样缺失时间）；
	//    - 高度 3 保持原哈希 h3、父哈希 h2 和交易列表，只把时间从缺失改为 0。
	//    分支只覆盖高度 2、3，旧链高度 4 不会自动保留：父区块之后的内容由
	//    提交的分支整体替换，新链顶就是分支末尾的高度 3。
	branch := []indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"alice->bob"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"carol->dave"}, Time: unix(0)},
	}
	dropped, err := index.Reorg(branch)
	fmt.Println("操作 2：以高度 1 的 h1 为父区块，提交高度 2、3 的分支")
	fmt.Printf("  Reorg 返回：err=%v，丢弃高度（升序）=%v\n", err, dropped)
	printHeights("操作后的主链：h1 及之前保留，其后由分支整体替换，新链顶是分支末尾", index, 4)
	fmt.Println("  三个高度在丢弃列表中的区别：")
	fmt.Println("    高度 2：新旧区块完全相同（哈希、父哈希、交易列表、时间都缺失），不列入；")
	fmt.Println("    高度 3：哈希、父哈希、交易列表都没变，但缺失时间与真实零秒是不同内容，必须列入；")
	fmt.Println("    高度 4：新链顶只有 3，旧高度 4 被整条删除，必须列入。")
	fmt.Println("  哈希对应关系（旧高度 4 的 h4 已随删除消失）：")
	printHashes(index, "h1", "h2", "h3", "h4")
	printMainChain(index)
	fmt.Println()

	// 3. 失败示例必须从原来的四块链重新开始，避免与操作 2 成功后的状态混淆。
	//    分支的高度 2 合法，最后一个区块高度 3 的父哈希故意写错：它必须等于
	//    分支前一个区块的哈希 h2。整份分支在应用前整体校验，因此返回错误、
	//    不报告丢弃高度，且没有任何区块提前生效。
	failed := oldChain()
	reference := oldChain() // 全新的四块链，作为逐字段比对的参照
	badBranch := []indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"alice->bob"}},
		{Height: 3, Hash: "h3", Parent: "WRONG-PARENT", Txs: []string{"carol->dave"}, Time: unix(0)},
	}
	dropped, err = failed.Reorg(badBranch)
	fmt.Println("操作 3：从全新的四块链出发，同一分支最后一个区块（高度 3）父链接错误")
	fmt.Printf("  Reorg 返回：err=%v\n", err)
	fmt.Printf("  丢弃高度=%v（失败时长度为 0，不报告任何丢弃高度）\n", dropped)
	printHeights("失败后的链：仍应是原来的四块链，高度 3 时间仍缺失，高度 4 仍然存在", failed, 4)
	fmt.Println("  哈希对应关系（与原链完全一致，h4 仍在高度 4）：")
	printHashes(failed, "h1", "h2", "h3", "h4")
	fmt.Printf("  与一条全新四块链逐字段比对：链顶一致=%v 区块表一致=%v 哈希表一致=%v\n",
		failed.Tip == reference.Tip,
		reflect.DeepEqual(failed.Blocks, reference.Blocks),
		reflect.DeepEqual(failed.ByHash, reference.ByHash))
	printMainChain(failed)
	fmt.Println()

	// 4. 区分“首块摄取的未索引起点”与“重组的父哈希”。
	//    空索引第一次 Append 高度 1 时，parent 只是链起点标识，无需已索引。
	start := indexroom.New()
	err = start.Append(indexroom.Block{Height: 1, Hash: "g1", Parent: "genesis", Txs: []string{"genesis-tx"}})
	fmt.Println("操作 4：首块摄取与重组的区别")
	fmt.Printf("  空索引首次摄取高度 1，父哈希 genesis 无需已索引：err=%v，链顶 tip=%d\n", err, start.Tip)
	// 同一个未索引的起点标识不能充当 Reorg 的父哈希：分支父哈希必须能在当前
	// 主链中解析到已索引区块，因此无法用它替换高度 1。
	dropped, err = start.Reorg([]indexroom.Block{
		{Height: 1, Hash: "r1", Parent: "genesis", Txs: []string{"replaced"}},
	})
	fmt.Printf("  以未索引起点 genesis 为父哈希提交高度 1 的分支：err=%v\n", err)
	fmt.Printf("  丢弃高度=%v，链顶仍为 tip=%d，高度 1 区块仍为 %s（调用整体被拒）\n",
		dropped, start.Tip, start.Blocks[1].Hash)
	// 正确做法：父哈希传已索引区块自己的哈希 g1，分支从它的下一高度 2 开始。
	dropped, err = start.Reorg([]indexroom.Block{
		{Height: 2, Hash: "r2", Parent: "g1", Txs: []string{"reorg-tx"}},
	})
	fmt.Printf("  改用已索引区块 g1 为父、分支从高度 2 开始：err=%v，丢弃高度=%v，链顶 tip=%d\n",
		err, dropped, start.Tip)
}
