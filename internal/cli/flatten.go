// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bodaay/HuggingFaceModelDownloader/pkg/hfdownloader"
)

func newFlattenCmd(ro *RootOpts) *cobra.Command {
	var (
		cacheDir  string
		revision  string
		isDataset bool
		all       bool
		yes       bool
	)

	cmd := &cobra.Command{
		Use:   "flatten [<repo>...] | --all",
		Short: "Convert downloaded repos to real files in models/ (removes them from hub/)",
		Long: `Convert repos already in the HuggingFace cache to flat storage, as if they
had been downloaded with --flatten: every file cached for one revision is
moved from hub/ into models/<owner>/<name> (or datasets/<owner>/<name>) as
a real file, and the repo's hub/ entry is deleted. Nothing is downloaded and
each file is stored once.

The revision kept is the one the last download recorded in hfd.yaml, else
main (choose another with -b). If other revisions of a repo are cached, or
partial files of an unfinished download, they would be deleted too, so the
repo is skipped unless you pass --yes.

Python libraries can't find a flattened repo by its id any more; load it by
folder path instead, e.g. AutoModel.from_pretrained("<cache>/models/owner/name").

Examples:
  hfdownloader flatten TheBloke/Mistral-7B-Instruct-v0.2-GGUF
  hfdownloader flatten --dataset nyu-mll/glue
  hfdownloader flatten --all
  hfdownloader flatten --all --yes     # also repos with other revisions cached`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name one or more repos, or pass --all")
			}
			if all && revision != "" {
				return errors.New("--revision can't be combined with --all")
			}
			if cacheDir == "" {
				if cfg := loadConfigMap(); cfg != nil {
					if v, ok := cfg["cache-dir"].(string); ok && v != "" {
						cacheDir = v
					}
				}
			}
			if cacheDir == "" {
				cacheDir = hfdownloader.DefaultCacheDir()
			}
			cache := hfdownloader.NewHFCache(cacheDir, hfdownloader.DefaultStaleTimeout)

			var repos []*hfdownloader.RepoDir
			if all {
				list, err := cache.ListRepos()
				if err != nil {
					return err
				}
				repos = list
			} else {
				for _, repo := range args {
					if !hfdownloader.IsValidModelName(repo) {
						return fmt.Errorf("invalid repo id %q (expected owner/name)", repo)
					}
					repoType := hfdownloader.RepoTypeModel
					if isDataset {
						repoType = hfdownloader.RepoTypeDataset
					}
					rd, err := cache.Repo(repo, repoType)
					if err != nil {
						return err
					}
					if !isDataset {
						// Fall back to a cached dataset of that name.
						if _, err := os.Stat(rd.Path()); os.IsNotExist(err) && !rd.IsFlat() {
							if ds, err := cache.Repo(repo, hfdownloader.RepoTypeDataset); err == nil {
								if _, err := os.Stat(ds.Path()); err == nil {
									rd = ds
								}
							}
						}
					}
					repos = append(repos, rd)
				}
			}
			if len(repos) == 0 {
				if !ro.Quiet {
					fmt.Println("Nothing to flatten: no repos in", cache.HubDir())
				}
				return nil
			}

			type outcome struct {
				Repo   string                      `json:"repo"`
				Result *hfdownloader.FlattenResult `json:"result,omitempty"`
				Error  string                      `json:"error,omitempty"`
			}
			var outcomes []outcome
			failed := 0
			for _, rd := range repos {
				res, err := rd.Flatten(hfdownloader.FlattenOptions{Revision: revision, DropOtherRevisions: yes})
				o := outcome{Repo: rd.RepoID(), Result: res}
				// Already flat is what was asked for: not a failure.
				if err != nil && !errors.Is(err, hfdownloader.ErrAlreadyFlat) {
					o.Error = err.Error()
					failed++
				}
				outcomes = append(outcomes, o)
				if ro.JSONOut || ro.Quiet {
					continue
				}
				var other *hfdownloader.OtherRevisionsError
				switch {
				case err == nil:
					fmt.Printf("Flattened %s (%s): %d files, %s -> %s\n", res.Repo, shortCommit(res.Commit), res.Files, humanSize(res.Bytes), res.Path)
					if len(res.DroppedRevisions) > 0 {
						fmt.Printf("  Deleted %d other cached revision(s)\n", len(res.DroppedRevisions))
					}
				case errors.As(err, &other):
					fmt.Fprintf(os.Stderr, "Skipped %s: %v; pass --yes to flatten it anyway\n", rd.RepoID(), err)
				case errors.Is(err, hfdownloader.ErrAlreadyFlat):
					fmt.Printf("Already flat: %s (%s)\n", rd.RepoID(), rd.FriendlyPath())
				default:
					fmt.Fprintf(os.Stderr, "Failed %s: %v\n", rd.RepoID(), err)
				}
			}
			if ro.JSONOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(outcomes); err != nil {
					return err
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d repo(s) not flattened", failed, len(repos))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "HuggingFace cache directory (default: ~/.cache/huggingface or HF_HOME)")
	cmd.Flags().StringVarP(&revision, "revision", "b", "", "Branch, tag or commit to keep (default: the last downloaded one, else main)")
	cmd.Flags().BoolVar(&isDataset, "dataset", false, "The repo is a dataset")
	cmd.Flags().BoolVar(&all, "all", false, "Flatten every repo in hub/")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Also flatten repos with other revisions or unfinished downloads cached (those are deleted)")
	return cmd
}
