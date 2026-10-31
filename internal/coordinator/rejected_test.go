package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/cloudflare"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/nvidianim"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/opencodego"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/synthetic"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

// stagingFakeModule adds the optional ConfigValidator to fakeModule and
// records every config Reconfigure applies.
type stagingFakeModule struct {
	fakeModule
	validate    func(json.RawMessage) error
	reconfigure func(json.RawMessage) error
	applied     []string
}

func (m *stagingFakeModule) ValidateConfig(_ providermodule.HostConfig, raw json.RawMessage) error {
	if m.validate == nil {
		return nil
	}
	return m.validate(raw)
}

func (m *stagingFakeModule) Reconfigure(_ context.Context, _ providermodule.HostConfig, raw json.RawMessage) error {
	if m.reconfigure != nil {
		if err := m.reconfigure(raw); err != nil {
			return err
		}
	}
	m.applied = append(m.applied, string(raw))
	return nil
}

func rejectVersion(version string) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		if strings.Contains(string(raw), `"v":`+version) {
			return fmt.Errorf("version %s rejected", version)
		}
		return nil
	}
}

// poolConfig returns a coordinator config giving every listed module the
// provider config {"v": version}.
func poolConfig(t *testing.T, version int, ids ...string) []byte {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	writeCoordinatorCPAConfigFixture(t, configPath, filepath.Join(root, "auth"), zaiIntegrationFixtureKey)
	raw := "cpa-config-path: " + configPath + "\nproviders:\n"
	for _, id := range ids {
		raw += fmt.Sprintf("  %s:\n    v: %d\n", id, version)
	}
	return []byte(raw)
}

func TestReconfigureStagesEveryModuleBeforeCommitting(t *testing.T) {
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha"}}
	beta := &stagingFakeModule{fakeModule: fakeModule{id: "beta"}, validate: rejectVersion("2")}
	c := New(alpha, beta)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha", "beta")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	if err := c.Reconfigure(poolConfig(t, 2, "alpha", "beta")); err == nil {
		t.Fatal("Reconfigure(v2) = nil error, want beta's validation error")
	}
	if want := []string{`{"v":1}`}; fmt.Sprint(alpha.applied) != fmt.Sprint(want) {
		t.Fatalf("alpha applied %v, want %v: a config beta rejects must never reach alpha", alpha.applied, want)
	}
}

func TestReconfigureRollsBackCommittedModulesWhenALaterCommitFails(t *testing.T) {
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha"}}
	beta := &stagingFakeModule{fakeModule: fakeModule{id: "beta"}, reconfigure: rejectVersion("2")}
	c := New(alpha, beta)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha", "beta")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	if err := c.Reconfigure(poolConfig(t, 2, "alpha", "beta")); err == nil {
		t.Fatal("Reconfigure(v2) = nil error, want beta's commit error")
	}
	if want := []string{`{"v":1}`, `{"v":2}`, `{"v":1}`}; fmt.Sprint(alpha.applied) != fmt.Sprint(want) {
		t.Fatalf("alpha applied %v, want %v: alpha must be restored to v1", alpha.applied, want)
	}
}

func TestReconfigureReportsAModuleThatRefusesRollback(t *testing.T) {
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha"}}
	alpha.reconfigure = func(raw json.RawMessage) error {
		if len(alpha.applied) > 0 && strings.Contains(string(raw), `"v":1`) {
			return errors.New("old config refused")
		}
		return nil
	}
	beta := &stagingFakeModule{fakeModule: fakeModule{id: "beta"}, reconfigure: rejectVersion("2")}
	c := New(alpha, beta)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha", "beta")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	if err := c.Reconfigure(poolConfig(t, 2, "alpha", "beta")); err == nil {
		t.Fatal("Reconfigure(v2) = nil error, want beta's commit error")
	}
	status := c.validationStatus()
	for _, want := range []string{"provider beta: version 2 rejected", "rollback provider alpha: old config refused"} {
		if !strings.Contains(status, want) {
			t.Fatalf("validation status %q does not contain %q", status, want)
		}
	}
}

