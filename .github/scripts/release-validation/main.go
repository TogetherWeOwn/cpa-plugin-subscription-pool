// release-validation validates release metadata, artifacts, documentation, and source files.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	pluginID                     = "subscription-pool"
	libraryName                  = pluginID + ".so"
	hostImageRepository          = "eceasy/cli-proxy-api"
	baselineHostImageTag         = "v7.2.150"
	baselineHostImageAMD64Digest = "sha256:dca10d258e7d83e7cbec21e1ba2919d416857ca83d40198abfd18b1e6558cdcd"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

const canonicalMITLicense = `MIT License

Copyright (c) 2026 TogetherWeOwn

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const canonicalNotice = `cpa-plugin-subscription-pool
Copyright (c) 2026 TogetherWeOwn

This product includes software developed by TogetherWeOwn and third-party software whose licenses are recorded in the Go module dependency metadata.
`

type registry struct {
	Plugins []registryPlugin `json:"plugins"`
}

type registryPlugin struct {
	ID      string          `json:"id"`
	Version string          `json:"version"`
	License string          `json:"license"`
	Release registryRelease `json:"release"`
}

type registryRelease struct {
	Archive   string `json:"archive"`
	Checksums string `json:"checksums"`
}

type canonicalRunStep struct {
	name             string
	id               string
	workingDirectory string
	env              map[string]string
	command          string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("release-validation", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "release", "validation mode: release, source, or version")
	root := flags.String("root", "", "repository root; defaults to the current directory")
	tag := flags.String("tag", "", "release tag, including the v prefix")
	version := flags.String("version", "", "release version without the v prefix")
	dist := flags.String("dist", "dist", "release artifact directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *root == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
		*root = workingDirectory
	}

	switch *mode {
	case "source":
		return validateSource(*root)
	case "release":
		return validateRelease(*root, *dist, *tag, *version)
	case "version":
		resolved, err := versionFromTag(*tag)
		if err != nil {
			return err
		}
		fmt.Println(resolved)
		return nil
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
}

func validateSource(root string) error {
	if err := validateDocumentation(root); err != nil {
		return err
	}
	if err := validateHostImagePin(root); err != nil {
		return err
	}
	if err := validateWorkflowActionPins(root); err != nil {
		return err
	}
	if err := validateReleaseWorkflowBoundary(root); err != nil {
		return err
	}
	files, err := trackedFiles(root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("source validation found no files")
	}
	return nil
}

func validateRelease(root, dist, tag, version string) error {
	if err := validateVersion(tag, version); err != nil {
		return err
	}
	if err := validateDocumentation(root); err != nil {
		return err
	}
	if err := validateRegistry(root, version); err != nil {
		return err
	}
	if err := validateChangelog(root, version); err != nil {
		return err
	}

	distPath := resolvePath(root, dist)
	libraryPath := filepath.Join(distPath, fmt.Sprintf("%s-v%s.so", pluginID, version))
	archivePath := filepath.Join(distPath, fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version))
	operatorPath := filepath.Join(distPath, fmt.Sprintf("%s-v%s-operator.zip", pluginID, version))
	checksumsPath := filepath.Join(distPath, "checksums.txt")
	artifactPaths := []string{
		libraryPath,
		archivePath,
		operatorPath,
		filepath.Join(distPath, "compatibility-evidence.json"),
		filepath.Join(distPath, "config.yaml.tmpl"),
		filepath.Join(distPath, "registry.json"),
		filepath.Join(distPath, "router-capacity-source.json"),
		filepath.Join(distPath, "verify-live.sh"),
		filepath.Join(distPath, "verify-live-opencodego.sh"),
		filepath.Join(distPath, "collector-zai.py"),
		filepath.Join(distPath, "collector-opencodego.py"),
		filepath.Join(distPath, "prepare-usage-dir.py"),
		filepath.Join(distPath, "remove-usage-output.py"),
		filepath.Join(distPath, "rollback.sh"),
		filepath.Join(distPath, "README.md"),
		filepath.Join(distPath, "release-sha.txt"),
	}
	for _, path := range artifactPaths {
		if err := requireNonEmpty(path); err != nil {
			return err
		}
	}
	if err := validateArchive(libraryPath, archivePath); err != nil {
		return err
	}
	if err := validateOperatorBundle(root, distPath, version); err != nil {
		return err
	}
	if err := validateReleaseSHA(filepath.Join(distPath, "release-sha.txt")); err != nil {
		return err
	}
	return validateChecksums(checksumsPath, artifactPaths)
}

func validateVersion(tag, version string) error {
	resolved, err := versionFromTag(tag)
	if err != nil {
		return err
	}
	if resolved != version {
		return fmt.Errorf("tag %q does not match version %q", tag, version)
	}
	return nil
}

func versionFromTag(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("tag %q must start with v", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("tag %q is not valid semantic version syntax", tag)
	}
	return version, nil
}

func validateRegistry(root, version string) error {
	path := filepath.Join(root, "registry.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}
	var value registry
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("parse registry: %w", err)
	}
	matches := make([]registryPlugin, 0, 1)
	for _, plugin := range value.Plugins {
		if plugin.ID == pluginID {
			matches = append(matches, plugin)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("registry must contain exactly one %q entry, found %d", pluginID, len(matches))
	}
	if matches[0].Version != version {
		return fmt.Errorf("registry version %q does not match release version %q", matches[0].Version, version)
	}
	if matches[0].License != "MIT" {
		return fmt.Errorf("registry license %q does not match LICENSE", matches[0].License)
	}
	wantArchive := fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version)
	if matches[0].Release.Archive != wantArchive {
		return fmt.Errorf("registry archive %q does not match release archive %q", matches[0].Release.Archive, wantArchive)
	}
	if matches[0].Release.Checksums != "checksums.txt" {
		return fmt.Errorf("registry checksums %q must be checksums.txt", matches[0].Release.Checksums)
	}
	return nil
}

func validateChangelog(root, version string) error {
	raw, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return fmt.Errorf("read changelog: %w", err)
	}
	text := string(raw)
	headingPattern := regexp.MustCompile(`(?m)^## \[` + regexp.QuoteMeta(version) + `\] - (\d{4}-\d{2}-\d{2})$`)
	if !headingPattern.MatchString(text) {
		return fmt.Errorf("changelog must contain a dated [%s] release heading", version)
	}
	link := fmt.Sprintf("[%s]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v%s", version, version)
	if !strings.Contains(text, link) {
		return fmt.Errorf("changelog is missing the %s release link", version)
	}
	return nil
}

