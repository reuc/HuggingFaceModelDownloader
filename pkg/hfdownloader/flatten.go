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
	// name is the file's repo path, its name in hfd.yaml.
	name string
	// src is the file holding its data inside the repo's hub/ entry: a blob,
	// or a snapshot file when snapshots are hardlinks or copies.
	src string
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
// downloading unchanged files again. It returns the file's content hash when
// it matches.
//
// LFS files must have the planned size and content hash. Hashing every large
// file on every update would be slow, so a file whose size, hash and
// modification time all match what the previous flat manifest recorded is
// trusted; one modified since (edited in place, say) is hashed. Other files
// are matched by size and git blob id, which reads them.
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
	var recordedMtime int64
	if f, ok := prev[it.RelativePath]; ok && f.Size == it.Size {
		recorded = strings.TrimPrefix(f.Blob, "blobs/")
		recordedMtime = f.ModTime
	}
	if it.SHA256 != "" {
		if recorded != it.SHA256 {
			return "", false
		}
		if recordedMtime != 0 && fi.ModTime().UnixNano() == recordedMtime {
			return recorded, true
		}
		if sha, err := computeSHA256(p); err == nil && sha == it.SHA256 {
			return sha, true
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

// flatStageSuffix names the files placeFlatFiles stages next to their
// destinations before renaming them into place.
const flatStageSuffix = ".hfd-flatten"

// placeFlatFiles puts each file's data from the repo's hub/ entry at its
// place in the friendly view as a real file, replacing the link that was
// there, and returns each placed file's modification time by friendly path.
//
// It is all-or-nothing up to the final renames: every file is first staged
// next to its destination, as a hardlink to its data (no extra disk space) or,
// where hardlinks aren't possible, a copy. Nothing in hub/ or the friendly
// view changes until all of them are staged, so a failure (disk full while
// copying, say) leaves the cache as it was. Then each staged file is renamed
// over its destination. The data stays in hub/ until removeHubEntry, so even
// a failed rename can be retried.
func (r *RepoDir) placeFlatFiles(files []flatFile) (map[string]int64, error) {
	base := r.FriendlyPath()
	type stagedFile struct{ tmp, dst, rel string }
	var staged []stagedFile
	unstage := func() {
		for _, s := range staged {
			_ = os.Remove(s.tmp)
		}
	}
	mtimes := make(map[string]int64)

	for _, f := range files {
		if unsafeRepoPath(f.rel) {
			unstage()
			return nil, fmt.Errorf("unsafe path %q", f.rel)
		}
		dst := filepath.Join(base, filepath.FromSlash(f.rel))
		if f.keep {
			if fi, err := os.Lstat(dst); err == nil {
				mtimes[f.rel] = fi.ModTime().UnixNano()
			}
			continue
		}
		if f.src == "" || !within(f.src, r.Path()) {
			unstage()
			return nil, fmt.Errorf("%s: data is not in %s", f.rel, r.Path())
		}
		srcInfo, err := os.Lstat(f.src)
		if err != nil {
			unstage()
			return nil, fmt.Errorf("%s: data missing: %w", f.rel, err)
		}
		if !srcInfo.Mode().IsRegular() {
			unstage()
			return nil, fmt.Errorf("%s: %s is not a regular file", f.rel, f.src)
		}
		dstInfo, err := os.Lstat(dst)
		if err == nil && dstInfo.IsDir() {
			unstage()
			return nil, fmt.Errorf("%s: a directory is in the way", f.rel)
		}
		if err == nil && dstInfo.Mode().IsRegular() && os.SameFile(srcInfo, dstInfo) {
			// Already the same file (hardlink mode): nothing to move.
			mtimes[f.rel] = dstInfo.ModTime().UnixNano()
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			unstage()
			return nil, fmt.Errorf("%s: %w", f.rel, err)
		}
		tmp := dst + flatStageSuffix
		_ = os.Remove(tmp) // left by an interrupted run
		if err := os.Link(f.src, tmp); err != nil {
			if err := CopyFileStream(f.src, tmp); err != nil {
				_ = os.Remove(tmp)
				unstage()
				return nil, fmt.Errorf("%s: %w", f.rel, err)
			}
		}
		staged = append(staged, stagedFile{tmp: tmp, dst: dst, rel: f.rel})
	}

	for i, s := range staged {
		if err := os.Rename(s.tmp, s.dst); err != nil {
			for _, rest := range staged[i:] {
				_ = os.Remove(rest.tmp)
			}
			return nil, fmt.Errorf("%s: %w", s.rel, err)
		}
		if fi, err := os.Lstat(s.dst); err == nil {
			mtimes[s.rel] = fi.ModTime().UnixNano()
		}
	}
	return mtimes, nil
}

// manifestConflict reports whether a file would land on the download
// manifest's own path (a repo with an hfd.yaml at its root): flattening
// would overwrite it with the manifest.
func manifestConflict(files []flatFile) bool {
	for _, f := range files {
		if f.rel == ManifestFilename {
			return true
		}
	}
	return false
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
	if r.downloadInProgress(active) {
		return false, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	// Links in the friendly view that pointed into hub/ (files of older
	// commits, not part of this download) are now broken; drop them.
	_, _ = cleanOrphansInDir(r.FriendlyPath())
	return true, nil
}

// partialFiles counts the in-progress download files in blobs/ (see
// downloadInProgress), however old.
func (r *RepoDir) partialFiles() int {
	n := 0
	_ = filepath.WalkDir(r.BlobsDir(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && isPartialName(d.Name()) {
			n++
		}
		return nil
	})
	return n
}

func isPartialName(name string) bool {
	return strings.HasPrefix(name, "tmp-") || strings.HasSuffix(name, ".incomplete") || strings.HasSuffix(name, ".part")
}

// downloadInProgress reports whether a download is still writing into this
// repo's blobs/: an in-progress temp file modified within the last `active`.
func (r *RepoDir) downloadInProgress(active time.Duration) bool {
	busy := false
	cutoff := time.Now().Add(-active)
	_ = filepath.WalkDir(r.BlobsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !isPartialName(d.Name()) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(cutoff) {
			busy = true
			return fs.SkipAll
		}
		return nil
	})
	return busy
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

// FlattenOptions configures RepoDir.Flatten.
type FlattenOptions struct {
	// Revision is the branch, tag or commit to keep; empty means the one the
	// last download recorded in hfd.yaml, else "main", else the only cached
	// snapshot.
	Revision string
	// DropOtherRevisions allows flattening a repo that has other revisions
	// or unfinished (paused or interrupted) downloads cached: they are
	// deleted with the hub/ entry. Without it, Flatten returns an
	// *OtherRevisionsError.
	DropOtherRevisions bool
}

// FlattenResult summarizes a flatten.
type FlattenResult struct {
	Repo             string   `json:"repo"`
	Commit           string   `json:"commit"`
	Path             string   `json:"path"`
	Files            int      `json:"files"`
	Bytes            int64    `json:"bytes"`
	DroppedRevisions []string `json:"droppedRevisions,omitempty"`
}

// ErrAlreadyFlat is returned by Flatten for a repo that is already stored flat.
var ErrAlreadyFlat = errors.New("already stored as real files")

// OtherRevisionsError is returned by Flatten when the repo's hub/ entry
// holds data that flattening would delete: other cached revisions, or
// partial files of unfinished (paused or interrupted) downloads.
type OtherRevisionsError struct {
	Repo    string
	Commit  string
	Others  []string
	Partial int // partial download files
}

func (e *OtherRevisionsError) Error() string {
	var what []string
	if len(e.Others) > 0 {
		what = append(what, fmt.Sprintf("%d other cached revision(s) (%s)", len(e.Others), strings.Join(shortCommits(e.Others), ", ")))
	}
	if e.Partial > 0 {
		what = append(what, fmt.Sprintf("%d partial file(s) of an unfinished download", e.Partial))
	}
	return fmt.Sprintf("%s has %s that flattening to %s would delete", e.Repo, strings.Join(what, " and "), shortCommit(e.Commit))
}

func shortCommits(cs []string) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = shortCommit(c)
	}
	return out
}

// Flatten converts a repo already in the HF cache to flat storage, as if it
// had been downloaded with Settings.Flatten, without contacting the Hub:
// every file cached for one revision is moved from hub/ into the friendly
// view (models/<owner>/<name> or datasets/<owner>/<name>) as a real file,
// hfd.yaml is rewritten with "storage: flat", and the repo's hub/ entry is
// deleted.
//
// All files of the revision's snapshot are kept, not only the ones the last
// download recorded in hfd.yaml (two filtered downloads of the same commit
// each record only their own files). Repos downloaded by Python's
// huggingface_hub work too.
func (r *RepoDir) Flatten(opts FlattenOptions) (*FlattenResult, error) {
	if r.IsFlat() {
		return nil, fmt.Errorf("%s: %w in %s", r.RepoID(), ErrAlreadyFlat, r.FriendlyPath())
	}
	if fi, err := os.Lstat(r.Path()); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not in the cache (no %s)", r.RepoID(), r.Path())
	}

	prev, _ := ReadManifest(filepath.Join(r.FriendlyPath(), ManifestFilename))
	revision := opts.Revision
	if revision == "" && prev != nil && prev.Commit != "" {
		if fi, err := os.Stat(r.SnapshotDir(prev.Commit)); err == nil && fi.IsDir() && validSnapshotName(prev.Commit) {
			revision = prev.Commit
		}
	}
	commit, err := r.resolveCommit(revision)
	if err != nil {
		return nil, err
	}

	snaps, _ := r.ListSnapshots()
	var others []string
	for _, s := range snaps {
		if s != commit {
			others = append(others, s)
		}
	}
	if r.downloadInProgress(r.cache.StaleTimeout) {
		return nil, fmt.Errorf("%s: a download is still writing to %s; try again when it finishes", r.RepoID(), r.Path())
	}
	if partial := r.partialFiles(); (len(others) > 0 || partial > 0) && !opts.DropOtherRevisions {
		return nil, &OtherRevisionsError{Repo: r.RepoID(), Commit: commit, Others: others, Partial: partial}
	}

	// Hashes the last download recorded, by repo path.
	known := make(map[string]ManifestFile)
	if prev != nil && prev.Commit == commit {
		for _, f := range prev.Files {
			known[f.Name] = f
		}
	}

	entries, err := r.snapshotEntries(commit)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		// No snapshot links (Windows builds before v3.4.0): use the manifest.
		var missing []string
		entries, missing, err = r.manifestEntries(commit)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("%s: %d file(s) missing from the cache: %s", r.RepoID(), len(missing), strings.Join(missing, ", "))
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: no files cached for %s", r.RepoID(), shortCommit(commit))
	}

	var files []flatFile
	var mfiles []ManifestFile
	var total int64
	for _, e := range entries {
		if unsafeRepoPath(e.rel) {
			return nil, fmt.Errorf("unsafe path %q in snapshot", e.rel)
		}
		src := e.src
		if fi, err := os.Lstat(src); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(src)
			if err != nil {
				return nil, fmt.Errorf("%s: broken link: %w", e.rel, err)
			}
			src = real
		}
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.rel, err)
		}
		// Content hash for hfd.yaml, so a later --flatten download of the
		// repo can skip the file: recorded, else the blob's own name (ours
		// and huggingface_hub's LFS blobs), else computed (small files).
		sha := ""
		if f, ok := known[e.rel]; ok && validSHA256Hex(strings.TrimPrefix(f.Blob, "blobs/")) {
			sha = strings.TrimPrefix(f.Blob, "blobs/")
		} else if b := filepath.Base(src); validSHA256Hex(b) && filepath.Dir(src) == filepath.Clean(r.BlobsDir()) {
			sha = b
		} else if sha, err = computeSHA256(src); err != nil {
			return nil, fmt.Errorf("%s: %w", e.rel, err)
		}
		lfs := info.Size() > 10<<20
		if f, ok := known[e.rel]; ok {
			lfs = f.LFS
		}
		files = append(files, flatFile{rel: e.rel, name: e.rel, src: src})
		mfiles = append(mfiles, ManifestFile{Name: e.rel, Blob: "blobs/" + sha, Size: info.Size(), LFS: lfs})
		total += info.Size()
	}
	if manifestConflict(files) {
		return nil, fmt.Errorf("%s has its own %s, which would be overwritten by the download manifest; it can't be flattened", r.RepoID(), ManifestFilename)
	}

	mtimes, err := r.placeFlatFiles(files)
	if err != nil {
		return nil, fmt.Errorf("move files to %s (hub/ entry kept): %w", r.FriendlyPath(), err)
	}
	for i := range mfiles {
		mfiles[i].ModTime = mtimes[mfiles[i].Name]
	}

	m := &DownloadManifest{Version: "1.0", Type: string(r.repoType), Repo: r.RepoID(), Branch: "main", CompletedAt: time.Now().UTC()}
	if prev != nil {
		m.Branch, m.Command, m.StartedAt = prev.Branch, prev.Command, prev.StartedAt
		if !prev.CompletedAt.IsZero() {
			m.CompletedAt = prev.CompletedAt
		}
	}
	if opts.Revision != "" && opts.Revision != commit {
		m.Branch = opts.Revision
	}
	if m.Command == "" {
		m.Command = "hfdownloader flatten " + r.RepoID()
	}
	m.Commit, m.Storage, m.Files, m.TotalFiles, m.TotalSize = commit, StorageFlat, mfiles, len(mfiles), total
	if _, err := m.Write(r.FriendlyPath()); err != nil {
		return nil, fmt.Errorf("files are in %s, but writing %s failed (hub/ entry kept): %w", r.FriendlyPath(), ManifestFilename, err)
	}

	if _, err := os.Lstat(r.Path()); err == nil {
		if err := os.RemoveAll(r.Path()); err != nil {
			return nil, fmt.Errorf("files are in %s, but removing %s failed: %w", r.FriendlyPath(), r.Path(), err)
		}
	}
	_, _ = cleanOrphansInDir(r.FriendlyPath())

	return &FlattenResult{Repo: r.RepoID(), Commit: commit, Path: r.FriendlyPath(), Files: len(files), Bytes: total, DroppedRevisions: others}, nil
}
