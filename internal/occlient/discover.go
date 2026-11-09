package occlient

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ServiceInfo mirrors OpenCode's background service registration.
type ServiceInfo struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	URL      string `json:"url"`
	Password string `json:"password"`
	PID      int    `json:"pid"`
}

// DiscoverService looks for a local OpenCode background service registration.
// The state directory holds the URL and password; the config directory may hold
// the password on its own. Environment overrides win.
func DiscoverService() (ServiceInfo, bool) {
	var out ServiceInfo
	seen := map[string]struct{}{}

	paths := make([]string, 0, 4)
	if p := os.Getenv("OPENCODE_SERVICE_FILE"); p != "" {
		paths = append(paths, p)
	}
	paths = append(paths, xdgPath("XDG_STATE_HOME", ".local/state", "opencode", "service.json"))
	paths = append(paths, xdgPath("XDG_CONFIG_HOME", ".config", "opencode", "service.json"))

	found := false
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var svc ServiceInfo
		if err := json.Unmarshal(data, &svc); err != nil {
			continue
		}
		found = true
		if out.URL == "" && svc.URL != "" {
			out.URL = svc.URL
		}
		if out.Password == "" && svc.Password != "" {
			out.Password = svc.Password
		}
		if out.ID == "" {
			out.ID = svc.ID
		}
		if out.Version == "" {
			out.Version = svc.Version
		}
		if out.PID == 0 {
			out.PID = svc.PID
		}
	}
	if !found || out.URL == "" {
		return ServiceInfo{}, false
	}
	return out, true
}

func xdgPath(env, fallbackDir, elem1, elem2 string) string {
	base := os.Getenv(env)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, filepath.FromSlash(fallbackDir))
	}
	return filepath.Join(base, elem1, elem2)
}
