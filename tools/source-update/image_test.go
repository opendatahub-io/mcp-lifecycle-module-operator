package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const imageTestSource = "5892d3dab0c46adf64a6e0d3109da8e71b814d37"

type registryFixture struct {
	rootDigest string
	root       []byte
	manifests  map[string][]byte
	blobs      map[string][]byte
}

func makeRegistryFixture(t *testing.T, source string, architectures ...string) registryFixture {
	return makeRegistryFixtureWithChildLabels(t, source, nil, architectures...)
}

func makeRegistryFixtureWithChildLabels(t *testing.T, source string, childLabels []map[string]string, architectures ...string) registryFixture {
	t.Helper()
	fixture := registryFixture{manifests: map[string][]byte{}, blobs: map[string][]byte{}}
	var descriptors []map[string]any
	for i, arch := range architectures {
		labels := map[string]string{
			"git.commit":                        source,
			"git.url":                           "https://github.com/opendatahub-io/mcp-lifecycle-operator",
			"org.opencontainers.image.revision": source,
			"vcs-ref":                           source,
		}
		if i < len(childLabels) && childLabels[i] != nil {
			labels = childLabels[i]
		}
		config, err := json.Marshal(map[string]any{
			"os": "linux", "architecture": arch,
			"config": map[string]any{"Labels": labels},
		})
		if err != nil {
			t.Fatal(err)
		}
		configDigest := bodyDigest(config)
		fixture.blobs[configDigest] = config
		manifest := map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)},
			"layers":        []any{},
		}
		if i == 0 {
			manifest["annotations"] = map[string]string{"build": "one"}
		} else {
			manifest["annotations"] = map[string]string{"build": "two"}
		}
		manifestBody, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifestDigest := bodyDigest(manifestBody)
		fixture.manifests[manifestDigest] = manifestBody
		descriptors = append(descriptors, map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    manifestDigest,
			"size":      len(manifestBody),
			"platform":  map[string]string{"os": "linux", "architecture": arch},
		})
	}
	root, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     descriptors,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.root = root
	fixture.rootDigest = bodyDigest(root)
	return fixture
}

