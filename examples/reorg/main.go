// 重组（Index.Reorg）完整示例：从一条高度 1..4 的链出发，以已索引的
// 高度 1 为父区块（分支首块高度 2，h2 与原块完全相同；h3 保持原哈希、
// 父哈希与交易列表，只把时间从缺失改为 0）做一次缩短重组；再用一个全新的
// 索引从同样的四块链出发，演示同一分支最后一个区块父链接错误时整支分支被
// 拒绝、不报告丢弃高度、原链顶/区块内容/哈希对应关系全部保持原状；最后
// 区分“首块摄取允许未索引的链起点标识”与“重组父哈希必须已索引”。
//
// 运行：go run ./examples/reorg
package main

import (
	"bytes"
	"fmt"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func mustAppend(index *indexroom.Index, block indexroom.Block) {
	if err := index.Append(block); err != nil {
		panic(err)
	}
}

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

// heightsText 把丢弃高度列表渲染成 [3,4] 这样的文本（空列表为 []），
// 便于直接看到“按升序报告高度”，而不是 Go 默认的 [3 4]。
func heightsText(heights []int64) string {
	var sb bytes.Buffer
	sb.WriteByte('[')
	for i, h := range heights {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprint(&sb, h)
	}
	sb.WriteByte(']')
	return sb.String()
}

// dumpChain 按高度升序打印每个主链区块的完整内容（哈希、父哈希、交易、
// 时间），并给出链顶与哈希→高度对应表，用来观察操作后的主链。
func dumpChain(title string, index *indexroom.Index) {
	fmt.Println(title)
	for h := int64(1); h <= index.Tip; h++ {
		b := index.Blocks[h]
		fmt.Printf("  高度 %d：hash=%s parent=%s txs=%q 时间=%s\n",
			h, b.Hash, b.Parent, b.Txs, timeText(b))
	}
	fmt.Printf("  链顶 tip=%d；ByHash=%v\n", index.Tip, index.ByHash)
}

// exportText 返回当前主链的规范化导出文本，用于逐字节核对失败前后的链内容。
func exportText(index *indexroom.Index) string {
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		panic(err)
	}
	return buf.String()
}

// newFourBlockChain 返回一条高度 1..4 的链，用于成功与失败两个场景。
// h1、h2、h4 有正常时间（链上只要有任一块带时间，再导出即为版本 2），
// h3 没有时间（这是成功重组要修改的唯一内容）。
func newFourBlockChain() *indexroom.Index {
	idx := indexroom.New()
	mustAppend(idx, indexroom.Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}, Time: unix(100)})
	mustAppend(idx, indexroom.Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: unix(200)})
	mustAppend(idx, indexroom.Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}})
	mustAppend(idx, indexroom.Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"t4"}, Time: unix(400)})
	return idx
}

// shorteningBranch 构造成功场景提交的分支：
//   - 第一个新区块（高度 2）的父哈希是 h1 —— 当前主链上已索引的高度 1；
//   - 高度 2 的块与原块完全相同（h2，时间 200，不进入丢弃列表）；
//   - 高度 3 保持原哈希 h3、父哈希 h2、交易 [t3]，只把时间从缺失改为 0；
//   - 分支在高度 3 结束，因此旧高度 4（h4）整体消失，新的链顶就是分支末尾的 h3。
func shorteningBranch() []indexroom.Block {
	return []indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: unix(200)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}, Time: unix(0)},
	}
}

// brokenBranch 构造失败场景提交的同一分支，但最后一个区块（高度 3）的
// 父哈希被改错：它应指向分支前一块的哈希 h2，却写成了未在分支中出现的
// "WRONG-PARENT"。
func brokenBranch() []indexroom.Block {
	return []indexroom.Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: unix(200)},
		{Height: 3, Hash: "h3", Parent: "WRONG-PARENT", Txs: []string{"t3"}, Time: unix(0)},
	}
}

