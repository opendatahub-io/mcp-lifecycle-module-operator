package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSourcePinRequiresCanonicalPlaceholderAndRealForkVersion(t *testing.T) {
	for _, version := range []string{"v1.2.3", "v0.0.0-20261015110633-5892d3dab0c4"} {
		t.Run(version, func(t *testing.T) {
			data, err := json.Marshal(sourceMod(placeholderVersion, version))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := sourcePin(data); err != nil || got != version {
				t.Fatalf("sourcePin() = %q, %v, want real fork version %q", got, err, version)
			}
		})
	}

	duplicate := sourceMod(placeholderVersion, "v1.2.3")
	duplicate["Require"] = append(require(placeholderVersion), require(placeholderVersion)...)
	for _, tc := range []struct {
		name string
		mod  map[string]any
	}{
		{name: "missing require", mod: map[string]any{"Replace": []any{}}},
		{name: "duplicate require", mod: duplicate},
		{name: "canonical not placeholder", mod: sourceMod("v1.2.3", "v1.2.3")},
		{name: "missing replacement", mod: map[string]any{"Require": require(placeholderVersion)}},
		{name: "missing replacement version", mod: sourceMod(placeholderVersion, "")},
		{name: "placeholder replacement", mod: sourceMod(placeholderVersion, placeholderVersion)},
		{name: "invalid replacement version", mod: sourceMod(placeholderVersion, "not-a-version")},
		{name: "mismatched fork", mod: sourceModTo(placeholderVersion, "v1.2.3", "example.test/fork")},
		{name: "versioned old path", mod: sourceModOldVersion("v1.2.3")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.mod)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sourcePin(data); err == nil {
				t.Fatal("sourcePin() accepted invalid module metadata")
			}
		})
	}
}

func TestModuleInfoRequiresRealForkVersionAndFullOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, path, version, origin string
		wantErr                     bool
	}{
		{"pseudo version", fork, "v0.0.0-20261015110633-5892d3dab0c4", strings.Repeat("a", 40), false},
		{"semver", fork, "v1.2.3", strings.Repeat("b", 40), false},
		{"wrong module", "example.test/fork", "v1.2.3", strings.Repeat("a", 40), true},
		{"empty version", fork, "", strings.Repeat("a", 40), true},
		{"placeholder version", fork, placeholderVersion, strings.Repeat("a", 40), true},
		{"invalid version", fork, "not-a-version", strings.Repeat("a", 40), true},
		{"short origin", fork, "v1.2.3", "5892d3d", true},
		{"nonhex origin", fork, "v1.2.3", strings.Repeat("z", 40), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"Path": tc.path, "Version": tc.version,
				"Origin": map[string]string{"Hash": tc.origin},
			})
			if err != nil {
				t.Fatal(err)
			}
			path, version, origin, err := moduleInfo(data)
			if (err != nil) != tc.wantErr {
				t.Fatalf("moduleInfo() = %q, %q, %q, %v", path, version, origin, err)
			}
		})
	}
}

func require(version string) []map[string]string {
	return []map[string]string{{"Path": canonical, "Version": version}}
}

func sourceMod(required, replacement string) map[string]any {
	return sourceModTo(required, replacement, fork)
}

func sourceModTo(required, replacement, replacementPath string) map[string]any {
	return map[string]any{
		"Require": require(required),
		"Replace": []map[string]any{{
			"Old": map[string]string{"Path": canonical},
			"New": map[string]string{"Path": replacementPath, "Version": replacement},
		}},
	}
}

func sourceModOldVersion(version string) map[string]any {
	mod := sourceMod(placeholderVersion, version)
	mod["Replace"] = []map[string]any{{
		"Old": map[string]string{"Path": canonical, "Version": "v1.0.0"},
		"New": map[string]string{"Path": fork, "Version": version},
	}}
	return mod
}
