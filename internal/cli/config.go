package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	ServerURL string `json:"server_url"`
	AgentID   string `json:"agent_id"`
	APIKey    string `json:"api_key"`
}

func ConfigPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("GTH_CONFIG")); override != "" {
		return override, nil
	}
	if override := strings.TrimSpace(os.Getenv("GITTHICKET_CONFIG")); override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gitthicket", "config.json"), nil
}

func Load() (Config, string, error) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, "", err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, path, fmt.Errorf("config not found; run `gth join` first")
		}
		return Config{}, path, err
	}

	var cfg Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return Config{}, path, err
	}
	if strings.TrimSpace(cfg.ServerURL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return Config{}, path, fmt.Errorf("config %s is incomplete", path)
	}
	return cfg, path, nil
}

func Save(cfg Config) (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
