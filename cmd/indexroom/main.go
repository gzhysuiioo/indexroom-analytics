// Command indexroom is the 链上索引与交易分析服务 entry point.
package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	case "register":
		os.Exit(runRegister(os.Stdin, os.Stdout, os.Stderr))
	case "version":
		fmt.Println("indexroom 0.1.0")
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: indexroom [command]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  demo      run the built-in chain ingestion demonstration")
	fmt.Fprintln(w, "  register  maintain the offline service registry from a JSON document on stdin")
	fmt.Fprintln(w, "  version   print the program version")
	fmt.Fprintln(w, "  help      show this help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "register input format (one JSON object on stdin):")
	fmt.Fprintln(w, "  {")
	fmt.Fprintln(w, "    \"requests\": [")
	fmt.Fprintln(w, "      {")
	fmt.Fprintln(w, "        \"service\": \"orders\",")
	fmt.Fprintln(w, "        \"expectedRevision\": 0,")
	fmt.Fprintln(w, "        \"instances\": [")
	fmt.Fprintln(w, "          {\"id\": \"a\", \"address\": \"orders-1.internal:8080\"},")
	fmt.Fprintln(w, "          {\"id\": \"b\", \"address\": \"[2001:db8::1]:9000\"}")
	fmt.Fprintln(w, "        ]")
	fmt.Fprintln(w, "      }")
	fmt.Fprintln(w, "    ]")
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "A run starts from an empty registry. Each request replaces one service's")
	fmt.Fprintln(w, "whole instance list after checking input validity and expectedRevision.")
	fmt.Fprintln(w, "Exit status is 0 when every registration succeeds and non-zero otherwise;")
	fmt.Fprintln(w, "malformed input or a non-array requests field is rejected without processing")
	fmt.Fprintln(w, "any registration.")
}

func runRegister(stdin io.Reader, stdout, stderr io.Writer) int {
	data, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "failed to read stdin: %v\n", err)
		return 2
	}
	registry := indexroom.NewRegistry()
	results, err := registry.ApplyBatch(data)
	if err != nil {
		fmt.Fprintf(stderr, "register: %v\n", err)
		return 2
	}
	services := registry.Services()
	output := struct {
		Results  []indexroom.RegisterResult `json:"results"`
		Services []indexroom.ServiceRecord  `json:"services"`
	}{Results: results, Services: services}
	payload, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "failed to encode result: %v\n", err)
		return 2
	}
	fmt.Fprintln(stdout, string(payload))
	if indexroom.HasFailure(results) {
		return 1
	}
	return 0
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
