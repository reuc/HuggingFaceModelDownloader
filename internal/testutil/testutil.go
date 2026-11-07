// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package testutil holds helpers shared by the module's test suites. It is
// imported only from _test.go files.
package testutil

import (
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"
)

// isolatedEnv lists the variables that would let a test reach the developer's
// real config or Hugging Face cache; RunIsolated clears them.
var isolatedEnv = []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "HF_HOME", "HF_HUB_CACHE", "HF_TOKEN"}

// RunIsolated runs the package's tests with HOME (and its per-OS equivalents)
// pointed at a throwaway directory, so no test can read or overwrite the
// developer's real config or HF cache. setup, when non-nil, is called with
// that directory before any test runs. It returns the exit code for os.Exit:
//
//	func TestMain(m *testing.M) { os.Exit(testutil.RunIsolated(m, nil)) }
func RunIsolated(m *testing.M, setup func(home string)) int {
	home, err := os.MkdirTemp("", "hfd-test-home-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "testutil: create test home: %v\n", err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(home); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "testutil: remove test home: %v\n", err)
		}
	}()

	for _, k := range []string{"HOME", "USERPROFILE"} {
		if err := os.Setenv(k, home); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "testutil: set %s: %v\n", k, err)
			return 1
		}
	}
	for _, k := range isolatedEnv {
		if err := os.Unsetenv(k); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "testutil: unset %s: %v\n", k, err)
			return 1
		}
	}
	if setup != nil {
		setup(home)
	}
	return m.Run()
}

// WriteFile is os.WriteFile that fails the test on error.
func WriteFile(t testing.TB, name string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, data, perm); err != nil {
		t.Fatal(err)
	}
}

// MkdirAll is os.MkdirAll that fails the test on error.
func MkdirAll(t testing.TB, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatal(err)
	}
}

// Symlink is os.Symlink that fails the test on error.
func Symlink(t testing.TB, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatal(err)
	}
}

// Chtimes is os.Chtimes that fails the test on error.
func Chtimes(t testing.TB, name string, atime, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(name, atime, mtime); err != nil {
		t.Fatal(err)
	}
}

// Unsetenv unsets key for the rest of the test and restores its previous
// value (or absence) when the test ends — the unset counterpart of t.Setenv.
func Unsetenv(t testing.TB, key string) {
	t.Helper()
	t.Setenv(key, "") // registers the restore of the original value
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// Secret returns prefix followed by a fresh random string, for use as a
// password or token in a test, so that no test carries a hard-coded
// credential. The random part is 26 characters from [A-Z2-7], which need no
// escaping in a URL.
func Secret(prefix string) string {
	return prefix + rand.Text()
}
