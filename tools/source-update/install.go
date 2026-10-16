package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	config    = "config/manifests/mcp-lifecycle-operator"
	installer = "internal/controller/resources/mcp-lifecycle-operator.yaml"
)

var rename = os.Rename

type installEntry struct {
	target, workspace, output, backup string
	backedUp, installed               bool
}

func installOutputs(root, stagedConfig, stagedInstaller, stagedPin string) error {
	entries := make([]installEntry, 0, 3)
	for _, item := range []struct {
		target, staged string
		dir            bool
	}{
		{filepath.Join(root, config), stagedConfig, true},
		{filepath.Join(root, installer), stagedInstaller, false},
		{filepath.Join(root, imagePin), stagedPin, false},
	} {
		if err := os.MkdirAll(filepath.Dir(item.target), 0755); err != nil {
			return errors.Join(err, cleanupEntries(entries))
		}
		workspace, err := os.MkdirTemp(filepath.Dir(item.target), ".operand-install-")
		if err != nil {
			return errors.Join(err, cleanupEntries(entries))
		}
		entry := installEntry{target: item.target, workspace: workspace, output: filepath.Join(workspace, "output"), backup: filepath.Join(workspace, "backup")}
		if item.dir {
			err = copyTree(item.staged, entry.output)
		} else {
			err = copyFile(item.staged, entry.output)
		}
		if err != nil {
			return errors.Join(err, os.RemoveAll(workspace), cleanupEntries(entries))
		}
		entries = append(entries, entry)
	}
	for i := range entries {
		entry := &entries[i]
		if _, err := os.Lstat(entry.target); err == nil {
			if err := rename(entry.target, entry.backup); err != nil {
				return rollbackInstall(entries, fmt.Errorf("backup %s: %w", entry.target, err))
			}
			entry.backedUp = true
		} else if !os.IsNotExist(err) {
			return rollbackInstall(entries, err)
		}
		if err := rename(entry.output, entry.target); err != nil {
			return rollbackInstall(entries, fmt.Errorf("install %s: %w", entry.target, err))
		}
		entry.installed = true
	}
	if err := cleanupEntries(entries); err != nil {
		fmt.Fprintf(os.Stderr, "warning: outputs installed, but temporary cleanup failed: %v\n", err)
	}
	return nil
}

func rollbackInstall(entries []installEntry, cause error) error {
	var recovery []string
	for i := len(entries) - 1; i >= 0; i-- {
		entry := &entries[i]
		if entry.installed {
			if err := os.RemoveAll(entry.target); err != nil {
				detail := fmt.Sprintf("remove %s: %v", entry.target, err)
				if entry.backedUp {
					detail += fmt.Sprintf("; original retained at %s", entry.backup)
				}
				recovery = append(recovery, detail)
				continue
			}
		}
		if entry.backedUp {
			if err := rename(entry.backup, entry.target); err != nil {
				recovery = append(recovery, fmt.Sprintf("original retained at %s (restore failed: %v)", entry.backup, err))
				continue
			}
			entry.backedUp = false
		}
	}
	for _, entry := range entries {
		if !entry.backedUp {
			if err := cleanupEntries([]installEntry{entry}); err != nil {
				recovery = append(recovery, err.Error())
			}
		}
	}
	if len(recovery) > 0 {
		return fmt.Errorf("%v; rollback incomplete: %s", cause, strings.Join(recovery, "; "))
	}
	return cause
}

func cleanupEntries(entries []installEntry) error {
	var errs []error
	for _, entry := range entries {
		if err := os.RemoveAll(entry.workspace); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", entry.workspace, err))
		}
	}
	return errors.Join(errs...)
}

func restoreFile(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".operand-restore-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
