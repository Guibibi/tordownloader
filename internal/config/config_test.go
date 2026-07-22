package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadArrInstances(t *testing.T) {
	p := writeConfig(t, `
arr:
  - name: sonarr
    url: "http://sonarr:8989/"
    api_key: "abc"
  - url: "http://radarr:7878"
    api_key: "def"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Arr) != 2 {
		t.Fatalf("expected 2 arr instances, got %d", len(cfg.Arr))
	}
	if cfg.Arr[0].Name != "sonarr" || cfg.Arr[0].APIKey != "abc" {
		t.Errorf("instance 0 = %+v", cfg.Arr[0])
	}
	// Name defaults to the URL when omitted.
	if cfg.Arr[1].Name != "http://radarr:7878" {
		t.Errorf("instance 1 name = %q, want its URL", cfg.Arr[1].Name)
	}
}

func TestLoadArrRequiresKeyAndURL(t *testing.T) {
	for _, body := range []string{
		"arr:\n  - url: \"http://sonarr:8989\"\n",                // missing api_key
		"arr:\n  - api_key: \"abc\"\n",                           // missing url
		"arr:\n  - url: \"sonarr:8989\"\n    api_key: \"abc\"\n", // no scheme
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("expected validation error for:\n%s", body)
		}
	}
}

func TestArrEnvShorthand(t *testing.T) {
	t.Setenv("TD_SONARR_URL", "http://sonarr:8989")
	t.Setenv("TD_SONARR_API_KEY", "envkey")
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Arr) != 1 || cfg.Arr[0].Name != "sonarr" || cfg.Arr[0].APIKey != "envkey" {
		t.Fatalf("arr from env = %+v", cfg.Arr)
	}
}

func TestArrEnvOverridesMatchingURL(t *testing.T) {
	t.Setenv("TD_SONARR_URL", "http://sonarr:8989")
	t.Setenv("TD_SONARR_API_KEY", "newkey")
	p := writeConfig(t, `
arr:
  - name: mysonarr
    url: "http://sonarr:8989/"
    api_key: "oldkey"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Arr) != 1 {
		t.Fatalf("expected env to merge into the file instance, got %d instances", len(cfg.Arr))
	}
	if cfg.Arr[0].APIKey != "newkey" || cfg.Arr[0].Name != "mysonarr" {
		t.Fatalf("merged instance = %+v", cfg.Arr[0])
	}
}
