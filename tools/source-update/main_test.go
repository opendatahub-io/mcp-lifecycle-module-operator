package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestVerificationTargetsKeepSourceRegenerationExplicit(t *testing.T) {
	for _, target := range []string{"verify", "verify-operand-manifests"} {
		t.Run(target, func(t *testing.T) {
			cmd := exec.Command("make", "--no-print-directory", "--dry-run", "--assume-old=manifests", "--assume-old=generate", "--assume-old=fmt", "--assume-old=tidy", target)
			cmd.Dir, cmd.Env = "../..", generatorEnv()
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s dry run: %v\n%s", target, err, out)
			}
			regenerates := strings.Contains(string(out), "go run ./tools/source-update verify")
			if regenerates != (target == "verify-operand-manifests") {
				t.Fatalf("unexpected source regeneration in %s:\n%s", target, out)
			}
			if target == "verify" && !strings.Contains(string(out), "go test ./tools/source-update") {
				t.Fatalf("local gate lost updater unit tests:\n%s", out)
			}
		})
	}
	workflow, err := os.ReadFile("../../.github/workflows/verify.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"verify", "verify-operand-manifests"} {
		if !strings.Contains(string(workflow), "run: make "+target+"\n") {
			t.Errorf("CI must explicitly run make %s", target)
		}
	}
}

func TestDeployUsesEmbeddedImageUnlessOverridden(t *testing.T) {
	key := "RELATED_IMAGE_ODH_MCP_LIFECYCLE_OPERATOR_IMAGE"
	mirror := "mirror.example/operator@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, override, want string
	}{
		{"default", "", key + "-"},
		{"empty", "MCPLO_OPERAND_IMAGE=", key + "-"},
		{"whitespace", "MCPLO_OPERAND_IMAGE=   ", key + "-"},
		{"mirror", "MCPLO_OPERAND_IMAGE=" + mirror, key + "=" + mirror},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--no-print-directory", "--dry-run", "--assume-old=manifests", "--assume-old=kustomize", "deploy"}
			if tc.override != "" {
				args = append(args, tc.override)
			}
			cmd := exec.Command("make", args...)
			cmd.Dir = "../.."
			for _, item := range generatorEnv() {
				if !strings.HasPrefix(item, "MCPLO_OPERAND_IMAGE=") {
					cmd.Env = append(cmd.Env, item)
				}
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("deploy dry run: %v\n%s", err, out)
			}
			if !strings.HasSuffix(string(out), "\t"+tc.want+"\n") {
				t.Fatalf("deploy did not render %q as its final argument:\n%s", tc.want, out)
			}
		})
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

