package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFleetConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFleetDisabledByDefaultKeepsDefaults(t *testing.T) {
	cfg, err := Load(writeFleetConfig(t, "server:\n  mode: debug\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fleet.Enabled {
		t.Fatal("fleet must default to disabled")
	}
	if cfg.Fleet.Workers != 8 || cfg.Fleet.ProbeConcurrency != 32 || cfg.Fleet.Probe.MinPerfRatio != 0.7 || cfg.AccessGW.PublicPort != 2222 {
		t.Fatalf("defaults not applied: %+v %+v", cfg.Fleet, cfg.AccessGW)
	}
}

func TestFleetEnabledListsEveryMissingKey(t *testing.T) {
	_, err := Load(writeFleetConfig(t, "server:\n  mode: debug\nfleet:\n  enabled: true\n"))
	if err == nil {
		t.Fatal("enabled fleet without required keys must fail to load")
	}
	for _, key := range []string{"fleet.controller_egress_ips", "fleet.base_images", "fleet.probe_image",
		"fleet.min_driver_version", "fleet.selftest_key_file", "security.credential_key", "accessgw.public_host",
		"accessgw.internal_listen", "accessgw.egress_ips", "accessgw.upstream_pubkey", "accessgw.hmac_secret",
		"accessgw.host_key_fingerprint"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("missing key %s not reported: %v", key, err)
		}
	}
}

const validFleetYAML = `
server:
  mode: debug
security:
  credential_key: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
fleet:
  enabled: true
  controller_egress_ips: ["123.56.177.241"]
  base_images:
    - name: pytorch-2.4-cuda12
      ref: registry.example.com/omnis/pytorch@sha256:%s
  probe_image: registry.example.com/omnis/probe@sha256:%s
  min_driver_version: "535.104"
  selftest_key_file: /etc/omnis/selftest_ed25519
accessgw:
  public_host: gw.example.com
  internal_listen: 10.0.0.2:9443
  egress_ips: 10.0.0.3, 47.0.0.3
  upstream_pubkey: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample gw
  hmac_secret: 0123456789abcdef0123456789abcdef
  host_key_fingerprint: SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s
`

func TestFleetEnabledValidConfigLoads(t *testing.T) {
	digest := strings.Repeat("a", 64)
	body := strings.ReplaceAll(validFleetYAML, "%s", digest)
	cfg, err := Load(writeFleetConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AccessGW.EgressIPs) != 2 || cfg.Fleet.BaseImages[0].Name != "pytorch-2.4-cuda12" {
		t.Fatalf("list parsing: %+v %+v", cfg.AccessGW, cfg.Fleet.BaseImages)
	}
}

func TestFleetRejectsTagOnlyImagesAndShortSecret(t *testing.T) {
	body := strings.ReplaceAll(validFleetYAML, "@sha256:%s", ":latest")
	body = strings.Replace(body, "hmac_secret: 0123456789abcdef0123456789abcdef", "hmac_secret: short", 1)
	_, err := Load(writeFleetConfig(t, body))
	if err == nil {
		t.Fatal("tag-only images must be rejected")
	}
	for _, want := range []string{"fleet.base_images[0]", "fleet.probe_image", "accessgw.hmac_secret 至少 32 字节"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
}

func TestFleetDenyCIDRs(t *testing.T) {
	digest := strings.Repeat("a", 64)
	base := strings.ReplaceAll(validFleetYAML, "%s", digest)
	ok := strings.Replace(base, "  selftest_key_file:", "  deny_cidrs: [\"47.236.0.0/16\", \"8.211.182.3\"]\n  selftest_key_file:", 1)
	cfg, err := Load(writeFleetConfig(t, ok))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Fleet.DenyCIDRs) != 2 || cfg.Fleet.DenyCIDRs[1] != "8.211.182.3" {
		t.Fatalf("deny_cidrs parsing: %+v", cfg.Fleet.DenyCIDRs)
	}
	bad := strings.Replace(base, "  selftest_key_file:", "  deny_cidrs: [\"47.236.0.0/99\"]\n  selftest_key_file:", 1)
	_, err = Load(writeFleetConfig(t, bad))
	if err == nil || !strings.Contains(err.Error(), "fleet.deny_cidrs 含非法 CIDR/IP: 47.236.0.0/99") {
		t.Fatalf("invalid deny_cidrs must be rejected: %v", err)
	}
}
