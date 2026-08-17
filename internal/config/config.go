package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Project  Project   `toml:"project"`
	Services []Service `toml:"services"`

	// Path is the absolute path of the .pairinrc.toml that was loaded.
	// Populated by Load; not parsed from TOML.
	Path string `toml:"-"`
}

type Project struct {
	Name string `toml:"name"`
}

type Service struct {
	Name         string   `toml:"name"`
	Short        string   `toml:"short"`
	Dir          string   `toml:"dir"`
	Cmd          string   `toml:"cmd"`
	Color        string   `toml:"color"`
	Healthcheck  string   `toml:"healthcheck"`
	// Exposes lists ports pairin cannot discover for itself. Ports are normally
	// read from the kernel by process group, which misses anything bound
	// outside it — a `docker compose up` service has its ports bound by the
	// daemon. Declared ports are shown alongside any that are discovered.
	Exposes      ExposeList `toml:"exposes"`
	DependsOn    []string `toml:"depends_on"`
	Restart      string   `toml:"restart"`       // "no" (default), "always", "on-failure", "on-success"
	RestartDelay string   `toml:"restart_delay"`  // duration string, e.g. "5s" (default: "3s")
	MaxRestarts  int      `toml:"max_restarts"`   // 0 = unlimited
}

// Exposed is one declared port, optionally labelled. A service fronting
// several things — a docker compose stack, say — wants to say which port is
// which, since a bare number tells you nothing about what answers on it.
type Exposed struct {
	Label string
	Port  int
}

// ExposeList is a service's declared ports.
type ExposeList []Exposed

// UnmarshalTOML accepts the several shapes a port list reasonably takes:
//
//	exposes = [5432, 9000]                  bare ports
//	exposes = ["db:5432", "redis:6379"]     labelled
//	exposes = [["db", 5432], ["redis", 6379]]
//	exposes = [{label = "db", port = 5432}]
//
// The bare form is accepted because it shipped first, and a config that worked
// yesterday has to keep working.
func (l *ExposeList) UnmarshalTOML(data any) error {
	items, ok := data.([]any)
	if !ok {
		return fmt.Errorf("exposes must be a list, got %T", data)
	}

	out := make(ExposeList, 0, len(items))
	for _, item := range items {
		e, err := parseExposed(item)
		if err != nil {
			return err
		}
		out = append(out, e)
	}
	*l = out
	return nil
}

func parseExposed(item any) (Exposed, error) {
	switch v := item.(type) {
	case int64:
		return Exposed{Port: int(v)}, nil
	case string:
		return parseExposedString(v)

	case []any:
		// ["label", port], in either order — the label is the string half.
		if len(v) != 2 {
			return Exposed{}, fmt.Errorf("exposes entry %v must be [label, port]", v)
		}
		var e Exposed
		for _, part := range v {
			switch p := part.(type) {
			case string:
				e.Label = p
			case int64:
				e.Port = int(p)
			default:
				return Exposed{}, fmt.Errorf("exposes entry %v must be a label and a port", v)
			}
		}
		if e.Port == 0 {
			return Exposed{}, fmt.Errorf("exposes entry %v has no port", v)
		}
		return e, nil

	case map[string]any:
		var e Exposed
		if label, ok := v["label"].(string); ok {
			e.Label = label
		}
		if port, ok := v["port"].(int64); ok {
			e.Port = int(port)
		}
		if e.Port == 0 {
			return Exposed{}, fmt.Errorf("exposes entry %v has no port", v)
		}
		return e, nil

	default:
		return Exposed{}, fmt.Errorf("exposes entry %v (%T) must be a port, \"label:port\", or [label, port]", item, item)
	}
}

// parseExposedString reads "5432" or "db:5432".
func parseExposedString(s string) (Exposed, error) {
	s = strings.TrimSpace(s)
	label, num := "", s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		label, num = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	port, err := strconv.Atoi(num)
	if err != nil {
		return Exposed{}, fmt.Errorf("exposes entry %q is not a port or \"label:port\"", s)
	}
	return Exposed{Label: label, Port: port}, nil
}

