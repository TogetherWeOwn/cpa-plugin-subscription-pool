package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		tag     string
		version string
		wantErr bool
	}{
		{name: "release", tag: "v0.2.0", version: "0.2.0"},
		{name: "prerelease", tag: "v0.2.0-rc.1", version: "0.2.0-rc.1"},
		{name: "tag mismatch", tag: "v0.1.1", version: "0.2.0", wantErr: true},
		{name: "missing tag prefix", tag: "0.2.0", version: "0.2.0", wantErr: true},
		{name: "leading zero", tag: "v0.01.0", version: "0.01.0", wantErr: true},
		{name: "make expression", tag: "v$(shell,id)", version: "$(shell,id)", wantErr: true},
		{name: "newline", tag: "v0.2.0\nnext", version: "0.2.0\nnext", wantErr: true},
		{name: "slash", tag: "v0.2.0/path", version: "0.2.0/path", wantErr: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateVersion(test.tag, test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateVersion() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateRelease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReleaseRejectsRegistryMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	writeRegistry(t, root, "0.1.1")
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "registry version") {
		t.Fatalf("validateRelease() error = %v, want registry version error", err)
	}
}

func TestValidateReleaseRejectsRegistryArchiveMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	writeRegistryValue(t, root, registryPlugin{
		ID:      pluginID,
		Version: "0.2.0",
		License: "MIT",
		Release: registryRelease{Archive: pluginID + "_0.1.1_linux_amd64.zip", Checksums: "checksums.txt"},
	})
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "registry archive") {
		t.Fatalf("validateRelease() error = %v, want registry archive error", err)
	}
}

func TestValidateReleaseRequiresChecksumsForBothArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	archive := filepath.Join(root, "dist", pluginID+"_0.2.0_linux_amd64.zip")
	writeChecksums(t, filepath.Join(root, "dist", "checksums.txt"), []string{archive})
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "want 16") {
		t.Fatalf("validateRelease() error = %v, want missing artifact checksum error", err)
	}
}

func TestValidateReleaseRejectsArchiveMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	archive := filepath.Join(root, "dist", pluginID+"_0.2.0_linux_amd64.zip")
	writeArchive(t, archive, []byte("different"))
	library := filepath.Join(root, "dist", pluginID+"-v0.2.0.so")
	writeChecksums(t, filepath.Join(root, "dist", "checksums.txt"), []string{library, archive})
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("validateRelease() error = %v, want archive mismatch error", err)
	}
}

func TestValidateDocumentationRejectsTruncatedLicense(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("MIT License\nPermission is hereby granted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentation(root); err == nil || !strings.Contains(err.Error(), "canonical MIT license") {
		t.Fatalf("validateDocumentation() error = %v, want canonical license error", err)
	}
}

func TestValidateDocumentationRejectsTruncatedNotice(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentation(root); err == nil || !strings.Contains(err.Error(), "canonical project notice") {
		t.Fatalf("validateDocumentation() error = %v, want canonical NOTICE error", err)
	}
}

func TestValidateDocumentationRejectsReorderedLicense(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	paragraphs := strings.Split(canonicalMITLicense, "\n\n")
	paragraphs[1], paragraphs[2] = paragraphs[2], paragraphs[1]
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte(strings.Join(paragraphs, "\n\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentation(root); err == nil || !strings.Contains(err.Error(), "canonical MIT license") {
		t.Fatalf("validateDocumentation() error = %v, want reordered license rejection", err)
	}
}

func TestValidateHostImagePinRejectsDigestDrift(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	path := filepath.Join(root, ".github", "host-images.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), baselineHostImageAMD64Digest, "sha256:"+strings.Repeat("0", 64), 1))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateHostImagePin(root); err == nil || !strings.Contains(err.Error(), "approved v7.2.150") {
		t.Fatalf("validateHostImagePin() error = %v, want digest mismatch", err)
	}
}

func TestValidateWorkflowActionPinsRejectsUnpinnedAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "steps:\n  - uses: actions/checkout@v7\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want unpinned action rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsFlowSyntax(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "steps:\n  - { uses: attacker/example@main }\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want flow-style unpinned action rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsAlias(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "unpinned: &unpinned attacker/example@main\nsteps:\n  - uses: *unpinned\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "cannot use YAML aliases") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want aliased action rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsAliasedKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "name: &uses uses\nsteps:\n  - *uses: attacker/example@main\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "cannot use YAML aliases") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want aliased key rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsUnpinnedYAMLWorkflow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "steps: []\n")
	writeWorkflow(t, root, "bypass.yaml", "steps:\n  - uses: attacker/example@main\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "bypass.yaml") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want .yaml action rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsExtraPublishCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := validReleaseWorkflow(t) + "      - name: Exfiltrate\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: printf '%s' \"$GH_TOKEN\" >/dev/null\n"
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want canonical publication rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsExtraPublishAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "      - name: Publish GitHub release\n", "      - { uses: attacker/example@"+strings.Repeat("a", 40)+" }\n      - name: Publish GitHub release\n", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want extra action rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsWorkflowDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "permissions:\n  contents: read", "defaults:\n  run:\n    shell: bash -c 'printf malicious-side-effect; bash {0}'\npermissions:\n  contents: read", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want workflow defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublishDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "    runs-on: ubuntu-24.04\n    permissions:\n      contents: write", "    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n    permissions:\n      contents: write", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publish defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "    outputs:\n      version:", "    defaults:\n      run:\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n    outputs:\n      version:", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want build defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildStepShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "      - name: Package\n        working-directory:", "      - name: Package\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n        working-directory:", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want build step shell rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublishContainer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "    runs-on: ubuntu-24.04\n    permissions:\n      contents: write", "    runs-on: ubuntu-24.04\n    container: attacker.invalid/credential-stealer:latest\n    permissions:\n      contents: write", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publish container rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublicationShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "      - name: Publish GitHub release\n        env:", "      - name: Publish GitHub release\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n        env:", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publication shell rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryAcceptsCanonicalWorkflow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseWorkflows(t, root, validReleaseWorkflow(t))
	if err := validateReleaseWorkflowBoundary(root); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPreviousDownloadAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333", "actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical action") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want previous download action rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildCommandMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "          make package VERSION=\"$VERSION\" OUT=\"$library\" ARCHIVE=\"$archive\"", "          make package VERSION=\"$VERSION\" OUT=\"$library\" ARCHIVE=\"$archive\"\n          printf injected >> \"$library\"", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical command") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want command mutation rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildEnvironmentMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "        env:\n          VERSION: ${{ steps.release.outputs.version }}\n          RAW_TAG: ${{ steps.target.outputs.tag }}\n        run: |\n          set -euo pipefail\n          library=", "        env:\n          VERSION: ${{ steps.release.outputs.version }}\n          RAW_TAG: ${{ steps.target.outputs.tag }}\n          BASH_ENV: ../release-controls/attacker.sh\n        run: |\n          set -euo pipefail\n          library=", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical environment") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want environment mutation rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsFoldedBuildCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "        run: |\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", "        run: >-\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "literal block style") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want folded build command rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsChangedRunChomping(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "        run: |\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", "        run: |-\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "exactly run: |") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want changed chomping rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsFoldedPublicationCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow(t), "        run: |\n          set -euo pipefail\n          actual_tag_object=", "        run: >-\n          set -euo pipefail\n          actual_tag_object=", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "literal block style") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want folded publication command rejection", err)
	}
}

