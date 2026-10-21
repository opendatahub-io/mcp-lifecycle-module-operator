package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/module"
)

const (
	canonical          = "github.com/kubernetes-sigs/mcp-lifecycle-operator"
	fork               = "github.com/opendatahub-io/mcp-lifecycle-operator"
	placeholderVersion = "v0.0.0"
	repo               = "https://github.com/opendatahub-io/mcp-lifecycle-operator.git"
)

func fullSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sourcePin(data []byte) (string, error) {
	var mod struct {
		Require []struct {
			Path, Version string
		} `json:"Require"`
		Replace []struct {
			Old struct {
				Path, Version string
			} `json:"Old"`
			New struct {
				Path, Version string
			} `json:"New"`
		} `json:"Replace"`
	}
	if err := json.Unmarshal(data, &mod); err != nil {
		return "", err
	}
	var required string
	requireCount := 0
	for _, item := range mod.Require {
		if item.Path == canonical {
			requireCount++
			required = item.Version
		}
	}
	if requireCount != 1 {
		return "", fmt.Errorf("go.mod must require %s exactly once", canonical)
	}
	var replacement string
	count := 0
	for _, item := range mod.Replace {
		if item.Old.Path == canonical {
			count++
			if item.Old.Version != "" || item.New.Path != fork {
				return "", fmt.Errorf("go.mod replacement must be unversioned %s => %s@version", canonical, fork)
			}
			replacement = item.New.Version
		}
	}
	if count != 1 || replacement == "" || module.Check(fork, replacement) != nil || replacement == placeholderVersion {
		return "", fmt.Errorf("go.mod must contain one replacement for %s pinned to a real %s version", canonical, fork)
	}
	if required != placeholderVersion {
		return "", fmt.Errorf("go.mod must require %s at placeholder version %s", canonical, placeholderVersion)
	}
	return replacement, nil
}

func moduleInfo(data []byte) (path, version, commit string, err error) {
	var info struct {
		Path    string
		Version string
		Origin  struct{ Hash string }
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return
	}
	path, version, commit = info.Path, info.Version, info.Origin.Hash
	if path != fork || version == "" || version == placeholderVersion || module.Check(fork, version) != nil || len(commit) != 40 || !fullSHA(commit) {
		err = fmt.Errorf("could not resolve immutable source provenance for %s", fork)
	}
	return
}

func moduleVersion(query, root string) (string, string, error) {
	out, err := command(root, true, "go", "list", "-m", "-json", fork+"@"+query)
	if err != nil {
		return "", "", err
	}
	_, version, commit, err := moduleInfo(out)
	if err != nil {
		return "", "", err
	}
	return version, commit, nil
}

func validateRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || ref == "HEAD" || strings.IndexFunc(ref, func(r rune) bool { return r < 32 }) >= 0 {
		return errors.New("source ref must be a branch, tag, or full commit SHA")
	}
	if fullSHA(ref) {
		return nil
	}
	_, err := command("", false, "git", "check-ref-format", "--allow-onelevel", ref)
	if err != nil {
		return fmt.Errorf("invalid source ref %q: %w", ref, err)
	}
	return nil
}

func sourceCommit(ref, checkout, remote string) (string, error) {
	if _, err := command("", false, "git", "init", checkout); err != nil {
		return "", err
	}
	if _, err := command("", false, "git", "-C", checkout, "remote", "add", "origin", remote); err != nil {
		return "", err
	}
	if _, err := command("", false, "git", "-C", checkout, "fetch", "--depth=1", "origin", ref); err != nil {
		return "", err
	}
	out, err := command("", true, "git", "-C", checkout, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if len(commit) != 40 || !fullSHA(commit) {
		return "", fmt.Errorf("resolved source commit %q is not a full 40-character SHA", commit)
	}
	return commit, nil
}
