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
	selectedImage, err := resolveOperandImage(commit)
	if err != nil {
		return err
	}
	generated := filepath.Join(stage, "generated")
	configStage, installerStage, err := generate(commit, selectedImage, generated)
	if err != nil {
		return err
	}
	pinStage := filepath.Join(generated, imagePin)
	if err := writeImagePin(pinStage, imagePinRecord{SourceCommit: commit, Image: selectedImage}); err != nil {
		return err
	}
	return applyUpdate(root, commit, ref, selectedImage, configStage, installerStage, pinStage)
}

func applyUpdate(root, commit, ref, selectedImage, configStage, installerStage, pinStage string) error {
	outputPath := os.Getenv("GITHUB_OUTPUT")
	outputSnapshot, err := snapshotWorkflowOutput(outputPath)
	if err != nil {
		return err
	}
	if outputPath != "" {
		file, err := os.OpenFile(outputPath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return errors.Join(err, outputSnapshot.restore())
		}
		_, writeErr := fmt.Fprintf(file, "source_commit=%s\noperand_image=%s\n", commit, selectedImage)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return errors.Join(writeErr, closeErr, outputSnapshot.restore())
		}
	}
	if err := installOutputs(root, configStage, installerStage, pinStage); err != nil {
		return errors.Join(err, outputSnapshot.restore())
	}
	fmt.Printf("Synchronized MCPLO source %s from ref %s\n", commit, ref)
	return nil
}

func installerDiffers(actual, expected string) bool {
	actualData, actualErr := os.ReadFile(actual)
	expectedData, expectedErr := os.ReadFile(expected)
	return actualErr != nil || expectedErr != nil || !bytes.Equal(actualData, expectedData)
}

func verify(root string) error {
	pin, err := readImagePin(filepath.Join(root, imagePin))
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "operand-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
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
	fmt.Printf("Operand manifests match MCPLO source %s, image %s\n", pin.SourceCommit, pin.Image)
	return nil
}
