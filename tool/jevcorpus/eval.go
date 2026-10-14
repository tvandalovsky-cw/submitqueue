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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	nethttp "net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uber-go/tally"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	changegit "github.com/uber/submitqueue/platform/base/change/git"
	gitrepo "github.com/uber/submitqueue/platform/git/repo"
	phttp "github.com/uber/submitqueue/platform/http"
	"github.com/uber/submitqueue/submitqueue/core/changeset"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
	"github.com/uber/submitqueue/submitqueue/extension/changediff"
	gitdiffer "github.com/uber/submitqueue/submitqueue/extension/changediff/git"
	"github.com/uber/submitqueue/submitqueue/extension/changeprovider"
	gitprovider "github.com/uber/submitqueue/submitqueue/extension/changeprovider/git"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer/jev"
	"github.com/uber/submitqueue/tool/jevcorpus/grade"
)

const (
	evalQueue  = "corpus-queue"
	evalRemote = "git.example.com"
	evalRepo   = "sandbox"
)

// sizeScale is the line count at which the size baseline prices a batch at
// 0.5. Only its ranking matters to AUC; it stands for a scorer that has
// learned nothing but "bigger is riskier".
const sizeScale = 250

type evalFlags struct {
	dataset, manifest, sandbox, scorers, out     string
	baseURL, baseURLEnv, tokenEnv, path, model   string
	expectModel, cacheDir                        string
	timeout                                      time.Duration
	diff, files, references, ahead               bool
	maxDiffBytes, maxContextBytes, maxAheadBytes int
	batchSize, batches, concurrency, limit, bins int
	seed                                         int64
}

// evalRow is one scored batch.
type evalRow struct {
	Batch    string   `json:"batch"`
	Branches []string `json:"branches"`
	Lands    bool     `json:"lands"`
	// Slices are the result groups the row is graded in.
	Slices []string `json:"slices"`
	// Compiles is whether the batch's changes pass the compile check, as
	// recorded at generation.
	Compiles  bool               `json:"compiles"`
	DiffBytes int                `json:"diff_bytes"`
	Scores    map[string]float64 `json:"scores"`
	Errors    map[string]string  `json:"errors,omitempty"`
	LatencyMs map[string]int64   `json:"latency_ms"`
	// Calls are the model requests made while scoring the row, by scorer.
	Calls map[string][]modelCall `json:"calls,omitempty"`
}

type evalResult struct {
	Dataset string                             `json:"dataset"`
	Scorers []string                           `json:"scorers"`
	Rows    []evalRow                          `json:"rows"`
	Reports map[string]map[string]grade.Report `json:"reports"`
}

func runEval(args []string) error {
	var f evalFlags
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	fs.StringVar(&f.dataset, "dataset", "", "dataset written by generate (required)")
	fs.StringVar(&f.manifest, "manifest", "", "manifest written by generate; defaults to the one beside -dataset")
	fs.StringVar(&f.sandbox, "sandbox", "", "sandbox repository written by generate (required)")
	fs.StringVar(&f.scorers, "scorers", "size,jev", "comma-separated scorers to grade: size, compile, jev")
	fs.StringVar(&f.out, "out", "", "write every row and report as JSON here")
	fs.StringVar(&f.baseURL, "base-url", "", "jev: proxy root; defaults to $LITE_URL")
	fs.StringVar(&f.baseURLEnv, "base-url-env", "LITE_URL", "jev: environment variable holding the proxy root")
	fs.StringVar(&f.tokenEnv, "token-env", "LITE_KEY", "jev: environment variable holding the virtual key")
	fs.StringVar(&f.path, "path", jev.DefaultPath, "jev: System One route")
	fs.StringVar(&f.model, "model", "jev", "jev: proxy model name")
	fs.StringVar(&f.expectModel, "expect-model", "", "jev: fail any answer whose response names another model, e.g. jev-1.13.0; empty accepts any")
	fs.StringVar(&f.cacheDir, "cache", "", "jev: directory of responses keyed by exact request bytes; a repeated request is answered from it")
	fs.DurationVar(&f.timeout, "timeout", 60*time.Second, "jev: bound on one model call")
	fs.BoolVar(&f.diff, "diff", true, "jev: send each change's diff")
	fs.BoolVar(&f.files, "files", false, "jev: also send touched files in full")
	fs.BoolVar(&f.references, "references", false, "jev: also send callers and definitions")
	fs.BoolVar(&f.ahead, "ahead", false, "also score each pair side with the other side queued ahead of it, sent as changes_ahead")
	fs.IntVar(&f.maxDiffBytes, "max-diff-bytes", 0, "jev: diff budget per batch; zero is the scorer's default")
	fs.IntVar(&f.maxContextBytes, "max-context-bytes", 0, "jev: context budget per batch; zero is the scorer's default")
	fs.IntVar(&f.maxAheadBytes, "max-ahead-bytes", 0, "jev: diff budget for changes ahead; zero is the scorer's default")
	fs.IntVar(&f.batchSize, "batch-size", 3, "changes per multi-change batch")
	fs.IntVar(&f.batches, "batches", 0, "multi-change batches to sample in addition to one batch per change")
	fs.IntVar(&f.concurrency, "concurrency", 4, "batches scored at once")
	fs.IntVar(&f.limit, "limit", 0, "score only the first n changes; zero scores all")
	fs.IntVar(&f.bins, "bins", 10, "calibration bins")
	fs.Int64Var(&f.seed, "seed", 1, "random seed for sampling multi-change batches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dataset == "" || f.sandbox == "" {
		return errors.New("-dataset and -sandbox are required")
	}
	if f.manifest == "" {
		f.manifest = manifestPathFor(f.dataset)
	}
	return evaluate(context.Background(), f)
}

