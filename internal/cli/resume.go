// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/reuc/HuggingFaceModelDownloader/internal/tui"
	"github.com/reuc/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// runDownload runs a download with the progress display the global flags
// select, and records it so that a failed or interrupted run can be
// continued with `hfdownloader resume`. Such an error is returned as a
// *resumableError, which Execute follows with the resume command.
func runDownload(ctx context.Context, ro *RootOpts, job hfdownloader.Job, cfg hfdownloader.Settings) error {
	tr := newDownloadTracker(defaultRecordStore(), job, cfg)

	var progress hfdownloader.ProgressFunc
	closeUI := func() {}
	switch {
	case ro.JSONOut:
		progress = jsonProgress(os.Stdout)
	case ro.Quiet:
		progress = cliProgress(ro, job)
	default:
		ui := tui.NewLiveRenderer(job, cfg)
		closeUI = ui.Close
		progress = ui.Handler()
	}

	err := hfdownloader.Download(ctx, job, cfg, tr.wrap(progress))
	closeUI()
	if tr.finish(ctx, err) {
		return &resumableError{err: err, id: tr.id()}
	}
	return err
}

// resumeHint is printed after the error of a download that can be resumed.
func resumeHint(w io.Writer, id string) {
	fmt.Fprintf(w, "Partially downloaded files were kept. To continue, run:\n  hfdownloader resume %s\n", id)
}

// resumeOpts are the resume command's flags.
type resumeOpts struct {
	list, all, forget bool

	// Transfer tuning to change for this attempt (and later ones).
	connections, maxActive, retries          int
	backoffInitial, backoffMax, stallTimeout string

	proxyURL, proxyUser, proxyPass string
	noEnvProxy                     bool
}

func newResumeCmd(ctx context.Context, ro *RootOpts) *cobra.Command {
	o := &resumeOpts{}
	cmd := &cobra.Command{
		Use:   "resume [ID|REPO]",
		Short: "Continue a download that failed or was interrupted",
		Long: `Continue a download that failed or was interrupted, with the same repo,
revision, file selection, destination and settings as the original command.
Files that are already complete are skipped and partially downloaded ones
continue from where they stopped.

Every download started from the CLI is recorded until it completes, so a
failure, Ctrl+C, a crash or a reboot leaves it here to resume. With no
argument, the only resumable download is resumed; when there are several,
pass its ID (or a unique prefix) or the repo, or use --all.

The access token is not recorded: pass --token, set HF_TOKEN or keep it in
the config file, as for any download. Proxy credentials are not recorded
either; they come from the config file or --proxy-user/--proxy-pass.`,
		Example: `  hfdownloader resume                 # resume the failed/interrupted download
  hfdownloader resume --list          # show resumable downloads
  hfdownloader resume 3f9a1c2b        # resume one by ID (or ID prefix)
  hfdownloader resume owner/repo      # resume by repo
  hfdownloader resume --all           # resume all, one after another
  hfdownloader resume --retries 20 -c 4 3f9a1c2b   # retry harder this time
  hfdownloader resume --forget 3f9a1c2b            # drop it from the list`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResume(ctx, cmd, ro, o, args)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&o.list, "list", false, "List failed and interrupted downloads and exit")
	f.BoolVar(&o.all, "all", false, "Resume every failed or interrupted download (matching the ID or repo, if given), one after another")
	f.BoolVar(&o.forget, "forget", false, "Remove the selected download(s) from the list instead of resuming; partial files stay in the cache and a later download of the same files continues from them")
	f.IntVarP(&o.connections, "connections", "c", 0, "Per-file concurrent connections (default: as recorded)")
	f.IntVar(&o.maxActive, "max-active", 0, "Maximum number of files downloading at once (default: as recorded)")
	f.IntVar(&o.retries, "retries", 0, "Max retry attempts per HTTP request/part (default: as recorded)")
	f.StringVar(&o.backoffInitial, "backoff-initial", "", "Initial retry backoff duration (default: as recorded)")
	f.StringVar(&o.backoffMax, "backoff-max", "", "Maximum retry backoff duration (default: as recorded)")
	f.StringVar(&o.stallTimeout, "stall-timeout", "", "Retry a transfer that receives no data for this long (default: as recorded)")
	f.StringVarP(&o.proxyURL, "proxy", "x", "", "Proxy URL (default: as recorded, else the config file)")
	f.StringVar(&o.proxyUser, "proxy-user", "", "Proxy authentication username")
	f.StringVar(&o.proxyPass, "proxy-pass", "", "Proxy authentication password")
	f.BoolVar(&o.noEnvProxy, "no-env-proxy", false, "Ignore HTTP_PROXY/HTTPS_PROXY environment variables")
	return cmd
}

