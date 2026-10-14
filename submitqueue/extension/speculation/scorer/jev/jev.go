// Copyright (c) 2026 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package jev prices a batch by asking a System One decision model (TypeSafe
// Jev, reached through a LiteLLM proxy) for the probability that the batch
// builds and lands. See README.md.
package jev

import (
	"context"
	"fmt"
	nethttp "net/http"
	"sort"
	"strings"
	"time"

	"github.com/uber-go/tally"
	"go.uber.org/zap"

	"github.com/uber/submitqueue/platform/metrics"
	"github.com/uber/submitqueue/submitqueue/core/changeset"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/changediff"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer"
)

// questionName keys the single noul question in every request.
const questionName = "batch_lands"

// maxFilesInState caps the files described to the model per batch. Beyond it
// the state carries only totals, so one sweeping refactor cannot blow the
// request size or the per-token cost.
const maxFilesInState = 200

// DefaultMaxDiffBytes and DefaultMaxContextBytes cap the diff text, and the
// file contents and references, sent per batch. Context is spent only after
// every diff, so it never displaces a change's own code. A diff past its cap is
// cut at a line boundary and marked truncated. Size them to the model's context
// window: the request also carries JSON escaping, file stats and the question.
const (
	DefaultMaxDiffBytes    = 64 << 10
	DefaultMaxContextBytes = 64 << 10
)

// requestBytesBuckets span 1KB to 512KB; a backend's context limit shows as
// the size above which requests fail.
var requestBytesBuckets = tally.MustMakeExponentialValueBuckets(1<<10, 2, 10)

// DefaultMaxAheadBytes caps the diff text of the changes ahead sent per batch.
// It is a budget of its own, so context from the queue never displaces the
// batch's own code.
const DefaultMaxAheadBytes = 32 << 10

// landsOnTopQuestion replaces landsQuestion when the state carries changes
// ahead. It asks about the batch alone, built on top of them: a change ahead
// that fails by itself is not this batch's failure.
var landsOnTopQuestion = systemOneQuestion{
	Type:         questionTypeNoul,
	Instructions: "Will this batch of code changes pass its CI build and merge cleanly when it is built on top of changes_ahead? Those changes are queued in front of this batch and land first; they are not part of it. Judge whether the two still build and pass together, in either direction: whether this batch relies on code that changes_ahead renames, removes, redefines or changes the signature of, and whether changes_ahead relies on code that this batch does, or both define the same name. A change may also include modified_files_after_change, the full text of files it modifies, and repository_context, unmodified code elsewhere in the repository that relates to the change.",
	Criteria: &noulCriteria{
		True:  "The batch builds and its tests pass on top of changes_ahead, and it merges without conflict.",
		False: "With changes_ahead in place, the batch fails to build, breaks tests, or cannot be merged.",
	},
}

var landsQuestion = systemOneQuestion{
	Type:         questionTypeNoul,
	Instructions: "Will this batch of code changes pass its CI build and merge cleanly into the target branch? Judge the code in each change's diff. A change may also include modified_files_after_change, the full text of files it modifies, and repository_context, unmodified code elsewhere in the repository that relates to the change.",
	Criteria: &noulCriteria{
		True:  "The batch builds, its tests pass, and it merges without conflict.",
		False: "The batch fails to build, breaks tests, or cannot be merged.",
	},
}

// Params configures a jev scorer.
type Params struct {
	// Config is the per-queue identity this scorer serves.
	Config scorer.Config
	// HTTPClient must be rooted at the LiteLLM base URL and authenticate every
	// request. Nil means the model is unreachable and every score falls back.
	HTTPClient *nethttp.Client
	// Path is the System One route on the proxy. Empty means DefaultPath.
	Path string
	// Model is the LiteLLM model name to ask.
	Model string
	// ExpectModel, when set, is the concrete model version an answer must name;
	// an answer from any other is a failure. A proxy alias or pass-through can
	// route to another model, whose prices are not comparable.
	ExpectModel string
	// Timeout bounds one model call. Zero means only ctx bounds it.
	Timeout time.Duration
	// Resolver resolves a batch into its changes. Nil makes every score fall
	// back.
	Resolver changeset.Resolver
	// Differ supplies each change's code. Nil sends file names and line counts
	// only; a Differ error makes the score fall back.
	Differ changediff.Provider
	// MaxDiffBytes and MaxContextBytes cap the code sent per batch. Zero means
	// DefaultMaxDiffBytes and DefaultMaxContextBytes.
	MaxDiffBytes, MaxContextBytes int
	// Ahead loads the batches a batch depends on, whose changes are then sent
	// as changes_ahead: queued in front and landing first. Nil sends none, and
	// the request is exactly what it is without the option.
	Ahead BatchLoader
	// MaxAheadBytes caps the diff text of changes ahead per batch. Zero means
	// DefaultMaxAheadBytes.
	MaxAheadBytes int
	// Cache is shared across scorers built from one configuration. Nil disables
	// caching.
	Cache *Cache
	// Fallback prices a batch the model could not. Nil returns the error
	// instead, which the generator replaces with its own default.
	Fallback scorer.Scorer
	// Logger receives one warning per fallback.
	Logger *zap.SugaredLogger
	// MetricsScope receives score latency and cache/fallback counters.
	MetricsScope tally.Scope
}

