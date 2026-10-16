package main

import (
	"fmt"
	"os"
)

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "operand source update:", err)
		os.Exit(1)
	}
}

func runMain(args []string) error {
	if len(args) < 1 || len(args) > 2 || (args[0] != "update" && args[0] != "verify") || (args[0] == "verify" && len(args) != 1) {
		return fmt.Errorf("usage: go run ./tools/source-update update [ref] | verify")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if args[0] == "verify" {
		return verify(root)
	}
	ref := "main"
	if envRef := os.Getenv("MCPLO_REF"); envRef != "" {
		ref = envRef
	}
	if len(args) == 2 {
		ref = args[1]
	}
	return update(root, ref)
}
