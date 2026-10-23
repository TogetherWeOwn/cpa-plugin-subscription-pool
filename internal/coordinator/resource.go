package coordinator

import (
	"strings"
	"time"
)

// isResourceStatusPath reports whether path is this plugin's usage page
// resource route, either as the module-relative path a ManagementCapable
// module would see ("/status") or as the host-facing path under this
// plugin's own resourcePluginBasePath. Resource routes are dispatched
// without authentication (the host's resource-route convention), so what
// they serve must never embed the management key or any other credential:
// only the redacted per-account fields each module already exposes.
func isResourceStatusPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	return path == resourceStatusPath || path == resourcePluginBasePath+resourceStatusPath
}

// zaiResourceAccount is the subset of the zai module's status the usage
// page reads.
type zaiResourceAccount struct {
	Name                string     `json:"name"`
	FiveHourUtilization float64    `json:"five_hour_utilization"`
	WeeklyUtilization   float64    `json:"weekly_utilization"`
	FiveHourResetsAt    *time.Time `json:"five_hour_resets_at"`
	WeeklyResetsAt      *time.Time `json:"weekly_resets_at"`
	QuotaSource         string     `json:"quota_source"`
	QuotaStale          bool       `json:"quota_stale"`
	Health              string     `json:"health"`
}

// opencodeResourceWindow is the window shape shared by the OpenCode Go and
// synthetic modules' status.
type opencodeResourceWindow struct {
	Known         bool       `json:"known"`
	Utilization   *float64   `json:"utilization"`
	Exhausted     bool       `json:"exhausted"`
	ResetAt       *time.Time `json:"resets_at"`
	Source        string     `json:"source"`
	Authoritative bool       `json:"authoritative"`
}

type opencodeResourceAccount struct {
	Name     string                            `json:"name"`
	Disabled bool                              `json:"disabled"`
	Windows  map[string]opencodeResourceWindow `json:"windows"`
}

type opencodeResourceStatus struct {
	Accounts []opencodeResourceAccount `json:"accounts"`
}

func opencodeSourceLabel(w opencodeResourceWindow) string {
	switch {
	case w.Authoritative:
		return "authoritative"
	case strings.HasPrefix(w.Source, "proxy-observed"):
		return "429-derived"
	case w.Source == "":
		return "unknown"
	default:
		return w.Source
	}
}

func clampPercent(percent float64) float64 {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}
