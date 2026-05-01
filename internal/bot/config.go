package bot

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	BotToken       string          `yaml:"bot_token"`
	EnabledUserIDs []int64         `yaml:"enabled_user_ids"`
	Webhooks       []WebhookConfig `yaml:"webhooks"`
}

type WebhookConfig struct {
	Name           string `yaml:"name"`
	URL            string `yaml:"url"`
	MatchingString string `yaml:"matching_string"`
	Header         string `yaml:"header,omitempty"`
	HeaderValue    string `yaml:"header_value,omitempty"`
	Insecure       bool   `yaml:"insecure,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config yaml: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.BotToken) == "" {
		return fmt.Errorf("bot_token is required")
	}
	if len(c.EnabledUserIDs) == 0 {
		return fmt.Errorf("enabled_user_ids must not be empty")
	}
	if len(c.Webhooks) == 0 {
		return fmt.Errorf("webhooks must not be empty")
	}
	for i, wh := range c.Webhooks {
		if strings.TrimSpace(wh.Name) == "" {
			return fmt.Errorf("webhooks[%d].name is required", i)
		}
		if strings.TrimSpace(wh.URL) == "" {
			return fmt.Errorf("webhooks[%d].url is required", i)
		}
		if strings.TrimSpace(wh.MatchingString) == "" {
			return fmt.Errorf("webhooks[%d].matching_string is required", i)
		}
		if (wh.Header == "") != (wh.HeaderValue == "") {
			return fmt.Errorf("webhooks[%d]: header and header_value must both be set or both omitted", i)
		}
	}
	return nil
}