// BatchLoader reads a batch by ID. A batch names its dependencies by ID only,
// and resolving their changes needs the batch itself.
type BatchLoader interface {
	Batch(ctx context.Context, queue, id string) (entity.Batch, error)
}

type jevScorer struct {
	p Params
}

// New returns a scorer.Scorer backed by a System One model. Model prices are
// cached by queue and batch ID; failures are not cached, so the next run asks
// again.
func New(p Params) scorer.Scorer {
	if p.Logger == nil {
		p.Logger = zap.NewNop().Sugar()
	}
	if p.MetricsScope == nil {
		p.MetricsScope = tally.NoopScope
	}
	if p.Path == "" {
		p.Path = DefaultPath
	}
	if p.MaxDiffBytes <= 0 {
		p.MaxDiffBytes = DefaultMaxDiffBytes
	}
	if p.MaxContextBytes <= 0 {
		p.MaxContextBytes = DefaultMaxContextBytes
	}
	if p.MaxAheadBytes <= 0 {
		p.MaxAheadBytes = DefaultMaxAheadBytes
	}
	return &jevScorer{p: p}
}

func (s *jevScorer) Score(ctx context.Context, batch entity.Batch, paths entity.SpeculationPathSet) (ret float64, retErr error) {
	op := metrics.Begin(s.p.MetricsScope, "score", metrics.FastLatencyBuckets)
	defer func() { op.Complete(retErr) }()

	key := batch.Queue + "/" + batch.ID
	if s.p.Ahead != nil {
		// A batch's dependencies can shrink as the batches ahead finish, and
		// with them the price; the key carries them so a stale price is a miss.
		deps := append([]string(nil), batch.Dependencies...)
		sort.Strings(deps)
		key += "?ahead=" + strings.Join(deps, ",")
	}
	if s.p.Cache != nil {
		if price, ok := s.p.Cache.get(key); ok {
			metrics.NamedCounter(s.p.MetricsScope, "score", "cache_hit", 1)
			return price, nil
		}
	}

	price, err := s.askModel(ctx, batch)
	if err == nil {
		if s.p.Cache != nil {
			s.p.Cache.put(key, price)
		}
		return price, nil
	}
	if s.p.Fallback == nil {
		return 0, err
	}
	metrics.NamedCounter(s.p.MetricsScope, "score", "fallback", 1)
	s.p.Logger.Warnw("jev scorer falling back", "queue", batch.Queue, "batch", batch.ID, "error", err)
	return s.p.Fallback.Score(ctx, batch, paths)
}

func (s *jevScorer) askModel(ctx context.Context, batch entity.Batch) (float64, error) {
	if s.p.HTTPClient == nil {
		return 0, fmt.Errorf("jev scorer has no endpoint configured")
	}
	if s.p.Resolver == nil {
		return 0, fmt.Errorf("jev scorer has no change resolver")
	}
	changes, err := s.p.Resolver.DetailedForBatch(ctx, batch)
	if err != nil {
		return 0, fmt.Errorf("resolve changes for batch %s: %w", batch.ID, err)
	}
	content, err := s.contentForBatch(ctx, batch)
	if err != nil {
		return 0, err
	}
	if s.p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.p.Timeout)
		defer cancel()
	}
	state := batchState(changes, content, s.p.MaxDiffBytes, s.p.MaxContextBytes)
	question := landsQuestion
	ahead, err := s.changesAhead(ctx, batch)
	if err != nil {
		return 0, err
	}
	if ahead != nil {
		state.ChangesAhead = ahead
		question = landsOnTopQuestion
	}
	ans, err := askNoul(ctx, s.p.HTTPClient, s.p.Path, s.p.Model, s.p.ExpectModel, questionName, question, state)
	if ans.RequestBytes > 0 {
		metrics.NamedHistogram(s.p.MetricsScope, "score", "request_bytes", requestBytesBuckets).RecordValue(float64(ans.RequestBytes))
	}
	if err != nil {
		return 0, err
	}
	s.p.Logger.Infow("jev scored batch", "queue", batch.Queue, "batch", batch.ID, "model", ans.Model, "probability", ans.P,
		"request_bytes", ans.RequestBytes, "input_tokens", ans.InputTokens)
	return ans.P, nil
}

