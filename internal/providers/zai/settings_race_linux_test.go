//go:build linux

package zai

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

type blockingSettings struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingSettings) MarshalJSON() ([]byte, error) {
	close(b.started)
	<-b.release
	return []byte(`{"version":1}`), nil
}

func TestSecureStoreCloseWaitsForActiveWrite(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.writeJSON("state.json", blockingSettings{started: started, release: release})
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("close raced active write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := store.validateDirectory(); err == nil {
		t.Fatal("closed store remained usable")
	}
}

func TestSettingsRenameSyncFailureIsRecoverablyCommitted(t *testing.T) {
	for _, failCall := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("directory_sync_%d", failCall), func(t *testing.T) {
			store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.close() }()
			identity := accountIdentity("fault")
			first := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "first", Plan: "pro"}}}
			if err := store.saveSettings(first); err != nil {
				t.Fatal(err)
			}
			calls := 0
			store.SetDirSyncForTest(func(dir *os.File) error {
				calls++
				if calls == failCall {
					return errors.New("injected directory sync failure")
				}
				return dir.Sync()
			})
			second := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "second", Plan: "pro"}}}
			if err := store.saveSettings(second); err != nil {
				t.Fatalf("recoverable post-rename sync failure was reported as rejection: %v", err)
			}
			store.SetDirSyncForTest(nil)
			recovered, err := store.recoverSettings()
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Accounts[identity].Name != "second" {
				t.Fatalf("recovery did not preserve acknowledged settings: %#v", recovered)
			}
		})
	}
}

func TestSettingsFirstDirectorySyncFailureRejectsBeforeDesiredMarker(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("first-sync")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	store.SetDirSyncForTest(func(*os.File) error {
		return errors.New("injected directory sync failure")
	})
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	if err := store.saveSettings(desired); err == nil {
		t.Fatal("settings were acknowledged before the intent marker became durable")
	}
	store.SetDirSyncForTest(nil)
	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "previous" {
		t.Fatalf("rejected settings became active after recovery: %#v", recovered)
	}
}

func TestSettingsSecondDirectorySyncFailureRemainsPending(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("second-sync")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetDirSyncForTest(func(dir *os.File) error {
		calls++
		if calls >= 2 {
			return errors.New("injected persistent directory sync failure")
		}
		return dir.Sync()
	})
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	if err := store.saveSettings(desired); err == nil || !settingsRecoveryPending(err) {
		t.Fatalf("settings outcome = %v, want recoverable pending commit", err)
	}
	store.SetDirSyncForTest(nil)
	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "desired" {
		t.Fatalf("pending settings were not recovered: %#v", recovered)
	}
}

func TestSettingsSecondDirectorySyncFailureAcknowledgesAlreadyCommittedSettings(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("second-sync-committed")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	calls := 0
	store.SetDirSyncForTest(func(dir *os.File) error {
		calls++
		if calls == 1 {
			return dir.Sync()
		}
		if calls == 2 {
			store.SetDirSyncForTest(nil)
			if err := store.writeJSON("settings.json", desired); err != nil {
				t.Fatalf("inject committed settings: %v", err)
			}
			store.SetDirSyncForTest(func(*os.File) error { return errors.New("injected persistent directory sync failure") })
		}
		return errors.New("injected persistent directory sync failure")
	})
	if err := store.saveSettings(desired); err != nil {
		t.Fatalf("already committed settings were rejected: %v", err)
	}
}

func TestSettingsCommitDirectorySyncFailureIsRecoverablyCommitted(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("commit-sync")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetDirSyncForTest(func(dir *os.File) error {
		calls++
		if calls >= 3 {
			return errors.New("injected persistent directory sync failure")
		}
		return dir.Sync()
	})
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	if err := store.saveSettings(desired); err == nil || !settingsRecoveryPending(err) {
		t.Fatalf("settings outcome = %v, want recoverable committed update", err)
	}
	store.SetDirSyncForTest(nil)
	if err := store.writeJSON("settings.json", previous); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "desired" {
		t.Fatalf("durable marker did not recover acknowledged commit: %#v", recovered)
	}
}

