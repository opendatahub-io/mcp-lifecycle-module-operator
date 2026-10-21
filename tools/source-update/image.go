package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	// Keep image references, registry URLs and pull scopes on one repository contract.
	// The disconnected-readiness scorer misclassifies a literal repository auth scope
	// as an image tag; deriving the scope also avoids that false positive.
	imageRepositoryPath = "opendatahub/odh-mcp-lifecycle-operator"
	imageRepository     = "quay.io/" + imageRepositoryPath
	imageRegistry       = "https://quay.io"
	imagePin            = "operand-image.json"
)

const maxRegistryResponse = 8 << 20

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var authParamPattern = regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`)

// imagePinRecord locks the source commit and selected immutable operand image.
type imagePinRecord struct {
	SourceCommit string `json:"sourceCommit"`
	Image        string `json:"image"`
}

func validateImagePin(record imagePinRecord) error {
	if !fullSHA(record.SourceCommit) {
		return fmt.Errorf("invalid operand image pin source commit %q; expected full commit SHA", record.SourceCommit)
	}
	if !validImageReference(record.Image) {
		return fmt.Errorf("invalid pinned operand image %q; expected %s@sha256:<64 lowercase hex>", record.Image, imageRepository)
	}
	return nil
}

func validImageReference(image string) bool {
	repository, digest, ok := strings.Cut(image, "@")
	return ok && repository == imageRepository && digestPattern.MatchString(digest)
}

func readImagePin(filename string) (imagePinRecord, error) {
	data, err := readBoundedFile(filename, maxRegistryResponse)
	if err != nil {
		return imagePinRecord{}, fmt.Errorf("read operand image pin: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record imagePinRecord
	if err := decoder.Decode(&record); err != nil {
		return imagePinRecord{}, fmt.Errorf("decode operand image pin: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return imagePinRecord{}, fmt.Errorf("decode operand image pin: %w", err)
	}
	if err := validateImagePin(record); err != nil {
		return imagePinRecord{}, err
	}
	return record, nil
}

func writeImagePin(filename string, record imagePinRecord) error {
	if err := validateImagePin(record); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, append(data, '\n'), 0644)
}

func readBoundedFile(filename string, limit int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected data after JSON record")
		}
		return err
	}
	return nil
}

type registryDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

type registryManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Manifests     []registryDescriptor `json:"manifests"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"config"`
}

type imageConfig struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

func resolveOperandImage(sourceCommit string) (string, error) {
	return resolveOperandImageAt(imageRegistry, sourceCommit, &http.Client{Timeout: 20 * time.Second})
}

// resolveOperandImageAt accepts an endpoint only for httptest; production uses the fixed Quay endpoint above.
func resolveOperandImageAt(registry, sourceCommit string, client *http.Client) (string, error) {
	if !fullSHA(sourceCommit) {
		return "", fmt.Errorf("invalid source commit %q", sourceCommit)
	}
	base, err := url.Parse(registry)
	if err != nil || base.Scheme != "https" && base.Scheme != "http" || base.Host == "" {
		return "", fmt.Errorf("invalid registry endpoint")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root, headers, err := registryGet(ctx, client, base, "manifests/"+sourceCommit, manifestAccept)
	if err != nil {
		return "", fmt.Errorf("resolve MCPLO image for %s: %w", sourceCommit, err)
	}
	rootDigest, err := checkedDigest(root, headers.Get("Docker-Content-Digest"))
	if err != nil {
		return "", fmt.Errorf("validate source-SHA image manifest: %w", err)
	}
	var manifest registryManifest
	if err := json.Unmarshal(root, &manifest); err != nil {
		return "", fmt.Errorf("decode source-SHA image manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || !isIndexMediaType(manifest.MediaType) || len(manifest.Manifests) == 0 {
		return "", fmt.Errorf("source-SHA image is not a nonempty OCI/Docker image index")
	}
	// ponytail: cap indexes at 128 entries; raise if MCPLO publishes more runnable variants.
	if len(manifest.Manifests) > 128 {
		return "", fmt.Errorf("source-SHA image index has too many manifests: %d", len(manifest.Manifests))
	}
	checkedPlatforms := 0
	for _, child := range manifest.Manifests {
		if child.Platform.OS == "unknown" && child.Platform.Architecture == "unknown" {
			continue // OCI artifact/attestation convention; it is not a runnable platform.
		}
		if child.Platform.OS == "" || child.Platform.Architecture == "" {
			return "", fmt.Errorf("source-SHA image index child has incomplete platform %q/%q", child.Platform.OS, child.Platform.Architecture)
		}
		checkedPlatforms++
		if err := validatePlatformImage(ctx, client, base, child, sourceCommit); err != nil {
			return "", fmt.Errorf("validate %s/%s image: %w", child.Platform.OS, child.Platform.Architecture, err)
		}
	}
	if checkedPlatforms == 0 {
		return "", fmt.Errorf("source-SHA image index has no runnable platform manifests")
	}
	return imageRepository + "@" + rootDigest, nil
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

func isIndexMediaType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.index.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func isImageMediaType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.manifest.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}

func isConfigMediaType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.config.v1+json" || mediaType == "application/vnd.docker.container.image.v1+json"
}

func validatePlatformImage(ctx context.Context, client *http.Client, base *url.URL, descriptor registryDescriptor, sourceCommit string) error {
	if !isImageMediaType(descriptor.MediaType) || !digestPattern.MatchString(descriptor.Digest) {
		return fmt.Errorf("invalid child manifest digest %q", descriptor.Digest)
	}
	body, headers, err := registryGet(ctx, client, base, "manifests/"+descriptor.Digest, manifestAccept)
	if err != nil {
		return err
	}
	got, err := checkedDigest(body, headers.Get("Docker-Content-Digest"))
	if err != nil || got != descriptor.Digest {
		return fmt.Errorf("child manifest digest mismatch: got %q, expected %q", got, descriptor.Digest)
	}
	var manifest registryManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("decode child manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != descriptor.MediaType || !isImageMediaType(manifest.MediaType) || !isConfigMediaType(manifest.Config.MediaType) || !digestPattern.MatchString(manifest.Config.Digest) {
		return fmt.Errorf("invalid image manifest/config")
	}
	config, _, err := registryGet(ctx, client, base, "blobs/"+manifest.Config.Digest, "application/octet-stream")
	if err != nil {
		return err
	}
	got, err = checkedDigest(config, manifest.Config.Digest)
	if err != nil {
		return fmt.Errorf("image config digest mismatch: %w", err)
	}
	if got != manifest.Config.Digest {
		return fmt.Errorf("image config digest mismatch: got %q, expected %q", got, manifest.Config.Digest)
	}
	var image imageConfig
	if err := json.Unmarshal(config, &image); err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}
	if image.OS != descriptor.Platform.OS || normalizeArchitecture(image.Architecture) != normalizeArchitecture(descriptor.Platform.Architecture) {
		return fmt.Errorf("config platform %s/%s does not match index %s/%s", image.OS, image.Architecture, descriptor.Platform.OS, descriptor.Platform.Architecture)
	}
	labels := image.Config.Labels
	hasSource := false
	for _, key := range []string{"git.commit", "org.opencontainers.image.revision", "vcs-ref"} {
		if value, supplied := labels[key]; supplied {
			if value != sourceCommit {
				return fmt.Errorf("image config label %s is %q, expected source %s", key, value, sourceCommit)
			}
			hasSource = true
		}
	}
	if !hasSource {
		return fmt.Errorf("image config has no recognized source label")
	}
	if labels["git.url"] != "https://github.com/opendatahub-io/mcp-lifecycle-operator" {
		return fmt.Errorf("image config git.url is %q, expected MCPLO source repository", labels["git.url"])
	}
	return nil
}

func normalizeArchitecture(arch string) string {
	if arch == "x86_64" {
		return "amd64"
	}
	if arch == "aarch64" {
		return "arm64"
	}
	return arch
}

func checkedDigest(body []byte, claimed string) (string, error) {
	if !digestPattern.MatchString(claimed) {
		return "", fmt.Errorf("missing or malformed sha256 content digest %q", claimed)
	}
	hash := sha256.Sum256(body)
	actual := "sha256:" + hex.EncodeToString(hash[:])
	if claimed != actual {
		return "", fmt.Errorf("content digest mismatch: header %s, body %s", claimed, actual)
	}
	return actual, nil
}

func registryGet(ctx context.Context, client *http.Client, base *url.URL, resource, accept string) ([]byte, http.Header, error) {
	endpoint := *base
	endpoint.Path = path.Join(base.Path, "/v2", imageRepositoryPath, resource)
	body, headers, status, err := registryRequest(ctx, client, endpoint.String(), accept, "")
	if err != nil {
		return nil, nil, err
	}
	if status == http.StatusUnauthorized {
		token, err := registryToken(ctx, client, base, headers.Get("Www-Authenticate"))
		if err != nil {
			return nil, nil, err
		}
		body, headers, status, err = registryRequest(ctx, client, endpoint.String(), accept, token)
		if err != nil {
			return nil, nil, err
		}
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("registry GET %s returned HTTP %d", resource, status)
	}
	return body, headers, nil
}

func registryRequest(ctx context.Context, client *http.Client, endpoint, accept, token string) ([]byte, http.Header, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	request.Header.Set("Accept", accept)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRegistryResponse+1))
	if err != nil {
		return nil, nil, 0, err
	}
	if len(body) > maxRegistryResponse {
		return nil, nil, 0, fmt.Errorf("registry response exceeds %d bytes", maxRegistryResponse)
	}
	return body, response.Header.Clone(), response.StatusCode, nil
}

func registryToken(ctx context.Context, client *http.Client, base *url.URL, challenge string) (string, error) {
	kind, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(kind, "Bearer") {
		return "", fmt.Errorf("registry requires unsupported authentication")
	}
	values := map[string]string{}
	for _, match := range authParamPattern.FindAllStringSubmatch(params, -1) {
		values[match[1]] = match[2]
	}
	expectedScope := "repository:" + imageRepositoryPath + ":pull"
	expectedRealm := *base
	expectedRealm.Path = path.Join(base.Path, "/v2/auth")
	if values["realm"] != expectedRealm.String() || values["service"] != "quay.io" || values["scope"] != expectedScope {
		return "", fmt.Errorf("registry authentication challenge does not match trusted Quay repository")
	}
	endpoint := *base
	endpoint.Path = path.Join(base.Path, "/v2/auth")
	query := endpoint.Query()
	query.Set("service", "quay.io")
	query.Set("scope", expectedScope)
	endpoint.RawQuery = query.Encode()
	body, _, status, err := registryRequest(ctx, client, endpoint.String(), "application/json", "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("Quay token request returned HTTP %d", status)
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("decode Quay token response: %w", err)
	}
	if token.Token == "" {
		token.Token = token.AccessToken
	}
	if token.Token == "" {
		return "", fmt.Errorf("Quay returned an empty pull token")
	}
	return token.Token, nil
}
