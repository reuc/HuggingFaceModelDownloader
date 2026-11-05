// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StorageFlat is the DownloadManifest.Storage value for a repo whose files
// were moved out of the HF cache (Settings.Flatten): the friendly view
// (models/<owner>/<name> or datasets/<owner>/<name>) holds the real data and
// there is no hub/ entry.
const StorageFlat = "flat"

// flatFile is one file of a finished download that Flatten places in the
// friendly view.
type flatFile struct {
	// rel is the file's path inside the friendly view (repo path, plus the
	// filter subdirectory when AppendFilterSubdir is set).
	rel string
	// sha is the blob holding the file's data.
	sha string
	// keep means the friendly view already holds this file's data as a real
	// file (it was found there and skipped), so there is nothing to move.
	keep bool
}

// IsFlat reports whether this repo is stored flat: its download manifest says
// so and there is no hub/ entry.
func (r *RepoDir) IsFlat() bool {
	if _, err := os.Lstat(r.Path()); err == nil {
		return false
	}
	m, err := ReadManifest(filepath.Join(r.FriendlyPath(), ManifestFilename))
	return err == nil && m.Storage == StorageFlat
}

// flatManifestFiles returns the files recorded by an earlier flat download of
// this repo, by repo path, or nil when the repo isn't stored flat.
func (r *RepoDir) flatManifestFiles() map[string]ManifestFile {
	m, err := ReadManifest(filepath.Join(r.FriendlyPath(), ManifestFilename))
	if err != nil || m.Storage != StorageFlat {
		return nil
	}
	out := make(map[string]ManifestFile, len(m.Files))
	for _, f := range m.Files {
		out[f.Name] = f
	}
	return out
}

// flatFileCurrent reports whether the friendly view already holds the data
// for a planned file as a real file, so a flat repo can be updated without
// downloading unchanged files again. LFS files are matched by size and the
// content hash recorded in the previous manifest, other files by size and git
// blob id. It returns the file's content hash when it matches.
func (r *RepoDir) flatFileCurrent(rel string, it PlanItem, prev map[string]ManifestFile) (string, bool) {
	if unsafeRepoPath(rel) {
		return "", false
	}
	p := filepath.Join(r.FriendlyPath(), filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != it.Size {
		return "", false
	}
	recorded := ""
	if f, ok := prev[it.RelativePath]; ok && f.Size == it.Size {
		recorded = strings.TrimPrefix(f.Blob, "blobs/")
	}
	if it.SHA256 != "" {
		if recorded == it.SHA256 {
			return recorded, true
		}
		return "", false
	}
	if it.GitOID != "" && matchesGitOID(p, it.GitOID) {
		if validSHA256Hex(recorded) {
			return recorded, true
		}
		if sha, err := computeSHA256(p); err == nil {
			return sha, true
		}
	}
	return "", false
}

// placeFlatFiles moves each file's data from blobs/ to its place in the
// friendly view, replacing the link that was there. A blob used by several
// files is copied for all but the last of them. Nothing is deleted from the
// friendly view, and the hub/ entry is left for removeHubEntry.
func (r *RepoDir) placeFlatFiles(files []flatFile) error {
	uses := make(map[string]int)
	for _, f := range files {
		if !f.keep {
			uses[f.sha]++
		}
	}
	base := r.FriendlyPath()
	for _, f := range files {
		if f.keep {
			continue
		}
		if unsafeRepoPath(f.rel) {
			return fmt.Errorf("unsafe path %q", f.rel)
		}
		if !validSHA256Hex(f.sha) {
			return fmt.Errorf("%s: invalid blob hash %q", f.rel, f.sha)
		}
		dst := filepath.Join(base, filepath.FromSlash(f.rel))
		if fi, err := os.Lstat(dst); err == nil && fi.IsDir() {
			return fmt.Errorf("%s: a directory is in the way", f.rel)
		}
		blob := r.BlobPath(f.sha)
		if _, err := os.Stat(blob); err != nil {
			return fmt.Errorf("%s: blob missing: %w", f.rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		uses[f.sha]--
		if uses[f.sha] > 0 {
			// Another file still needs this blob: copy, don't move.
			if err := CopyFileStream(blob, dst); err != nil {
				return fmt.Errorf("%s: %w", f.rel, err)
			}
			continue
		}
		// Rename replaces the symlink or hardlink at dst in one step. If the
		// friendly view is on another filesystem, copy instead.
		if err := os.Rename(blob, dst); err != nil {
			if err := CopyFileStream(blob, dst); err != nil {
				return fmt.Errorf("%s: %w", f.rel, err)
			}
			_ = os.Remove(blob)
		}
	}
	return nil
}

// removeHubEntry deletes the repo's hub/ directory once its files have been
// moved to the friendly view. It is left alone (removed=false) while another
// process is still writing a download into it: an in-progress temp file
// modified within the last `active`.
func (r *RepoDir) removeHubEntry(active time.Duration) (removed bool, err error) {
	path := r.Path()
	if fi, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	} else if !fi.IsDir() {
		return false, fmt.Errorf("%s is not a directory", path)
	}
	busy := false
	cutoff := time.Now().Add(-active)
	_ = filepath.WalkDir(r.BlobsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "tmp-") && !strings.HasSuffix(name, ".incomplete") && !strings.HasSuffix(name, ".part") {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(cutoff) {
			busy = true
			return fs.SkipAll
		}
		return nil
	})
	if busy {
		return false, nil
	}
	return true, os.RemoveAll(path)
}

// ListFlatRepos returns the repos stored flat (see Settings.Flatten): those
// with a "flat" download manifest in models/<owner>/<name> or
// datasets/<owner>/<name> and no hub/ entry. ListRepos only sees hub/.
func (c *HFCache) ListFlatRepos() ([]*RepoDir, error) {
	var out []*RepoDir
	for _, view := range []struct {
		dir string
		typ RepoType
	}{{c.ModelsDir(), RepoTypeModel}, {c.DatasetsDir(), RepoTypeDataset}} {
		owners, err := os.ReadDir(view.dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			names, err := os.ReadDir(filepath.Join(view.dir, o.Name()))
			if err != nil {
				continue
			}
			for _, n := range names {
				if !n.IsDir() {
					continue
				}
				rd, err := c.Repo(o.Name()+"/"+n.Name(), view.typ)
				if err != nil || !rd.IsFlat() {
					continue
				}
				out = append(out, rd)
			}
		}
	}
	return out, nil
}

// FlatFiles lists the files of a flat repo (paths relative to its friendly
// view directory, without the download manifest) and their total size.
func (r *RepoDir) FlatFiles() (files []string, sizes []int64, total int64, err error) {
	root := r.FriendlyPath()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFilename {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, rel)
		sizes = append(sizes, info.Size())
		total += info.Size()
		return nil
	})
	return files, sizes, total, err
}
