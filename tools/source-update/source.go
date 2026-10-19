package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const repo = "https://github.com/opendatahub-io/mcp-lifecycle-operator.git"

func fullSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
	return strings.TrimSpace(string(out)), err
}
