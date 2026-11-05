// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"log"

	"github.com/bodaay/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// convertSummary counts what convertOnBoot did with the repos in hub/.
type convertSummary struct {
	Converted, Skipped, Failed int
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
func (s *Server) convertOnBoot() convertSummary {
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
		log.Printf("convert-on-boot: warning: --flatten is off, so new downloads will still be stored in %s (set FLATTEN=true too)", cache.HubDir())
	}
	if len(repos) == 0 {
		return sum
	}

	log.Printf("convert-on-boot: converting completed downloads in %s to real files...", cache.HubDir())
	for _, rd := range repos {
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
			log.Printf("   skipped %s: %v; use Convert to real files in the Cache browser (it asks first) or `hfdownloader flatten --yes`", rd.RepoID(), err)
		default:
			sum.Failed++
			log.Printf("   could not convert %s: %v", rd.RepoID(), err)
		}
	}
	log.Printf("convert-on-boot: %d converted, %d skipped, %d failed", sum.Converted, sum.Skipped, sum.Failed)
	return sum
}
