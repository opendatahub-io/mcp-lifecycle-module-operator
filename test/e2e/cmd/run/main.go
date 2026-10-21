package main

import (
	"os"

	"github.com/kubernetes-sigs/mcp-lifecycle-operator/test/e2e/runner"
)

func main() {
	r := runner.New()
	r.RepoAbbr = "module"
	os.Exit(r.Run(os.Args[1:]).ExitCode)
}