func validateDocumentation(root string) error {
	license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return fmt.Errorf("read LICENSE: %w", err)
	}
	if normalizeDocument(string(license)) != normalizeDocument(canonicalMITLicense) {
		return errors.New("LICENSE does not match the canonical MIT license")
	}

	notice, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		return fmt.Errorf("read NOTICE: %w", err)
	}
	if normalizeDocument(string(notice)) != normalizeDocument(canonicalNotice) {
		return errors.New("NOTICE does not match the canonical project notice")
	}
	return nil
}

func normalizeDocument(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func validateHostImagePin(root string) error {
	matrixRaw, err := os.ReadFile(filepath.Join(root, ".github", "host-images.json"))
	if err != nil {
		return fmt.Errorf("read host image matrix: %w", err)
	}
	var matrix struct {
		Repository string `json:"repository"`
		Platform   string `json:"platform"`
		Baseline   struct {
			Tag            string `json:"tag"`
			ManifestDigest string `json:"manifest_digest"`
		} `json:"baseline"`
	}
	if err := json.Unmarshal(matrixRaw, &matrix); err != nil {
		return fmt.Errorf("parse host image matrix: %w", err)
	}
	if matrix.Repository != hostImageRepository || matrix.Platform != "linux/amd64" || matrix.Baseline.Tag != baselineHostImageTag || matrix.Baseline.ManifestDigest != baselineHostImageAMD64Digest {
		return errors.New("host image matrix does not preserve the approved v7.2.150 linux/amd64 baseline")
	}
	deployedRaw, err := os.ReadFile(filepath.Join(root, "deploy", "deployed-host-image.json"))
	if err != nil {
		return fmt.Errorf("read deployed host image pin: %w", err)
	}
	var deployed struct {
		Repository     string `json:"repository"`
		Tag            string `json:"tag"`
		Platform       string `json:"platform"`
		ManifestDigest string `json:"manifest_digest"`
	}
	if err := json.Unmarshal(deployedRaw, &deployed); err != nil {
		return fmt.Errorf("parse deployed host image pin: %w", err)
	}
	if deployed.Repository != hostImageRepository || deployed.Platform != "linux/amd64" || !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(deployed.Tag) || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(deployed.ManifestDigest) {
		return errors.New("deployed host image pin is invalid")
	}
	for _, path := range []string{
		filepath.Join(root, ".github", "scripts", "resolve-host-images.sh"),
		filepath.Join(root, ".github", "scripts", "run-host-matrix.sh"),
		filepath.Join(root, ".github", "workflows", "host-compatibility.yml"),
	} {
		if err := requireNonEmpty(path); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkflowActionPins(root string) error {
	var paths []string
	for _, extension := range []string{"*.yml", "*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(root, ".github", "workflows", extension))
		if err != nil {
			return fmt.Errorf("list workflows: %w", err)
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		return errors.New("no GitHub workflows found")
	}
	pinned := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read workflow %s: %w", filepath.Base(path), err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return fmt.Errorf("parse workflow %s: %w", filepath.Base(path), err)
		}
		var uses []*yaml.Node
		collectWorkflowUses(&document, &uses)
		for _, node := range uses {
			if node.Kind != yaml.ScalarNode {
				return fmt.Errorf("workflow %s:%d action uses must be a scalar and cannot use YAML aliases", filepath.Base(path), node.Line)
			}
			value := strings.TrimSpace(node.Value)
			if strings.HasPrefix(value, "./") {
				continue
			}
			parts := strings.Split(value, "@")
			if len(parts) != 2 || !pinned.MatchString(parts[1]) {
				return fmt.Errorf("workflow %s:%d action is not pinned to a full commit SHA", filepath.Base(path), node.Line)
			}
		}
	}
	return nil
}

func collectWorkflowUses(node *yaml.Node, uses *[]*yaml.Node) {
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if key.Kind != yaml.ScalarNode {
				*uses = append(*uses, key)
			} else if key.Value == "uses" {
				*uses = append(*uses, value)
			}
			collectWorkflowUses(value, uses)
		}
		return
	}
	for _, child := range node.Content {
		collectWorkflowUses(child, uses)
	}
}

