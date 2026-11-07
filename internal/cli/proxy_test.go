// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/reuc/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// With --json, `proxy test` writes only the JSON result to stdout — also when
// the test fails — so the output can be piped to a JSON parser.
func TestProxyTest_JSONOutputIsOnlyJSON(t *testing.T) {
	// A port that was free a moment ago: connecting to it is refused at once.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := newProxyTestCmd(&RootOpts{JSONOut: true})
	cmd.SetArgs([]string{"--proxy", "http://alice:s3cret@127.0.0.1:" + port, "--url", "http://example.invalid/", "--timeout", "5s"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	var runErr error
	out := captureOutput(t, func() { runErr = cmd.Execute() })

	if runErr == nil {
		t.Fatal("expected the proxy test to fail")
	}
	var res hfdownloader.ProxyTestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not a single JSON value: %v\n%s", err, out)
	}
	if res.Success || res.Error == "" {
		t.Errorf("result = %+v, want a failure with an error message", res)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("password leaked into the output:\n%s", out)
	}
}
