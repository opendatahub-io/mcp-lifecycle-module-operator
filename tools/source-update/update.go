package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func treeFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return files, err
}

func treeDiff(expected, actual string) ([]string, error) {
	want, err := treeFiles(expected)
	if err != nil {
		return nil, err
	}
	got := map[string][]byte{}
	if _, err := os.Lstat(actual); err == nil {
		got, err = treeFiles(actual)
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var diffs []string
	for name, data := range want {
		if actualData, ok := got[name]; !ok {
			diffs = append(diffs, "missing: "+name)
		} else if !bytes.Equal(data, actualData) {
			diffs = append(diffs, "changed: "+name)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			diffs = append(diffs, "added: "+name)
		}
	}
	slices.Sort(diffs)
	return diffs, nil
}

func update(root, ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "operand-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	checkout := filepath.Join(stage, "resolve")
	commit, err := sourceCommit(ref, checkout, repo)
	if err != nil {
		return err
	}
	version, originCommit, err := moduleVersion(commit, root)
	if err != nil {
		return err
	}
	if originCommit != commit {
		return fmt.Errorf("Go did not resolve %s@%s to the same immutable commit", fork, commit)
	}
	configStage, installerStage, err := generate(originCommit, filepath.Join(stage, "generated"))
	if err != nil {
		return err
	}
	return applyUpdate(root, version, originCommit, ref, configStage, installerStage)
}

func applyUpdate(root, version, commit, ref, configStage, installerStage string) error {
	modPath, sumPath := filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")
	oldMod, err := os.ReadFile(modPath)
	if err != nil {
		return err
	}
	oldModInfo, err := os.Stat(modPath)
	if err != nil {
		return err
	}
	oldSum, sumErr := os.ReadFile(sumPath)
	sumExists := sumErr == nil
	if sumErr != nil && !os.IsNotExist(sumErr) {
		return sumErr
	}
	var oldSumMode fs.FileMode
	if sumExists {
		info, err := os.Stat(sumPath)
		if err != nil {
			return err
		}
		oldSumMode = info.Mode().Perm()
	}
	rollback := func(cause error) error {
		var failures []string
		if err := restoreFile(modPath, oldMod, oldModInfo.Mode().Perm()); err != nil {
			failures = append(failures, "go.mod: "+err.Error())
		}
		if sumExists {
			if err := restoreFile(sumPath, oldSum, oldSumMode); err != nil {
				failures = append(failures, "go.sum: "+err.Error())
			}
		} else if err := os.Remove(sumPath); err != nil && !os.IsNotExist(err) {
			failures = append(failures, "go.sum: "+err.Error())
		}
		if len(failures) > 0 {
			return fmt.Errorf("%v; module-file rollback failed: %s", cause, strings.Join(failures, "; "))
		}
		return cause
	}
	if _, err := command(root, false, "go", "mod", "edit", "-require="+canonical+"@"+version, "-replace="+canonical+"="+fork+"@"+version); err != nil {
		return rollback(err)
	}
	if _, err := command(root, false, "go", "mod", "tidy"); err != nil {
		return rollback(err)
	}
	modJSON, err := command(root, true, "go", "mod", "edit", "-json")
	if err != nil {
		return rollback(err)
	}
	pinned, err := sourcePin(modJSON)
	if err != nil || pinned != version {
		if err == nil {
			err = errors.New("go mod tidy changed the synchronized source pin")
		}
		return rollback(err)
	}
	if output := os.Getenv("GITHUB_OUTPUT"); output != "" {
		file, err := os.OpenFile(output, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return rollback(err)
		}
		_, writeErr := fmt.Fprintf(file, "source_commit=%s\nsource_version=%s\n", commit, version)
		closeErr := file.Close()
		if writeErr != nil {
			return rollback(writeErr)
		}
		if closeErr != nil {
			return rollback(closeErr)
		}
	}
	if err := installOutputs(root, configStage, installerStage); err != nil {
		return rollback(err)
	}
	fmt.Printf("Synchronized MCPLO source %s (%s) from ref %s\n", commit, version, ref)
	return nil
}

func verify(root string) error {
	out, err := command(root, true, "go", "mod", "edit", "-json")
	if err != nil {
		return err
	}
	pinned, err := sourcePin(out)
	if err != nil {
		return err
	}
	version, commit, err := moduleVersion(pinned, root)
	if err != nil {
		return err
	}
	if version != pinned {
		return fmt.Errorf("pinned version %s resolved as %s", pinned, version)
	}
	stage, err := os.MkdirTemp("", "operand-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	configStage, installerStage, err := generate(commit, stage)
	if err != nil {
		return err
	}
	diffs, err := treeDiff(configStage, filepath.Join(root, config))
	if err != nil {
		return err
	}
	actualInstaller, err := os.ReadFile(filepath.Join(root, installer))
	installerData, stageErr := os.ReadFile(installerStage)
	installerDiff := err != nil || stageErr != nil || !bytes.Equal(actualInstaller, installerData)
	if len(diffs) > 0 || installerDiff {
		fmt.Fprintf(os.Stderr, "Operand source drift for %s (%s):\n", commit, pinned)
		for _, diff := range diffs {
			fmt.Fprintf(os.Stderr, "  config %s\n", diff)
		}
		if installerDiff {
			fmt.Fprintf(os.Stderr, "  installer differs or is missing: %s\n", installer)
		}
		return fmt.Errorf("run make update-operand-manifests MCPLO_REF=%s", commit)
	}
	fmt.Printf("Operand manifests match MCPLO source %s (%s)\n", commit, pinned)
	return nil
}
