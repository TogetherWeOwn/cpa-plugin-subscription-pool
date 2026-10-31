package main

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

func validateReleaseWorkflowShape(raw []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("parse release workflow structure: %w", err)
	}
	workflow, err := yamlMapping(&document, "release workflow")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(workflow, "release workflow", "name", "on", "permissions", "jobs"); err != nil {
		return err
	}
	trigger, err := yamlMapping(workflow["on"], "release workflow trigger")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(trigger, "release workflow trigger", "push"); err != nil {
		return err
	}
	push, err := yamlMapping(trigger["push"], "release push trigger")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(push, "release push trigger", "tags"); err != nil {
		return err
	}
	tags, err := yamlStringSequence(push["tags"], "release tag trigger")
	if err != nil || len(tags) != 1 || tags[0] != "v*" {
		return errors.New("release push trigger must contain only v* tags")
	}
	permissions, err := yamlStringMap(workflow["permissions"], "release workflow permissions")
	if err != nil || len(permissions) != 1 || permissions["contents"] != "read" {
		return errors.New("release workflow must default contents permission to read")
	}
	jobs, err := yamlMapping(workflow["jobs"], "release workflow jobs")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(jobs, "release workflow jobs", "build", "host-matrix", "publish"); err != nil {
		return err
	}
	build, err := yamlMapping(jobs["build"], "release build job")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(build, "release build job", "runs-on", "outputs", "steps"); err != nil {
		return err
	}
	if yamlScalarValue(build["runs-on"]) != "ubuntu-24.04" {
		return errors.New("release build job must use the approved runner")
	}
	outputs, err := yamlStringMap(build["outputs"], "release build outputs")
	if err != nil || len(outputs) != 5 || outputs["version"] != "${{ steps.release.outputs.version }}" || outputs["tag"] != "${{ steps.target.outputs.tag }}" || outputs["tag_object"] != "${{ steps.target.outputs.tag_object }}" || outputs["release_sha"] != "${{ steps.release.outputs.release_sha }}" || outputs["artifacts_sha256"] != "${{ steps.stage.outputs.artifacts_sha256 }}" {
		return errors.New("release build job must export only the validated version, tag, tag object, release SHA, and staged artifacts digest")
	}
	steps := build["steps"]
	if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) != 13 {
		return errors.New("release build job must contain exactly the canonical release steps")
	}
	actions := map[int]string{
		0:  "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		1:  "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		4:  "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		7:  "golangci/golangci-lint-action@ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a",
		12: "actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9",
	}
	commands := map[int]canonicalRunStep{
		2: {name: "Validate trusted release controls", workingDirectory: "release-controls", command: `set -euo pipefail
			go test ./.github/scripts/release-validation
			go run -buildvcs=false ./.github/scripts/release-validation -mode source
			.github/scripts/select-release-tag_test.sh`},
		3: {name: "Select release tag", id: "target", workingDirectory: "release-controls", env: map[string]string{"EVENT_NAME": "${{ github.event_name }}", "EVENT_REF": "${{ github.ref }}", "EVENT_SHA": "${{ github.sha }}"}, command: `set -euo pipefail
			raw_tag=$(.github/scripts/select-release-tag.sh \
			  "$EVENT_NAME" "$EVENT_REF" "$EVENT_SHA" .)
			tag_object=$(git rev-parse "$raw_tag")
			printf 'tag=%s\n' "$raw_tag" >> "$GITHUB_OUTPUT"
			printf 'tag_object=%s\n' "$tag_object" >> "$GITHUB_OUTPUT"`},
		5: {name: "Validate tag and provenance", id: "release", workingDirectory: "release-source", env: map[string]string{"RAW_TAG": "${{ steps.target.outputs.tag }}", "EXPECTED_TAG_OBJECT": "${{ steps.target.outputs.tag_object }}"}, command: `set -euo pipefail
			version=$(go run -buildvcs=false ./.github/scripts/release-validation -mode version -tag "$RAW_TAG")
			test "$(git rev-parse "$RAW_TAG")" = "$EXPECTED_TAG_OBJECT"
			release_sha=$(git rev-parse "$RAW_TAG^{commit}")
			test "$(git rev-parse HEAD)" = "$release_sha"
			git fetch --no-tags origin main
			git merge-base --is-ancestor "$release_sha" origin/main
			printf 'version=%s\n' "$version" >> "$GITHUB_OUTPUT"
			printf 'release_sha=%s\n' "$release_sha" >> "$GITHUB_OUTPUT"`},
		6: {name: "Verify", workingDirectory: "release-source", command: `make fmt-check
			make vet
			make test
			make test-release
			make validate-source
			make scan-secrets`},
		8: {name: "Resolve immutable host image matrix", workingDirectory: "release-source", command: `set -euo pipefail
			.github/scripts/resolve-host-images.sh > "$RUNNER_TEMP/host-images.json"`},
		9: {name: "Package", workingDirectory: "release-source", env: map[string]string{"VERSION": "${{ steps.release.outputs.version }}", "RAW_TAG": "${{ steps.target.outputs.tag }}"}, command: `set -euo pipefail
			library="dist/subscription-pool-v${VERSION}.so"
			archive="dist/subscription-pool_${VERSION}_linux_amd64.zip"
			make package VERSION="$VERSION" OUT="$library" ARCHIVE="$archive" \
			  OPERATOR="dist/subscription-pool-v${VERSION}-operator.zip" \
			  RELEASE_SHA="$(git rev-parse "$RAW_TAG^{commit}")" \
			  COMPATIBILITY_EVIDENCE="$RUNNER_TEMP/host-images.json"`},
		10: {name: "Verify release artifacts", workingDirectory: "release-source", env: map[string]string{"VERSION": "${{ steps.release.outputs.version }}", "RAW_TAG": "${{ steps.target.outputs.tag }}"}, command: `set -euo pipefail
			library="dist/subscription-pool-v${VERSION}.so"
			archive="dist/subscription-pool_${VERSION}_linux_amd64.zip"
			nm -D "$library" | grep -Eq '[[:space:]]cliproxy_plugin_init$'
			test "$(unzip -Z1 "$archive")" = "subscription-pool.so"
			cmp "$library" <(unzip -p "$archive" subscription-pool.so)
			go run -buildvcs=false ./.github/scripts/release-validation \
			  -mode release -version "$VERSION" -tag "$RAW_TAG"`},
		11: {name: "Stage release artifacts", id: "stage", workingDirectory: "release-source", env: map[string]string{"VERSION": "${{ steps.release.outputs.version }}", "RELEASE_SHA": "${{ steps.release.outputs.release_sha }}"}, command: `set -euo pipefail
			mkdir release-artifacts
			cp "dist/subscription-pool-v${VERSION}.so" \
			   "dist/subscription-pool_${VERSION}_linux_amd64.zip" \
			   "dist/subscription-pool-v${VERSION}-operator.zip" \
			   dist/checksums.txt \
			   dist/compatibility-evidence.json \
			   dist/config.yaml.tmpl \
			   dist/registry.json \
			   dist/router-capacity-source.json \
			   dist/verify-live.sh \
			   dist/verify-live-opencodego.sh \
			   dist/collector-zai.py \
			   dist/collector-opencodego.py \
			   dist/prepare-usage-dir.py \
			   dist/remove-usage-output.py \
			   dist/rollback.sh \
			   dist/README.md \
			   dist/release-sha.txt \
			   release-artifacts/
			test "$(<release-artifacts/release-sha.txt)" = "$RELEASE_SHA"
			export LC_ALL=C
			digest=$(cd release-artifacts && sha256sum -- * | sha256sum | cut -d' ' -f1)
			printf 'artifacts_sha256=%s\n' "$digest" >> "$GITHUB_OUTPUT"`},
	}
	for index, node := range steps.Content {
		step, err := yamlMapping(node, fmt.Sprintf("release build step %d", index+1))
		if err != nil {
			return err
		}
		if approved, ok := actions[index]; ok {
			if err := validateReleaseActionStep(index, step, approved); err != nil {
				return err
			}
			continue
		}
		canonical, ok := commands[index]
		if !ok {
			return fmt.Errorf("release build step %d is not approved", index+1)
		}
		if err := validateReleaseCommandStep(raw, "release build", index, step, canonical); err != nil {
			return err
		}
	}
	if err := validateHostMatrixJob(raw, jobs["host-matrix"]); err != nil {
		return err
	}
	return validatePublishJob(raw, jobs["publish"])
}

