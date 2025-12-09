package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/soracom/multus-multivpc-cni/pkg/config"
)

func TestLoadEmptyPathReturnsDefault(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.NetworkProfiles) != 0 {
		t.Fatalf("expected empty network profiles, got %d", len(cfg.NetworkProfiles))
	}
}

func TestLoadParsesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.yaml")
	yaml := []byte(`networkProfiles:
  profile-a:
    subnetId: subnet-123
    securityGroupIds:
      - sg-1
    description: test profile
    preferredEniCount: 1
`)
	if err := os.WriteFile(path, yaml, 0o600); err != nil {
		t.Fatalf("failed writing temp file: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	profile, ok := cfg.ResolveProfile("profile-a")
	if !ok {
		t.Fatalf("expected profile-a to be present")
	}
	if profile.SubnetID != "subnet-123" {
		t.Fatalf("unexpected subnet id: %s", profile.SubnetID)
	}
	if len(profile.SecurityGroupIDs) != 1 || profile.SecurityGroupIDs[0] != "sg-1" {
		t.Fatalf("unexpected security groups: %#v", profile.SecurityGroupIDs)
	}
	if profile.PreferredENICount != 1 {
		t.Fatalf("unexpected preferred ENI count: %d", profile.PreferredENICount)
	}
}

func TestLoadReturnsErrorForMissingFile(t *testing.T) {
	_, err := config.Load("/nonexistent/path.yaml")
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestResolveProfileMissing(t *testing.T) {
	cfg := &config.Config{NetworkProfiles: map[string]config.NetworkProfile{}}
	if _, ok := cfg.ResolveProfile("missing"); ok {
		t.Fatalf("expected resolve of missing profile to return false")
	}
}