func evaluate(ctx context.Context, f evalFlags) error {
	ds, err := replay.Load(f.dataset)
	if err != nil {
		return err
	}
	m, err := loadManifest(f.manifest)
	if err != nil {
		return err
	}
	bySHA := make(map[string]ManifestChange, len(m.Changes))
	for _, c := range m.Changes {
		bySHA[c.SHA] = c
	}

	sandbox, err := filepath.Abs(f.sandbox)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "jevcorpus-eval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	repo, err := gitrepo.NewRepo(gitrepo.RepoConfig{Path: filepath.Join(tmp, "copy"), RemoteURL: sandbox, Target: ds.BaseBranch})
	if err != nil {
		return err
	}
	if err := repo.Provision(ctx); err != nil {
		return err
	}
	provider := gitprovider.New(gitprovider.Params{
		Config: changeprovider.Config{QueueName: evalQueue}, Repo: repo,
		Logger: zap.NewNop().Sugar(), MetricsScope: tally.NoopScope,
	})

	var changes []ManifestChange
	resolver := corpusResolver{queue: evalQueue, provider: provider, uriByRequest: map[string]string{}}
	for _, a := range ds.Attempts {
		if a.ReplaySHA == "" {
			continue
		}
		mc, ok := bySHA[a.ReplaySHA]
		if !ok {
			return fmt.Errorf("dataset change %s is missing from the manifest", a.ReplaySHA)
		}
		if f.limit > 0 && len(changes) == f.limit {
			break
		}
		changes = append(changes, mc)
		resolver.uriByRequest[strconv.Itoa(len(changes))] = changegit.ChangeID{
			Scheme: "git", Remote: evalRemote, Repo: evalRepo, Ref: "refs/heads/" + mc.Branch, CommitSHA: mc.SHA,
		}.String()
	}
	if len(changes) == 0 {
		return errors.New("dataset has no materialized changes")
	}

	batches, rows := planEvalBatches(changes, m.Pairs, f, rand.New(rand.NewSource(f.seed)))
	loader := batchesByID{}
	for _, b := range batches {
		loader[b.ID] = b
	}
	compiled := map[string]bool{}
	for _, row := range rows {
		compiled[row.Batch] = row.Compiles
	}
	names, scorers, err := buildScorers(f, resolver, repo, loader, compileScorer(compiled))
	if err != nil {
		return err
	}
	logf("scoring %d batches with %s", len(batches), strings.Join(names, ", "))
	scoreRows(ctx, batches, rows, names, scorers, f.concurrency)

	result := evalResult{Dataset: f.dataset, Scorers: names, Rows: rows, Reports: gradeRows(rows, names, f.bins)}
	printReports(result)
	if f.out != "" {
		return writeJSON(f.out, result)
	}
	return nil
}