// contentForBatch returns each change's code keyed by URI, or nil without a
// Differ.
func (s *jevScorer) contentForBatch(ctx context.Context, batch entity.Batch) (map[string]entity.ChangeDiff, error) {
	if s.p.Differ == nil {
		return nil, nil
	}
	raw, err := s.p.Resolver.ChangesForBatch(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("resolve change URIs for batch %s: %w", batch.ID, err)
	}
	content := make(map[string]entity.ChangeDiff)
	for _, c := range raw {
		diffs, err := s.p.Differ.Diff(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("diff changes for batch %s: %w", batch.ID, err)
		}
		for _, d := range diffs {
			content[d.URI] = d
		}
	}
	return content, nil
}

// changesAhead describes the changes of batch's unfinished dependencies, or
// nil when there are none or no Ahead loader. A dependency that has finished is
// skipped: one that landed is already in the target, and one that failed or
// was cancelled never will be. Only diffs are sent for them, never full files
// or references, and a diff that cannot be read is a failure as for the
// batch's own.
func (s *jevScorer) changesAhead(ctx context.Context, batch entity.Batch) (*changesAheadState, error) {
	if s.p.Ahead == nil || len(batch.Dependencies) == 0 {
		return nil, nil
	}
	deps := append([]string(nil), batch.Dependencies...)
	sort.Strings(deps)
	var all entity.BatchChanges
	content := map[string]entity.ChangeDiff{}
	for _, id := range deps {
		dep, err := s.p.Ahead.Batch(ctx, batch.Queue, id)
		if err != nil {
			return nil, fmt.Errorf("load batch %s ahead of %s: %w", id, batch.ID, err)
		}
		if dep.State.IsTerminal() {
			continue
		}
		changes, err := s.p.Resolver.DetailedForBatch(ctx, dep)
		if err != nil {
			return nil, fmt.Errorf("resolve changes for batch %s ahead of %s: %w", id, batch.ID, err)
		}
		all.Changes = append(all.Changes, changes.Changes...)
		code, err := s.contentForBatch(ctx, dep)
		if err != nil {
			return nil, err
		}
		for uri, d := range code {
			content[uri] = entity.ChangeDiff{URI: uri, Patch: d.Patch}
		}
	}
	if len(all.Changes) == 0 {
		return nil, nil
	}
	doc := batchState(all, content, s.p.MaxAheadBytes, 0)
	return &changesAheadState{Description: changesAheadDescription, Changes: doc.Changes}, nil
}

// The types below are the JSON the model reads. Field names are chosen to be
// self-describing to it, and the change is kept apart from the repository
// context around it so neither can be mistaken for the other.
type changeState struct {
	URI           string      `json:"uri"`
	Author        string      `json:"author,omitempty"`
	FileStats     []fileState `json:"file_stats,omitempty"`
	Diff          string      `json:"diff,omitempty"`
	DiffTruncated bool        `json:"diff_truncated,omitempty"`
	// ModifiedFiles and RepositoryContext are absent without a Differ asked
	// for them.
	ModifiedFiles     []fileContentState      `json:"modified_files_after_change,omitempty"`
	RepositoryContext *repositoryContextState `json:"repository_context,omitempty"`
	ContextTruncated  bool                    `json:"context_truncated,omitempty"`
}

type fileState struct {
	Path     string `json:"path"`
	Added    int    `json:"lines_added"`
	Deleted  int    `json:"lines_deleted"`
	Modified int    `json:"lines_modified,omitempty"`
}

type fileContentState struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// repositoryContextDescription labels the block for the model, which otherwise
// sees it nested under a change and could read it as part of that change.
const repositoryContextDescription = "Unmodified code elsewhere in the repository, read at this change's head. This change does not modify any of it."

type repositoryContextState struct {
	Description string            `json:"description"`
	Callers     []callerState     `json:"callers_of_changed_declarations,omitempty"`
	Definitions []definitionState `json:"definitions_of_called_functions,omitempty"`
}

type callerState struct {
	Symbol            string        `json:"symbol"`
	DeclarationBefore string        `json:"declaration_before_change"`
	DeclarationAfter  string        `json:"declaration_after_change"`
	CallSite          codeLineState `json:"call_site"`
}

type definitionState struct {
	Symbol     string        `json:"symbol"`
	Definition codeLineState `json:"definition"`
}

type codeLineState struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// removedDeclaration stands in for an empty after-declaration, which would
// otherwise read as missing data rather than as a deletion.
const removedDeclaration = "(removed by this change)"