const testImage = "quay.io/opendatahub/odh-mcp-lifecycle-operator@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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
			t.Setenv("EXPECTED_IMAGE", testImage)
			if failBuild {
				t.Setenv("FAIL_BUILD", "1")
			} else {
				t.Setenv("FAIL_BUILD", "")
			}
			installer := filepath.Join(stage, "installer.yaml")
			err := renderInstaller(source, stage, installer, fake, testImage)
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
			wantCommands := overlay + "|edit set image controller=" + testImage + "\n" + overlay + "|build " + overlay + "\n"
			if readErr != nil || string(commands) != wantCommands {
				t.Fatalf("kustomize calls = %q, %v, want %q", commands, readErr, wantCommands)
			}
			data, readErr := os.ReadFile(installer)
			if failBuild {
				if !os.IsNotExist(readErr) {
					t.Fatalf("installer exists after failed build: %q, %v", data, readErr)
				}
			} else if readErr != nil || string(data) != "image: "+testImage+"\n" {
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

func stagedPin(t *testing.T, dir string) string {
	t.Helper()
	filename := filepath.Join(dir, "staged-image-pin.json")
	if err := os.WriteFile(filename, []byte("new image pin\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return filename
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
		hasOldPin bool
	}{
		{"config replacement", 0, "output", false},
		{"installer replacement", 1, "output", false},
		{"config backup", 0, "backup", false},
		{"pin replacement with original", 2, "output", true},
		{"pin replacement without original", 2, "output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stagedConfig, stagedInstaller := filepath.Join(root, "stage-config"), filepath.Join(root, "stage-installer")
			stagedPin := stagedPin(t, root)
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

			pinTarget := filepath.Join(root, imagePin)
			if tc.hasOldPin {
				if err := os.WriteFile(pinTarget, []byte("old image pin\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			realRename := rename
			t.Cleanup(func() { rename = realRename })
			targets := []string{configTarget, installerTarget, pinTarget}
			rename = func(old, new string) error {
				if (tc.operation == "output" && new == targets[tc.target] && filepath.Base(old) == "output") ||
					(tc.operation == "backup" && old == targets[tc.target] && filepath.Base(new) == "backup") {
					return os.ErrPermission
				}
				return realRename(old, new)
			}
			if err := installOutputs(root, stagedConfig, stagedInstaller, stagedPin); err == nil {
				t.Fatal("expected injected replacement failure")
			}
			if got, err := os.ReadFile(filepath.Join(configTarget, "old.yaml")); err != nil || string(got) != "old config" {
				t.Fatalf("config original not restored: %q, %v", got, err)
			}
			if got, err := os.ReadFile(installerTarget); err != nil || string(got) != "old installer" {
				t.Fatalf("installer original not restored: %q, %v", got, err)
			}
			gotPin, pinErr := os.ReadFile(pinTarget)
			if tc.hasOldPin && (pinErr != nil || string(gotPin) != "old image pin\n") {
				t.Fatalf("image pin original not restored: %q, %v", gotPin, pinErr)
			}
			if !tc.hasOldPin && !os.IsNotExist(pinErr) {
				t.Fatalf("new image pin remains after rollback: %q, %v", gotPin, pinErr)
			}
		})
	}
}

func TestInstallStagesOnTargetFilesystem(t *testing.T) {
	root, staged := t.TempDir(), t.TempDir()
	stagedConfig, stagedInstaller := filepath.Join(staged, "config"), filepath.Join(staged, "installer.yaml")
	stagedPin := stagedPin(t, staged)
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
		if old == stagedConfig || old == stagedInstaller || old == stagedPin {
			return syscall.EXDEV
		}
		return realRename(old, new)
	}
	if err := installOutputs(root, stagedConfig, stagedInstaller, stagedPin); err != nil {
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

func TestWorkflowOutputSnapshotRestoresOriginalOnLaterFailure(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "github-output")
	original := []byte("previous=entry\n")
	if err := os.WriteFile(filename, original, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotWorkflowOutput(filename)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("source_commit=partial\noperand_image=partial\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filename); err != nil || string(got) != string(original) {
		t.Fatalf("workflow output rollback = %q, %v", got, err)
	}
}

func TestUpdateMetadataFailureLeavesModulesUntouchedBeforeOutputInstall(t *testing.T) {
	root := t.TempDir()
	modPath := filepath.Join(root, "go.mod")
	oldMod := []byte("old module bytes\n")
	if err := os.WriteFile(modPath, oldMod, 0640); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	stagedConfig, stagedInstaller := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
	stagedPin := stagedPin(t, stage)
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
	output := filepath.Join(root, "missing-directory", "github-output")
	t.Setenv("GITHUB_OUTPUT", output)
	if err := applyUpdate(root, strings.Repeat("a", 40), "test-ref", testImage, stagedConfig, stagedInstaller, stagedPin); err == nil {
		t.Fatal("expected workflow-output open failure")
	}
	if got, err := os.ReadFile(modPath); err != nil || string(got) != string(oldMod) {
		t.Fatalf("go.mod changed = %q, %v", got, err)
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

func TestApplyUpdateLeavesModuleFilesUntouched(t *testing.T) {
	for _, modulesPresent := range []bool{false, true} {
		for _, failInstall := range []bool{false, true} {
			t.Run(fmt.Sprintf("modules=%t/failure=%t", modulesPresent, failInstall), func(t *testing.T) {
				root, stage := t.TempDir(), t.TempDir()
				for _, name := range []string{"go.mod", "go.sum"} {
					if modulesPresent {
						if err := os.WriteFile(filepath.Join(root, name), []byte("untouched "+name), 0640); err != nil {
							t.Fatal(err)
						}
					}
				}
				stagedConfig, stagedInstaller := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
				if err := os.Mkdir(stagedConfig, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stagedInstaller, []byte("new installer"), 0644); err != nil {
					t.Fatal(err)
				}
				pin := stagedPin(t, stage)
				output := filepath.Join(root, "github-output")
				if err := os.WriteFile(output, []byte("previous=entry\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GITHUB_OUTPUT", output)
				realRename := rename
				t.Cleanup(func() { rename = realRename })
				rename = func(old, new string) error {
					if failInstall && new == filepath.Join(root, imagePin) {
						return os.ErrPermission
					}
					return realRename(old, new)
				}
				if err := applyUpdate(root, imageTestSource, "test-ref", testImage, stagedConfig, stagedInstaller, pin); (err != nil) != failInstall {
					t.Fatalf("applyUpdate() = %v, want failure %t", err, failInstall)
				}
				for _, name := range []string{"go.mod", "go.sum"} {
					data, err := os.ReadFile(filepath.Join(root, name))
					if modulesPresent {
						info, statErr := os.Stat(filepath.Join(root, name))
						if err != nil || string(data) != "untouched "+name || statErr != nil || info.Mode().Perm() != 0640 {
							t.Fatalf("%s changed: %q, %v, %v", name, data, err, statErr)
						}
					} else if !os.IsNotExist(err) {
						t.Fatalf("%s was created: %q, %v", name, data, err)
					}
				}
				wantOutput := "previous=entry\n"
				if !failInstall {
					wantOutput += "source_commit=" + imageTestSource + "\noperand_image=" + testImage + "\n"
				}
				if got, err := os.ReadFile(output); err != nil || string(got) != wantOutput {
					t.Fatalf("workflow output = %q, %v, want %q", got, err, wantOutput)
				}
			})
		}
	}
}

type forbiddenRegistryTransport struct{ t *testing.T }

func (transport forbiddenRegistryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Errorf("verification rediscovered registry image: %s", request.URL)
	return nil, os.ErrPermission
}

func TestVerifyUsesSavedLockWithoutModuleOrRegistryResolution(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	if err := writeImagePin(filepath.Join(root, imagePin), imagePinRecord{SourceCommit: imageTestSource, Image: testImage}); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"go":   "#!/bin/sh\n[ \"$*\" = 'env GOVERSION' ] || exit 1\nprintf 'go1.26.5\\n'\n",
		"make": "#!/bin/sh\nmkdir -p \"$2/bin\"\ncp \"$TEST_KUSTOMIZE\" \"$2/bin/kustomize\"\n",
		"kustomize": `#!/bin/sh
case "$1" in
  edit) printf '%s\n' "${4#controller=}" > selected-image ;;
  build) read -r image < "$2/selected-image"; printf 'image: %s\n' "$image" ;;
  *) exit 1 ;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_KUSTOMIZE", filepath.Join(bin, "kustomize"))
	realCommand, realTransport := command, http.DefaultTransport
	t.Cleanup(func() { command, http.DefaultTransport = realCommand, realTransport })
	http.DefaultTransport = forbiddenRegistryTransport{t}
	checkoutMismatch := false
	fetches := 0
	command = func(dir string, capture bool, name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("unexpected source/module command: %s %v", name, args)
		}
		if args[0] == "init" {
			source := args[1]
			for name, data := range map[string]string{"go.mod": "module example.test/source\n\ngo 1.26.3\n", "config/manifest.yaml": "config\n", "config/overlays/odh/kustomization.yaml": "resources: []\n"} {
				filename := filepath.Join(source, name)
				if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(data), 0644); err != nil {
					t.Fatal(err)
				}
			}
			return nil, nil
		}
		switch strings.Join(args[2:], " ") {
		case "remote add origin " + repo, "checkout --detach FETCH_HEAD":
			return nil, nil
		case "fetch --depth=1 origin " + imageTestSource:
			fetches++
			return nil, nil
		case "rev-parse FETCH_HEAD^{commit}":
			return []byte(imageTestSource), nil
		case "rev-parse HEAD":
			if checkoutMismatch {
				return []byte(strings.Repeat("f", 40)), nil
			}
			return []byte(imageTestSource), nil
		default:
			t.Fatalf("verification did not use saved commit: %v", args)
			return nil, os.ErrInvalid
		}
	}
	for name, data := range map[string]string{
		config + "/manifest.yaml":                   "config\n",
		config + "/overlays/odh/kustomization.yaml": "resources: []\n",
		installer: "image: " + testImage + "\n",
	} {
		filename := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := verify(root); err != nil {
		t.Fatal(err)
	}
	checkoutMismatch = true
	if err := verify(root); err == nil || !strings.Contains(err.Error(), "checkout mismatch") {
		t.Fatalf("verify() accepted mismatched checkout: %v", err)
	}
	checkoutMismatch = false
	for _, filename := range []string{filepath.Join(root, config, "manifest.yaml"), filepath.Join(root, installer)} {
		original, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte("drift\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := verify(root); err == nil || !strings.Contains(err.Error(), "MCPLO_REF="+imageTestSource) {
			t.Fatalf("verify() accepted drift: %v", err)
		}
		if err := os.WriteFile(filename, original, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, imagePin), []byte(`{"sourceCommit":"short"}`), 0644); err != nil {
		t.Fatal(err)
	}
	before := fetches
	if err := verify(root); err == nil || fetches != before {
		t.Fatalf("verify() did not reject invalid lock before generation: fetches=%d, error=%v", fetches, err)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("verification created root %s: %v", name, err)
		}
	}
}

func TestInstallCleansPreparedWorkspaceWhenNextParentIsInvalid(t *testing.T) {
	root, stage := t.TempDir(), t.TempDir()
	stagedConfig, stagedInstaller := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
	stagedPin := stagedPin(t, stage)
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
	if err := installOutputs(root, stagedConfig, stagedInstaller, stagedPin); err == nil {
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
