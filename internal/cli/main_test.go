// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"testing"

	"github.com/reuc/HuggingFaceModelDownloader/internal/testutil"
)

// TestMain points HOME (and its per-OS equivalents) at a throwaway directory
// so no test can read or overwrite the developer's real config or HF cache —
// settings tests used to persist ~/.config/hfdownloader.json.
func TestMain(m *testing.M) {
	os.Exit(testutil.RunIsolated(m, nil))
}