func bodyDigest(body []byte) string {
	hash := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func setFixturePlatform(t *testing.T, fixture *registryFixture, index int, platform map[string]string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(fixture.root, &root); err != nil {
		t.Fatal(err)
	}
	descriptors := root["manifests"].([]any)
	descriptor := descriptors[index].(map[string]any)
	if platform == nil {
		delete(descriptor, "platform")
	} else {
		descriptor["platform"] = platform
	}
	fixture.root, _ = json.Marshal(root)
	fixture.rootDigest = bodyDigest(fixture.root)
}

func (fixture registryFixture) handler(t *testing.T, root func() registryFixture, rootStatus int, rootDigest string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		validPath := strings.HasPrefix(r.URL.Path, "/v2/opendatahub/odh-mcp-lifecycle-operator/manifests/") || strings.HasPrefix(r.URL.Path, "/v2/opendatahub/odh-mcp-lifecycle-operator/blobs/")
		if r.Method != http.MethodGet || !validPath {
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
		}
		var body []byte
		var digest string
		missingDigest := false
		if strings.HasSuffix(r.URL.Path, "/manifests/"+imageTestSource) {
			current := root()
			body, digest = current.root, current.rootDigest
			if rootStatus != http.StatusOK {
				w.WriteHeader(rootStatus)
				return
			}
			if rootDigest == "missing" {
				missingDigest = true
			} else if rootDigest != "" {
				digest = rootDigest
			}
		} else if strings.Contains(r.URL.Path, "/manifests/") {
			digest = pathTail(r.URL.Path)
			body = fixture.manifests[digest]
		} else if strings.Contains(r.URL.Path, "/blobs/") {
			digest = pathTail(r.URL.Path)
			body = fixture.blobs[digest]
		} else {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if body == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !missingDigest {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func pathTail(value string) string {
	return value[strings.LastIndex(value, "/")+1:]
}

func TestResolveOperandImageValidatesAllRunnablePlatforms(t *testing.T) {
	fixture := makeRegistryFixture(t, imageTestSource, "amd64", "arm64")
	fixtureHandler := fixture.handler(t, func() registryFixture { return fixture }, http.StatusOK, "")
	childManifestRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") && !strings.HasSuffix(r.URL.Path, "/manifests/"+imageTestSource) {
			childManifestRequests++
		}
		fixtureHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	got, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client())
	want := imageRepository + "@" + fixture.rootDigest
	if err != nil || got != want {
		t.Fatalf("resolveOperandImageAt() = %q, %v, want %q", got, err, want)
	}
	if childManifestRequests != 2 {
		t.Fatalf("checked %d child manifests, want both runnable platforms", childManifestRequests)
	}
}

func TestResolveOperandImageSourceLabels(t *testing.T) {
	repository := "https://github.com/opendatahub-io/mcp-lifecycle-operator"
	for _, key := range []string{"git.commit", "org.opencontainers.image.revision", "vcs-ref"} {
		for _, tc := range []struct {
			name    string
			labels  map[string]string
			wantErr string
		}{
			{name: "only recognized label", labels: map[string]string{key: imageTestSource, "git.url": repository}},
			{name: "no recognized label", labels: map[string]string{"other.revision": imageTestSource, "git.url": repository}, wantErr: "no recognized source label"},
			{name: "conflicting label", labels: map[string]string{"git.commit": imageTestSource, "org.opencontainers.image.revision": imageTestSource, "vcs-ref": imageTestSource, key: strings.Repeat("f", 40), "git.url": repository}, wantErr: "label " + key},
			{name: "empty label", labels: map[string]string{"git.commit": imageTestSource, "org.opencontainers.image.revision": imageTestSource, "vcs-ref": imageTestSource, key: "", "git.url": repository}, wantErr: "label " + key},
			{name: "missing repository", labels: map[string]string{key: imageTestSource}, wantErr: "git.url"},
			{name: "wrong repository", labels: map[string]string{key: imageTestSource, "git.url": "https://example.com/other"}, wantErr: "git.url"},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				// A matching first platform must not hide bad labels on another runnable platform.
				fixture := makeRegistryFixtureWithChildLabels(t, imageTestSource, []map[string]string{nil, tc.labels}, "amd64", "arm64")
				server := httptest.NewServer(fixture.handler(t, func() registryFixture { return fixture }, http.StatusOK, ""))
				defer server.Close()
				got, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client())
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("resolveOperandImageAt() = %q, %v, want error containing %q", got, err, tc.wantErr)
					}
				} else if err != nil || got != imageRepository+"@"+fixture.rootDigest {
					t.Fatalf("resolveOperandImageAt() = %q, %v", got, err)
				}
			})
		}
	}
}

func TestResolveOperandImageRejectsIncompletePlatforms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform map[string]string
		fetches  int
	}{
		{name: "absent platform", fetches: 1},
		{name: "missing architecture", platform: map[string]string{"os": "linux"}, fetches: 1},
		{name: "missing OS", platform: map[string]string{"architecture": "arm64"}, fetches: 1},
		{name: "unknown OS with real architecture", platform: map[string]string{"os": "unknown", "architecture": "arm64"}, fetches: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := makeRegistryFixtureWithChildLabels(t, imageTestSource, []map[string]string{nil, {"git.commit": strings.Repeat("f", 40)}}, "amd64", "arm64")
			setFixturePlatform(t, &fixture, 1, tc.platform)
			fixtureHandler := fixture.handler(t, func() registryFixture { return fixture }, http.StatusOK, "")
			childManifestRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/manifests/") && !strings.HasSuffix(r.URL.Path, "/manifests/"+imageTestSource) {
					childManifestRequests++
				}
				fixtureHandler.ServeHTTP(w, r)
			}))
			defer server.Close()
			if _, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client()); err == nil {
				t.Fatal("resolveOperandImageAt() accepted an unchecked wrong-source child")
			}
			if childManifestRequests != tc.fetches {
				t.Fatalf("fetched %d child manifests, want %d", childManifestRequests, tc.fetches)
			}
		})
	}
}

