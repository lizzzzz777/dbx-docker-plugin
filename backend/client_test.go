package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestIsWindowsPipePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"//./pipe/docker_engine", true},
		{`\\.\pipe\docker_engine`, true},
		{`\\.\pipe\`, true},
		{"/var/run/docker.sock", false},
		{"var/run/docker.sock", false},
		{"npipe:////./pipe/docker_engine", false},
	}
	for _, c := range cases {
		if got := isWindowsPipePath(c.in); got != c.want {
			t.Errorf("isWindowsPipePath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWindowsPipeURLPath(t *testing.T) {
	if got := windowsPipeURLPath(`\\.\pipe\docker_engine`); got != "//./pipe/docker_engine" {
		t.Fatalf("windowsPipeURLPath = %q", got)
	}
}

func TestLocalSocketDefault(t *testing.T) {
	got := localSocketDefault()
	if runtime.GOOS == "windows" {
		if !isWindowsPipePath(got) {
			t.Fatalf("windows default %q is not a pipe path", got)
		}
	} else if got != "/var/run/docker.sock" {
		t.Fatalf("default = %q, want /var/run/docker.sock", got)
	}
}

func TestValidateConfigSocketPath(t *testing.T) {
	base := connectionPayload{}
	cases := []struct {
		name    string
		cfg     pluginConfig
		wantErr string
	}{
		{"unix empty socket auto", pluginConfig{Protocol: "unix"}, ""},
		{"unix linux socket", pluginConfig{Protocol: "unix", SocketPath: "/var/run/docker.sock"}, ""},
		{"unix windows pipe", pluginConfig{Protocol: "unix", SocketPath: `\\.\pipe\docker_engine`}, ""},
		{"unix relative path", pluginConfig{Protocol: "unix", SocketPath: "var/run/docker.sock"}, "Invalid Docker socket path"},
		{"unix nul byte", pluginConfig{Protocol: "unix", SocketPath: "/var/run/docker.sock\x00"}, "Invalid Docker socket path"},
		{"nc empty socket ok", pluginConfig{Protocol: "unix-over-nc", SSHHost: "h"}, ""},
		{"nc needs ssh host", pluginConfig{Protocol: "unix-over-nc"}, "SSH host"},
	}
	for _, c := range cases {
		err := validateConfig(base, c.cfg, runtimeEndpoint{})
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
			}
		}
	}
}