func validateReleaseActionStep(index int, step map[string]*yaml.Node, approved string) error {
	if err := requireOnlyYAMLKeys(step, fmt.Sprintf("release build step %d", index+1), "name", "uses", "with"); err != nil {
		return err
	}
	if yamlScalarValue(step["uses"]) != approved {
		return fmt.Errorf("release build action step %d must use the canonical pinned action", index+1)
	}
	with, err := yamlStringMap(step["with"], fmt.Sprintf("release build action step %d inputs", index+1))
	if err != nil {
		return err
	}
	switch index {
	case 0:
		if len(with) != 4 || with["fetch-depth"] != "0" || with["persist-credentials"] != "false" || with["ref"] != "${{ github.sha }}" || with["path"] != "release-controls" {
			return errors.New("trusted control checkout must use the workflow SHA without credentials")
		}
	case 1:
		if len(with) != 3 || with["go-version-file"] != "release-controls/go.mod" || with["cache-dependency-path"] != "release-controls/go.sum" || with["cache"] != "true" {
			return errors.New("Go setup must use only the trusted control module files")
		}
	case 4:
		if len(with) != 4 || with["fetch-depth"] != "0" || with["persist-credentials"] != "false" || with["ref"] != "${{ steps.target.outputs.tag }}" || with["path"] != "release-source" {
			return errors.New("release source checkout must use only the selected immutable tag without credentials")
		}
	case 7:
		if len(with) != 2 || with["version"] != "v2.13.2" || with["working-directory"] != "release-source" {
			return errors.New("release lint must use only the immutable source checkout")
		}
	case 12:
		if len(with) != 4 || with["name"] != "release-artifacts" || with["path"] != "release-source/release-artifacts/" || with["if-no-files-found"] != "error" || with["retention-days"] != "1" {
			return errors.New("release artifact upload must use only the canonical immutable-source inputs")
		}
	}
	return nil
}

