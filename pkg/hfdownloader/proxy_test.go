// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"strings"
	"testing"
)

func TestGetProxyInfoString_RedactsPasswords(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
	tests := []struct {
		name string
		cfg  *ProxyConfig
		want string
	}{
		{"no proxy", nil, "None (direct connection)"},
		{"plain", &ProxyConfig{URL: "http://proxy:8080"}, "http://proxy:8080"},
		{"password in URL", &ProxyConfig{URL: "http://alice:s3cret@proxy:8080"}, "http://alice:xxxxx@proxy:8080"},
		{"separate credentials", &ProxyConfig{URL: "socks5://proxy:1080", Username: "alice", Password: "s3cret"}, "socks5://alice:xxxxx@proxy:1080"},
		{"username only", &ProxyConfig{URL: "http://proxy:8080", Username: "alice"}, "http://alice@proxy:8080"},
		{"unparsable", &ProxyConfig{URL: "http://alice:s3cret@proxy:80%zz"}, "(set; not shown because it may contain credentials)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetProxyInfoString(tt.cfg)
			if got != tt.want {
				t.Errorf("GetProxyInfoString() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "s3cret") {
				t.Errorf("password leaked: %q", got)
			}
		})
	}
}

func TestGetProxyInfoString_RedactsEnvironmentProxy(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", "http://bob:hunter2@corp-proxy:3128")
	got := GetProxyInfoString(nil)
	if want := "Environment: http://bob:xxxxx@corp-proxy:3128"; got != want {
		t.Errorf("GetProxyInfoString(nil) = %q, want %q", got, want)
	}
}
