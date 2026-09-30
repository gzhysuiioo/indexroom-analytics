// Command indexroom is the 链上索引与交易分析服务 entry point.
package main

import (
	"fmt"
	"os"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("indexroom 0.1.0")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: indexroom [demo|version|help]")
}

func runDemo() {
	index := indexroom.New()
	blocks := []indexroom.Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t3"}},
	}
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			fmt.Printf("append refused at height %d: %v\n", block.Height, err)
			continue
		}
		fmt.Printf("indexed height=%d txs=%d\n", block.Height, len(block.Txs))
	}
	if err := index.Append(indexroom.Block{Height: 3, Hash: "h3", Parent: "h1"}); err != nil {
		fmt.Println("non-linear block refused:", err)
	}
	dropped, err := index.Reorg([]indexroom.Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t4", "t5"}}})
	fmt.Printf("reorg dropped=%v err=%v tip=%d\n", dropped, err, index.Tip)
}