func validateReleaseWorkflowBoundary(root string) error {
	ciPath := filepath.Join(root, ".github", "workflows", "ci.yml")
	ciRaw, err := os.ReadFile(ciPath)
	if err != nil {
		return fmt.Errorf("read CI workflow: %w", err)
	}
	var ciDocument yaml.Node
	if err := yaml.Unmarshal(ciRaw, &ciDocument); err != nil {
		return fmt.Errorf("parse CI workflow: %w", err)
	}
	ciWorkflow, err := yamlMapping(&ciDocument, "CI workflow")
	if err != nil {
		return err
	}
	ciTrigger, err := yamlMapping(ciWorkflow["on"], "CI workflow trigger")
	if err != nil {
		return err
	}
	ciPush, err := yamlMapping(ciTrigger["push"], "CI push trigger")
	if err != nil {
		return err
	}
	ciBranches, err := yamlStringSequence(ciPush["branches"], "CI push branches")
	if err != nil || len(ciBranches) != 1 || ciBranches[0] != "main" {
		return errors.New("CI push trigger must contain only main")
	}

	path := filepath.Join(root, ".github", "workflows", "release.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release workflow: %w", err)
	}
	return validateReleaseWorkflowShape(raw)
}

func requireCanonicalRunScalar(raw []byte, node *yaml.Node, context string) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Style != yaml.LiteralStyle {
		return fmt.Errorf("%s run command must use literal block style", context)
	}
	lineStart := node.Line - 1
	if lineStart < 0 {
		return fmt.Errorf("%s run command has an invalid source position", context)
	}
	lines := bytes.Split(raw, []byte{'\n'})
	if lineStart >= len(lines) {
		return fmt.Errorf("%s run command has an invalid source position", context)
	}
	declaration := strings.TrimSpace(string(lines[lineStart]))
	if declaration != "run: |" {
		return fmt.Errorf("%s run command must use exactly run: |", context)
	}
	return nil
}

