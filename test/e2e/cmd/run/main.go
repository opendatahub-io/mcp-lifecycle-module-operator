package main

import (
	"os"

	"github.com/kubernetes-sigs/mcp-lifecycle-operator/test/e2e/runner"
)

func main() {
	r := runner.New()
	r.Component = "mcp-lifecycle-operator"
	r.RepoAbbr = "module"
	args := runnerArgs(os.Args[1:])
	os.Exit(r.Run(args).ExitCode)
}

func runnerArgs(args []string) []string {
	if len(args) > 0 && args[0] == "exec" {
		return args
	}
	return append([]string{"-test.timeout=15m"}, args...)
}