func TestSettingsMarkerRemovalSyncFailurePreservesCommittedSettings(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("marker-removal-durability")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetDirSyncForTest(func(dir *os.File) error {
		calls++
		if calls >= 4 {
			return errors.New("injected directory sync failure")
		}
		return dir.Sync()
	})
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	if err := store.saveSettings(desired); err != nil {
		t.Fatalf("durably committed settings were rejected after marker cleanup failure: %v", err)
	}
	store.SetDirSyncForTest(nil)
	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "desired" {
		t.Fatalf("committed settings were lost after marker cleanup failure: %#v", recovered)
	}
}

func TestSettingsRecoveryAcceptsLegacyMarkerWithoutPreviousSnapshot(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("legacy-marker")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	legacy := struct {
		Version        int          `json:"version"`
		PreviousDigest string       `json:"previous_digest"`
		Desired        settingsFile `json:"desired"`
	}{Version: settingsRecoveryVersion, PreviousDigest: settingsDigest(previous), Desired: desired}
	if err := store.writeJSON(settingsRecoveryName, legacy); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "desired" {
		t.Fatalf("legacy marker was not rolled forward: %#v", recovered)
	}
}

func TestSettingsRecoveryRollsAcknowledgedUpdateForwardFromPreviousDigest(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("roll-forward")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	recovery := settingsRecovery{Version: settingsRecoveryVersion, PreviousDigest: settingsDigest(previous), Desired: &desired}
	if err := store.writeJSON(settingsRecoveryName, recovery); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "desired" {
		t.Fatalf("recovery kept previous settings: %#v", recovered)
	}
	loaded, err := store.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Accounts[identity].Name != "desired" {
		t.Fatalf("roll-forward was not persisted: %#v", loaded)
	}
}

func TestSecureStoreReadConfinedDuringDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := newSecureStore(filepath.Join(root, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	trusted := settingsFile{Version: 1}
	if err := store.writeJSON("settings.json", trusted); err != nil {
		t.Fatal(err)
	}

	original := store.dir + ".original"
	attacker := filepath.Join(t.TempDir(), "attacker")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatal(err)
	}
	malicious := []byte(`{"version":1,"threshold_percent":99}` + "\n")
	if err := os.WriteFile(filepath.Join(attacker, "settings.json"), malicious, 0o600); err != nil {
		t.Fatal(err)
	}
	store.SetFileOpenForTest(func(dir *os.File, name string) (*os.File, error) {
		if err := os.Rename(store.dir, original); err != nil {
			return nil, err
		}
		if err := os.Symlink(attacker, store.dir); err != nil {
			return nil, err
		}
		return securestore.OpenFileAt(dir, name)
	})

	var loaded settingsFile
	found, err := store.readJSONIfExists("settings.json", &loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("trusted settings were not found")
	}
	if loaded.ThresholdPercent != nil {
		t.Fatalf("read followed replacement path and loaded attacker settings: %#v", loaded)
	}
}

func TestSecureStoreWriteConfinedDuringDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := newSecureStore(filepath.Join(root, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	original := store.dir + ".original"
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- store.writeJSON("settings.json", blockingSettings{started: started, release: release})
	}()
	<-started
	if err := os.Rename(store.dir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.dir); err != nil {
		t.Fatal(err)
	}
	close(release)

	if err := <-result; err == nil || (!strings.Contains(err.Error(), "replaced") && !strings.Contains(err.Error(), "not a directory")) {
		t.Fatalf("save error = %v, want replaced directory rejection", err)
	}
	if _, err := os.Stat(filepath.Join(target, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("redirected write reached symlink target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(original, "settings.json")); err != nil {
		t.Fatalf("descriptor-relative write was not confined to original directory: %v", err)
	}
}