func buildScorers(f evalFlags, resolver changeset.Resolver, repo *gitrepo.Repo, ahead jev.BatchLoader, compile scorer.Scorer) ([]string, map[string]scorer.Scorer, error) {
	cfg := scorer.Config{QueueName: evalQueue}
	scorers := map[string]scorer.Scorer{}
	var names []string
	for _, name := range strings.Split(f.scorers, ",") {
		name = strings.TrimSpace(name)
		switch name {
		case "":
			continue
		case "size":
			scorers[name] = sizeScorer{resolver: resolver}
		case "compile":
			scorers[name] = compile
		case "jev":
			client, err := jevClient(f)
			if err != nil {
				return nil, nil, err
			}
			var differ changediff.Provider
			if f.diff {
				differ = gitdiffer.New(gitdiffer.Params{
					Config: changediff.Config{QueueName: evalQueue}, Repo: repo, MetricsScope: tally.NoopScope,
					IncludeFiles: f.files, IncludeReferences: f.references,
				})
			}
			// No fallback and no cache: a failure must show as a failure, and
			// every batch is asked once.
			scorers[name] = jev.New(jev.Params{
				Config: cfg, HTTPClient: client, Path: f.path, Model: f.model, ExpectModel: f.expectModel, Timeout: f.timeout,
				Resolver: resolver, Differ: differ, MaxDiffBytes: f.maxDiffBytes, MaxContextBytes: f.maxContextBytes,
				Ahead: ahead, MaxAheadBytes: f.maxAheadBytes,
			})
		default:
			return nil, nil, fmt.Errorf("unknown scorer %q; want size, compile or jev", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, nil, errors.New("-scorers names no scorer")
	}
	return names, scorers, nil
}

func jevClient(f evalFlags) (*nethttp.Client, error) {
	baseURL := f.baseURL
	if baseURL == "" {
		baseURL = os.Getenv(f.baseURLEnv)
	}
	token := os.Getenv(f.tokenEnv)
	if baseURL == "" || token == "" {
		return nil, fmt.Errorf("jev needs a proxy root (-base-url or $%s) and a key ($%s); drop it from -scorers to grade baselines only", f.baseURLEnv, f.tokenEnv)
	}
	client, err := phttp.NewClient(baseURL)
	if err != nil {
		return nil, err
	}
	client.Timeout = f.timeout
	client.Transport = &modelCallTransport{
		base:     &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), Base: client.Transport},
		cacheDir: f.cacheDir,
	}
	return client, nil
}

// compileScorer prices a batch from the compile check recorded at
// generation: a type-checker's verdict, with no model. It is the baseline for
// any conflict a compiler can see, and blind to one that only tests reveal.
type compileScorer map[string]bool

func (c compileScorer) Score(_ context.Context, batch entity.Batch, _ entity.SpeculationPathSet) (float64, error) {
	if c[batch.ID] {
		return 0.99, nil
	}
	return 0.01, nil
}

// batchesByID is a jev.BatchLoader over the batches the evaluation planned,
// standing in for the orchestrator's batch store.
type batchesByID map[string]entity.Batch

func (m batchesByID) Batch(_ context.Context, _, id string) (entity.Batch, error) {
	b, ok := m[id]
	if !ok {
		return entity.Batch{}, fmt.Errorf("no planned batch %s", id)
	}
	return b, nil
}

// sizeScorer prices a batch by its size alone, falling as lines grow.
type sizeScorer struct {
	resolver changeset.Resolver
}

func (s sizeScorer) Score(ctx context.Context, batch entity.Batch, _ entity.SpeculationPathSet) (float64, error) {
	changes, err := s.resolver.DetailedForBatch(ctx, batch)
	if err != nil {
		return 0, err
	}
	return 1 / (1 + float64(changes.TotalLinesChanged())/sizeScale), nil
}