func TestResolveOperandImageSkipsExplicitUnknownPlatformAttestation(t *testing.T) {
	fixture := makeRegistryFixture(t, imageTestSource, "amd64", "arm64")
	setFixturePlatform(t, &fixture, 1, map[string]string{"os": "unknown", "architecture": "unknown"})
	server := httptest.NewServer(fixture.handler(t, func() registryFixture { return fixture }, http.StatusOK, ""))
	defer server.Close()
	got, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client())
	if err != nil || got != imageRepository+"@"+fixture.rootDigest {
		t.Fatalf("explicit unknown/unknown child = %q, %v", got, err)
	}
}

func TestResolveOperandImageUsesAnonymousQuayPullToken(t *testing.T) {
	fixture := makeRegistryFixture(t, imageTestSource, "amd64")
	fixtureHandler := fixture.handler(t, func() registryFixture { return fixture }, http.StatusOK, "")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/auth" {
			if r.URL.Query().Get("service") != "quay.io" || r.URL.Query().Get("scope") != "repository:opendatahub/odh-mcp-lifecycle-operator:pull" {
				t.Errorf("unexpected Quay token request query %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"token":"anonymous-pull-token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer anonymous-pull-token" {
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="quay.io",scope="repository:opendatahub/odh-mcp-lifecycle-operator:pull"`, server.URL+"/v2/auth"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fixtureHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	got, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client())
	if err != nil || got != imageRepository+"@"+fixture.rootDigest {
		t.Fatalf("authenticated public pull = %q, %v", got, err)
	}
}

func TestResolveOperandImageRejectsUntrustedChallengesBeforeTokenRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		realm   string
		service string
		scope   string
	}{
		{name: "wrong realm", realm: "/untrusted/auth", service: "quay.io", scope: "repository:opendatahub/odh-mcp-lifecycle-operator:pull"},
		{name: "wrong service", realm: "/v2/auth", service: "other.io", scope: "repository:opendatahub/odh-mcp-lifecycle-operator:pull"},
		{name: "wrong repository", realm: "/v2/auth", service: "quay.io", scope: "repository:opendatahub/other:pull"},
		{name: "missing scope", realm: "/v2/auth", service: "quay.io"},
		{name: "widened actions", realm: "/v2/auth", service: "quay.io", scope: "repository:opendatahub/odh-mcp-lifecycle-operator:pull,push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var unexpectedRequests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/opendatahub/odh-mcp-lifecycle-operator/manifests/"+imageTestSource {
					unexpectedRequests.Add(1)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
					t.Errorf("first registry request must be an anonymous GET: %s, authorization %q", r.Method, r.Header.Get("Authorization"))
				}
				challenge := fmt.Sprintf(`Bearer realm=%q,service=%q`, server.URL+tc.realm, tc.service)
				if tc.scope != "" {
					challenge += fmt.Sprintf(`,scope=%q`, tc.scope)
				}
				w.Header().Set("Www-Authenticate", challenge)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			if _, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client()); err == nil || !strings.Contains(err.Error(), "does not match trusted Quay repository") {
				t.Fatalf("untrusted challenge error = %v", err)
			}
			if got := unexpectedRequests.Load(); got != 0 {
				t.Fatalf("made %d requests beyond the initial manifest GET, want no token request", got)
			}
		})
	}
}

func TestResolveOperandImageRejectsRegistryAndSourceFailures(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		claimedDigest  string
		mismatchSource bool
	}{
		{name: "404", status: http.StatusNotFound},
		{name: "malformed digest", status: http.StatusOK, claimedDigest: "sha256:not-a-digest"},
		{name: "missing digest", status: http.StatusOK, claimedDigest: "missing"},
		{name: "hash mismatch", status: http.StatusOK, claimedDigest: "sha256:" + strings.Repeat("0", 64)},
		{name: "source mismatch", status: http.StatusOK, mismatchSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imageSource := imageTestSource
			if tc.mismatchSource {
				imageSource = strings.Repeat("f", 40)
			}
			fixture := makeRegistryFixture(t, imageSource, "amd64")
			server := httptest.NewServer(fixture.handler(t, func() registryFixture { return fixture }, tc.status, tc.claimedDigest))
			defer server.Close()
			if _, err := resolveOperandImageAt(server.URL, imageTestSource, server.Client()); err == nil {
				t.Fatal("resolveOperandImageAt() unexpectedly succeeded")
			}
		})
	}
}

func TestResolveOperandImageRetainsIndexAndRediscoversOnlyWhenAsked(t *testing.T) {
	first := makeRegistryFixture(t, imageTestSource, "amd64")
	second := makeRegistryFixture(t, imageTestSource, "amd64")
	// Make the second publication differ while keeping its source/config labels.
	var index map[string]any
	if err := json.Unmarshal(second.root, &index); err != nil {
		t.Fatal(err)
	}
	index["annotations"] = map[string]string{"build": "rebuild"}
	second.root, _ = json.Marshal(index)
	second.rootDigest = bodyDigest(second.root)
	var calls int
	server := httptest.NewServer(first.handler(t, func() registryFixture {
		calls++
		if calls%2 == 0 {
			return second
		}
		return first
	}, http.StatusOK, ""))
	defer server.Close()
	client := server.Client()
	one, err := resolveOperandImageAt(server.URL, imageTestSource, client)
	if err != nil {
		t.Fatal(err)
	}
	two, err := resolveOperandImageAt(server.URL, imageTestSource, client)
	if err != nil {
		t.Fatal(err)
	}
	if one == two || !strings.HasSuffix(one, "@"+first.rootDigest) || !strings.HasSuffix(two, "@"+second.rootDigest) {
		t.Fatalf("source-SHA rebuilds were not independently selected as indexes: %q, %q", one, two)
	}
	pinPath := filepath.Join(t.TempDir(), imagePin)
	if err := writeImagePin(pinPath, imagePinRecord{SourceCommit: imageTestSource, Image: one}); err != nil {
		t.Fatal(err)
	}
	pin, err := readImagePin(pinPath)
	if err != nil || pin.Image != one || calls != 2 {
		t.Fatalf("reading verification pin performed discovery or changed selection: %+v, calls=%d, err=%v", pin, calls, err)
	}
}

func TestReadImagePinValidatesSourceAndImmutableImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record imagePinRecord
	}{
		{name: "short source", record: imagePinRecord{SourceCommit: "5892d3d", Image: testImage}},
		{name: "nonhex source", record: imagePinRecord{SourceCommit: strings.Repeat("z", 40), Image: testImage}},
		{name: "missing source", record: imagePinRecord{Image: testImage}},
		{name: "tagged image", record: imagePinRecord{SourceCommit: imageTestSource, Image: imageRepository + ":latest"}},
		{name: "wrong repository", record: imagePinRecord{SourceCommit: imageTestSource, Image: "example.com/operator@sha256:" + strings.Repeat("0", 64)}},
		{name: "bad digest", record: imagePinRecord{SourceCommit: imageTestSource, Image: imageRepository + "@sha256:bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), imagePin)
			data, _ := json.Marshal(tc.record)
			if err := os.WriteFile(filename, data, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := readImagePin(filename); err == nil {
				t.Fatal("readImagePin() unexpectedly accepted invalid lock")
			}
		})
	}
	filename := filepath.Join(t.TempDir(), imagePin)
	if err := writeImagePin(filename, imagePinRecord{SourceCommit: imageTestSource, Image: testImage}); err != nil {
		t.Fatal(err)
	}
	if pin, err := readImagePin(filename); err != nil || pin.SourceCommit != imageTestSource || pin.Image != testImage {
		t.Fatalf("readImagePin() = %+v, %v", pin, err)
	}
	for _, data := range []string{
		`null`, `{}`, `{"sourceCommit": 1}`, `{"unknown": true}`,
		fmt.Sprintf(`{"sourceCommit": %q, "image": %q, "unknown": true}`, imageTestSource, testImage),
		fmt.Sprintf(`{"sourceCommit": %q, "image": %q} {}`, imageTestSource, testImage),
	} {
		if err := os.WriteFile(filename, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readImagePin(filename); err == nil {
			t.Fatalf("readImagePin() accepted invalid JSON lock: %s", data)
		}
	}
}

func TestInstallerImageDriftIsByteDrift(t *testing.T) {
	dir := t.TempDir()
	actual, expected := filepath.Join(dir, "actual.yaml"), filepath.Join(dir, "expected.yaml")
	if err := os.WriteFile(actual, []byte("image: "+strings.Replace(testImage, "0123", "ffff", 1)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expected, []byte("image: "+testImage+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !installerDiffers(actual, expected) {
		t.Fatal("verification accepted installer image drift")
	}
}
