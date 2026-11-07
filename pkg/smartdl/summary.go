// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// RepoSummary is the one-row overview of a repo shown in the web UI's bulk
// download list. Fields the repo doesn't reveal are left empty.
type RepoSummary struct {
	Repo      string   `json:"repo"`
	IsDataset bool     `json:"is_dataset"`
	Type      RepoType `json:"type"`

	Architecture  string `json:"architecture,omitempty"`
	Parameters    string `json:"parameters,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Precision     string `json:"precision,omitempty"`
	// PrecisionDetail lists everything Precision abbreviates (e.g. every
	// GGUF quantization).
	PrecisionDetail string `json:"precision_detail,omitempty"`

	Size      int64  `json:"size"`
	SizeHuman string `json:"size_human"`
	FileCount int    `json:"file_count"`

	// Ambiguous is set when the repo also exists as a dataset; the model is
	// summarized.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

// hubModelInfo is the part of /api/models/{repo}?expand[]=... used here.
type hubModelInfo struct {
	Safetensors *struct {
		Parameters map[string]int64 `json:"parameters"`
		Total      int64            `json:"total"`
	} `json:"safetensors"`
	GGUF *struct {
		Total         int64  `json:"total"`
		Architecture  string `json:"architecture"`
		ContextLength int    `json:"context_length"`
	} `json:"gguf"`
	Config map[string]interface{} `json:"config"`
}

// Summarize analyzes repo's main revision and condenses it into a
// RepoSummary. kind is "model", "dataset" or "" to auto-detect; an
// auto-detected repo that is both is summarized as the model.
func (a *Analyzer) Summarize(ctx context.Context, repo, kind string) (*RepoSummary, error) {
	info, err := a.analyze(ctx, repo, kind == "dataset", kind == "model", "main")
	ambiguous := false
	if errors.Is(err, ErrBothExist) {
		ambiguous = true
		info, err = a.analyze(ctx, repo, false, true, "main")
	}
	if err != nil {
		return nil, err
	}

	var hub *hubModelInfo
	if !info.IsDataset {
		hub, _ = a.fetchHubModelInfo(ctx, repo) // non-fatal: the analysis has fallbacks
	}
	s := summarize(info, hub)
	s.Ambiguous = ambiguous
	return s, nil
}

// fetchHubModelInfo fetches the Hub's parameter counts and GGUF metadata.
func (a *Analyzer) fetchHubModelInfo(ctx context.Context, repo string) (*hubModelInfo, error) {
	reqURL := fmt.Sprintf("%s/api/models/%s?expand[]=safetensors&expand[]=gguf&expand[]=config", a.endpoint, pathEscapeAll(repo))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	a.addAuth(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model info: %s", resp.Status)
	}
	var out hubModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// summarize builds a RepoSummary from an analysis and the Hub's model info
// (nil if unavailable).
func summarize(info *RepoInfo, hub *hubModelInfo) *RepoSummary {
	s := &RepoSummary{
		Repo:      info.Repo,
		IsDataset: info.IsDataset,
		Type:      info.Type,
		Size:      info.TotalSize,
		SizeHuman: info.TotalSizeHuman,
		FileCount: info.FileCount,
	}
	if info.IsDataset {
		return s
	}
	if hub == nil {
		hub = &hubModelInfo{}
	}
	cfg, _ := info.Metadata["config.json"].(map[string]interface{})
	if cfg == nil {
		cfg = hub.Config
	}

	// Architecture
	switch {
	case info.Transformers != nil && info.Transformers.Architecture != "":
		s.Architecture = info.Transformers.Architecture
	case info.Quantized != nil && info.Quantized.ModelArchitecture != "":
		s.Architecture = info.Quantized.ModelArchitecture
	case hub.GGUF != nil && hub.GGUF.Architecture != "":
		s.Architecture = hub.GGUF.Architecture
	case info.Diffusers != nil && info.Diffusers.PipelineType != "":
		s.Architecture = info.Diffusers.PipelineType
	default:
		s.Architecture = configArchitecture(cfg)
		if s.Architecture == "" {
			s.Architecture = configArchitecture(hub.Config)
		}
	}
	if s.Architecture == "" && info.LoRA != nil && info.LoRA.BaseModel != "" {
		s.Architecture = "LoRA of " + info.LoRA.BaseModel
	}

	// Parameters: the Hub's exact counts, then the analyzer's estimates.
	switch {
	case hub.Safetensors != nil && hub.Safetensors.Total > 0:
		s.Parameters = formatParameterCount(hub.Safetensors.Total)
	case hub.GGUF != nil && hub.GGUF.Total > 0:
		s.Parameters = formatParameterCount(hub.GGUF.Total)
	case info.Transformers != nil && info.Transformers.EstimatedParameters != "":
		s.Parameters = "~" + info.Transformers.EstimatedParameters
	case info.GGUF != nil && info.GGUF.ParameterCount != "":
		s.Parameters = info.GGUF.ParameterCount
	}

	// Context length: the trained maximum, not sliding_window.
	if s.ContextLength = configContextLength(cfg); s.ContextLength == 0 {
		if hub.GGUF != nil && hub.GGUF.ContextLength > 0 {
			s.ContextLength = hub.GGUF.ContextLength
		} else if info.Transformers != nil {
			s.ContextLength = info.Transformers.ContextLength
		}
	}

	s.Precision, s.PrecisionDetail = summaryPrecision(info, hub)
	if d := info.Diffusers; d != nil && len(d.Variants) > 0 && s.Precision != "" {
		// SDXL: fp32 weights plus fp16 variant files.
		s.Precision += " (+" + strings.ToUpper(strings.Join(d.Variants, ", ")) + " variant)"
	}
	return s
}

// configArchitecture returns architectures[0] (or model_type) from a config.
func configArchitecture(cfg map[string]interface{}) string {
	if cfg == nil {
		return ""
	}
	if archs, ok := cfg["architectures"].([]interface{}); ok && len(archs) > 0 {
		if arch, ok := archs[0].(string); ok && arch != "" {
			return arch
		}
	}
	if mt, ok := cfg["model_type"].(string); ok {
		return mt
	}
	return ""
}

// configContextLength reads the context window from a config, including the
// text_config / llm_config of vision-language models.
func configContextLength(cfg map[string]interface{}) int {
	if cfg == nil {
		return 0
	}
	for _, key := range []string{"max_position_embeddings", "max_seq_len", "max_sequence_length", "seq_length", "n_positions", "n_ctx"} {
		if v, ok := cfg[key].(float64); ok && v > 0 {
			return int(v)
		}
	}
	for _, sub := range []string{"text_config", "llm_config", "language_config"} {
		if c, ok := cfg[sub].(map[string]interface{}); ok {
			if n := configContextLength(c); n > 0 {
				return n
			}
		}
	}
	return 0
}

// summaryPrecision describes the weights' precision or quantization, plus
// the full list when the short form abbreviates it.
func summaryPrecision(info *RepoInfo, hub *hubModelInfo) (string, string) {
	if info.GGUF != nil && len(info.GGUF.Quantizations) > 0 {
		names := make([]string, len(info.GGUF.Quantizations))
		for i, q := range info.GGUF.Quantizations {
			names[i] = q.Name
		}
		sort.Strings(names)
		all := strings.Join(names, ", ")
		if len(names) <= 3 {
			return all, ""
		}
		return fmt.Sprintf("GGUF (%d quants)", len(names)), all
	}

	if q := info.Quantized; q != nil && q.Method != "" {
		method := strings.ToUpper(q.Method)
		switch {
		case len(info.QuantBranches) > 0:
			names := make([]string, len(info.QuantBranches))
			for i, b := range info.QuantBranches {
				names[i] = b.Name
			}
			return fmt.Sprintf("%s (%d bitrates)", method, len(names)), strings.Join(names, ", ")
		case q.BitsPerWeight > 0:
			return fmt.Sprintf("%s %sbpw", method, strconv.FormatFloat(q.BitsPerWeight, 'f', -1, 64)), ""
		case q.Bits > 0:
			return fmt.Sprintf("%s %d-bit", method, q.Bits), ""
		}
		return method, ""
	}

	if p, detail := safetensorsPrecision(hub); p != "" {
		return p, detail
	}

	if info.Transformers != nil && info.Transformers.Precision != "" {
		return strings.ToUpper(info.Transformers.Precision), ""
	}
	return "", ""
}

// safetensorsPrecision returns the Hub's safetensors dtypes, dominant first
// (e.g. "BF16" and detail "BF16, F32"), or "" if unknown.
func safetensorsPrecision(hub *hubModelInfo) (string, string) {
	if hub == nil || hub.Safetensors == nil || len(hub.Safetensors.Parameters) == 0 {
		return "", ""
	}
	p := hub.Safetensors.Parameters
	dtypes := make([]string, 0, len(p))
	for d := range p {
		dtypes = append(dtypes, d)
	}
	sort.Slice(dtypes, func(i, j int) bool {
		if p[dtypes[i]] != p[dtypes[j]] {
			return p[dtypes[i]] > p[dtypes[j]]
		}
		return dtypes[i] < dtypes[j]
	})
	detail := ""
	if len(dtypes) > 1 {
		detail = strings.Join(dtypes, ", ")
	}
	return dtypes[0], detail
}
