package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDerivedDefaultsMemoryExtractorInheritsChatModel(t *testing.T) {
	cfg := Config{DeepSeek: LLMConfig{Model: "deepseek/deepseek-v4-flash"}}

	cfg.resolveDerivedDefaults()

	if cfg.Memory.ExtractorModel != cfg.DeepSeek.Model {
		t.Fatalf("extractor model = %q, want %q", cfg.Memory.ExtractorModel, cfg.DeepSeek.Model)
	}
}

func TestResolveDerivedDefaultsKeepsExplicitMemoryExtractorModel(t *testing.T) {
	cfg := Config{
		DeepSeek: LLMConfig{Model: "deepseek/deepseek-v4-flash"},
		Memory:   MemoryConfig{ExtractorModel: "memory-model"},
	}

	cfg.resolveDerivedDefaults()

	if cfg.Memory.ExtractorModel != "memory-model" {
		t.Fatalf("extractor model = %q, want explicit override", cfg.Memory.ExtractorModel)
	}
}

func TestLoadVoiceRealtimeDefaultsAndEnvironment(t *testing.T) {
	configPath := writeConfigFile(t, "voice:\n  realtime:\n    enabled: true\n")
	t.Setenv("VOICE_REALTIME_API_KEY", "test-realtime-key")
	t.Setenv("VOICE_REALTIME_WORKSPACE_ID", "workspace-env")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	realtime := cfg.Voice.Realtime
	if !realtime.Enabled || realtime.Provider != "dashscope_paraformer" {
		t.Fatalf("realtime provider config = %#v", realtime)
	}
	if realtime.APIKey != "test-realtime-key" || realtime.WorkspaceID != "workspace-env" {
		t.Fatalf("realtime credentials were not read from environment")
	}
	if realtime.Model != "paraformer-realtime-v2" || realtime.SampleRate != 16000 || realtime.FrameMS != 100 {
		t.Fatalf("realtime protocol defaults = %#v", realtime)
	}
	if realtime.StartTimeoutSeconds != 8 || realtime.FinishTimeoutSeconds != 8 {
		t.Fatalf("realtime timeout defaults = %#v", realtime)
	}
}

func TestLoadVoiceRealtimeAPIKeyCannotComeFromYAML(t *testing.T) {
	configPath := writeConfigFile(t, "voice:\n  realtime:\n    api_key: yaml-secret-must-be-ignored\n")
	t.Setenv("VOICE_REALTIME_API_KEY", "")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Voice.Realtime.APIKey != "" {
		t.Fatalf("API key loaded from YAML: %q", cfg.Voice.Realtime.APIKey)
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