func yamlMapping(node *yaml.Node, context string) (map[string]*yaml.Node, error) {
	if node == nil {
		return nil, fmt.Errorf("%s is missing", context)
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("%s is not a single document", context)
		}
		node = node.Content[0]
	}
	if node.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%s cannot use YAML aliases", context)
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", context)
	}
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" {
			return nil, fmt.Errorf("%s contains a non-string key at line %d", context, key.Line)
		}
		if _, exists := result[key.Value]; exists {
			return nil, fmt.Errorf("%s contains duplicate key %q", context, key.Value)
		}
		result[key.Value] = node.Content[index+1]
	}
	return result, nil
}

func yamlStringSequence(node *yaml.Node, context string) ([]string, error) {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a sequence", context)
	}
	result := make([]string, 0, len(node.Content))
	for _, value := range node.Content {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return nil, fmt.Errorf("%s values must be strings", context)
		}
		result = append(result, value.Value)
	}
	return result, nil
}

func yamlStringMap(node *yaml.Node, context string) (map[string]string, error) {
	if node == nil {
		return map[string]string{}, nil
	}
	mapping, err := yamlMapping(node, context)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(mapping))
	for key, value := range mapping {
		if value.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s value %q must be a scalar", context, key)
		}
		result[key] = value.Value
	}
	return result, nil
}

func yamlScalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func requireOnlyYAMLKeys(mapping map[string]*yaml.Node, context string, allowed ...string) error {
	allowlist := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowlist[key] = struct{}{}
	}
	for key := range mapping {
		if _, exists := allowlist[key]; !exists {
			return fmt.Errorf("%s contains unapproved key %q", context, key)
		}
	}
	return nil
}

func equalStringMaps(actual, expected map[string]string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for key, expectedValue := range expected {
		if actual[key] != expectedValue {
			return false
		}
	}
	return true
}