// planEvalBatches returns one batch per change, one batch per pair holding
// both sides, then f.batches sampled batches of f.batchSize distinct single
// changes. A sampled batch lands only if every change in it does, matching the
// replay runner; a pair lands as it was verified together.
func planEvalBatches(changes []ManifestChange, pairs []ManifestPair, f evalFlags, r *rand.Rand) ([]entity.Batch, []evalRow) {
	var batches []entity.Batch
	var rows []evalRow
	add := func(members []int, lands, compiles bool, slices []string, ahead ...string) {
		b := entity.Batch{ID: strconv.Itoa(len(batches) + 1), Queue: evalQueue, Dependencies: ahead}
		row := evalRow{Batch: b.ID, Lands: lands, Compiles: compiles, Slices: slices}
		for _, i := range members {
			b.Contains = append(b.Contains, strconv.Itoa(i+1))
			row.Branches = append(row.Branches, changes[i].Branch)
			row.DiffBytes += changes[i].DiffBytes
		}
		batches = append(batches, b)
		rows = append(rows, row)
	}
	indexOf := map[string]int{}
	var singles []int
	for i, c := range changes {
		indexOf[c.Branch] = i
		add([]int{i}, c.Lands, c.Compiles, singleSlices(c))
		if c.Pair == "" {
			singles = append(singles, i)
		}
	}
	for _, p := range pairs {
		a, okA := indexOf[p.A]
		b, okB := indexOf[p.B]
		if !okA || !okB {
			continue
		}
		add([]int{a, b}, p.Lands, p.CompilesTogether, []string{"pairs", "pair " + pairFamily(p.Mutator), "pair " + p.Mutator})
		if f.ahead {
			// Each change's own batch has the ID of its position, as added
			// above; the other side is named as a dependency by that ID.
			family := "ahead " + pairFamily(p.Mutator)
			add([]int{b}, p.Lands, p.CompilesTogether, []string{"ahead", family, "ahead " + p.Mutator, "ahead: B with A ahead"}, strconv.Itoa(a+1))
			add([]int{a}, p.Lands, p.CompilesTogether, []string{"ahead", family, "ahead " + p.Mutator, "ahead: A with B ahead"}, strconv.Itoa(b+1))
		}
	}
	if f.batchSize > 1 && f.batchSize <= len(singles) {
		for n := 0; n < f.batches; n++ {
			var members []int
			lands, compiles := true, true
			for _, k := range r.Perm(len(singles))[:f.batchSize] {
				members = append(members, singles[k])
				lands = lands && changes[singles[k]].Lands
				compiles = compiles && changes[singles[k]].Compiles
			}
			add(members, lands, compiles, []string{fmt.Sprintf("batch of %d", f.batchSize)})
		}
	}
	return batches, rows
}

// pairFamily groups a conflicting pair mutator with its compatible twin, so a
// family's AUC asks whether a scorer tells the two apart.
func pairFamily(mutator string) string {
	if family, _, ok := strings.Cut(mutator, "-vs-"); ok {
		return "family " + family
	}
	return "family helper"
}

func singleSlices(c ManifestChange) []string {
	if c.Pair != "" {
		return []string{"pair member alone"}
	}
	slices := []string{"all", "tier " + string(c.Tier), "category " + c.Category()}
	if c.Needle.Position != NeedleNone {
		slices = append(slices, "needle "+string(c.Needle.Position))
		if c.Needle.BeyondDiffBudget {
			slices = append(slices, "needle beyond diff budget")
		}
	}
	return slices
}

func scoreRows(ctx context.Context, batches []entity.Batch, rows []evalRow, names []string, scorers map[string]scorer.Scorer, concurrency int) {
	work := make(chan int)
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	finished := 0
	for w := 0; w < max(concurrency, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				row := &rows[i]
				row.Scores, row.Errors, row.LatencyMs = map[string]float64{}, map[string]string{}, map[string]int64{}
				for _, name := range names {
					began := time.Now()
					callCtx, calls := withModelCallLog(ctx)
					p, err := scorers[name].Score(callCtx, batches[i], entity.SpeculationPathSet{})
					row.LatencyMs[name] = time.Since(began).Milliseconds()
					if made := calls.list(); len(made) > 0 {
						if row.Calls == nil {
							row.Calls = map[string][]modelCall{}
						}
						row.Calls[name] = made
					}
					if err != nil {
						row.Errors[name] = err.Error()
						continue
					}
					row.Scores[name] = p
				}
				progressMu.Lock()
				finished++
				if finished%10 == 0 || finished == len(rows) {
					logf("scored %d/%d", finished, len(rows))
				}
				progressMu.Unlock()
			}
		}()
	}
	for i := range rows {
		work <- i
	}
	close(work)
	wg.Wait()
}