// ParsedRestartDelay returns the restart delay as a time.Duration.
// Defaults to 3s if not set or invalid.
func (s *Service) ParsedRestartDelay() time.Duration {
	if s.RestartDelay == "" {
		return 3 * time.Second
	}
	d, err := time.ParseDuration(s.RestartDelay)
	if err != nil {
		return 3 * time.Second
	}
	return d
}

// RestartPolicy returns the normalized restart policy.
// Defaults to "no" if not set.
func (s *Service) RestartPolicy() string {
	if s.Restart == "" {
		return "no"
	}
	return s.Restart
}

var validRestartPolicies = map[string]bool{
	"no":         true,
	"always":     true,
	"on-failure": true,
	"on-success": true,
}

const configFileName = ".pairinrc.toml"

func Load() (*Config, error) {
	path, err := findConfig()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom loads a specific .pairinrc.toml by absolute path. The supervisor
// uses this because it's passed the path explicitly (the supervisor's cwd
// isn't guaranteed to be the project dir).
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.Path = path

	if len(cfg.Services) == 0 {
		return nil, fmt.Errorf("no services defined in %s", path)
	}

	// Resolve service dirs relative to config file location
	configDir := filepath.Dir(path)
	for i := range cfg.Services {
		if !filepath.IsAbs(cfg.Services[i].Dir) {
			cfg.Services[i].Dir = filepath.Join(configDir, cfg.Services[i].Dir)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks that dependency references are valid and acyclic.
func (cfg *Config) Validate() error {
	nameSet := make(map[string]int, len(cfg.Services))
	for i, svc := range cfg.Services {
		nameSet[svc.Name] = i
	}

	for _, svc := range cfg.Services {
		for _, dep := range svc.DependsOn {
			depIdx, exists := nameSet[dep]
			if !exists {
				return fmt.Errorf("service %q depends on %q, which does not exist", svc.Name, dep)
			}
			if cfg.Services[depIdx].Healthcheck == "" {
				return fmt.Errorf("service %q depends on %q, but %q has no healthcheck", svc.Name, dep, dep)
			}
		}

		// Validate restart policy
		if svc.Restart != "" && !validRestartPolicies[svc.Restart] {
			return fmt.Errorf("service %q has invalid restart policy %q (must be no, always, on-failure, or on-success)", svc.Name, svc.Restart)
		}

		// Validate restart_delay is parseable
		if svc.RestartDelay != "" {
			if _, err := time.ParseDuration(svc.RestartDelay); err != nil {
				return fmt.Errorf("service %q has invalid restart_delay %q: %w", svc.Name, svc.RestartDelay, err)
			}
		}

		// Validate max_restarts is non-negative
		if svc.MaxRestarts < 0 {
			return fmt.Errorf("service %q has negative max_restarts %d", svc.Name, svc.MaxRestarts)
		}

		for _, e := range svc.Exposes {
			if e.Port < 1 || e.Port > 65535 {
				return fmt.Errorf("service %q exposes port %d, which is not a valid TCP port", svc.Name, e.Port)
			}
		}
	}

	// Detect circular dependencies using Kahn's algorithm
	if len(cfg.Services) > 0 {
		inDegree := make(map[string]int, len(cfg.Services))
		adj := make(map[string][]string, len(cfg.Services))
		for _, svc := range cfg.Services {
			if _, ok := inDegree[svc.Name]; !ok {
				inDegree[svc.Name] = 0
			}
			for _, dep := range svc.DependsOn {
				adj[dep] = append(adj[dep], svc.Name)
				inDegree[svc.Name]++
			}
		}

		queue := make([]string, 0)
		for name, deg := range inDegree {
			if deg == 0 {
				queue = append(queue, name)
			}
		}

		visited := 0
		for len(queue) > 0 {
			node := queue[0]
			queue = queue[1:]
			visited++
			for _, next := range adj[node] {
				inDegree[next]--
				if inDegree[next] == 0 {
					queue = append(queue, next)
				}
			}
		}

		if visited != len(cfg.Services) {
			return fmt.Errorf("circular dependency detected among services")
		}
	}

	return nil
}

func findConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		path := filepath.Join(dir, configFileName)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("%s not found (searched from current directory to root)", configFileName)
}
