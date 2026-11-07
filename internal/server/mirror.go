// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reuc/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// --- Mirror Target Management ---

// MirrorTargetRequest is the request body for adding a mirror target.
type MirrorTargetRequest struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// handleMirrorTargetsList returns all configured mirror targets.
func (s *Server) handleMirrorTargetsList(w http.ResponseWriter, r *http.Request) {
	cfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}

	// Convert to sorted list
	type targetInfo struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		Description string `json:"description,omitempty"`
		Exists      bool   `json:"exists"`
	}

	var targets []targetInfo
	for name, t := range cfg.Targets {
		// Check if path exists
		_, err := os.Stat(t.Path)
		targets = append(targets, targetInfo{
			Name:        name,
			Path:        t.Path,
			Description: t.Description,
			Exists:      err == nil,
		})
	}

	// Sort by name
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Name < targets[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"targets": targets,
		"count":   len(targets),
	})
}

// handleMirrorTargetAdd adds a new mirror target.
func (s *Server) handleMirrorTargetAdd(w http.ResponseWriter, r *http.Request) {
	var req MirrorTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: name", "")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: path", "")
		return
	}

	// Validate name (alphanumeric, dash, underscore only)
	if !isValidTargetName(req.Name) {
		writeError(w, http.StatusBadRequest, "Invalid target name", "Use only letters, numbers, dashes, and underscores")
		return
	}

	// A target is a directory the mirror endpoints copy into, read from and
	// (with deleteExtra) delete under, so letting any client register an
	// arbitrary path gave the API arbitrary filesystem access. Web-added
	// targets are confined to the operator's --mirror-dir.
	mirrorDir := s.settingsConfig().MirrorDir
	if mirrorDir == "" {
		writeError(w, http.StatusBadRequest, "Adding mirror targets from the web is disabled (start the server with --mirror-dir)",
			"start the server with --mirror-dir PATH (or set mirror-dir in the config file) to allow targets under that directory, or register targets on the server with `hfdownloader mirror target add <name> <path>`")
		return
	}

	// Resolve to absolute path
	absPath, err := filepath.Abs(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	if !pathWithin(absPath, mirrorDir) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Target path must be inside %s", mirrorDir),
			"web-managed mirror targets are confined to the server's --mirror-dir")
		return
	}

	cfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}

	// Never repoint a target that was registered on the server (outside
	// --mirror-dir): a client could otherwise redirect the operator's own
	// pushes.
	if existing, ok := cfg.Get(req.Name); ok && !pathWithin(existing.Path, mirrorDir) {
		writeError(w, http.StatusForbidden, "Target is managed on the server",
			fmt.Sprintf("%q points outside --mirror-dir; change it with `hfdownloader mirror target add` on the server", req.Name))
		return
	}

	cfg.Add(req.Name, absPath, req.Description)

	if err := cfg.Save(""); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save targets", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": fmt.Sprintf("Added target %q", req.Name),
		"target": map[string]string{
			"name":        req.Name,
			"path":        absPath,
			"description": req.Description,
		},
	})
}

// handleMirrorTargetRemove removes a mirror target.
func (s *Server) handleMirrorTargetRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Missing target name", "")
		return
	}

	cfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}

	existing, ok := cfg.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "Target not found", name)
		return
	}
	// Only targets the web could have added (inside --mirror-dir) can be
	// removed from the web; server-registered ones are managed with the CLI.
	mirrorDir := s.settingsConfig().MirrorDir
	if mirrorDir == "" || !pathWithin(existing.Path, mirrorDir) {
		writeError(w, http.StatusForbidden, "Target is managed on the server",
			fmt.Sprintf("remove %q with `hfdownloader mirror target remove %s` on the server", name, name))
		return
	}
	cfg.Remove(name)

	if err := cfg.Save(""); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save targets", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": fmt.Sprintf("Removed target %q", name),
	})
}

// --- Mirror Operations ---

// MirrorDiffRequest is the request body for diff operation.
type MirrorDiffRequest struct {
	Target     string `json:"target"`     // Target name or path
	RepoFilter string `json:"repoFilter"` // Optional filter by repo name
}

// MirrorDiffEntry represents a difference between source and target.
type MirrorDiffEntry struct {
	Repo       string `json:"repo"`
	Type       string `json:"type"`
	Status     string `json:"status"` // "missing", "outdated", "extra"
	LocalSize  int64  `json:"localSize,omitempty"`
	RemoteSize int64  `json:"remoteSize,omitempty"`
	SizeHuman  string `json:"sizeHuman,omitempty"`
}

