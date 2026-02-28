package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Project  Project   `toml:"project"`
	Services []Service `toml:"services"`
}

type Project struct {
	Name string `toml:"name"`
}

type Service struct {
	Name        string   `toml:"name"`
	Short       string   `toml:"short"`
	Dir         string   `toml:"dir"`
	Cmd         string   `toml:"cmd"`
	Color       string   `toml:"color"`
	Healthcheck string   `toml:"healthcheck"`
	DependsOn   []string `toml:"depends_on"`
}

const configFileName = ".pairinrc.toml"

func Load() (*Config, error) {
	path, err := findConfig()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

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
