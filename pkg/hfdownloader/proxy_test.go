// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"strings"
	"testing"

	"github.com/reuc/HuggingFaceModelDownloader/internal/testutil"
)

// clearProxyEnv empties the proxy variables GetProxyInfoString falls back to.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
}

func TestGetProxyInfoString_RedactsPasswords(t *testing.T) {
	clearProxyEnv(t)
	user, pass := testutil.Secret("u"), testutil.Secret("")
	creds := user + ":" + pass + "@"
	tests := []struct {
		name string
		cfg  *ProxyConfig
		want string
	}{
		{"no proxy", nil, "None (direct connection)"},
		{"plain", &ProxyConfig{URL: "http://proxy:8080"}, "http://proxy:8080"},
		{"password in URL", &ProxyConfig{URL: "http://" + creds + "proxy:8080"}, "http://" + user + ":xxxxx@proxy:8080"},
		{"separate credentials", &ProxyConfig{URL: "socks5://proxy:1080", Username: user, Password: pass}, "socks5://" + user + ":xxxxx@proxy:1080"},
		{"username only", &ProxyConfig{URL: "http://proxy:8080", Username: user}, "http://" + user + "@proxy:8080"},
		{"unparsable", &ProxyConfig{URL: "http://" + creds + "proxy:80%zz"}, "(set; not shown because it may contain credentials)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetProxyInfoString(tt.cfg)
			if got != tt.want {
				t.Errorf("GetProxyInfoString() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, pass) {
				t.Errorf("password leaked: %q", got)
			}
		})
	}
}

func TestGetProxyInfoString_RedactsEnvironmentProxy(t *testing.T) {
	clearProxyEnv(t)
	user, pass := testutil.Secret("u"), testutil.Secret("")
	t.Setenv("HTTPS_PROXY", "http://"+user+":"+pass+"@corp-proxy:3128")
	got := GetProxyInfoString(nil)
	if want := "Environment: http://" + user + ":xxxxx@corp-proxy:3128"; got != want {
		t.Errorf("GetProxyInfoString(nil) = %q, want %q", got, want)
	}
}