// changesAheadDescription labels the changes ahead for the model, which
// otherwise could read them as part of the batch it is judging.
const changesAheadDescription = "Changes queued ahead of this batch. They land first and this batch is built on top of them; they are not part of this batch."

type changesAheadState struct {
	Description string        `json:"description"`
	Changes     []changeState `json:"changes"`
}

type batchStateDoc struct {
	Queue             string        `json:"queue"`
	ChangeCount       int           `json:"change_count"`
	TotalFiles        int           `json:"total_files_changed"`
	TotalLinesChanged int           `json:"total_lines_changed"`
	FilesTruncated    bool          `json:"files_truncated,omitempty"`
	Changes           []changeState `json:"changes"`
	// ChangesAhead is absent without an Ahead loader or unfinished
	// dependencies.
	ChangesAhead *changesAheadState `json:"changes_ahead,omitempty"`
}

// batchState describes a batch to the model. Changes are sorted by URI so the
// same batch always produces the same request.
func batchState(changes entity.BatchChanges, content map[string]entity.ChangeDiff, maxDiffBytes, maxContextBytes int) batchStateDoc {
	infos := append([]entity.ChangeInfo(nil), changes.Changes...)
	sort.Slice(infos, func(i, j int) bool { return infos[i].URI < infos[j].URI })

	doc := batchStateDoc{
		Queue:             changes.Queue,
		ChangeCount:       len(infos),
		TotalFiles:        changes.TotalFiles(),
		TotalLinesChanged: changes.TotalLinesChanged(),
		Changes:           make([]changeState, 0, len(infos)),
	}
	budget := maxFilesInState
	diffBudget := maxDiffBytes
	for _, info := range infos {
		cs := changeState{URI: info.URI, Author: info.Details.Author.Email}
		if cs.Author == "" {
			cs.Author = info.Details.Author.Name
		}
		for _, f := range info.Details.ChangedFiles {
			if budget == 0 {
				doc.FilesTruncated = true
				break
			}
			cs.FileStats = append(cs.FileStats, fileState{Path: f.Path, Added: f.LinesAdded, Deleted: f.LinesDeleted, Modified: f.LinesModified})
			budget--
		}
		cs.Diff, cs.DiffTruncated = truncatePatch(content[info.URI].Patch, diffBudget)
		diffBudget -= len(cs.Diff)
		doc.Changes = append(doc.Changes, cs)
	}

	contextBudget := maxContextBytes
	for i := range doc.Changes {
		contextBudget = addContext(&doc.Changes[i], content[doc.Changes[i].URI], contextBudget)
	}
	return doc
}

// addContext fills cs's files and repository context from code within budget
// bytes and returns what is left. The fixed per-reference cost approximates
// its JSON keys.
func addContext(cs *changeState, code entity.ChangeDiff, budget int) int {
	for _, f := range code.Files {
		text, truncated := truncatePatch(f.Content, budget-len(f.Path))
		if text == "" {
			cs.ContextTruncated = true
			break
		}
		cs.ModifiedFiles = append(cs.ModifiedFiles, fileContentState{Path: f.Path, Content: text, Truncated: truncated})
		budget -= len(f.Path) + len(text)
	}
	if len(code.References) == 0 {
		return budget
	}
	repoCtx := &repositoryContextState{Description: repositoryContextDescription}
	for _, r := range code.References {
		cost := len(r.Symbol) + len(r.Path) + len(r.Text) + len(r.DeclarationBefore) + len(r.DeclarationAfter) + 96
		if cost > budget {
			cs.ContextTruncated = true
			break
		}
		budget -= cost
		site := codeLineState{Path: r.Path, Line: r.Line, Text: r.Text}
		switch r.Kind {
		case entity.CodeReferenceKindCaller:
			after := r.DeclarationAfter
			if after == "" {
				after = removedDeclaration
			}
			repoCtx.Callers = append(repoCtx.Callers, callerState{Symbol: r.Symbol, DeclarationBefore: r.DeclarationBefore, DeclarationAfter: after, CallSite: site})
		case entity.CodeReferenceKindDefinition:
			repoCtx.Definitions = append(repoCtx.Definitions, definitionState{Symbol: r.Symbol, Definition: site})
		}
	}
	if len(repoCtx.Callers)+len(repoCtx.Definitions) > 0 {
		cs.RepositoryContext = repoCtx
	}
	return budget
}

// truncatePatch cuts patch to at most limit bytes at a line boundary.
func truncatePatch(patch string, limit int) (string, bool) {
	if len(patch) <= limit {
		return patch, false
	}
	if limit <= 0 {
		return "", true
	}
	cut := patch[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i+1]
	} else {
		cut = ""
	}
	return cut, true
}
