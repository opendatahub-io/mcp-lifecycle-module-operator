package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSourcePin(t *testing.T) {
	version := "v0.0.0-20261014120423-4b22a898f27e"
	mod := map[string]any{
		"Require": []any{map[string]string{"Path": canonical, "Version": version}},
		"Replace": []any{map[string]any{
			"Old": map[string]string{"Path": canonical},
			"New": map[string]string{"Path": fork, "Version": version},
		}},
	}
	data, _ := json.Marshal(mod)
	if got, err := sourcePin(data); err != nil || got != version {
		t.Fatalf("sourcePin() = %q, %v", got, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing requirement": func(m map[string]any) { m["Require"] = []any{} },
		"missing replacement": func(m map[string]any) { m["Replace"] = []any{} },
		"wrong module": func(m map[string]any) {
			m["Replace"].([]any)[0].(map[string]any)["New"] = map[string]string{"Path": "example.test/wrong", "Version": version}
		},
		"different versions": func(m map[string]any) {
			m["Replace"].([]any)[0].(map[string]any)["New"] = map[string]string{"Path": fork, "Version": "v1.2.3"}
		},
		"unversioned replacement": func(m map[string]any) {
			m["Replace"].([]any)[0].(map[string]any)["New"] = map[string]string{"Path": fork}
		},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, _ := json.Marshal(mod)
			var copy map[string]any
			_ = json.Unmarshal(encoded, &copy)
			mutate(copy)
			encoded, _ = json.Marshal(copy)
			if _, err := sourcePin(encoded); err == nil {
				t.Fatal("expected invalid pin to fail")
			}
		})
	}
}

func TestModuleInfoAcceptsGoVersionsAndRequiresFullOrigin(t *testing.T) {
	commit := strings.Repeat("4b22a898f27e", 3) + "4b22"
	for _, ver := range []string{"v0.1.0", "v0.0.0-20261014120423-4b22a898f27e", "v0.1.1-0.20261014120423-4b22a898f27e", "v0.2.0-rc.1.0.20261014120423-4b22a898f27e"} {
		data, _ := json.Marshal(map[string]any{"Path": fork, "Version": ver, "Origin": map[string]string{"Hash": commit}})
		path, gotVersion, gotCommit, err := moduleInfo(data)
		if err != nil || path != fork || gotVersion != ver || gotCommit != commit {
			t.Fatalf("moduleInfo(%s) = %q, %q, %q, %v", ver, path, gotVersion, gotCommit, err)
		}
	}
	for _, tc := range []struct{ path, hash string }{
		{"example.test/wrong", commit}, {fork, "4b22a898f27e"}, {fork, strings.Repeat("z", 40)},
	} {
		data, _ := json.Marshal(map[string]any{"Path": tc.path, "Version": "v1.0.0", "Origin": map[string]string{"Hash": tc.hash}})
		if _, _, _, err := moduleInfo(data); err == nil {
			t.Fatalf("moduleInfo accepted invalid provenance: %s", data)
		}
	}
}

func TestGeneratorEnvironmentIsCheckoutLocal(t *testing.T) {
	t.Setenv("MAKEFLAGS", "sentinel-parent-flags")
	t.Setenv("MFLAGS", "sentinel-parent-mflags")
	t.Setenv("MAKEOVERRIDES", "sentinel-parent-overrides")
	t.Setenv("GNUMAKEFLAGS", "sentinel-gnu-flags")
	t.Setenv("MAKEFILES", "/outside/Makefile")
	t.Setenv("GOWORK", "/outside/go.work")
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("LOCALBIN", "/outside/bin")
	t.Setenv("KUSTOMIZE", "/outside/kustomize")
	t.Setenv("CONTROLLER_GEN", "/outside/controller-gen")
	env := strings.Join(generatorEnv(), "\n")
	for _, inherited := range []string{"MAKEFLAGS=", "MFLAGS=", "MAKEOVERRIDES=", "GNUMAKEFLAGS=", "MAKEFILES="} {
		if strings.Contains(env, inherited) {
			t.Errorf("generator environment retained %q", inherited)
		}
	}
	for _, setting := range []string{"GOWORK=off", "GOTOOLCHAIN=local"} {
		if !strings.Contains(env, setting) {
			t.Errorf("generator environment missing %q", setting)
		}
	}
	args := strings.Join(generatorMakeArgs(filepath.Join(t.TempDir(), "checkout")), "\n")
	for _, setting := range []string{"manifests", "generate", "kustomize", "LOCALBIN=", "KUSTOMIZE=", "CONTROLLER_GEN=", "KUSTOMIZE_VERSION=v5.7.1", "CONTROLLER_TOOLS_VERSION=v0.21.0"} {
		if !strings.Contains(args, setting) {
			t.Errorf("generator make arguments missing %q", setting)
		}
	}
	for _, unused := range []string{"build-installer", "IMG=", "IMAGE_TAG=", "KUSTOMIZE_DEFAULT_DIR="} {
		if strings.Contains(args, unused) {
			t.Errorf("generator make arguments unexpectedly contain %q", unused)
		}
	}
	if strings.Contains(args, "/outside/") {
		t.Fatalf("generator make arguments use inherited tool path: %s", args)
	}
}

