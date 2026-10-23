package coordinator

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

const (
	// ledgerStateName deliberately avoids a .json suffix: the host walks the
	// auth directory and parses every *.json file as a possible credential.
	ledgerStateName = "ledger.state"
	ledgerSubdir    = "usage"
	// ledgerSaveEvery bounds how often usage traffic triggers a save.
	ledgerSaveEvery = 5 * time.Minute
)

// ledgerPersistence saves the usage ledger under the plugin's own secure
// auth-dir namespace. Every failure is soft: the usage page is diagnostic,
// so persistence can never fail scheduling, usage handling or reconfigure.
type ledgerPersistence struct {
	mu  sync.Mutex
	dir string
	// ready is set once dir's saved ledger has loaded (or there was none).
	// Saves wait for it, so a file that fails to load (corrupt, from a
	// newer version, unreadable) is never overwritten, and the ledger
	// stays in the directory it loaded from until the plugin restarts.
	ready   bool
	saving  bool
	savedAt time.Time
	// lastCode is the closed-vocabulary failure the public page shows
	// (open_failed, read_failed, decode_failed, write_failed); lastErr is
	// its text, which can carry paths and goes to the authenticated route
	// only.
	lastCode string
	lastErr  string
	wg       sync.WaitGroup
}

func (p *ledgerPersistence) failLocked(code string, err error) {
	p.lastCode, p.lastErr = code, err.Error()
}

// attachLedgerStore points persistence at pluginAuthDir/usage and loads the
// saved ledger. Until a load succeeds, every reconfigure retries it and
// nothing is saved; after that, a changed auth directory applies to the
// ledger at the next restart, so the history in the new directory is never
// replaced. Records counted before a late successful load are merged with
// it: they are this process's own, so nothing is counted twice.
func (c *Coordinator) attachLedgerStore(pluginAuthDir string) {
	p := &c.ledgerStore
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready {
		return
	}
	p.dir = filepath.Join(pluginAuthDir, ledgerSubdir)
	store, err := securestore.New(p.dir)
	if err != nil {
		p.failLocked("open_failed", err)
		return
	}
	defer func() { _ = store.Close() }()
	var raw rawState
	found, err := store.ReadJSONIfExists(ledgerStateName, &raw)
	if err != nil {
		p.failLocked("read_failed", err)
		return
	}
	if found {
		if err := c.ledger.UnmarshalState(raw, c.runtimeNow()); err != nil {
			p.failLocked("decode_failed", err)
			return
		}
	}
	p.ready = true
	p.lastCode, p.lastErr = "", ""
}

// rawState lets securestore's JSON reader hand back the ledger's own bytes.
type rawState []byte

func (r *rawState) UnmarshalJSON(data []byte) error {
	*r = append((*r)[:0], data...)
	return nil
}

func (r rawState) MarshalJSON() ([]byte, error) { return r, nil }

// recordUsage attributes record to the module that accepted it and counts
// it in the ledger, then schedules a save when one is due.
func (c *Coordinator) recordUsage(entry moduleEntry, record pluginapi.UsageRecord) {
	account := ""
	if attributor, ok := entry.module.(providermodule.UsageAttributor); ok {
		account = attributor.UsageAccount(record)
	}
	now := c.runtimeNow()
	c.ledger.Record(entry.id, account, record, now)
	c.mu.RLock()
	stopped := c.stopped
	c.mu.RUnlock()
	if stopped {
		// Shutdown already flushed; never start a background write after it.
		return
	}
	c.maybeSaveLedger(now, false)
}

// maybeSaveLedger writes the ledger when it changed and the last save is
// older than ledgerSaveEvery (or force). Saves run in the background and
// never overlap; force (shutdown) saves synchronously.
func (c *Coordinator) maybeSaveLedger(now time.Time, force bool) {
	p := &c.ledgerStore
	p.mu.Lock()
	if !p.ready || p.saving || !c.ledger.Dirty() || (!force && now.Sub(p.savedAt) < ledgerSaveEvery) {
		p.mu.Unlock()
		return
	}
	p.saving = true
	p.savedAt = now
	dir := p.dir
	p.wg.Add(1)
	p.mu.Unlock()

	save := func() {
		defer p.wg.Done()
		err := c.saveLedger(dir, now)
		p.mu.Lock()
		p.saving = false
		if err != nil {
			p.failLocked("write_failed", err)
		} else {
			p.lastCode, p.lastErr = "", ""
		}
		p.mu.Unlock()
	}
	if force {
		save()
		return
	}
	go save()
}

// saveLedger writes the ledger and clears its dirty flag only after the
// write succeeded, so a failed save is retried by the next save or the
// shutdown flush.
func (c *Coordinator) saveLedger(dir string, now time.Time) error {
	raw, generation, err := c.ledger.MarshalStateGeneration(now)
	if err != nil {
		return err
	}
	store, err := securestore.New(dir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := store.WriteJSON(ledgerStateName, rawState(raw)); err != nil {
		return err
	}
	c.ledger.MarkSaved(generation)
	return nil
}

// flushLedger waits for any background save and then saves synchronously.
func (c *Coordinator) flushLedger() {
	c.ledgerStore.wg.Wait()
	c.maybeSaveLedger(c.runtimeNow(), true)
	c.ledgerStore.wg.Wait()
}

func (c *Coordinator) ledgerPersistenceStatus() (persisted bool, code, detail string) {
	p := &c.ledgerStore
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ready && p.lastCode == "", p.lastCode, p.lastErr
}
