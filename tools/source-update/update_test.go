package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyUpdateUsesCanonicalPlaceholderAndRealForkVersion(t *testing.T) {
	for _, version := range []string{"v1.2.3", "v0.0.0-20261015110633-5892d3dab0c4"} {
		t.Run(version, func(t *testing.T) {
			root, stage := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			configStage, installerStage := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
			if err := os.Mkdir(configStage, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installerStage, []byte("installer"), 0644); err != nil {
				t.Fatal(err)
			}
			pinStage := stagedPin(t, stage)
			fakeModuleUpdaterCommand(t, root, "", version)
			output := filepath.Join(root, "github-output")
			if err := os.WriteFile(output, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GITHUB_OUTPUT", output)
			if err := applyUpdate(root, version, imageTestSource, "test-ref", testImage, configStage, installerStage, pinStage); err != nil {
				t.Fatal(err)
			}
			mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			wantRequire := "require " + canonical + " " + placeholderVersion
			wantReplace := "replace " + canonical + " => " + fork + " " + version
			if !strings.Contains(string(mod), wantRequire) || !strings.Contains(string(mod), wantReplace) {
				t.Fatalf("go.mod = %q, want %q and %q", mod, wantRequire, wantReplace)
			}
			gotOutput, err := os.ReadFile(output)
			wantOutput := "source_commit=" + imageTestSource + "\nsource_version=" + version + "\noperand_image=" + testImage + "\n"
			if err != nil || string(gotOutput) != wantOutput {
				t.Fatalf("workflow output = %q, %v, want %q", gotOutput, err, wantOutput)
			}
		})
	}
}

func TestUpdateResolutionAndGenerationFailuresLeaveRootUntouched(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		queryErr, badOrigin, imageErr bool
		generate                      bool
		wantError                     string
		wantImage, wantGeneration     bool
	}{
		{name: "module query", queryErr: true, wantError: "injected module query failure"},
		{name: "origin mismatch", badOrigin: true, wantError: "Go resolved " + fork + "@" + imageTestSource + " to source " + strings.Repeat("f", 40)},
		{name: "image lookup", imageErr: true, wantError: "injected image lookup failure", wantImage: true},
		{name: "generation", generate: true, wantError: "injected generation failure", wantImage: true, wantGeneration: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			originalMod := []byte("module test\n")
			modPath := filepath.Join(root, "go.mod")
			if err := os.WriteFile(modPath, originalMod, 0640); err != nil {
				t.Fatal(err)
			}
			modBefore, err := os.Stat(modPath)
			if err != nil {
				t.Fatal(err)
			}
			oldCommand, oldResolve, oldGenerate := command, resolveImage, generateOutputs
			t.Cleanup(func() { command, resolveImage, generateOutputs = oldCommand, oldResolve, oldGenerate })
			queryDir, checkout := "", ""
			command = func(dir string, _ bool, name string, args ...string) ([]byte, error) {
				switch name {
				case "git":
					switch {
					case len(args) == 2 && args[0] == "init":
						checkout = args[1]
					case len(args) == 6 && args[0] == "-C" && args[1] == checkout && args[2] == "remote" && args[3] == "add" && args[4] == "origin" && args[5] == repo:
					case len(args) == 6 && args[0] == "-C" && args[1] == checkout && args[2] == "fetch" && args[3] == "--depth=1" && args[4] == "origin" && args[5] == imageTestSource:
					case len(args) == 4 && args[0] == "-C" && args[1] == checkout && args[2] == "rev-parse" && args[3] == "FETCH_HEAD^{commit}":
						return []byte(imageTestSource), nil
					default:
						t.Fatalf("unexpected git command: %v", args)
					}
					return nil, nil
				case "go":
					if strings.Join(args, " ") != "list -m -json "+fork+"@"+imageTestSource {
						t.Fatalf("unexpected Go command in %q: %v", dir, args)
					}
					if dir == "" || dir == root {
						t.Fatalf("module query dir = %q, want isolated stage outside root %q", dir, root)
					}
					queryDir = dir
					if !strings.Contains("\n"+strings.Join(localToolchainEnv(), "\n")+"\n", "\nGOWORK=off\n") {
						t.Fatalf("module query environment does not set GOWORK=off")
					}
					if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("query side effect\n"), 0644); err != nil {
						t.Fatalf("write isolated query side effect: %v", err)
					}
					if tc.queryErr {
						return nil, errors.New("injected module query failure")
					}
					origin := imageTestSource
					if tc.badOrigin {
						origin = strings.Repeat("f", 40)
					}
					return []byte(fmt.Sprintf(`{"Path":%q,"Version":"v0.0.0-20261015110633-5892d3dab0c4","Origin":{"Hash":%q}}`, fork, origin)), nil
				default:
					t.Fatalf("unexpected command %s in %q: %v", name, dir, args)
					return nil, nil
				}
			}
			imageCalled, generationCalled := false, false
			resolveImage = func(string) (string, error) {
				imageCalled = true
				if tc.imageErr {
					return "", errors.New("injected image lookup failure")
				}
				return testImage, nil
			}
			generateOutputs = func(_, _, _ string) (string, string, error) {
				generationCalled = true
				if tc.generate {
					return "", "", errors.New("injected generation failure")
				}
				return "", "", nil
			}
			if err := update(root, imageTestSource); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("update() error = %v, want %q", err, tc.wantError)
			}
			if queryDir == "" || queryDir == root {
				t.Fatalf("module query dir = %q, want isolated stage", queryDir)
			}
			if imageCalled != tc.wantImage || generationCalled != tc.wantGeneration {
				t.Fatalf("downstream calls = image:%t generation:%t, want image:%t generation:%t", imageCalled, generationCalled, tc.wantImage, tc.wantGeneration)
			}
			if got, err := os.ReadFile(modPath); err != nil || string(got) != string(originalMod) {
				t.Errorf("go.mod after failed update = %q, %v; want original bytes", got, err)
			}
			if info, err := os.Stat(modPath); err != nil || info.Mode().Perm() != 0640 || !info.ModTime().Equal(modBefore.ModTime()) {
				t.Errorf("go.mod after failed update = %v, %v; want original mode and mtime", info, err)
			}
			if got, err := os.ReadFile(filepath.Join(root, "go.sum")); !os.IsNotExist(err) {
				t.Errorf("go.sum after failed update = %q, %v; want absent", got, err)
			}
		})
	}
}