func TestReconfigureRollsBackEveryModuleOnRegistryConflict(t *testing.T) {
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha", ownedIDs: []string{"shared"}}}
	beta := &stagingFakeModule{fakeModule: fakeModule{id: "beta"}}
	c := New(alpha, beta)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha", "beta")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	beta.ownedIDs = []string{"shared"}
	if err := c.Reconfigure(poolConfig(t, 2, "alpha", "beta")); err == nil {
		t.Fatal("Reconfigure(v2) = nil error, want ownership conflict")
	}
	for _, module := range []*stagingFakeModule{alpha, beta} {
		if got := module.applied[len(module.applied)-1]; got != `{"v":1}` {
			t.Fatalf("%s last applied %s, want rollback to v1", module.id, got)
		}
	}
	if got := c.registry.providerFor("shared"); got != "alpha" {
		t.Fatalf("registry owner of shared = %q, want the previous registry (alpha) live", got)
	}
}

func TestRejectedConfigFailsClosedForGuardedCandidates(t *testing.T) {
	picked := false
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha", recognize: recognizeID("alpha-"), ownedIDs: []string{"owned-by-alpha"}, pick: func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		picked = true
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: "round-robin"}, nil
	}}, validate: rejectVersion("2")}
	c := New(alpha)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	if err := c.Reconfigure(poolConfig(t, 2, "alpha")); err == nil {
		t.Fatal("Reconfigure(v2) = nil error, want rejection")
	}

	for _, candidate := range []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}, {ID: "owned-by-alpha"}} {
		_, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "other"}, candidate}})
		var rejected *configRejectedError
		if !errors.As(err, &rejected) || rejected.Coded().Code != "coordinator_config_rejected" {
			t.Fatalf("Pick(%s) error = %v, want coordinator_config_rejected", candidate.ID, err)
		}
	}
	if picked {
		t.Fatal("a module's Pick ran while the config was rejected")
	}
	if resp, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "other"}}}); err != nil || resp.Handled {
		t.Fatalf("Pick(unrelated) = %#v, %v; want Handled:false", resp, err)
	}
	status := c.managementStatus()
	if status.Status != "reconfigure_rejected" || !strings.Contains(status.ValidationError, "version 2 rejected") {
		t.Fatalf("status = %q / %q, want the rejection reported", status.Status, status.ValidationError)
	}

	if err := c.Reconfigure(poolConfig(t, 3, "alpha")); err != nil {
		t.Fatalf("Reconfigure(v3) error = %v", err)
	}
	if resp, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}}); err != nil || !resp.Handled || !picked {
		t.Fatalf("Pick after a valid reconfigure = %#v, %v; want the module's own pick", resp, err)
	}
}

func TestRejectedFirstConfigFailsClosedForRecognizedCandidates(t *testing.T) {
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha", recognize: recognizeID("alpha-")}}
	c := New(alpha)
	if err := c.Reconfigure([]byte("cpa-config-path: " + filepath.Join(t.TempDir(), "missing.yaml") + "\n")); err == nil {
		t.Fatal("Reconfigure(unreadable CPA config) = nil error, want rejection")
	}
	if _, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}}); err == nil {
		t.Fatal("Pick(recognized) after a rejected first config = nil error, want fail closed")
	}
	if len(alpha.applied) != 0 {
		t.Fatalf("alpha applied %v, want nothing", alpha.applied)
	}
}

func TestRecordedLifecycleErrorFailsClosed(t *testing.T) {
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha", recognize: recognizeID("alpha-")}}
	c := New(alpha)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	_ = c.RecordError(fmt.Errorf("lifecycle request body is invalid"))
	if _, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}}); err == nil {
		t.Fatal("Pick after an undecodable lifecycle request = nil error, want fail closed")
	}
}

