package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestArrEnvPartialPairWarns(t *testing.T) {
	t.Setenv("TD_RADARR_URL", "http://radarr:7878")
	t.Setenv("TD_RADARR_API_KEY", "")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Arr) != 0 {
		t.Fatalf("half-set pair must not register an instance, got %+v", cfg.Arr)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %v", cfg.Warnings)
	}
	if want := "TD_RADARR_API_KEY"; !strings.Contains(cfg.Warnings[0], want) {
		t.Errorf("warning %q should name the missing var %s", cfg.Warnings[0], want)
	}
}

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in   string
		want ByteSize
	}{
		{"1024", 1024},
		{"0", 0},
		{"50KB", 50 * KiB},
		{"50kb", 50 * KiB},
		{"50KiB", 50 * KiB},
		{"1.5MB", ByteSize(1.5 * float64(MiB))},
		{"2G", 2 * GiB},
		{" 4MiB ", 4 * MiB},
		{"900B", 900},
	}
	for _, c := range cases {
		got, err := ParseByteSize(c.in)
		if err != nil {
			t.Errorf("ParseByteSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "fast", "-5MB", "MB"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) = nil error, want a rejection", bad)
		}
	}
}

func TestLoadByteSizeFromYAML(t *testing.T) {
	// Both spellings must land on the same byte count: a plain integer for
	// someone scripting the file, a suffixed string for someone reading it.
	p := writeConfig(t, "failure:\n  min_speed: \"250KB\"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Failure.MinSpeed != 250*KiB {
		t.Errorf("min_speed = %d, want %d", cfg.Failure.MinSpeed, 250*KiB)
	}

	p = writeConfig(t, "failure:\n  min_speed: 256000\n")
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Failure.MinSpeed != 256000 {
		t.Errorf("min_speed = %d, want 256000", cfg.Failure.MinSpeed)
	}

	// An explicit 0 must survive as "no floor" rather than being re-defaulted.
	p = writeConfig(t, "failure:\n  min_speed: 0\n")
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Failure.MinSpeed != 0 {
		t.Errorf("min_speed = %d, want 0 (check disabled)", cfg.Failure.MinSpeed)
	}
}
