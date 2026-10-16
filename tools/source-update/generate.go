package main

import (
	"bytes"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var command = runCommand

func runCommand(dir string, capture bool, name string, args ...string) ([]byte, error) {
	return runCommandEnv(dir, capture, localToolchainEnv(), name, args...)
}

func runCommandEnv(dir string, capture bool, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if !capture {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil, nil
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}

func localToolchainEnv() []string {
	return setEnv(os.Environ(), "GOTOOLCHAIN", "local")
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func generatorEnv() []string {
	env := localToolchainEnv()
	filtered := env[:0]
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "GNUMAKEFLAGS", "MAKEFILES":
			continue
		}
		filtered = append(filtered, item)
	}
	return setEnv(filtered, "GOWORK", "off")
}

func generatorCommand(dir string, capture bool, name string, args ...string) ([]byte, error) {
	return runCommandEnv(dir, capture, generatorEnv(), name, args...)
}

func checkGeneratorGo(source string) error {
	data, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		return err
	}
	var required string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			required = "go" + fields[1]
			break
		}
	}
	out, err := generatorCommand(source, true, "go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(out))
	compiler, _, _ := strings.Cut(current, " ")
	if !version.IsValid(required) || !version.IsValid(compiler) {
		return fmt.Errorf("cannot determine generator Go minimum/current version (%q, %q)", required, current)
	}
	if version.Compare(compiler, required) < 0 {
		return fmt.Errorf("generator source requires Go %s; local compiler is %s", strings.TrimPrefix(required, "go"), current)
	}
	return nil
}

func generate(commit, image, stage string) (string, string, error) {
	if err := os.MkdirAll(stage, 0755); err != nil {
		return "", "", err
	}
	source := filepath.Join(stage, "source")
	if _, err := sourceCommit(commit, source, repo); err != nil {
		return "", "", err
	}
	if _, err := command(source, false, "git", "-C", source, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return "", "", err
	}
	out, err := command(source, true, "git", "-C", source, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	actual := strings.TrimSpace(string(out))
	if actual != commit {
		return "", "", fmt.Errorf("checkout mismatch: requested %s, got %s", commit, actual)
	}
	if err := checkGeneratorGo(source); err != nil {
		return "", "", err
	}
	if _, err := generatorCommand("", false, "make", generatorMakeArgs(source)...); err != nil {
		return "", "", err
	}
	configStage := filepath.Join(stage, "config")
	if err := copyTree(filepath.Join(source, "config"), configStage); err != nil {
		return "", "", err
	}
	installerStage := filepath.Join(stage, "installer.yaml")
	if err := renderInstaller(source, stage, installerStage, filepath.Join(source, "bin", "kustomize"), image); err != nil {
		return "", "", err
	}
	return configStage, installerStage, nil
}

func generatorMakeArgs(source string) []string {
	toolDir := filepath.Join(source, "bin")
	return []string{
		"-C", source, "-f", "Makefile-ocp.mk", "manifests", "generate", "kustomize",
		"LOCALBIN=" + toolDir,
		"KUSTOMIZE=" + filepath.Join(toolDir, "kustomize"),
		"CONTROLLER_GEN=" + filepath.Join(toolDir, "controller-gen"),
		"KUSTOMIZE_VERSION=v5.7.1",
		"CONTROLLER_TOOLS_VERSION=v0.21.0",
	}
}

func renderInstaller(source, stage, installer, kustomize, image string) error {
	if !validImageReference(image) {
		return fmt.Errorf("invalid pinned operand image %q", image)
	}
	overlay := filepath.Join(stage, "installer-overlay")
	if err := os.MkdirAll(overlay, 0755); err != nil {
		return err
	}
	if err := os.Symlink(filepath.Join(source, "config/overlays/odh"), filepath.Join(overlay, "base")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("resources:\n- base\n"), 0644); err != nil {
		return err
	}
	if _, err := generatorCommand(overlay, true, kustomize, "edit", "set", "image", "controller="+image); err != nil {
		return err
	}
	data, err := generatorCommand(overlay, true, kustomize, "build", overlay)
	if err != nil {
		return err
	}
	return os.WriteFile(installer, data, 0644)
}

func copyTree(source, target string) error {
	return os.CopyFS(target, os.DirFS(source))
}

func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, info.Mode().Perm())
}