func TestApplyUpdateRejectsPostTidyModuleDriftAndRollsBack(t *testing.T) {
	version := "v1.2.3"
	for _, tc := range []struct {
		name, require, replacement, wantError string
	}{
		{"replacement version", placeholderVersion, "v1.2.4", "go mod tidy changed the synchronized source pin"},
		{"canonical require", "v1.2.4", version, "go.mod must require " + canonical + " at placeholder version " + placeholderVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, stage := t.TempDir(), t.TempDir()
			modPath := filepath.Join(root, "go.mod")
			oldMod := []byte("original module\n")
			if err := os.WriteFile(modPath, oldMod, 0640); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "github-output")
			oldOutput := []byte("previous=entry\n")
			if err := os.WriteFile(output, oldOutput, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GITHUB_OUTPUT", output)

			configStage, installerStage := filepath.Join(stage, "config"), filepath.Join(stage, "installer.yaml")
			if err := os.Mkdir(configStage, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configStage, "new.yaml"), []byte("new config"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installerStage, []byte("new installer"), 0644); err != nil {
				t.Fatal(err)
			}
			pinStage := stagedPin(t, stage)
			configTarget := filepath.Join(root, config, "old.yaml")
			if err := os.MkdirAll(filepath.Dir(configTarget), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configTarget, []byte("old config"), 0644); err != nil {
				t.Fatal(err)
			}
			installerTarget := filepath.Join(root, installer)
			if err := os.MkdirAll(filepath.Dir(installerTarget), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installerTarget, []byte("old installer"), 0644); err != nil {
				t.Fatal(err)
			}
			pinTarget := filepath.Join(root, imagePin)
			if err := os.WriteFile(pinTarget, []byte("old image pin\n"), 0644); err != nil {
				t.Fatal(err)
			}

			beforeTidy, err := json.Marshal(sourceMod(placeholderVersion, version))
			if err != nil {
				t.Fatal(err)
			}
			afterTidy, err := json.Marshal(sourceMod(tc.require, tc.replacement))
			if err != nil {
				t.Fatal(err)
			}
			oldCommand := command
			t.Cleanup(func() { command = oldCommand })
			tidied := false
			command = func(_ string, _ bool, name string, args ...string) ([]byte, error) {
				if name != "go" {
					return nil, fmt.Errorf("unexpected command %s %v", name, args)
				}
				switch strings.Join(args, " ") {
				case "mod edit -json":
					if tidied {
						return afterTidy, nil
					}
					return beforeTidy, nil
				case "mod edit -require=" + canonical + "@" + placeholderVersion + " -replace=" + canonical + "=" + fork + "@" + version:
					return nil, os.WriteFile(modPath, []byte(fmt.Sprintf("module test\n\nrequire %s %s\n\nreplace %s => %s %s\n", canonical, placeholderVersion, canonical, fork, version)), 0644)
				case "mod tidy":
					if err := os.WriteFile(modPath, []byte(fmt.Sprintf("module test\n\nrequire %s %s\n\nreplace %s => %s %s\n", canonical, tc.require, canonical, fork, tc.replacement)), 0644); err != nil {
						return nil, err
					}
					if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte("tidied sums\n"), 0644); err != nil {
						return nil, err
					}
					tidied = true
					return nil, nil
				default:
					return nil, fmt.Errorf("unexpected Go module command: %v", args)
				}
			}

			err = applyUpdate(root, version, imageTestSource, "test-ref", testImage, configStage, installerStage, pinStage)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("applyUpdate() error = %v, want %q", err, tc.wantError)
			}
			if !tidied {
				t.Fatal("tidy was not reached")
			}
			if got, err := os.ReadFile(modPath); err != nil || string(got) != string(oldMod) {
				t.Fatalf("go.mod rollback = %q, %v", got, err)
			}
			if info, err := os.Stat(modPath); err != nil || info.Mode().Perm() != 0640 {
				t.Fatalf("go.mod mode after rollback = %v, %v, want 0640", info, err)
			}
			if _, err := os.Stat(filepath.Join(root, "go.sum")); !os.IsNotExist(err) {
				t.Fatalf("new go.sum remains after rollback: %v", err)
			}
			if got, err := os.ReadFile(output); err != nil || string(got) != string(oldOutput) {
				t.Fatalf("workflow output changed before guard: %q, %v", got, err)
			}
			for path, want := range map[string]string{
				configTarget:    "old config",
				installerTarget: "old installer",
				pinTarget:       "old image pin\n",
			} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Errorf("installed output %s changed before guard: %q, %v", path, got, err)
				}
			}
		})
	}
}