func runResume(ctx context.Context, cmd *cobra.Command, ro *RootOpts, o *resumeOpts, args []string) error {
	store := defaultRecordStore()
	recs, err := store.list()
	if err != nil {
		return fmt.Errorf("read download records in %s: %w", store.dir, err)
	}
	now := time.Now()
	out := cmd.OutOrStdout()

	if o.list {
		if len(args) > 0 || o.all || o.forget {
			return errors.New("--list can't be combined with an ID, --all or --forget")
		}
		return printRecords(out, recs, now, ro.JSONOut)
	}

	arg := ""
	if len(args) > 0 {
		arg = strings.TrimSpace(args[0])
	}
	if o.forget && arg == "" && !o.all {
		return errors.New("--forget needs an ID or repo, or --all")
	}
	selected, err := pickRecords(recs, arg, o.all, now)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		if !ro.JSONOut {
			fmt.Fprintln(out, "No failed or interrupted downloads to resume.")
		}
		return nil
	}

	if o.forget {
		for _, r := range selected {
			if err := store.remove(r.ID); err != nil {
				return err
			}
			if !ro.JSONOut {
				fmt.Fprintf(out, "Forgot %s  %s\n", r.ID, r.label())
			}
		}
		return nil
	}

	// Token and proxy settings from the config file, as a download would use.
	var fromConfig hfdownloader.Settings
	if err := applySettingsDefaults(cmd, ro, &fromConfig); err != nil {
		return err
	}

	var failed []string
	for i, r := range selected {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job, cfg := r.job(), r.settings()
		applyResumeOverrides(cmd, o, &cfg)
		cfg.Proxy = resumeProxy(cmd, o, fromConfig.Proxy, cfg.Proxy)
		cfg.Token = strings.TrimSpace(ro.Token)
		if cfg.Token == "" {
			cfg.Token = strings.TrimSpace(os.Getenv("HF_TOKEN"))
		}

		if !ro.JSONOut {
			fmt.Fprintf(out, "Resuming %s  %s (attempt %d)\n", r.ID, r.label(), r.Attempts+1)
		}
		err := runDownload(ctx, ro, job, cfg)
		if err == nil {
			continue
		}
		if len(selected) == 1 || ctx.Err() != nil {
			return err
		}
		// --all: report this one and carry on with the rest.
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", r.label(), err)
		failed = append(failed, r.ID)
		if i < len(selected)-1 && !ro.JSONOut {
			fmt.Fprintln(out)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d downloads did not finish (%s); see: hfdownloader resume --list", len(failed), len(selected), strings.Join(failed, ", "))
	}
	return nil
}

// pickRecords selects the records a resume (or --forget) applies to: those
// matching arg (an ID, ID prefix or repo; empty matches all) that aren't
// still running. Without --all exactly one must be resumable.
func pickRecords(recs []*downloadRecord, arg string, all bool, now time.Time) ([]*downloadRecord, error) {
	var matched, ready []*downloadRecord
	var running *downloadRecord
	for _, r := range recs {
		if arg != "" && !matchesRecord(r, arg) {
			continue
		}
		matched = append(matched, r)
		if r.state(now) == statusRunning {
			if running == nil {
				running = r
			}
			continue
		}
		ready = append(ready, r)
	}

	switch {
	case arg != "" && len(matched) == 0:
		return nil, fmt.Errorf("no failed or interrupted download matches %q (see: hfdownloader resume --list)", arg)
	case all || len(ready) == 1:
		return ready, nil
	case len(ready) > 1:
		var b strings.Builder
		if arg == "" {
			fmt.Fprintf(&b, "%d downloads can be resumed; pass an ID or repo, or --all:\n", len(ready))
		} else {
			fmt.Fprintf(&b, "%q matches %d downloads; pass one of their IDs, or --all:\n", arg, len(ready))
		}
		for _, r := range ready {
			fmt.Fprintf(&b, "  %s  %s\n", r.ID, r.label())
		}
		return nil, errors.New(strings.TrimRight(b.String(), "\n"))
	case running != nil && arg != "":
		// Two processes writing the same partial files would corrupt them.
		return nil, fmt.Errorf("%s (%s) is still downloading in another process (pid %d on %s); wait for it to finish, or stop it and resume again", running.label(), running.ID, running.PID, running.Host)
	default:
		return nil, nil
	}
}