func validateReleaseCommandStep(raw []byte, scope string, index int, step map[string]*yaml.Node, canonical canonicalRunStep) error {
	if err := requireOnlyYAMLKeys(step, fmt.Sprintf("%s step %d", scope, index+1), "name", "id", "working-directory", "env", "run"); err != nil {
		return err
	}
	if yamlScalarValue(step["name"]) != canonical.name || yamlScalarValue(step["id"]) != canonical.id || yamlScalarValue(step["working-directory"]) != canonical.workingDirectory {
		return fmt.Errorf("%s step %d must use the canonical identity and working directory", scope, index+1)
	}
	env, err := yamlStringMap(step["env"], fmt.Sprintf("%s step %d environment", scope, index+1))
	if err != nil {
		return err
	}
	if !equalStringMaps(env, canonical.env) {
		return fmt.Errorf("%s step %d must use only the canonical environment", scope, index+1)
	}
	if err := requireCanonicalRunScalar(raw, step["run"], fmt.Sprintf("%s step %d", scope, index+1)); err != nil {
		return err
	}
	if normalizeShellCommand(yamlScalarValue(step["run"])) != normalizeShellCommand(canonical.command) {
		return fmt.Errorf("%s step %d must use the canonical command", scope, index+1)
	}
	return nil
}