func TestVerifyRejectsReturnedForkVersionMismatchBeforeGeneration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pinned, resolved := "v1.2.3", "v1.2.4"
	mod, err := json.Marshal(sourceMod(placeholderVersion, pinned))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeImagePin(filepath.Join(root, imagePin), imagePinRecord{SourceCommit: imageTestSource, Image: testImage}); err != nil {
		t.Fatal(err)
	}
	oldCommand := command
	t.Cleanup(func() { command = oldCommand })
	queries := 0
	command = func(dir string, _ bool, name string, args ...string) ([]byte, error) {
		if name != "go" {
			t.Fatalf("unexpected command %s %s %v; expected root metadata query", name, dir, args)
		}
		switch strings.Join(args, " ") {
		case "mod edit -json":
			if dir != root {
				t.Fatalf("go mod edit -json dir = %q, want project root %q", dir, root)
			}
			return mod, nil
		case "list -m -json " + fork + "@" + pinned:
			queries++
			if dir == root {
				t.Fatalf("module query dir = %q, want isolated stage", dir)
			}
			return []byte(fmt.Sprintf(`{"Path":%q,"Version":%q,"Origin":{"Hash":%q}}`, fork, resolved, imageTestSource)), nil
		default:
			t.Fatalf("unexpected Go command in %q: %v; expected root metadata query or isolated pinned query", dir, args)
			return nil, os.ErrInvalid
		}
	}
	if err := verify(root); err == nil || !strings.Contains(err.Error(), "pinned version "+pinned+" resolved as "+resolved) {
		t.Fatalf("verify() error = %v, want returned-version mismatch", err)
	}
	if queries != 1 {
		t.Fatalf("fork version queries = %d, want 1", queries)
	}
}