// matchesRecord reports whether arg names r: its ID, a prefix of at least 4
// characters of it, or its repo (case-insensitive, optionally @revision).
func matchesRecord(r *downloadRecord, arg string) bool {
	a := strings.ToLower(arg)
	if a == r.ID || (len(a) >= 4 && strings.HasPrefix(r.ID, a)) {
		return true
	}
	repo := strings.ToLower(r.Job.Repo)
	return a == repo || a == repo+"@"+strings.ToLower(r.Job.Revision)
}

// applyResumeOverrides applies the tuning flags given to resume.
func applyResumeOverrides(cmd *cobra.Command, o *resumeOpts, cfg *hfdownloader.Settings) {
	f := cmd.Flags()
	if f.Changed("connections") {
		cfg.Concurrency = o.connections
	}
	if f.Changed("max-active") {
		cfg.MaxActiveDownloads = o.maxActive
	}
	if f.Changed("retries") {
		cfg.Retries = o.retries
	}
	if f.Changed("backoff-initial") {
		cfg.BackoffInitial = o.backoffInitial
	}
	if f.Changed("backoff-max") {
		cfg.BackoffMax = o.backoffMax
	}
	if f.Changed("stall-timeout") {
		cfg.StallTimeout = o.stallTimeout
	}
}

// resumeProxy combines proxy settings the way a download does: the config
// file first, then what the original command chose (recorded), then the
// flags given to resume.
func resumeProxy(cmd *cobra.Command, o *resumeOpts, fromConfig, recorded *hfdownloader.ProxyConfig) *hfdownloader.ProxyConfig {
	var p *hfdownloader.ProxyConfig
	if fromConfig != nil {
		c := *fromConfig
		p = &c
	}
	ensure := func() {
		if p == nil {
			p = &hfdownloader.ProxyConfig{}
		}
	}
	if recorded != nil {
		ensure()
		if recorded.URL != "" {
			p.URL = recorded.URL
		}
		p.NoEnvProxy = recorded.NoEnvProxy
	}
	f := cmd.Flags()
	if f.Changed("proxy") {
		ensure()
		p.URL = o.proxyURL
	}
	if f.Changed("proxy-user") {
		ensure()
		p.Username = o.proxyUser
	}
	if f.Changed("proxy-pass") {
		ensure()
		p.Password = o.proxyPass
	}
	if f.Changed("no-env-proxy") {
		ensure()
		p.NoEnvProxy = o.noEnvProxy
	}
	return p
}

// listedRecord is a record as `resume --list --json` prints it.
type listedRecord struct {
	*downloadRecord
	State string `json:"state"`
}

// printRecords lists the recorded downloads, most recently active first.
func printRecords(w io.Writer, recs []*downloadRecord, now time.Time, asJSON bool) error {
	if asJSON {
		out := make([]listedRecord, 0, len(recs))
		for _, r := range recs {
			out = append(out, listedRecord{downloadRecord: r, State: r.state(now)})
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(recs) == 0 {
		fmt.Fprintln(w, "No failed or interrupted downloads.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPROGRESS\tLAST ACTIVE\tDOWNLOAD")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.state(now), progressSummary(r.Progress), r.UpdatedAt.Local().Format("2006-01-02 15:04"), r.label())
		if r.Error != "" {
			fmt.Fprintf(tw, "\t  error: %s\n", firstLine(r.Error, 120))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nResume one with: hfdownloader resume <ID>   (or --all)")
	return nil
}

func progressSummary(p recordProgress) string {
	if p.FilesTotal == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d files, %s of %s", p.FilesDone, p.FilesTotal, humanSize(p.BytesDone), humanSize(p.BytesTotal))
}

// firstLine returns the first line of s, cut to at most n characters.
func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}