func validateHostMatrixJob(raw []byte, node *yaml.Node) error {
	job, err := yamlMapping(node, "release host matrix job")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(job, "release host matrix job", "needs", "runs-on", "permissions", "steps"); err != nil {
		return err
	}
	if yamlScalarValue(job["needs"]) != "build" || yamlScalarValue(job["runs-on"]) != "ubuntu-24.04" {
		return errors.New("release host matrix job must depend only on build and use the approved runner")
	}
	permissions, err := yamlStringMap(job["permissions"], "release host matrix job permissions")
	if err != nil || len(permissions) != 1 || permissions["contents"] != "read" {
		return errors.New("release host matrix job must hold only contents read permission")
	}
	steps := job["steps"]
	if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) != 4 {
		return errors.New("release host matrix job must contain exactly the canonical checkout, Go setup, download, and matrix steps")
	}
	actions := map[int]string{
		0: "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		1: "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		2: "actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333",
	}
	names := []string{"Check out immutable release source", "Set up Go", "Download release artifacts"}
	for index, approved := range actions {
		step, err := yamlMapping(steps.Content[index], fmt.Sprintf("release host matrix step %d", index+1))
		if err != nil {
			return err
		}
		if err := requireOnlyYAMLKeys(step, fmt.Sprintf("release host matrix step %d", index+1), "name", "uses", "with"); err != nil {
			return err
		}
		if yamlScalarValue(step["name"]) != names[index] || yamlScalarValue(step["uses"]) != approved {
			return fmt.Errorf("release host matrix step %d must use the canonical pinned action", index+1)
		}
		with, err := yamlStringMap(step["with"], fmt.Sprintf("release host matrix action step %d inputs", index+1))
		if err != nil {
			return err
		}
		switch index {
		case 0:
			if len(with) != 4 || with["fetch-depth"] != "1" || with["persist-credentials"] != "false" || with["ref"] != "${{ needs.build.outputs.release_sha }}" || with["path"] != "release-source" {
				return errors.New("host matrix checkout must use only the recorded release SHA without credentials")
			}
		case 1:
			if len(with) != 3 || with["go-version-file"] != "release-source/go.mod" || with["cache-dependency-path"] != "release-source/go.sum" || with["cache"] != "true" {
				return errors.New("host matrix Go setup must use only the release source module files")
			}
		case 2:
			if len(with) != 2 || with["name"] != "release-artifacts" || with["path"] != "host-matrix-artifacts" {
				return errors.New("host matrix artifact download must use only the canonical inputs outside the source checkout")
			}
		}
	}
	matrix, err := yamlMapping(steps.Content[3], "release host matrix command step")
	if err != nil {
		return err
	}
	return validateReleaseCommandStep(raw, "release host matrix", 3, matrix, canonicalRunStep{
		name:             "Test host compatibility matrix",
		workingDirectory: "release-source",
		env:              map[string]string{"VERSION": "${{ needs.build.outputs.version }}", "HOST_MATRIX_NAMESPACE": "root"},
		command: `set -euo pipefail
			sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix VERSION="$VERSION" OUT="$GITHUB_WORKSPACE/host-matrix-artifacts/subscription-pool-v${VERSION}.so" HOST_IMAGES="$GITHUB_WORKSPACE/host-matrix-artifacts/compatibility-evidence.json" HOST_MATRIX_WORK="$RUNNER_TEMP/host-matrix"`,
	})
}

