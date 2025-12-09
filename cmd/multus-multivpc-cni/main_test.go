package main

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
)

func TestDeriveDelegateName(t *testing.T) {
	tests := []struct {
		name         string
		conf         *pluginConf
		defaultName  string
		expectedName string
	}{
		{
			name:         "uses default when conf nil",
			conf:         nil,
			defaultName:  "default",
			expectedName: "default",
		},
		{
			name:         "falls back to plugin name",
			conf:         nil,
			defaultName:  "",
			expectedName: pluginName,
		},
		{
			name:         "prefers default over conf name",
			conf:         &pluginConf{NetConf: cnitypes.NetConf{Name: "ignored"}},
			defaultName:  "chosen",
			expectedName: "chosen",
		},
		{
			name:         "uses default even when network profile set",
			conf:         &pluginConf{NetConf: cnitypes.NetConf{}, NetworkProfile: "np"},
			defaultName:  "chosen",
			expectedName: "chosen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveDelegateName(tt.conf, tt.defaultName); got != tt.expectedName {
				t.Fatalf("deriveDelegateName() = %q, want %q", got, tt.expectedName)
			}
		})
	}
}

func TestConfigureDelegateSetsDefaults(t *testing.T) {
	conf := &pluginConf{
		NetConf:        cnitypes.NetConf{Name: "cni-main", CNIVersion: "0.3.1"},
		NetworkProfile: "np",
	}

	base := configureDelegate(conf, "master0", map[string]any{
		"type": "ipvlan",
	}, ipvlanDelegateName)
	if base["name"] != ipvlanDelegateName {
		t.Fatalf("expected name to be %s, got %v", ipvlanDelegateName, base["name"])
	}
	if base["master"] != "master0" {
		t.Fatalf("expected master to be master0, got %v", base["master"])
	}
	if base["cniVersion"] != "0.3.1" {
		t.Fatalf("expected cniVersion to be 0.3.1, got %v", base["cniVersion"])
	}

	conf2 := &pluginConf{NetworkProfile: "np2", NetConf: cnitypes.NetConf{CNIVersion: "0.4.0"}}
	post := configureDelegate(conf2, "", map[string]any{"type": "sbr"}, sbrDelegateName)
	if post["type"] != "sbr" {
		t.Fatalf("expected type sbr, got %v", post["type"])
	}
	if post["name"] != sbrDelegateName {
		t.Fatalf("expected default name %s, got %v", sbrDelegateName, post["name"])
	}
	if post["cniVersion"] != "0.4.0" {
		t.Fatalf("expected cniVersion to be 0.4.0, got %v", post["cniVersion"])
	}
}