func TestRenderInstallerKeepsSourceClean(t *testing.T) {
	for _, failBuild := range []bool{false, true} {
		name := "success"
		if failBuild {
			name = "build failure"
		}
		t.Run(name, func(t *testing.T) {
			stage := t.TempDir()
			source := filepath.Join(stage, "source")
			base := filepath.Join(source, "config", "overlays", "odh")
			manager := filepath.Join(source, "config", "manager", "kustomization.yaml")
			if err := os.MkdirAll(base, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(manager), 0755); err != nil {
				t.Fatal(err)
			}
			managerBytes := []byte("resources:\n- controller.yaml\n")
			if err := os.WriteFile(manager, managerBytes, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "kustomization.yaml"), []byte("resources: []\n"), 0644); err != nil {
				t.Fatal(err)
			}

			bin := t.TempDir()
			trace, fake := filepath.Join(stage, "commands.log"), filepath.Join(bin, "kustomize")
			script := `#!/bin/sh
printf '%s|%s\n' "$PWD" "$*" >> "$TRACE"
case "$1" in
  edit) printf '\n# edited overlay\n' >> kustomization.yaml ;;
  build) [ "$FAIL_BUILD" != 1 ] || exit 1; printf 'image: %s\n' "$EXPECTED_IMAGE" ;;
  *) exit 2 ;;
esac
`
			if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TRACE", trace)
			t.Setenv("EXPECTED_IMAGE", image)
			if failBuild {
				t.Setenv("FAIL_BUILD", "1")
			} else {
				t.Setenv("FAIL_BUILD", "")
			}
			installer := filepath.Join(stage, "installer.yaml")
			err := renderInstaller(source, stage, installer, fake)
			if (err != nil) != failBuild {
				t.Fatalf("renderInstaller() error = %v, want failure %v", err, failBuild)
			}
			if got, readErr := os.ReadFile(manager); readErr != nil || string(got) != string(managerBytes) {
				t.Fatalf("source manager kustomization changed: %q, %v", got, readErr)
			}
			overlay := filepath.Join(stage, "installer-overlay")
			if target, linkErr := os.Readlink(filepath.Join(overlay, "base")); linkErr != nil || target != base {
				t.Fatalf("overlay base symlink = %q, %v, want %q", target, linkErr, base)
			}
			if data, readErr := os.ReadFile(filepath.Join(overlay, "kustomization.yaml")); readErr != nil || !strings.Contains(string(data), "edited overlay") {
				t.Fatalf("temporary overlay was not edited: %q, %v", data, readErr)
			}
			commands, readErr := os.ReadFile(trace)
			wantCommands := overlay + "|edit set image controller=" + image + "\n" + overlay + "|build " + overlay + "\n"
			if readErr != nil || string(commands) != wantCommands {
				t.Fatalf("kustomize calls = %q, %v, want %q", commands, readErr, wantCommands)
			}
			data, readErr := os.ReadFile(installer)
			if failBuild {
				if !os.IsNotExist(readErr) {
					t.Fatalf("installer exists after failed build: %q, %v", data, readErr)
				}
			} else if readErr != nil || string(data) != "image: "+image+"\n" {
				t.Fatalf("installer = %q, %v", data, readErr)
			}
		})
	}
}

func TestGeneratorGoVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/source\n\ngo 1.26.3\ntoolchain go1.27.9\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nprintf '%s\\n' \"$TEST_GENERATOR_GO_VERSION\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, tc := range []struct {
		current string
		wantErr bool
	}{
		{"go1.26.5", false},
		{"go1.26.7 (Red Hat 1.26.7-2.el9_8)", false},
		{"go1.26.2 (Red Hat 1.26.2-1.el9_8)", true},
		{"", true},
	} {
		t.Run(tc.current, func(t *testing.T) {
			t.Setenv("TEST_GENERATOR_GO_VERSION", tc.current)
			if err := checkGeneratorGo(root); (err != nil) != tc.wantErr {
				t.Fatalf("checkGeneratorGo(%q) = %v, want error %v", tc.current, err, tc.wantErr)
			}
		})
	}
}