func validatePublishJob(raw []byte, node *yaml.Node) error {
	publish, err := yamlMapping(node, "release publish job")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(publish, "release publish job", "needs", "runs-on", "permissions", "steps"); err != nil {
		return err
	}
	needs, err := yamlStringSequence(publish["needs"], "release publish job needs")
	if err != nil || len(needs) != 2 || needs[0] != "build" || needs[1] != "host-matrix" || yamlScalarValue(publish["runs-on"]) != "ubuntu-24.04" {
		return errors.New("release publish job must depend on build and host-matrix and use the approved runner")
	}
	permissions, err := yamlStringMap(publish["permissions"], "release publish job permissions")
	if err != nil || len(permissions) != 1 || permissions["contents"] != "write" {
		return errors.New("release publish job must hold only contents write permission")
	}
	steps := publish["steps"]
	if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) != 3 {
		return errors.New("release publish job must contain exactly the artifact download, digest verification, and canonical publication steps")
	}
	download, err := yamlMapping(steps.Content[0], "release artifact download step")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(download, "release artifact download step", "name", "uses", "with"); err != nil {
		return err
	}
	with, err := yamlStringMap(download["with"], "release artifact download inputs")
	if err != nil || yamlScalarValue(download["uses"]) != "actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333" || len(with) != 2 || with["name"] != "release-artifacts" || with["path"] != "release-artifacts" {
		return errors.New("release artifact download step must use the canonical action and inputs")
	}
	verify, err := yamlMapping(steps.Content[1], "release artifact digest step")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(verify, "release artifact digest step", "name", "env", "run"); err != nil {
		return err
	}
	verifyEnv, err := yamlStringMap(verify["env"], "release artifact digest environment")
	if err != nil || yamlScalarValue(verify["name"]) != "Verify release artifacts digest" || len(verifyEnv) != 1 || verifyEnv["EXPECTED_ARTIFACTS_SHA256"] != "${{ needs.build.outputs.artifacts_sha256 }}" {
		return errors.New("release artifact digest step must use the canonical name and environment")
	}
	if err := requireCanonicalRunScalar(raw, verify["run"], "release artifact digest step"); err != nil {
		return err
	}
	const verifyCommand = `set -euo pipefail
		export LC_ALL=C
		test -n "$EXPECTED_ARTIFACTS_SHA256"
		actual=$(cd release-artifacts && sha256sum -- * | sha256sum | cut -d' ' -f1)
		test "$actual" = "$EXPECTED_ARTIFACTS_SHA256"`
	if normalizeShellCommand(yamlScalarValue(verify["run"])) != normalizeShellCommand(verifyCommand) {
		return errors.New("release artifact digest step must use the canonical verification command")
	}
	publication, err := yamlMapping(steps.Content[2], "release publication step")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(publication, "release publication step", "name", "env", "run"); err != nil {
		return err
	}
	env, err := yamlStringMap(publication["env"], "release publication environment")
	if err != nil || len(env) != 5 || env["GH_TOKEN"] != "${{ github.token }}" || env["GH_REPO"] != "${{ github.repository }}" || env["VERSION"] != "${{ needs.build.outputs.version }}" || env["RAW_TAG"] != "${{ needs.build.outputs.tag }}" || env["EXPECTED_TAG_OBJECT"] != "${{ needs.build.outputs.tag_object }}" {
		return errors.New("release publication environment must contain exactly the canonical variables")
	}
	if err := requireCanonicalRunScalar(raw, publication["run"], "release publication step"); err != nil {
		return err
	}
	const command = `set -euo pipefail
		actual_tag_object=$(gh api "repos/${GH_REPO}/git/ref/tags/${RAW_TAG}" --jq .object.sha)
		test "$actual_tag_object" = "$EXPECTED_TAG_OBJECT"
		gh release create "$RAW_TAG" \
		  "release-artifacts/subscription-pool-v${VERSION}.so" \
		  "release-artifacts/subscription-pool_${VERSION}_linux_amd64.zip" \
		  "release-artifacts/subscription-pool-v${VERSION}-operator.zip" \
		  release-artifacts/checksums.txt \
		  release-artifacts/compatibility-evidence.json \
		  release-artifacts/config.yaml.tmpl \
		  release-artifacts/registry.json \
		  release-artifacts/router-capacity-source.json \
		  release-artifacts/verify-live.sh \
		  release-artifacts/verify-live-opencodego.sh \
		  release-artifacts/collector-zai.py \
		  release-artifacts/collector-opencodego.py \
		  release-artifacts/prepare-usage-dir.py \
		  release-artifacts/remove-usage-output.py \
		  release-artifacts/rollback.sh \
		  release-artifacts/README.md \
		  release-artifacts/release-sha.txt \
		  --verify-tag --generate-notes`
	if normalizeShellCommand(yamlScalarValue(publication["run"])) != normalizeShellCommand(command) {
		return errors.New("release publish job must use the canonical publication command")
	}
	return nil
}