// handleMirrorDiff compares local cache with a target.
func (s *Server) handleMirrorDiff(w http.ResponseWriter, r *http.Request) {
	var req MirrorDiffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: target", "")
		return
	}

	// Resolve target path
	targetsCfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}
	targetPath, ok := registeredTargetPath(targetsCfg, req.Target)
	if !ok {
		writeError(w, http.StatusBadRequest, "Unknown mirror target", unknownTargetDetails(req.Target))
		return
	}

	// Local cache
	cacheDir := s.config.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	// Scan both caches
	localEntries, err := scanCacheForMirror(cacheDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to scan local cache", err.Error())
		return
	}

	targetEntries, err := scanCacheForMirror(targetPath)
	if err != nil {
		// Target might not exist yet
		targetEntries = nil
	}

	// Build maps
	localMap := make(map[string]mirrorEntry)
	for _, e := range localEntries {
		if req.RepoFilter == "" || strings.Contains(strings.ToLower(e.Repo), strings.ToLower(req.RepoFilter)) {
			localMap[e.Repo] = e
		}
	}

	targetMap := make(map[string]mirrorEntry)
	for _, e := range targetEntries {
		if req.RepoFilter == "" || strings.Contains(strings.ToLower(e.Repo), strings.ToLower(req.RepoFilter)) {
			targetMap[e.Repo] = e
		}
	}

	// Calculate diff
	var diffs []MirrorDiffEntry

	// Missing in target (exists locally but not in target)
	for repo, local := range localMap {
		if _, ok := targetMap[repo]; !ok {
			diffs = append(diffs, MirrorDiffEntry{
				Repo:      repo,
				Type:      local.Type,
				Status:    "missing",
				LocalSize: local.Size,
				SizeHuman: humanSizeBytes(local.Size),
			})
		}
	}

	// Extra in target (not in local)
	for repo, remote := range targetMap {
		if _, ok := localMap[repo]; !ok {
			diffs = append(diffs, MirrorDiffEntry{
				Repo:       repo,
				Type:       remote.Type,
				Status:     "extra",
				RemoteSize: remote.Size,
				SizeHuman:  humanSizeBytes(remote.Size),
			})
		}
	}

	// Outdated (different commits)
	for repo, local := range localMap {
		if remote, ok := targetMap[repo]; ok {
			if local.Commit != remote.Commit && local.Commit != "" && remote.Commit != "" {
				diffs = append(diffs, MirrorDiffEntry{
					Repo:       repo,
					Type:       local.Type,
					Status:     "outdated",
					LocalSize:  local.Size,
					RemoteSize: remote.Size,
					SizeHuman:  humanSizeBytes(local.Size),
				})
			}
		}
	}

	// Sort by status, then repo
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].Status != diffs[j].Status {
			return diffs[i].Status < diffs[j].Status
		}
		return diffs[i].Repo < diffs[j].Repo
	})

	// Calculate summary
	var missingSize, extraSize int64
	var missingCount, extraCount, outdatedCount int
	for _, d := range diffs {
		switch d.Status {
		case "missing":
			missingSize += d.LocalSize
			missingCount++
		case "extra":
			extraSize += d.RemoteSize
			extraCount++
		case "outdated":
			outdatedCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"diffs":      diffs,
		"localPath":  cacheDir,
		"targetPath": targetPath,
		"summary": map[string]any{
			"missing":          missingCount,
			"missingSizeHuman": humanSizeBytes(missingSize),
			"extra":            extraCount,
			"extraSizeHuman":   humanSizeBytes(extraSize),
			"outdated":         outdatedCount,
			"inSync":           len(diffs) == 0,
		},
	})
}

// MirrorSyncRequest is the request body for push/pull operations.
type MirrorSyncRequest struct {
	Target      string `json:"target"`      // Target name or path
	RepoFilter  string `json:"repoFilter"`  // Optional filter by repo name
	DryRun      bool   `json:"dryRun"`      // Show what would be done without doing it
	Verify      bool   `json:"verify"`      // Verify integrity after copy
	DeleteExtra bool   `json:"deleteExtra"` // Delete repos in destination not in source
	Force       bool   `json:"force"`       // Re-copy incomplete/outdated repos
}

// handleMirrorPush pushes local repos to target.
func (s *Server) handleMirrorPush(w http.ResponseWriter, r *http.Request) {
	var req MirrorSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: target", "")
		return
	}

	// Resolve target path
	targetsCfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}
	targetPath, ok := registeredTargetPath(targetsCfg, req.Target)
	if !ok {
		writeError(w, http.StatusBadRequest, "Unknown mirror target", unknownTargetDetails(req.Target))
		return
	}

	// Local cache
	cacheDir := s.config.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	result, err := mirrorSync(cacheDir, targetPath, req.RepoFilter, req.DryRun, req.Verify, req.DeleteExtra, req.Force)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Mirror sync failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleMirrorPull pulls repos from target to local.
