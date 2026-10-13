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
	timeout                                      time.Duration
	diff, files, references                      bool
	maxDiffBytes, maxContextBytes                int
	batchSize, batches, concurrency, limit, bins int
	seed                                         int64
}

// evalRow is one scored batch.
type evalRow struct {
	Batch    string   `json:"batch"`
	Branches []string `json:"branches"`
	Lands    bool     `json:"lands"`
	// Slices are the result groups the row is graded in.
	Slices    []string           `json:"slices"`
	DiffBytes int                `json:"diff_bytes"`
	Scores    map[string]float64 `json:"scores"`
	Errors    map[string]string  `json:"errors,omitempty"`
	LatencyMs map[string]int64   `json:"latency_ms"`
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
	fs.StringVar(&f.scorers, "scorers", "size,jev", "comma-separated scorers to grade: size, jev")
	fs.StringVar(&f.out, "out", "", "write every row and report as JSON here")
	fs.StringVar(&f.baseURL, "base-url", "", "jev: proxy root; defaults to $LITE_URL")
	fs.StringVar(&f.baseURLEnv, "base-url-env", "LITE_URL", "jev: environment variable holding the proxy root")
	fs.StringVar(&f.tokenEnv, "token-env", "LITE_KEY", "jev: environment variable holding the virtual key")
	fs.StringVar(&f.path, "path", jev.DefaultPath, "jev: System One route")
	fs.StringVar(&f.model, "model", "jev", "jev: proxy model name")
	fs.DurationVar(&f.timeout, "timeout", 60*time.Second, "jev: bound on one model call")
	fs.BoolVar(&f.diff, "diff", true, "jev: send each change's diff")
	fs.BoolVar(&f.files, "files", false, "jev: also send touched files in full")
	fs.BoolVar(&f.references, "references", false, "jev: also send callers and definitions")
	fs.IntVar(&f.maxDiffBytes, "max-diff-bytes", 0, "jev: diff budget per batch; zero is the scorer's default")
	fs.IntVar(&f.maxContextBytes, "max-context-bytes", 0, "jev: context budget per batch; zero is the scorer's default")
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

	names, scorers, err := buildScorers(f, resolver, repo)
	if err != nil {
		return err
	}
	batches, rows := planEvalBatches(changes, f, rand.New(rand.NewSource(f.seed)))
	logf("scoring %d batches with %s", len(batches), strings.Join(names, ", "))
	scoreRows(ctx, batches, rows, names, scorers, f.concurrency)

	result := evalResult{Dataset: f.dataset, Scorers: names, Rows: rows, Reports: gradeRows(rows, names, f.bins)}
	printReports(result)
	if f.out != "" {
		return writeJSON(f.out, result)
	}
	return nil
}

func buildScorers(f evalFlags, resolver changeset.Resolver, repo *gitrepo.Repo) ([]string, map[string]scorer.Scorer, error) {
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
				Config: cfg, HTTPClient: client, Path: f.path, Model: f.model, Timeout: f.timeout,
				Resolver: resolver, Differ: differ, MaxDiffBytes: f.maxDiffBytes, MaxContextBytes: f.maxContextBytes,
			})
		default:
			return nil, nil, fmt.Errorf("unknown scorer %q; want size or jev", name)
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
	client.Transport = &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), Base: client.Transport}
	return client, nil
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

// planEvalBatches returns one batch per change, then f.batches sampled
// batches of f.batchSize distinct changes. A batch lands only if every change
// in it does, matching the replay runner.
func planEvalBatches(changes []ManifestChange, f evalFlags, r *rand.Rand) ([]entity.Batch, []evalRow) {
	var batches []entity.Batch
	var rows []evalRow
	add := func(members []int, slices []string) {
		b := entity.Batch{ID: strconv.Itoa(len(batches) + 1), Queue: evalQueue}
		row := evalRow{Batch: b.ID, Lands: true, Slices: slices}
		for _, i := range members {
			b.Contains = append(b.Contains, strconv.Itoa(i+1))
			row.Branches = append(row.Branches, changes[i].Branch)
			row.Lands = row.Lands && changes[i].Lands
			row.DiffBytes += changes[i].DiffBytes
		}
		batches = append(batches, b)
		rows = append(rows, row)
	}
	for i, c := range changes {
		add([]int{i}, singleSlices(c))
	}
	if f.batchSize > 1 && f.batchSize <= len(changes) {
		for n := 0; n < f.batches; n++ {
			add(r.Perm(len(changes))[:f.batchSize], []string{fmt.Sprintf("batch of %d", f.batchSize)})
		}
	}
	return batches, rows
}

func singleSlices(c ManifestChange) []string {
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
					p, err := scorers[name].Score(ctx, batches[i], entity.SpeculationPathSet{})
					row.LatencyMs[name] = time.Since(began).Milliseconds()
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
		for _, row := range res.Rows {
			if _, ok := row.Errors[name]; ok {
				failed++
			}
		}
		fmt.Printf("\n%s: %d batches, %d failed to score\n", name, len(res.Rows), failed)
		fmt.Printf("%-34s %4s %5s %6s %6s %7s %6s %7s %7s\n", "slice", "n", "lands", "auc", "brier", "logloss", "ece", "p|lands", "p|fails")
		reports := res.Reports[name]
		slices := make([]string, 0, len(reports))
		for s := range reports {
			slices = append(slices, s)
		}
		sort.Slice(slices, func(i, j int) bool { return sliceOrder(slices[i]) < sliceOrder(slices[j]) })
		for _, s := range slices {
			r := reports[s]
			auc := "    -"
			if r.AUCDefined {
				auc = fmt.Sprintf("%6.3f", r.AUC)
			}
			fmt.Printf("%-34s %4d %5d %6s %6.3f %7.3f %6.3f %7s %7s\n", s, r.N, r.Lands, auc, r.Brier, r.LogLoss, r.ECE,
				meanOrDash(r.MeanLands, r.Lands), meanOrDash(r.MeanFails, r.N-r.Lands))
		}
	}
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
