// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestFilename is the standard name for manifest files.
const ManifestFilename = "hfd.yaml"

// DownloadManifest captures the complete record of a download job.
// This file is written to the friendly view root (e.g., models/Owner/Repo/hfd.yaml)
// and contains all information needed to understand and reproduce the download.
type DownloadManifest struct {
	// Metadata
	Version string `yaml:"version" json:"version"` // Manifest format version

	// Repository info
	Type     string `yaml:"type" json:"type"`           // "model" or "dataset"
	Repo     string `yaml:"repo" json:"repo"`           // "owner/name"
	Branch   string `yaml:"branch" json:"branch"`       // Branch name (usually "main")
	Commit   string `yaml:"commit" json:"commit"`       // Commit hash
	RepoPath string `yaml:"repo_path" json:"repo_path"` // Relative path to hub cache (e.g., "hub/models--owner--repo"); empty when flat
	// Storage is "flat" (StorageFlat) when the files in this directory are
	// the real data and the repo has no hub/ entry (Settings.Flatten). Empty
	// means the usual HF cache layout, where this directory holds links.
	Storage string `yaml:"storage,omitempty" json:"storage,omitempty"`

	// Timing
	StartedAt   time.Time `yaml:"started_at" json:"started_at"`     // When download started
	CompletedAt time.Time `yaml:"completed_at" json:"completed_at"` // When download completed

	// CLI command used (token stripped for security)
	Command string `yaml:"command" json:"command"`

	// Results
	TotalSize  int64          `yaml:"total_size" json:"total_size"`   // Total bytes downloaded
	TotalFiles int            `yaml:"total_files" json:"total_files"` // Number of files
	Files      []ManifestFile `yaml:"files" json:"files"`
}

// ManifestFile represents a single file in the manifest.
// The Blob field contains the relative path from the repo directory to the blob,
// making it easy for automation scripts to locate and copy files.
type ManifestFile struct {
	Name string `yaml:"name" json:"name"` // Original filename (e.g., "model.Q4_K_M.gguf")
	Blob string `yaml:"blob" json:"blob"` // Relative path to blob (e.g., "blobs/abc123...")
	Size int64  `yaml:"size" json:"size"` // File size in bytes
	LFS  bool   `yaml:"lfs" json:"lfs"`   // Is LFS file
	// ModTime is the file's modification time (Unix nanoseconds) in a flat
	// repo, recorded when it was placed. A later --flatten download trusts a
	// file whose size, hash and ModTime still match without re-hashing it.
	ModTime int64 `yaml:"mtime,omitempty" json:"mtime,omitempty"`
}

// ManifestBuilder helps construct a manifest during download.
type ManifestBuilder struct {
	manifest  *DownloadManifest
	startTime time.Time
}

// NewManifestBuilder creates a new manifest builder for a download job.
func NewManifestBuilder(job Job, command string) *ManifestBuilder {
	repoType := "model"
	if job.IsDataset {
		repoType = "dataset"
	}

	branch := job.Revision
	if branch == "" {
		branch = "main"
	}

	// Build repo path (relative to cache root)
	repoPath := "hub/" + RepoTypeName(job.IsDataset) + "--" + strings.ReplaceAll(job.Repo, "/", "--")

	return &ManifestBuilder{
		startTime: time.Now().UTC(),
		manifest: &DownloadManifest{
			Version:   "1.0",
			Type:      repoType,
			Repo:      job.Repo,
			Branch:    branch,
			RepoPath:  repoPath,
			StartedAt: time.Now().UTC(),
			Command:   command,
			Files:     []ManifestFile{},
		},
	}
}

// SetCommit sets the commit hash.
func (b *ManifestBuilder) SetCommit(commit string) {
	b.manifest.Commit = commit
}

// SetFlat records that the download is stored flat (Settings.Flatten): the
// friendly view holds the real files and there is no hub/ entry. Blob entries
// still name each file's content hash, which the next download uses to skip
// unchanged files.
func (b *ManifestBuilder) SetFlat() {
	b.manifest.Storage = StorageFlat
	b.manifest.RepoPath = ""
}

// SetModTimes records each file's modification time (by file name) in a flat
// manifest; see ManifestFile.ModTime.
func (b *ManifestBuilder) SetModTimes(mtimes map[string]int64) {
	for i := range b.manifest.Files {
		b.manifest.Files[i].ModTime = mtimes[b.manifest.Files[i].Name]
	}
}

// AddFile adds a file to the manifest.
func (b *ManifestBuilder) AddFile(name, blobHash string, size int64, isLFS bool) {
	b.manifest.Files = append(b.manifest.Files, ManifestFile{
		Name: name,
		Blob: "blobs/" + blobHash,
		Size: size,
		LFS:  isLFS,
	})
	b.manifest.TotalSize += size
	b.manifest.TotalFiles++
}

// Build finalizes the manifest with completion time.
func (b *ManifestBuilder) Build() *DownloadManifest {
	b.manifest.CompletedAt = time.Now().UTC()
	return b.manifest
}

// Write writes the manifest to the friendly view directory.
func (m *DownloadManifest) Write(friendlyPath string) (string, error) {
	manifestPath := filepath.Join(friendlyPath, ManifestFilename)

	// Ensure directory exists
	if err := os.MkdirAll(friendlyPath, 0o755); err != nil {
		return "", fmt.Errorf("create manifest dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}

	// Add header comment
	header := fmt.Sprintf("# HuggingFace %s: %s\n", m.Type, m.Repo)
	header += fmt.Sprintf("# Downloaded: %s\n", m.CompletedAt.Format(time.RFC3339))
	header += "# Generated by hfdownloader\n"
	header += "# Run 'hfdownloader list' to see all downloaded repos\n\n"
	content := header + string(data)

	// Write a temp file and rename it into place: if the friendly view has a
	// link at this path (a repo with its own hfd.yaml), the link is replaced
	// instead of written through, which would overwrite the cached file.
	tmp, err := os.CreateTemp(friendlyPath, ManifestFilename+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	_, werr := tmp.WriteString(content)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), manifestPath)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write manifest: %w", werr)
	}

	return manifestPath, nil
}

// ReadManifest reads a manifest from a file.
func ReadManifest(path string) (*DownloadManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var manifest DownloadManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// RepoTypeName returns the type name prefix for repo directories.
func RepoTypeName(isDataset bool) string {
	if isDataset {
		return "datasets"
	}
	return "models"
}