func main() {
	// 1. 成功场景：一条高度 1..4 的旧链。
	idx := newFourBlockChain()
	fmt.Println("操作 1：高度 1 到 4 的旧链")
	dumpChain("主链内容：", idx)
	fmt.Printf("  h3 的时间：%s —— 旧 h3 没有时间，这不是 0\n", timeText(idx.Blocks[3]))
	fmt.Println()

	// 2. 以高度 1（h1）为父提交缩短分支：高度 2 完全相同，高度 3 只改时间，
	//    分支在 3 结束，旧高度 4 被整体删除。Reorg 是“分支整体替换父之后
	//    全部主链内容”，不是只应用变化的块。
	dropped, err := idx.Reorg(shorteningBranch())
	if err != nil {
		panic(err)
	}
	fmt.Printf("操作 2：以高度 1（h1）为父提交高度 2、3 的缩短分支，Reorg 返回\n")
	fmt.Printf("  err=%v\n", err)
	fmt.Printf("  丢弃高度 dropped=%s（升序）\n", heightsText(dropped))
	fmt.Println("  解释：该列表报告旧内容发生变化或被删除的高度——")
	fmt.Println("  · 高度 2 提交的块与旧块完全相同，不列入；")
	fmt.Println("  · 高度 3 哈希/父哈希/交易都没变，但时间由缺失变成真实零秒，旧内容变了，必须列入；")
	fmt.Println("  · 高度 4 在分支覆盖范围之外，旧块被删除，必须列入；")
	fmt.Println("  · 它不是全部提交高度（提交的是 2、3），也不是消失的哈希数量（h4 只有一个）。")
	dumpChain("操作后的主链：", idx)
	fmt.Printf("  h3 现在的时间：%s —— 真实零秒，与缺失严格不同\n", timeText(idx.Blocks[3]))
	fmt.Printf("  高度 4 是否存在：%v；h4 是否还在哈希表：%v\n",
		idx.Blocks[4].Hash != "", idx.ByHash["h4"] != 0)
	page, err := idx.QueryTxs(indexroom.TxQuery{From: 1, To: 4})
	if err != nil {
		panic(err)
	}
	fmt.Printf("  查询高度 [1, 4]：ToHeight=%d TotalMatches=%d MatchedBlocks=%d\n",
		page.ToHeight, page.TotalMatches, page.MatchedBlocks)
	for _, hit := range page.Hits {
		fmt.Printf("    命中 height=%d block=%s tx=%q position=%d\n",
			hit.Height, hit.BlockHash, hit.TxID, hit.Position)
	}
	fmt.Println()

	// 3. 失败场景：用一个全新的索引从原来的四块链重新开始（避免与成功后的
	//    状态混淆）。同一分支，只是最后一个区块父链接错误。
	fresh := newFourBlockChain()
	before := exportText(fresh)
	fmt.Println("操作 3：新索引，仍是原来的高度 1..4 链（失败场景从这条链开始）")
	fmt.Printf("  失败前链顶 tip=%d；h3 时间=%s\n", fresh.Tip, timeText(fresh.Blocks[3]))
	badDropped, badErr := fresh.Reorg(brokenBranch())
	fmt.Printf("  提交最后一块父链接错误的分支：err=%v\n", badErr)
	fmt.Printf("  重组失败只表现为非 nil 错误（本功能未导出专用哨兵错误）\n")
	fmt.Printf("  返回的丢弃高度 dropped=%s —— 出错时不报告任何高度，前面合法的块也不会提前生效\n",
		heightsText(badDropped))
	fmt.Printf("  失败后链顶 tip=%d（应仍为 4）\n", fresh.Tip)
	fmt.Println("  失败后的主链（应与操作 1 的旧链完全一致：h3 仍无时间，h4 仍在）：")
	for h := int64(1); h <= fresh.Tip; h++ {
		b := fresh.Blocks[h]
		fmt.Printf("    高度 %d：hash=%s parent=%s txs=%q 时间=%s\n",
			h, b.Hash, b.Parent, b.Txs, timeText(b))
	}
	fmt.Printf("  ByHash=%v\n", fresh.ByHash)
	fmt.Printf("  失败前后导出逐字节一致：%v；h3 时间仍缺失=%v；h4 仍在哈希表=%v\n",
		exportText(fresh) == before, fresh.Blocks[3].Time == nil, fresh.ByHash["h4"] != 0)
	fmt.Println()

	// 4. 区分：空索引上的第一个块，父哈希只是链起点标识，不需要已索引；
	//    但 Reorg 的父哈希必须对应当前主链中已索引的区块。
	fmt.Println("操作 4：首块摄取的未索引起点 vs 重组的父哈希")
	first := indexroom.New()
	firstErr := first.Append(indexroom.Block{Height: 1, Hash: "b1", Parent: "unindexed-start", Txs: []string{"x"}})
	fmt.Printf("  空索引 Append 高度 1、parent=%q：err=%v（该标识只是链起点，无需已索引）\n",
		"unindexed-start", firstErr)
	_, reorgErr := first.Reorg([]indexroom.Block{
		{Height: 1, Hash: "r1", Parent: "unindexed-start", Txs: []string{"y"}},
	})
	fmt.Printf("  对同一个未索引标识发起 Reorg（试图从高度 1 整体替换）：err=%v\n", reorgErr)
	fmt.Println("  即：首块摄取允许用未索引的起点标识，不代表重组也能以这个标识为父哈希替换高度 1；")
	fmt.Println("  重组第一个新区块的父哈希必须是当前主链上已经索引的区块，分支从它的下一高度开始。")
}