func TestVerifyQueryFailureLeavesRootModuleFilesUntouched(t *testing.T) {
	const version = "v0.0.0-20261020144941-3d03622e7b27"
	for _, sumExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("go.sum exists=%t", sumExists), func(t *testing.T) {
			root := t.TempDir()
			modPath, sumPath := filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")
			originalMod, originalSum := []byte("original module bytes\n"), []byte("original sums\n")
			if err := os.WriteFile(modPath, originalMod, 0640); err != nil {
				t.Fatal(err)
			}
			if sumExists {
				if err := os.WriteFile(sumPath, originalSum, 0600); err != nil {
					t.Fatal(err)
				}
			}
			modJSON, err := json.Marshal(sourceMod(placeholderVersion, version))
			if err != nil {
				t.Fatal(err)
			}
			modBefore, err := os.Stat(modPath)
			if err != nil {
				t.Fatal(err)
			}
			var sumBefore os.FileInfo
			if sumExists {
				sumBefore, err = os.Stat(sumPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			oldCommand := command
			t.Cleanup(func() { command = oldCommand })
			queries, queryDir := 0, ""
			command = func(dir string, _ bool, name string, args ...string) ([]byte, error) {
				if name != "go" {
					t.Fatalf("unexpected verify command %s %s %v; expected go query in isolated stage", name, dir, args)
				}
				switch strings.Join(args, " ") {
				case "mod edit -json":
					if dir != root {
						t.Fatalf("go mod edit -json dir = %q, want root %q", dir, root)
					}
					return modJSON, nil
				case "list -m -json " + fork + "@" + version:
					queries++
					if dir == root {
						t.Fatalf("module query dir = %q, want isolated stage", dir)
					}
					queryDir = dir
					if !strings.Contains("\n"+strings.Join(localToolchainEnv(), "\n")+"\n", "\nGOWORK=off\n") {
						t.Fatalf("module query environment lacks GOWORK=off")
					}
					if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("injected query side effect\n"), 0644); err != nil {
						return nil, err
					}
					return nil, errors.New("injected verify module query failure")
				default:
					t.Fatalf("unexpected Go command %v; expected isolated list query", args)
					return nil, os.ErrInvalid
				}
			}

			err = verify(root)
			if err == nil || !strings.Contains(err.Error(), "injected verify module query failure") {
				t.Fatalf("verify() error = %v, want injected module query failure", err)
			}
			if queries != 1 || queryDir == "" || queryDir == root {
				t.Fatalf("module query count/dir = %d/%q, want one query in isolated stage", queries, queryDir)
			}
			if got, err := os.ReadFile(modPath); err != nil || string(got) != string(originalMod) {
				t.Fatalf("go.mod after verify = %q, %v; want original bytes", got, err)
			}
			if info, err := os.Stat(modPath); err != nil || info.Mode().Perm() != 0640 || !info.ModTime().Equal(modBefore.ModTime()) {
				t.Fatalf("go.mod after verify = %v, %v; want original mode and mtime", info, err)
			}
			gotSum, err := os.ReadFile(sumPath)
			if sumExists {
				if err != nil || string(gotSum) != string(originalSum) {
					t.Fatalf("go.sum after verify = %q, %v; want original bytes", gotSum, err)
				}
				if info, statErr := os.Stat(sumPath); statErr != nil || info.Mode().Perm() != 0600 || !info.ModTime().Equal(sumBefore.ModTime()) {
					t.Fatalf("go.sum after verify = %v, %v; want original mode and mtime", info, statErr)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("go.sum created by verify remains: %q, %v", gotSum, err)
			}
		})
	}
}

func TestApplyUpdateRollsBackModuleEditAndTidyFailures(t *testing.T) {
	for _, failAt := range []string{"edit", "tidy"} {
		for _, sumExists := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sum=%t", failAt, sumExists), func(t *testing.T) {
				root, stage := t.TempDir(), t.TempDir()
				oldMod := []byte("original module\n")
				if err := os.WriteFile(filepath.Join(root, "go.mod"), oldMod, 0640); err != nil {
					t.Fatal(err)
				}
				oldSum := []byte("original sums\n")
				if sumExists {
					if err := os.WriteFile(filepath.Join(root, "go.sum"), oldSum, 0600); err != nil {
						t.Fatal(err)
					}
				}
				configStage := filepath.Join(stage, "config")
				if err := os.Mkdir(configStage, 0755); err != nil {
					t.Fatal(err)
				}
				installerStage := filepath.Join(stage, "installer.yaml")
				if err := os.WriteFile(installerStage, []byte("installer"), 0644); err != nil {
					t.Fatal(err)
				}
				pin := stagedPin(t, stage)
				fakeModuleUpdaterCommand(t, root, failAt, "v1.2.3")
				err := applyUpdate(root, "v1.2.3", imageTestSource, "test-ref", testImage, configStage, installerStage, pin)
				if err == nil || !strings.Contains(err.Error(), "injected go mod "+failAt+" failure") {
					t.Fatalf("applyUpdate() error = %v, want injected go mod %s failure", err, failAt)
				}
				modPath := filepath.Join(root, "go.mod")
				if got, err := os.ReadFile(modPath); err != nil || string(got) != string(oldMod) {
					t.Fatalf("go.mod rollback = %q, %v", got, err)
				}
				if info, err := os.Stat(modPath); err != nil || info.Mode().Perm() != 0640 {
					t.Fatalf("go.mod mode after rollback = %v, %v, want 0640", info, err)
				}
				gotSum, err := os.ReadFile(filepath.Join(root, "go.sum"))
				if sumExists && (err != nil || string(gotSum) != string(oldSum)) {
					t.Fatalf("go.sum rollback = %q, %v", gotSum, err)
				}
				if sumExists {
					if info, statErr := os.Stat(filepath.Join(root, "go.sum")); statErr != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("go.sum mode after rollback = %v, %v, want 0600", info, statErr)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("new go.sum remains: %q, %v", gotSum, err)
				}
			})
		}
	}
}