func gradeRows(rows []evalRow, names []string, bins int) map[string]map[string]grade.Report {
	out := map[string]map[string]grade.Report{}
	for _, name := range names {
		bySlice := map[string][]grade.Prediction{}
		for _, row := range rows {
			p, ok := row.Scores[name]
			if !ok {
				continue
			}
			for _, s := range row.Slices {
				bySlice[s] = append(bySlice[s], grade.Prediction{P: p, Lands: row.Lands})
			}
		}
		out[name] = map[string]grade.Report{}
		for s, preds := range bySlice {
			out[name][s] = grade.Grade(preds, bins)
		}
	}
	return out
}

func printReports(res evalResult) {
	for _, name := range res.Scorers {
		failed := 0
		failedBySlice := map[string]int{}
		for _, row := range res.Rows {
			if _, ok := row.Errors[name]; ok {
				failed++
				for _, s := range row.Slices {
					failedBySlice[s]++
				}
			}
		}
		fmt.Printf("\n%s: %d batches, %d failed to score\n", name, len(res.Rows), failed)
		if line := describeModelCalls(res.Rows, name); line != "" {
			fmt.Println(line)
		}
		// Failed rows are left out of every metric, so a slice whose failures
		// share a cause (size, say) is graded on a biased sample.
		fmt.Printf("%-34s %4s %4s %5s %6s %6s %7s %6s %7s %7s\n", "slice", "n", "fail", "lands", "auc", "brier", "logloss", "ece", "p|lands", "p|fails")
		reports := res.Reports[name]
		slices := make([]string, 0, len(reports))
		for s := range reports {
			slices = append(slices, s)
		}
		for s := range failedBySlice {
			if _, ok := reports[s]; !ok {
				slices = append(slices, s)
			}
		}
		sort.Slice(slices, func(i, j int) bool { return sliceOrder(slices[i]) < sliceOrder(slices[j]) })
		for _, s := range slices {
			r := reports[s]
			auc := "    -"
			if r.AUCDefined {
				auc = fmt.Sprintf("%6.3f", r.AUC)
			}
			fmt.Printf("%-34s %4d %4d %5d %6s %6.3f %7.3f %6.3f %7s %7s\n", s, r.N, failedBySlice[s], r.Lands, auc, r.Brier, r.LogLoss, r.ECE,
				meanOrDash(r.MeanLands, r.Lands), meanOrDash(r.MeanFails, r.N-r.Lands))
		}
	}
}

// describeModelCalls summarizes the model requests a scorer made: which models
// answered, how many came from the cache, and the request sizes that succeeded
// and failed, which locate a backend's size limit. Empty for a scorer that
// made none.
func describeModelCalls(rows []evalRow, name string) string {
	models := map[string]int{}
	calls, cached, maxOK, minFailed, maxTokens := 0, 0, 0, 0, 0
	for _, row := range rows {
		for _, c := range row.Calls[name] {
			calls++
			if c.Cached {
				cached++
			}
			if c.Status >= 200 && c.Status < 300 {
				models[c.Model]++
				maxOK = max(maxOK, c.RequestBytes)
				maxTokens = max(maxTokens, c.InputTokens)
			} else if minFailed == 0 || c.RequestBytes < minFailed {
				minFailed = c.RequestBytes
			}
		}
	}
	if calls == 0 {
		return ""
	}
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, fmt.Sprintf("%s=%d", m, models[m]))
	}
	sort.Strings(names)
	line := fmt.Sprintf("  calls %d (cached %d); answered by %s; largest answered request %d bytes", calls, cached, strings.Join(names, ", "), maxOK)
	if maxTokens > 0 {
		line += fmt.Sprintf(" (max %d input tokens)", maxTokens)
	}
	if minFailed > 0 {
		line += fmt.Sprintf("; smallest failed request %d bytes", minFailed)
	}
	return line
}

// sliceOrder sorts "all" first, then tiers in size order, then everything
// else alphabetically.
func sliceOrder(s string) string {
	if s == "all" {
		return "0"
	}
	for i, t := range tiers {
		if s == "tier "+string(t) {
			return "1" + strconv.Itoa(i)
		}
	}
	return "2" + s
}

func meanOrDash(mean float64, n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", mean)
}
