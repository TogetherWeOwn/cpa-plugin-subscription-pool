package securestore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveJSONDeletesAndIsIdempotent(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteJSON("doc.json", sampleDoc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveJSON("doc.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(store.Dir(), "doc.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doc.json still present after removal: %v", err)
	}
	if err := store.RemoveJSON("doc.json"); err != nil {
		t.Fatalf("removing a missing file = %v, want nil", err)
	}
}

// RemoveJSON must never reach outside the store directory, whatever name it is
// handed: a traversal name would let a caller delete the CPA auth files that
// sit next to the store.
func TestRemoveJSONRejectsNamesOutsideStore(t *testing.T) {
	root := t.TempDir()
	store, err := New(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(root, "neighbour.json")
	if err := os.WriteFile(neighbour, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.Dir(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(store.Dir(), "sub", "doc.json")
	if err := os.WriteFile(nested, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../neighbour.json", "sub/doc.json", neighbour, "", ".", ".."} {
		if err := store.RemoveJSON(name); err == nil {
			t.Fatalf("RemoveJSON(%q) succeeded", name)
		}
	}
	for _, path := range []string{neighbour, nested} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed through a rejected name: %v", path, err)
		}
	}
}

func TestRemoveJSONReportsDirectorySyncFailureAsNeedsRecovery(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteJSON("doc.json", sampleDoc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	store.SetDirSyncForTest(func(*os.File) error { return errors.New("injected fsync failure") })
	err = store.RemoveJSON("doc.json")
	if OutcomeOf(err) != WriteNeedsRecovery {
		t.Fatalf("outcome = %v (err %v), want WriteNeedsRecovery", OutcomeOf(err), err)
	}
	if _, errStat := os.Lstat(filepath.Join(store.Dir(), "doc.json")); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("doc.json present after unlink with failed fsync: %v", errStat)
	}
}

func TestRemoveJSONRejectsClosedStore(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteJSON("doc.json", sampleDoc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	dir := store.Dir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveJSON("doc.json"); err == nil {
		t.Fatal("closed store removed a file")
	}
	if _, err := os.Stat(filepath.Join(dir, "doc.json")); err != nil {
		t.Fatalf("closed store removed doc.json: %v", err)
	}
}
