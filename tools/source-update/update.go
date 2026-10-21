package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

type workflowOutputSnapshot struct {
	path   string
	data   []byte
	mode   fs.FileMode
	exists bool
}

func snapshotWorkflowOutput(filename string) (workflowOutputSnapshot, error) {
	snapshot := workflowOutputSnapshot{path: filename}
	if filename == "" {
		return snapshot, nil
	}
	data, err := os.ReadFile(filename)
	if err == nil {
		info, statErr := os.Stat(filename)
		if statErr != nil {
			return snapshot, statErr
		}
		snapshot.data, snapshot.mode, snapshot.exists = data, info.Mode().Perm(), true
		return snapshot, nil
	}
	if !os.IsNotExist(err) {
		return snapshot, err
	}
	return snapshot, nil
}

func (snapshot workflowOutputSnapshot) restore() error {
	if snapshot.path == "" {
		return nil
	}
	if snapshot.exists {
		return restoreFile(snapshot.path, snapshot.data, snapshot.mode)
	}
	if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type moduleFileSnapshot struct {
	path   string
	data   []byte
	mode   fs.FileMode
	exists bool
}

type moduleFilesSnapshot []moduleFileSnapshot

func snapshotModuleFiles(root string) (moduleFilesSnapshot, error) {
	files := make(moduleFilesSnapshot, 0, 2)
	for _, name := range []string{"go.mod", "go.sum"} {
		filename := filepath.Join(root, name)
		data, err := os.ReadFile(filename)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		file := moduleFileSnapshot{path: filename, data: data, exists: err == nil}
		if file.exists {
			info, err := os.Stat(filename)
			if err != nil {
				return nil, err
			}
			file.mode = info.Mode().Perm()
		}
		if name == "go.mod" && !file.exists {
			return nil, fmt.Errorf("read go.mod: %w", os.ErrNotExist)
		}
		files = append(files, file)
	}
	return files, nil
}

func (files moduleFilesSnapshot) restore() error {
	var errs []error
	for _, file := range files {
		if file.exists {
			if err := restoreFile(file.path, file.data, file.mode); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", filepath.Base(file.path), err))
			}
		} else if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", filepath.Base(file.path), err))
		}
	}
	return errors.Join(errs...)
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
	version, originCommit, err := moduleVersion(commit, stage)
	if err != nil {
		return err
	}
	if originCommit != commit {
		return fmt.Errorf("Go resolved %s@%s to source %s", fork, commit, originCommit)
	}
	selectedImage, err := resolveImage(originCommit)
	if err != nil {
		return err
	}
	generated := filepath.Join(stage, "generated")
	configStage, installerStage, err := generateOutputs(originCommit, selectedImage, generated)
	if err != nil {
		return err
	}
	pinStage := filepath.Join(generated, imagePin)
	if err := writeImagePin(pinStage, imagePinRecord{SourceCommit: commit, Image: selectedImage}); err != nil {
		return err
	}
	return applyUpdate(root, version, commit, ref, selectedImage, configStage, installerStage, pinStage)
}

var resolveImage = resolveOperandImage
var generateOutputs = generate

func applyUpdate(root, version, commit, ref, selectedImage, configStage, installerStage, pinStage string) error {
	moduleFiles, err := snapshotModuleFiles(root)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		return errors.Join(cause, moduleFiles.restore())
	}
	if _, err := command(root, false, "go", "mod", "edit", "-require="+canonical+"@"+placeholderVersion, "-replace="+canonical+"="+fork+"@"+version); err != nil {
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
	outputPath := os.Getenv("GITHUB_OUTPUT")
	outputSnapshot, err := snapshotWorkflowOutput(outputPath)
	if err != nil {
		return rollback(err)
	}
	if outputPath != "" {
		file, err := os.OpenFile(outputPath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return rollback(errors.Join(err, outputSnapshot.restore()))
		}
		_, writeErr := fmt.Fprintf(file, "source_commit=%s\nsource_version=%s\noperand_image=%s\n", commit, version, selectedImage)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return rollback(errors.Join(writeErr, closeErr, outputSnapshot.restore()))
		}
	}
	if err := installOutputs(root, configStage, installerStage, pinStage); err != nil {
		return rollback(errors.Join(err, outputSnapshot.restore()))
	}
	fmt.Printf("Synchronized MCPLO source %s (%s) from ref %s\n", commit, version, ref)
	return nil
}

func installerDiffers(actual, expected string) bool {
	actualData, actualErr := os.ReadFile(actual)
	expectedData, expectedErr := os.ReadFile(expected)
	return actualErr != nil || expectedErr != nil || !bytes.Equal(actualData, expectedData)
}

func verify(root string) error {
	modJSON, err := command(root, true, "go", "mod", "edit", "-json")
	if err != nil {
		return err
	}
	pinned, err := sourcePin(modJSON)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "operand-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	version, commit, err := moduleVersion(pinned, stage)
	if err != nil {
		return err
	}
	if version != pinned {
		return fmt.Errorf("pinned version %s resolved as %s", pinned, version)
	}
	pin, err := readImagePin(filepath.Join(root, imagePin))
	if err != nil {
		return err
	}
	if commit != pin.SourceCommit {
		return fmt.Errorf("Go source %s@%s resolves to %s, operand image record pins %s", fork, pinned, commit, pin.SourceCommit)
	}
	configStage, installerStage, err := generate(pin.SourceCommit, pin.Image, stage)
	if err != nil {
		return err
	}
	diffs, err := treeDiff(configStage, filepath.Join(root, config))
	if err != nil {
		return err
	}
	installerDiff := installerDiffers(filepath.Join(root, installer), installerStage)
	if len(diffs) > 0 || installerDiff {
		fmt.Fprintf(os.Stderr, "Operand source drift for %s:\n", pin.SourceCommit)
		for _, diff := range diffs {
			fmt.Fprintf(os.Stderr, "  config %s\n", diff)
		}
		if installerDiff {
			fmt.Fprintf(os.Stderr, "  installer differs or is missing: %s\n", installer)
		}
		return fmt.Errorf("run make update-operand-manifests MCPLO_REF=%s", pin.SourceCommit)
	}
	fmt.Printf("Operand manifests match MCPLO source %s (%s), image %s\n", pin.SourceCommit, pinned, pin.Image)
	return nil
}
