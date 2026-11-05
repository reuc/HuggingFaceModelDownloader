// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log"

	"github.com/bodaay/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// convertSummary counts what convertOnBoot did with the repos in hub/.
// Remaining is how many were left for the next start because the server
// began shutting down.
type convertSummary struct {
	Converted, Skipped, Failed, Remaining int
}

// convertOnBoot converts every completed download in hub/ to real files in
// models/<owner>/<name> (or datasets/), like `hfdownloader flatten --all`
// (serve --convert-on-boot, CONVERT_ON_BOOT=true). It runs at startup before
// restored jobs resume and before the API is served, so nothing writes to
// hub/ meanwhile. Converted repos leave hub/, so later boots only pick up
// what's new.
//
// Nothing is ever deleted without asking: repos with a queued, running or
// paused download (their partial data is in hub/), and repos with other
// revisions or a leftover partial download cached, are skipped and logged;
// the Cache browser's Convert to real files asks before deleting those.
//
// When ctx is cancelled (the server is asked to stop), it stops before the
// next repo; the rest are converted on the next start. A repo already being
// converted is finished first (stopping mid-repo would be safe too: files
// are staged before anything moves, and hub/ is only removed at the end).
func (s *Server) convertOnBoot(ctx context.Context) convertSummary {
	var sum convertSummary
	cfg := s.config
	if cfg.LocalDir != "" {
		log.Printf("convert-on-boot: skipped, the server writes real files to --local-dir %s and doesn't use the HF cache", cfg.LocalDir)
		return sum
	}
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}
	cache := hfdownloader.NewHFCache(cacheDir, hfdownloader.DefaultStaleTimeout)
	repos, err := cache.ListRepos()
	if err != nil {
		log.Printf("convert-on-boot: could not list %s: %v", cache.HubDir(), err)
		return sum
	}
	if !cfg.Flatten {
		log.Printf("convert-on-boot: warning: --flatten is off, so new downloads will still be stored in %s (add --flatten, or FLATTEN=true in Docker)", cache.HubDir())
	}
	if len(repos) == 0 {
		return sum
	}

	log.Printf("convert-on-boot: converting completed downloads in %s to real files...", cache.HubDir())
	for i, rd := range repos {
		if ctx.Err() != nil {
			sum.Remaining = len(repos) - i
			log.Printf("convert-on-boot: stopping, the server is shutting down; %d repo(s) left for the next start", sum.Remaining)
			break
		}
		isDataset := rd.Type() == hfdownloader.RepoTypeDataset
		if s.jobs.HasActiveJob(rd.RepoID(), isDataset) {
			sum.Skipped++
			if cfg.Flatten {
				log.Printf("   skipped %s: it has a queued, running or paused download (it becomes real files when that finishes)", rd.RepoID())
			} else {
				log.Printf("   skipped %s: it has a queued, running or paused download", rd.RepoID())
			}
			continue
		}
		res, err := rd.Flatten(hfdownloader.FlattenOptions{})
		var other *hfdownloader.OtherRevisionsError
		switch {
		case err == nil:
			sum.Converted++
			log.Printf("   converted %s: %d file(s), %s -> %s", res.Repo, res.Files, humanSizeBytes(res.Bytes), res.Path)
		case errors.As(err, &other):
			sum.Skipped++
			log.Printf("   skipped %s: %v; to convert it anyway, use Convert to real files in the Cache browser (it asks first) or run `%s`", rd.RepoID(), err, flattenCommand(rd.RepoID(), isDataset))
		default:
			sum.Failed++
			log.Printf("   could not convert %s: %v", rd.RepoID(), err)
		}
	}
	log.Printf("convert-on-boot: %d converted, %d skipped, %d failed", sum.Converted, sum.Skipped, sum.Failed)
	return sum
}

// flattenCommand is the CLI command that converts one repo, deleting its
// other cached revisions and unfinished downloads.
func flattenCommand(repo string, isDataset bool) string {
	if isDataset {
		return "hfdownloader flatten --yes --dataset " + repo
	}
	return "hfdownloader flatten --yes " + repo
}
