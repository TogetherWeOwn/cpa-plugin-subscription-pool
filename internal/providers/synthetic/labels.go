package synthetic

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

// Lane names are public: the usage page (an unauthenticated resource route)
// shows them and the usage ledger keys its history by them. So a name is
// the operator-configured provider name plus an ordinal ("synthetic#1",
// "synthetic-infinite#2"), never anything computed from a credential: an
// auth ID or key hash would let anyone who can guess a key confirm it, and
// correlate installations.
//
// Ordinals stick to their credential. The auth ID -> ordinal assignment is
// kept server-side in <auth-dir>/subscription-pool/providers/synthetic/
// lane-labels.state (0600, through securestore), so reordering api-key
// entries, before or across a restart, never moves a name, or the history
// keyed by it, to another key. A new credential takes its provider's lowest
// free ordinal, so a rotated key inherits the ordinal of the key it
// replaced. Learned lanes (credentials seen at runtime but absent from the
// last reconfigure) are "<provider>#learned-<n>" for the process lifetime.
const (
	laneLabelsName    = "lane-labels.state"
	laneLabelsVersion = 1
	maxLaneOrdinal    = 1 << 16
)

// laneLabel is one credential's ordinal within its provider.
type laneLabel struct {
	Provider string `json:"provider"`
	Ordinal  int    `json:"ordinal"`
}

func (l laneLabel) name() string {
	return l.Provider + "#" + strconv.Itoa(l.Ordinal)
}

// laneLabelsFile is the on-disk shape. Keys are auth IDs: credential
// derived, so they stay on the server and never reach a public payload.
type laneLabelsFile struct {
	Version int                  `json:"version"`
	Lanes   map[string]laneLabel `json:"lanes"`
}

// assignLaneNames names every lane in lanes and returns the assignment.
// order lists the auth IDs in config order. Credentials in previous keep
// their ordinal; the rest take their provider's lowest free ordinal in
// config order.
func assignLaneNames(order []string, lanes map[string]*lane, previous map[string]laneLabel) map[string]laneLabel {
	next := make(map[string]laneLabel, len(order))
	used := make(map[string]map[int]bool)
	take := func(id string, label laneLabel) {
		if used[label.Provider] == nil {
			used[label.Provider] = make(map[int]bool)
		}
		used[label.Provider][label.Ordinal] = true
		next[id] = label
		lanes[id].Name, lanes[id].ordinal = label.name(), label.Ordinal
	}
	for _, id := range order {
		item := lanes[id]
		label, known := previous[id]
		if known && label.Provider == item.Provider && label.Ordinal >= 1 && label.Ordinal <= maxLaneOrdinal && !used[item.Provider][label.Ordinal] {
			take(id, label)
		}
	}
	for _, id := range order {
		if _, named := next[id]; named {
			continue
		}
		provider := lanes[id].Provider
		ordinal := 1
		for used[provider][ordinal] {
			ordinal++
		}
		take(id, laneLabel{Provider: provider, Ordinal: ordinal})
	}
	return next
}

func sameLabels(a, b map[string]laneLabel) bool {
	if len(a) != len(b) {
		return false
	}
	for id, label := range a {
		if other, ok := b[id]; !ok || other != label {
			return false
		}
	}
	return true
}

func laneLabelsDir(authDir string) string {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return ""
	}
	return filepath.Join(authDir, "providers", providerID)
}

// loadLaneLabels reads the saved assignment. A missing file is an empty
// assignment; any failure is soft and reported on the authenticated status.
func loadLaneLabels(dir string) (map[string]laneLabel, error) {
	store, err := securestore.New(dir)
	if err != nil {
		return nil, fmt.Errorf("open lane labels: %w", err)
	}
	defer func() { _ = store.Close() }()
	var file laneLabelsFile
	found, err := store.ReadJSONIfExists(laneLabelsName, &file)
	if err != nil {
		return nil, fmt.Errorf("read lane labels: %w", err)
	}
	if !found {
		return map[string]laneLabel{}, nil
	}
	if file.Version != laneLabelsVersion {
		return nil, fmt.Errorf("read lane labels: unsupported version %d", file.Version)
	}
	labels := make(map[string]laneLabel, len(file.Lanes))
	for id, label := range file.Lanes {
		if strings.TrimSpace(id) != "" && label.Provider != "" && label.Ordinal >= 1 && label.Ordinal <= maxLaneOrdinal {
			labels[id] = label
		}
	}
	return labels, nil
}

func saveLaneLabels(dir string, labels map[string]laneLabel) error {
	store, err := securestore.New(dir)
	if err != nil {
		return fmt.Errorf("open lane labels: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.WriteJSON(laneLabelsName, laneLabelsFile{Version: laneLabelsVersion, Lanes: labels}); err != nil {
		return fmt.Errorf("write lane labels: %w", err)
	}
	return nil
}

// nameLanesLocked assigns lane names for a Reconfigure and persists the
// assignment when it changed. The caller holds m.labelsMu (not m.mu). It
// returns the error to show on the authenticated status, or "".
func (m *Module) nameLanesLocked(authDir string, order []string, lanes map[string]*lane) string {
	if m.labels == nil {
		m.labels = make(map[string]laneLabel)
	}
	dir := laneLabelsDir(authDir)
	if dir != m.labelsDir {
		m.labelsDir, m.labelsLoaded, m.labelsSaved, m.labelsLoadErr = dir, false, false, ""
	}
	if dir != "" && !m.labelsLoaded {
		m.labelsLoaded = true
		saved, err := loadLaneLabels(dir)
		if err != nil {
			// Names fall back to config order and the next save replaces
			// the unreadable file; the failure stays visible meanwhile.
			m.labelsLoadErr = bounded(err.Error(), 256)
		} else {
			// Merge, so credentials named before a store came into view
			// keep their names where the saved file has no claim.
			for id, label := range saved {
				m.labels[id] = label
			}
			m.labelsSaved = true
		}
	}
	next := assignLaneNames(order, lanes, m.labels)
	changed := !sameLabels(next, m.labels)
	m.labels = next
	if dir == "" {
		return ""
	}
	if changed || !m.labelsSaved {
		if err := saveLaneLabels(dir, next); err != nil {
			m.labelsSaved = false
			return bounded(err.Error(), 256)
		}
		m.labelsSaved = true
	}
	return m.labelsLoadErr
}