func (s *Server) handleMirrorPull(w http.ResponseWriter, r *http.Request) {
	var req MirrorSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: target", "")
		return
	}

	// Resolve target path
	targetsCfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}
	targetPath, ok := registeredTargetPath(targetsCfg, req.Target)
	if !ok {
		writeError(w, http.StatusBadRequest, "Unknown mirror target", unknownTargetDetails(req.Target))
		return
	}

	// Local cache
	cacheDir := s.config.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	// Pull is push in reverse (target -> local)
	result, err := mirrorSync(targetPath, cacheDir, req.RepoFilter, req.DryRun, req.Verify, req.DeleteExtra, req.Force)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Mirror sync failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// --- Helper types and functions ---

type mirrorEntry struct {
	Type   string
	Repo   string
	Commit string
	Size   int64
	Path   string
}

// scanCacheForMirror scans the cache directory for mirror operations.
func scanCacheForMirror(cacheDir string) ([]mirrorEntry, error) {
	var entries []mirrorEntry

	hubDir := filepath.Join(cacheDir, "hub")
	if _, err := os.Stat(hubDir); os.IsNotExist(err) {
		return entries, nil
	}

	// deepcode ignore PT: cacheDir is the server's cache or a registered target path; the API takes only a target name, looked up in the server-side registry
	items, err := os.ReadDir(hubDir)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		if !item.IsDir() {
			continue
		}

		name := item.Name()
		var repoType string
		var repoName string

		if strings.HasPrefix(name, "models--") {
			repoType = "model"
			repoName = strings.TrimPrefix(name, "models--")
		} else if strings.HasPrefix(name, "datasets--") {
			repoType = "dataset"
			repoName = strings.TrimPrefix(name, "datasets--")
		} else {
			continue
		}

		// Convert owner--repo to owner/repo
		repoName = strings.Replace(repoName, "--", "/", 1)

		// Get size by walking blobs directory
		blobsDir := filepath.Join(hubDir, name, "blobs")
		var totalSize int64
		filepath.Walk(blobsDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				totalSize += info.Size()
			}
			return nil
		})

		// Try to read commit from refs/main
		var commit string
		refPath := filepath.Join(hubDir, name, "refs", "main")
		// deepcode ignore PT: cacheDir is the server's cache or a registered target path, never a request value; name comes from os.ReadDir of its hub dir
		if data, err := os.ReadFile(refPath); err == nil {
			commit = strings.TrimSpace(string(data))
		}

		entries = append(entries, mirrorEntry{
			Type:   repoType,
			Repo:   repoName,
			Commit: commit,
			Size:   totalSize,
			Path:   filepath.Join(hubDir, name),
		})
	}

	return entries, nil
}

// MirrorSyncResult represents the result of a sync operation.
type MirrorSyncResult struct {
	Success          bool     `json:"success"`
	DryRun           bool     `json:"dryRun"`
	Copied           int      `json:"copied"`
	CopiedSize       int64    `json:"copiedSize"`
	CopiedSizeHuman  string   `json:"copiedSizeHuman"`
	Deleted          int      `json:"deleted"`
	DeletedSize      int64    `json:"deletedSize"`
	DeletedSizeHuman string   `json:"deletedSizeHuman"`
	Repos            []string `json:"repos,omitempty"`
	Errors           []string `json:"errors,omitempty"`
	Message          string   `json:"message"`
}