func TestRefsAndMissingLocalRef(t *testing.T) {
	for _, ref := range []string{"main", "rhoai-3.7-ea.1", "release/v1", strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		if err := validateRef(ref); err != nil {
			t.Errorf("validateRef(%q): %v", ref, err)
		}
	}
	for _, ref := range []string{"", "-upload-pack=evil", "HEAD", "bad..ref", "line\nbreak"} {
		if err := validateRef(ref); err == nil {
			t.Errorf("validateRef(%q) unexpectedly succeeded", ref)
		}
	}

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.Mkdir(repoPath, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repoPath, "init", "-q")
	if err := os.WriteFile(filepath.Join(repoPath, "file"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, repoPath, "add", ".")
	git(t, repoPath, "-c", "commit.gpgsign=false", "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture")
	if _, err := sourceCommit("missing-ref", filepath.Join(root, "checkout"), repoPath); err == nil {
		t.Fatal("fetch of missing local ref unexpectedly succeeded")
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestInstallRollbackOnReplacementOrBackupFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    int
		operation string
	}{
		{"config replacement", 0, "output"},
		{"installer replacement", 1, "output"},
		{"config backup", 0, "backup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stagedConfig, stagedInstaller := filepath.Join(root, "stage-config"), filepath.Join(root, "stage-installer")
			if err := os.Mkdir(stagedConfig, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stagedConfig, "new.yaml"), []byte("new config"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stagedInstaller, []byte("new installer"), 0644); err != nil {
				t.Fatal(err)
			}
			configTarget, installerTarget := filepath.Join(root, config), filepath.Join(root, installer)
			if err := os.MkdirAll(configTarget, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configTarget, "old.yaml"), []byte("old config"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(installerTarget), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installerTarget, []byte("old installer"), 0644); err != nil {
				t.Fatal(err)
			}

			realRename := rename
			t.Cleanup(func() { rename = realRename })
			targets := []string{configTarget, installerTarget}
			rename = func(old, new string) error {
				if (tc.operation == "output" && new == targets[tc.target] && filepath.Base(old) == "output") ||
					(tc.operation == "backup" && old == targets[tc.target] && filepath.Base(new) == "backup") {
					return os.ErrPermission
				}
				return realRename(old, new)
			}
			if err := installOutputs(root, stagedConfig, stagedInstaller); err == nil {
				t.Fatal("expected injected replacement failure")
			}
			if got, err := os.ReadFile(filepath.Join(configTarget, "old.yaml")); err != nil || string(got) != "old config" {
				t.Fatalf("config original not restored: %q, %v", got, err)
			}
			if got, err := os.ReadFile(installerTarget); err != nil || string(got) != "old installer" {
				t.Fatalf("installer original not restored: %q, %v", got, err)
			}
		})
	}
}

func TestInstallStagesOnTargetFilesystem(t *testing.T) {
	root, staged := t.TempDir(), t.TempDir()
	stagedConfig, stagedInstaller := filepath.Join(staged, "config"), filepath.Join(staged, "installer.yaml")
	if err := os.Mkdir(stagedConfig, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagedConfig, "manifest.yaml"), []byte("config"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedInstaller, []byte("installer"), 0644); err != nil {
		t.Fatal(err)
	}
	realRename := rename
	t.Cleanup(func() { rename = realRename })
	rename = func(old, new string) error {
		if old == stagedConfig || old == stagedInstaller {
			return syscall.EXDEV
		}
		return realRename(old, new)
	}
	if err := installOutputs(root, stagedConfig, stagedInstaller); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, config, "manifest.yaml")); err != nil || string(data) != "config" {
		t.Fatalf("config install = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, installer)); err != nil || string(data) != "installer" {
		t.Fatalf("installer install = %q, %v", data, err)
	}
}