func normalizeShellCommand(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func trackedFiles(root string) ([]string, error) {
	// Git's path list is the deterministic source of files to scan. It includes
	// tracked and untracked, non-ignored files while excluding generated output.
	command := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	if len(output) == 0 {
		return nil, nil
	}
	parts := strings.Split(string(output[:len(output)-1]), "\x00")
	files := make([]string, 0, len(parts))
	for _, name := range parts {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

func requireNonEmpty(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required artifact %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("required artifact %s is empty or not a regular file", filepath.Base(path))
	}
	return nil
}

func validateArchive(libraryPath, archivePath string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer archive.Close()
	if len(archive.File) != 1 || archive.File[0].Name != libraryName {
		entries := make([]string, 0, len(archive.File))
		for _, entry := range archive.File {
			entries = append(entries, entry.Name)
		}
		return fmt.Errorf("archive entries %q, want exactly %q", entries, libraryName)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		return fmt.Errorf("open archive entry: %w", err)
	}
	defer entry.Close()
	library, err := os.Open(libraryPath)
	if err != nil {
		return fmt.Errorf("open library: %w", err)
	}
	defer library.Close()
	equal, err := readersEqual(library, entry)
	if err != nil {
		return fmt.Errorf("compare archive library: %w", err)
	}
	if !equal {
		return errors.New("archive library does not match versioned shared library")
	}
	return nil
}

func readersEqual(left, right io.Reader) (bool, error) {
	leftHash := sha256.New()
	rightHash := sha256.New()
	if _, err := io.Copy(leftHash, left); err != nil {
		return false, err
	}
	if _, err := io.Copy(rightHash, right); err != nil {
		return false, err
	}
	return string(leftHash.Sum(nil)) == string(rightHash.Sum(nil)), nil
}

func validateOperatorBundle(root, distPath, version string) error {
	bundle := filepath.Join(distPath, fmt.Sprintf("%s-v%s-operator.zip", pluginID, version))
	if _, err := os.Stat(bundle); err != nil {
		return fmt.Errorf("operator bundle is required: %w", err)
	}
	archive, err := zip.OpenReader(bundle)
	if err != nil {
		return fmt.Errorf("open operator bundle: %w", err)
	}
	defer archive.Close()
	want := map[string]os.FileMode{
		"compatibility-evidence.json": 0o644,
		"config.yaml.tmpl":            0o644,
		"registry.json":               0o644,
		"router-capacity-source.json": 0o644,
		"verify-live.sh":              0o755,
		"verify-live-opencodego.sh":   0o755,
		"collector-zai.py":            0o755,
		"collector-opencodego.py":     0o755,
		"prepare-usage-dir.py":        0o755,
		"remove-usage-output.py":      0o755,
		"rollback.sh":                 0o755,
		"README.md":                   0o644,
		"release-sha.txt":             0o644,
	}
	seen := make(map[string]struct{}, len(archive.File))
	for _, entry := range archive.File {
		name := entry.Name
		if name == "" || filepath.Base(name) != name || strings.Contains(name, "..") || entry.FileInfo().IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("operator bundle contains unsafe entry %q", name)
		}
		mode, ok := want[name]
		if !ok {
			return fmt.Errorf("operator bundle contains unexpected entry %q", name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("operator bundle contains duplicate entry %q", name)
		}
		if entry.Mode().Perm() != mode {
			return fmt.Errorf("operator bundle entry %s has mode %04o, want %04o", name, entry.Mode().Perm(), mode)
		}
		stagedPath := filepath.Join(distPath, name)
		if err := requireNonEmpty(stagedPath); err != nil {
			return err
		}
		entryReader, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open operator bundle entry %s: %w", name, err)
		}
		staged, err := os.Open(stagedPath)
		if err != nil {
			_ = entryReader.Close()
			return fmt.Errorf("open staged operator artifact %s: %w", name, err)
		}
		equal, err := readersEqual(entryReader, staged)
		errEntryClose := entryReader.Close()
		errStagedClose := staged.Close()
		if err != nil {
			return fmt.Errorf("compare operator bundle entry %s: %w", name, err)
		}
		if errEntryClose != nil || errStagedClose != nil {
			return fmt.Errorf("close operator bundle entry %s", name)
		}
		if !equal {
			return fmt.Errorf("operator bundle entry %s does not match staged artifact", name)
		}
		if name == "config.yaml.tmpl" || name == "README.md" {
			raw, err := os.ReadFile(stagedPath)
			if err != nil {
				return fmt.Errorf("read staged operator artifact %s: %w", name, err)
			}
			if bytes.Contains(raw, []byte("${RELEASE_SHA}")) || bytes.Contains(raw, []byte("${REGISTRY_SHA256}")) {
				return fmt.Errorf("operator bundle entry %s contains unresolved release placeholders", name)
			}
		}
		seen[name] = struct{}{}
	}
	if len(seen) != len(want) {
		return fmt.Errorf("operator bundle contains %d entries, want %d", len(seen), len(want))
	}
	for name := range want {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("operator bundle is missing %s", name)
		}
	}
	registryRaw, err := os.ReadFile(filepath.Join(distPath, "registry.json"))
	if err != nil {
		return fmt.Errorf("read staged registry: %w", err)
	}
	sourceRegistry, err := os.ReadFile(filepath.Join(root, "registry.json"))
	if err != nil {
		return fmt.Errorf("read source registry: %w", err)
	}
	if !bytes.Equal(registryRaw, sourceRegistry) {
		return errors.New("staged registry.json does not match reviewed source")
	}
	return nil
}

func validateReleaseSHA(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release SHA: %w", err)
	}
	value := strings.TrimSuffix(string(raw), "\n")
	if len(value) != 40 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(value) || string(raw) != value+"\n" {
		return errors.New("release-sha.txt must contain exactly one lowercase 40-character commit SHA")
	}
	return nil
}

func validateChecksums(path string, artifactPaths []string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open checksums: %w", err)
	}
	defer file.Close()

	checksums := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 {
			return fmt.Errorf("invalid checksum line %q", line)
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return fmt.Errorf("invalid checksum for %s: %w", parts[1], err)
		}
		name := strings.TrimPrefix(parts[1], "*")
		if filepath.Base(name) != name {
			return fmt.Errorf("checksum name %q must be a base filename", name)
		}
		if _, exists := checksums[name]; exists {
			return fmt.Errorf("duplicate checksum for %s", name)
		}
		checksums[name] = strings.ToLower(parts[0])
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	if len(checksums) != len(artifactPaths) {
		return fmt.Errorf("checksums contains %d entries, want %d", len(checksums), len(artifactPaths))
	}
	for _, artifactPath := range artifactPaths {
		name := filepath.Base(artifactPath)
		want, exists := checksums[name]
		if !exists {
			return fmt.Errorf("checksums missing %s", name)
		}
		got, err := fileSHA256(artifactPath)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for checksum: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("checksum %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}