// blockingReconfigure returns a reconfigure hook that, once armed, signals
// entered and blocks until release is closed.
func blockingReconfigure(armed *bool, entered chan<- struct{}, release <-chan struct{}) func(json.RawMessage) error {
	return func(json.RawMessage) error {
		if *armed {
			entered <- struct{}{}
			<-release
		}
		return nil
	}
}

func TestPickWaitsForAnInFlightCommit(t *testing.T) {
	armed := false
	entered, release := make(chan struct{}), make(chan struct{})
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha", recognize: recognizeID("alpha-")}, reconfigure: blockingReconfigure(&armed, entered, release)}
	beta := &stagingFakeModule{fakeModule: fakeModule{id: "beta"}, reconfigure: rejectVersion("2")}
	c := New(alpha, beta)
	if err := c.Reconfigure(poolConfig(t, 1, "alpha", "beta")); err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	armed = true
	go func() { _ = c.Reconfigure(poolConfig(t, 2, "alpha", "beta")) }()
	<-entered

	picked := make(chan error, 1)
	go func() {
		_, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}})
		picked <- err
	}()
	select {
	case err := <-picked:
		t.Fatalf("Pick returned (%v) while modules were mid-commit", err)
	case <-time.After(50 * time.Millisecond):
	}
	armed = false
	close(release)
	if err := <-picked; err == nil {
		t.Fatal("Pick after the failed commit = nil error, want the rejection")
	}
}

func TestOlderReconfigureCannotClearANewerRejection(t *testing.T) {
	armed := false
	entered, release := make(chan struct{}), make(chan struct{})
	alpha := &stagingFakeModule{fakeModule: fakeModule{id: "alpha", recognize: recognizeID("alpha-")}, reconfigure: blockingReconfigure(&armed, entered, release)}
	c := New(alpha)
	armed = true
	done := make(chan error, 1)
	go func() { done <- c.Reconfigure(poolConfig(t, 1, "alpha")) }()
	<-entered
	recorded := make(chan struct{})
	go func() {
		_ = c.RecordError(fmt.Errorf("lifecycle request body is invalid"))
		close(recorded)
	}()
	armed = false
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Reconfigure(v1) error = %v", err)
	}
	<-recorded
	if _, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}}); err == nil {
		t.Fatal("Pick = nil error: the earlier reconfigure cleared the later rejection")
	}
}

// A provider config the module's ValidateConfig rejects must still show on
// that module's own status route, as it did when Reconfigure rejected it.
func TestValidationRejectionShowsOnModuleStatus(t *testing.T) {
	for name, module := range map[string]providermodule.Module{
		"zai":         zai.NewModule(),
		"opencode-go": opencodego.NewModule(),
		"synthetic":   synthetic.NewModule(),
	} {
		t.Run(name, func(t *testing.T) {
			defer func() { _ = module.Close(context.Background()) }()
			c := New(module)
			root := t.TempDir()
			configPath := filepath.Join(root, "config.yaml")
			writeCoordinatorCPAConfigFixture(t, configPath, filepath.Join(root, "auth"), zaiIntegrationFixtureKey)
			raw := "cpa-config-path: " + configPath + "\nproviders:\n  " + module.ID() + ":\n    threshold-percent: not-a-number\n"
			if err := c.Reconfigure([]byte(raw)); err == nil {
				t.Fatal("Reconfigure(invalid provider config) = nil error, want rejection")
			}
			rawStatus, err := module.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Status          string `json:"status"`
				ValidationError string `json:"validation_error"`
			}
			if err := json.Unmarshal(rawStatus, &status); err != nil {
				t.Fatal(err)
			}
			if status.Status != "reconfigure_rejected" || status.ValidationError == "" {
				t.Fatalf("module status = %+v, want reconfigure_rejected with a validation error", status)
			}
		})
	}
}

