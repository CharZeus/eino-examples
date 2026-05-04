package config

import (
	"log"
	"os"

	"gopkg.in/yaml.v3"
)

type Claude struct {
	BaseURL   string `yaml:"BaseURL"`
	APIKey    string `yaml:"APIKey"`
	AuthToken string `yaml:"AuthToken"`
	Model     string `yaml:"Model"`
	MaxTokens int    `yaml:"MaxTokens"`
}

type Config struct {
	Claude Claude `yaml:"Claude"`
}

func InitConfig(path string) *Config {
	var cfg Config
	content, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("init config err: %v", err)
	}
	err = yaml.Unmarshal(content, &cfg)
	if err != nil {
		log.Fatalf("Unmarshal config err: %v", err)
	}
	log.Println("init config success")
	return &cfg
}