func TestTreeDiffRejectsDanglingLinkAndLaterExtra(t *testing.T) {
	root := t.TempDir()
	want, got := filepath.Join(root, "want"), filepath.Join(root, "got")
	if err := os.Mkdir(want, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(got, 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{want, got} {
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("same"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(got, "zz-broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(got, "zzz-extra.yaml"), []byte("extra"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := treeDiff(want, got); err == nil {
		t.Fatal("treeDiff accepted a partially walked tree")
	}
}

func TestUpdateMetadataFailureRollsBackModulesBeforeOutputInstall(t *testing.T) {
	root := t.TempDir()
	modPath := filepath.Join(root, "go.mod")
	oldMod := []byte("old module bytes\n")
	if err := os.WriteFile(modPath, oldMod, 0640); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	stagedConfig, stagedInstaller := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
	if err := os.Mkdir(stagedConfig, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagedConfig, "new.yaml"), []byte("new config"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedInstaller, []byte("new installer"), 0644); err != nil {
		t.Fatal(err)
	}
	configTarget, installerTarget := filepath.Join(root, config), filepath.Join(root, installer)
	if err := os.MkdirAll(configTarget, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configTarget, "old.yaml"), []byte("old config"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(installerTarget), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installerTarget, []byte("old installer"), 0644); err != nil {
		t.Fatal(err)
	}
	version := "v0.0.0-20261014120423-4b22a898f27e"
	pinJSON, _ := json.Marshal(map[string]any{
		"Require": []any{map[string]string{"Path": canonical, "Version": version}},
		"Replace": []any{map[string]any{"Old": map[string]string{"Path": canonical}, "New": map[string]string{"Path": fork, "Version": version}}},
	})
	realCommand := command
	t.Cleanup(func() { command = realCommand })
	command = func(dir string, capture bool, name string, args ...string) ([]byte, error) {
		if name != "go" {
			return realCommand(dir, capture, name, args...)
		}
		switch {
		case len(args) >= 3 && args[0] == "mod" && args[1] == "edit" && args[2] == "-json":
			return pinJSON, nil
		case len(args) >= 2 && args[0] == "mod" && args[1] == "edit":
			return nil, os.WriteFile(modPath, []byte("updated module bytes\n"), 0640)
		case len(args) >= 2 && args[0] == "mod" && args[1] == "tidy":
			return nil, os.WriteFile(filepath.Join(root, "go.sum"), []byte("updated sum\n"), 0640)
		default:
			return nil, os.ErrInvalid
		}
	}
	output := filepath.Join(root, "missing-directory", "github-output")
	t.Setenv("GITHUB_OUTPUT", output)
	if err := applyUpdate(root, version, strings.Repeat("a", 40), "test-ref", stagedConfig, stagedInstaller); err == nil {
		t.Fatal("expected workflow-output open failure")
	}
	if got, err := os.ReadFile(modPath); err != nil || string(got) != string(oldMod) {
		t.Fatalf("go.mod rollback = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("originally absent go.sum remains: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(configTarget, "old.yaml")); err != nil || string(got) != "old config" {
		t.Fatalf("config changed before metadata succeeded: %q, %v", got, err)
	}
	if got, err := os.ReadFile(installerTarget); err != nil || string(got) != "old installer" {
		t.Fatalf("installer changed before metadata succeeded: %q, %v", got, err)
	}
}

func TestInstallCleansPreparedWorkspaceWhenNextParentIsInvalid(t *testing.T) {
	root, stage := t.TempDir(), t.TempDir()
	stagedConfig, stagedInstaller := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
	if err := os.Mkdir(stagedConfig, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedInstaller, []byte("installer"), 0644); err != nil {
		t.Fatal(err)
	}
	configParent := filepath.Join(root, config)
	blocker := filepath.Join(root, "internal", "controller", "resources")
	if err := os.MkdirAll(filepath.Dir(blocker), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installOutputs(root, stagedConfig, stagedInstaller); err == nil {
		t.Fatal("expected invalid installer parent to fail")
	}
	workspaces, err := filepath.Glob(filepath.Join(filepath.Dir(configParent), ".operand-install-*"))
	if err != nil || len(workspaces) != 0 {
		t.Fatalf("prepared workspaces remain: %v, %v", workspaces, err)
	}
}

func TestTreeDiffReportsAddedDeletedAndChanged(t *testing.T) {
	root := t.TempDir()
	want, got := filepath.Join(root, "want"), filepath.Join(root, "got")
	if err := os.Mkdir(want, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(got, 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"deleted.yaml": "old", "changed.yaml": "old"} {
		if err := os.WriteFile(filepath.Join(want, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{"added.yaml": "new", "changed.yaml": "new"} {
		if err := os.WriteFile(filepath.Join(got, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	diffs, err := treeDiff(want, got)
	if err != nil {
		t.Fatal(err)
	}
	wantDiffs := []string{"added: added.yaml", "changed: changed.yaml", "missing: deleted.yaml"}
	if strings.Join(diffs, "\n") != strings.Join(wantDiffs, "\n") {
		t.Fatalf("treeDiff() = %v, want %v", diffs, wantDiffs)
	}
}
