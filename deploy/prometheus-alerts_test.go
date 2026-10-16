package deploy_test

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPromAlerts_ValidYAML(t *testing.T) {
	raw, err := os.ReadFile("prometheus-alerts.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(string(raw), "alert: AIProbeSlowDuration") {
		t.Fatalf("expected AIProbeSlowDuration alert")
	}
}

func TestDockerfile_Builds(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"FROM golang:1.25 AS builder", "FROM scratch", "CGO_ENABLED=0"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
}