// mirrorSync copies repos from source to destination.
func mirrorSync(srcCache, dstCache, repoFilter string, dryRun, verify, deleteExtra, force bool) (*MirrorSyncResult, error) {
	result := &MirrorSyncResult{
		Success: true,
		DryRun:  dryRun,
	}

	// Scan source
	srcEntries, err := scanCacheForMirror(srcCache)
	if err != nil {
		return nil, fmt.Errorf("scan source: %w", err)
	}

	// Scan destination
	dstEntries, err := scanCacheForMirror(dstCache)
	if err != nil {
		// Destination might not exist yet
		dstEntries = nil
	}

	srcMap := make(map[string]mirrorEntry)
	for _, e := range srcEntries {
		srcMap[e.Repo] = e
	}

	dstMap := make(map[string]mirrorEntry)
	for _, e := range dstEntries {
		dstMap[e.Repo] = e
	}

	// Find repos to copy
	var toCopy []mirrorEntry
	for _, e := range srcEntries {
		if repoFilter != "" && !strings.Contains(strings.ToLower(e.Repo), strings.ToLower(repoFilter)) {
			continue
		}

		if _, ok := dstMap[e.Repo]; !ok {
			// Missing in destination
			toCopy = append(toCopy, e)
		} else if force {
			// Check if destination needs update
			relPath, _ := filepath.Rel(srcCache, e.Path)
			dstPath := filepath.Join(dstCache, relPath)
			if needsUpdate, _ := compareRepoIntegrity(e.Path, dstPath); needsUpdate {
				toCopy = append(toCopy, e)
			}
		}
	}

	// Find repos to delete
	var toDelete []mirrorEntry
	if deleteExtra {
		for _, e := range dstEntries {
			if repoFilter != "" && !strings.Contains(strings.ToLower(e.Repo), strings.ToLower(repoFilter)) {
				continue
			}
			if _, ok := srcMap[e.Repo]; !ok {
				toDelete = append(toDelete, e)
			}
		}
	}

	// Calculate sizes
	for _, e := range toCopy {
		result.CopiedSize += e.Size
		result.Repos = append(result.Repos, e.Repo)
	}
	for _, e := range toDelete {
		result.DeletedSize += e.Size
	}

	result.Copied = len(toCopy)
	result.Deleted = len(toDelete)
	result.CopiedSizeHuman = humanSizeBytes(result.CopiedSize)
	result.DeletedSizeHuman = humanSizeBytes(result.DeletedSize)

	if dryRun {
		result.Message = fmt.Sprintf("Would copy %d repos (%s)", len(toCopy), result.CopiedSizeHuman)
		if len(toDelete) > 0 {
			result.Message += fmt.Sprintf(", delete %d repos (%s)", len(toDelete), result.DeletedSizeHuman)
		}
		return result, nil
	}

	if len(toCopy) == 0 && len(toDelete) == 0 {
		result.Message = "Nothing to do - destination is in sync"
		return result, nil
	}

	// Copy each repo
	for _, e := range toCopy {
		if err := hfdownloader.CopyRepoCache(e.Path, srcCache, dstCache); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("copy %s: %v", e.Repo, err))
			result.Success = false
		}

		if verify {
			if err := hfdownloader.VerifyRepoCache(e.Path, srcCache, dstCache); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("verify %s: %v", e.Repo, err))
				result.Success = false
			}
		}
	}

	// Delete extra repos
	for _, e := range toDelete {
		// deepcode ignore PT: e.Path is <target>/hub/<entry> from listing a registered target; the API takes only a target name, looked up in the server-side registry (web-added targets are confined to --mirror-dir)
		if err := os.RemoveAll(e.Path); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("delete %s: %v", e.Repo, err))
			result.Success = false
		}
	}

	result.Message = fmt.Sprintf("Copied %d repos (%s)", len(toCopy), result.CopiedSizeHuman)
	if len(toDelete) > 0 {
		result.Message += fmt.Sprintf(", deleted %d repos (%s)", len(toDelete), result.DeletedSizeHuman)
	}

	return result, nil
}

// Repo copy/verify primitives live in pkg/hfdownloader (CopyRepoCache,
// VerifyRepoCache, CopyFileStream, SameFileSHA256) and are shared with the CLI
// mirror path.

// compareRepoIntegrity compares source and destination repos.
func compareRepoIntegrity(srcPath, dstPath string) (needsUpdate bool, reason string) {
	if _, err := os.Stat(dstPath); os.IsNotExist(err) {
		return true, "missing"
	}

	// Check if destination has same blob count
	srcBlobs := countBlobs(srcPath)
	dstBlobs := countBlobs(dstPath)

	if srcBlobs > dstBlobs {
		return true, "incomplete"
	}

	return false, ""
}

func countBlobs(repoPath string) int {
	blobsDir := filepath.Join(repoPath, "blobs")
	count := 0
	filepath.Walk(blobsDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// registeredTargetPath resolves a mirror request's target to the path of a
// registered target. Only names from the targets file are accepted. The CLI's
// ResolvePath also takes raw paths, but over the API that let any client copy
// the cache to, read from, or (with deleteExtra) delete under any directory on
// the server.
func registeredTargetPath(cfg *hfdownloader.TargetsConfig, target string) (string, bool) {
	t, ok := cfg.Get(target)
	if !ok || t.Path == "" {
		return "", false
	}
	return t.Path, true
}

func unknownTargetDetails(target string) string {
	return fmt.Sprintf("%q is not a registered target name (the API does not accept paths); register it on the server with `hfdownloader mirror target add <name> <path>`", target)
}

// pathWithin reports whether path is root or inside it. Both are made
// absolute, symlinks in their existing prefix are resolved (so a link inside
// root can't point out of it) and the result is compared lexically.
func pathWithin(path, root string) bool {
	p, r := resolveExisting(path), resolveExisting(root)
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting makes p absolute and resolves symlinks in its longest
// existing prefix (the target itself need not exist yet).
func resolveExisting(p string) string {
	p, _ = filepath.Abs(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// isValidTargetName checks if a target name contains only safe characters.
func isValidTargetName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_') {
			return false
		}
	}
	return true
}