// After a rejected first config no module has a snapshot, so a bare-ID
// candidate carries nothing a module recognizes. The auth IDs the host
// config gives Z.ai must fail closed anyway.
func TestRejectedFirstConfigFailsClosedForHostZaiAuthIDs(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	writeCoordinatorCPAConfigFixture(t, configPath, filepath.Join(root, "auth"), zaiIntegrationFixtureKey)

	accepted := zai.NewModule()
	defer func() { _ = accepted.Close(context.Background()) }()
	if err := New(accepted).Reconfigure([]byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    default-plan: pro\n")); err != nil {
		t.Fatalf("Reconfigure(valid) error = %v", err)
	}
	owned := accepted.OwnedAuthIDs(context.Background())
	if len(owned) == 0 {
		t.Fatal("zai owns no auth IDs under the valid config")
	}

	rejected := zai.NewModule()
	defer func() { _ = rejected.Close(context.Background()) }()
	c := New(rejected)
	if err := c.Reconfigure([]byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    threshold-percent: not-a-number\n")); err == nil {
		t.Fatal("Reconfigure(invalid) = nil error, want rejection")
	}
	for _, id := range owned {
		_, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: id}}})
		if err == nil || !strings.Contains(err.Error(), "coordinator_config_rejected") {
			t.Fatalf("Pick(bare %q) error = %v, want coordinator_config_rejected", id, err)
		}
	}
}

// Before any configuration is accepted no module recognizes anything, so
// whatever the cause of the rejection, every pick fails closed: a candidate
// can never be shown to be unguarded and fall through to the host.
func TestRejectedFirstConfigFailsEveryPickClosed(t *testing.T) {
	writeCPA := func(t *testing.T, authDir, extraPlugin string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		raw := "auth-dir: " + authDir + "\nplugins:\n  enabled: true\n  configs:\n    subscription-pool:\n      enabled: true\n      priority: 1000\n" + extraPlugin
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	causes := map[string]func(t *testing.T) string{
		"parse error": func(t *testing.T) string { return "providers: [\n" },
		"unreadable CPA config": func(t *testing.T) string {
			return "cpa-config-path: " + filepath.Join(t.TempDir(), "missing.yaml") + "\n"
		},
		"non-coexisting plugin": func(t *testing.T) string {
			return "cpa-config-path: " + writeCPA(t, filepath.Join(t.TempDir(), "auth"), "    other-scheduler:\n      enabled: true\n      priority: 10\n") + "\n"
		},
		"bad auth dir": func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return "cpa-config-path: " + writeCPA(t, file, "") + "\n"
		},
	}
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "bare-auth-id"},
		{ID: "zai-auth", Provider: "zai-coding-plan", Attributes: map[string]string{"compat_name": "zai-coding-plan"}},
		{ID: "ocg-auth", Provider: "opencode-go"},
		{ID: "syn-auth", Provider: "openai-compatible-synthetic", Attributes: map[string]string{"compat_name": "synthetic"}},
		{ID: "cf-auth", Provider: "openai-compatible-cloudflare-workers-ai", Attributes: map[string]string{"compat_name": "cloudflare-workers-ai"}},
		{ID: "nim-auth", Provider: "openai-compatible-nvidia-nim", Attributes: map[string]string{"compat_name": "nvidia-nim"}},
	}
	for name, config := range causes {
		t.Run(name, func(t *testing.T) {
			modules := []providermodule.Module{zai.NewModule(), opencodego.NewModule(), synthetic.NewModule(), cloudflare.NewModule(), nvidianim.NewModule()}
			defer func() {
				for _, module := range modules {
					_ = module.Close(context.Background())
				}
			}()
			c := New(modules...)
			if err := c.Reconfigure([]byte(config(t))); err == nil {
				t.Fatal("Reconfigure = nil error, want rejection")
			}
			for _, candidate := range candidates {
				resp, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{candidate}})
				var rejected *configRejectedError
				if !errors.As(err, &rejected) {
					t.Fatalf("Pick(%s) = %+v, %v; want coordinator_config_rejected", candidate.ID, resp, err)
				}
			}
		})
	}
}
