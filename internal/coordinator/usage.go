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
	mu      sync.Mutex
	dir     string
	loaded  bool
	saving  bool
	savedAt time.Time
	lastErr string
	wg      sync.WaitGroup
}

// attachLedgerStore points persistence at pluginAuthDir/usage and loads the
// saved ledger the first time a directory is known.
func (c *Coordinator) attachLedgerStore(pluginAuthDir string) {
	p := &c.ledgerStore
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dir = filepath.Join(pluginAuthDir, ledgerSubdir)
	if p.loaded {
		return
	}
	p.loaded = true
	store, err := securestore.New(p.dir)
	if err != nil {
		p.lastErr = "open: " + err.Error()
		return
	}
	defer func() { _ = store.Close() }()
	var raw rawState
	found, err := store.ReadJSONIfExists(ledgerStateName, &raw)
	if err != nil {
		p.lastErr = "read: " + err.Error()
		return
	}
	if !found {
		return
	}
	if err := c.ledger.UnmarshalState(raw, c.runtimeNow()); err != nil {
		p.lastErr = "decode: " + err.Error()
	}
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
	if p.dir == "" || p.saving || !c.ledger.Dirty() || (!force && now.Sub(p.savedAt) < ledgerSaveEvery) {
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
			p.lastErr = "write: " + err.Error()
		} else {
			p.lastErr = ""
		}
		p.mu.Unlock()
	}
	if force {
		save()
		return
	}
	go save()
}

func (c *Coordinator) saveLedger(dir string, now time.Time) error {
	raw, err := c.ledger.MarshalState(now)
	if err != nil {
		return err
	}
	store, err := securestore.New(dir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return store.WriteJSON(ledgerStateName, rawState(raw))
}

// flushLedger waits for any background save and then saves synchronously.
func (c *Coordinator) flushLedger() {
	c.ledgerStore.wg.Wait()
	c.maybeSaveLedger(c.runtimeNow(), true)
	c.ledgerStore.wg.Wait()
}

func (c *Coordinator) ledgerPersistenceStatus() (persisted bool, lastErr string) {
	p := &c.ledgerStore
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dir != "" && p.lastErr == "", p.lastErr
}