func writeReleaseWorkflows(t *testing.T, root, release string) {
	t.Helper()
	writeWorkflow(t, root, "ci.yml", "name: CI\non:\n  push:\n    branches: [main]\n  pull_request:\n")
	writeWorkflow(t, root, "release.yml", release)
}

func writeWorkflow(t *testing.T, root, name, contents string) {
	t.Helper()
	workflowDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func validReleaseWorkflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeReleaseFixture(t *testing.T, root, version string) {
	t.Helper()
	writeDocumentation(t, root)
	writeRegistry(t, root, version)
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(dist, pluginID+"-v"+version+".so")
	archive := filepath.Join(dist, pluginID+"_"+version+"_linux_amd64.zip")
	if err := os.WriteFile(library, []byte("shared-library"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeArchive(t, archive, []byte("shared-library"))
	for name, mode := range operatorModes {
		body := "fixture-" + name
		if name == "registry.json" {
			raw, err := os.ReadFile(filepath.Join(root, "registry.json"))
			if err != nil {
				t.Fatal(err)
			}
			body = string(raw)
		}
		if name == "release-sha.txt" {
			body = strings.Repeat("a", 40) + "\n"
		}
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeOperatorBundleFromDist(t, dist, version)
	artifacts := []string{library, archive, filepath.Join(dist, pluginID+"-v"+version+"-operator.zip")}
	for name := range operatorModes {
		artifacts = append(artifacts, filepath.Join(dist, name))
	}
	writeChecksums(t, filepath.Join(dist, "checksums.txt"), artifacts)
}

func writeOperatorBundleFromDist(t *testing.T, dist, version string) {
	t.Helper()
	file, err := os.Create(filepath.Join(dist, pluginID+"-v"+version+"-operator.zip"))
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, mode := range operatorModes {
		raw, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			t.Fatal(err)
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeDocumentation(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	matrix := map[string]any{
		"repository": hostImageRepository,
		"platform":   "linux/amd64",
		"baseline": map[string]string{
			"tag":             baselineHostImageTag,
			"manifest_digest": baselineHostImageAMD64Digest,
		},
	}
	matrixRaw, err := json.Marshal(matrix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "host-images.json"), matrixRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	deployed := map[string]string{
		"repository":      hostImageRepository,
		"tag":             "v7.2.151",
		"platform":        "linux/amd64",
		"manifest_digest": "sha256:" + strings.Repeat("1", 64),
	}
	deployedRaw, err := json.Marshal(deployed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "deployed-host-image.json"), deployedRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, ".github", "scripts", "resolve-host-images.sh"),
		filepath.Join(root, ".github", "scripts", "run-host-matrix.sh"),
		filepath.Join(root, ".github", "workflows", "host-compatibility.yml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte(canonicalMITLicense), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte(canonicalNotice), 0o644); err != nil {
		t.Fatal(err)
	}
	changelog := "## [0.2.0] - 2026-09-08\n\n[0.2.0]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.2.0\n"
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRegistry(t *testing.T, root, version string) {
	t.Helper()
	writeRegistryValue(t, root, registryPlugin{
		ID:      pluginID,
		Version: version,
		License: "MIT",
		Release: registryRelease{
			Archive:   pluginID + "_" + version + "_linux_amd64.zip",
			Checksums: "checksums.txt",
		},
	})
}

func writeRegistryValue(t *testing.T, root string, plugin registryPlugin) {
	t.Helper()
	value := registry{Plugins: []registryPlugin{plugin}}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "registry.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeArchive(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: libraryName, Method: zip.Deflate}
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeChecksums(t *testing.T, path string, artifacts []string) {
	t.Helper()
	var lines strings.Builder
	for _, artifact := range artifacts {
		raw, err := os.ReadFile(artifact)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		lines.WriteString(hex.EncodeToString(sum[:]))
		lines.WriteString("  ")
		lines.WriteString(filepath.Base(artifact))
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
