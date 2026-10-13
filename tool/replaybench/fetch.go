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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
)

// workflowRun is the subset of a GitHub Actions run the dataset needs.
type workflowRun struct {
	HeadSHA      string    `json:"head_sha"`
	HeadBranch   string    `json:"head_branch"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	CreatedAt    time.Time `json:"created_at"`
	RunStartedAt time.Time `json:"run_started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// groupBranch is GitHub's merge-group branch: gh-readonly-queue/<base>/pr-<n>-<parent sha>.
var groupBranch = regexp.MustCompile(`^gh-readonly-queue/(.+)/pr-(\d+)-([0-9a-f]{40})$`)

func runFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	repo := fs.String("repo", "", "upstream repository, owner/name (required)")
	baseBranch := fs.String("base-branch", "main", "branch the merge queue lands onto")
	since := fs.String("since", "", "first day of the window, YYYY-MM-DD (required)")
	until := fs.String("until", "", "last day of the window, YYYY-MM-DD (required)")
	maxAttempts := fs.Int("max", 40, "keep at most this many attempts, oldest first")
	out := fs.String("out", "", "dataset path to write (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" || *since == "" || *until == "" || *out == "" {
		return errors.New("-repo, -since, -until, and -out are required")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN must be set (for example: export GITHUB_TOKEN=$(gh auth token))")
	}

	runs, err := listMergeGroupRuns(token, *repo, *since+".."+*until)
	if err != nil {
		return err
	}
	ds := replay.Dataset{Repo: *repo, BaseBranch: *baseBranch, Attempts: attemptsFromRuns(runs, *baseBranch)}
	if len(ds.Attempts) > *maxAttempts {
		ds.Attempts = ds.Attempts[:*maxAttempts]
	}
	fails := 0
	for _, a := range ds.Attempts {
		if a.Outcome == replay.OutcomeFail {
			fails++
		}
	}
	fmt.Printf("%s: %d attempts (%d failed) from %d runs\n", *repo, len(ds.Attempts), fails, len(runs))
	return writeDataset(*out, ds)
}

// attemptsFromRuns groups runs by the merge-group commit they tested. A group
// fails if any run failed and passes if every run passed; a group whose runs
// were cancelled or are unfinished has no outcome and is dropped — GitHub
// cancels the groups behind a failure, and those say nothing about their PR.
func attemptsFromRuns(runs []workflowRun, baseBranch string) []replay.Attempt {
	type group struct {
		attempt replay.Attempt
		verdict string
	}
	groups := map[string]*group{}
	for _, r := range runs {
		m := groupBranch.FindStringSubmatch(r.HeadBranch)
		if m == nil || m[1] != baseBranch {
			continue
		}
		g, ok := groups[r.HeadSHA]
		if !ok {
			pr, _ := strconv.Atoi(m[2])
			g = &group{attempt: replay.Attempt{PR: pr, GroupSHA: r.HeadSHA, ParentSHA: m[3], QueuedAtMs: r.CreatedAt.UnixMilli()}, verdict: "pass"}
			groups[r.HeadSHA] = g
		}
		g.attempt.QueuedAtMs = min(g.attempt.QueuedAtMs, r.CreatedAt.UnixMilli())
		if !r.RunStartedAt.IsZero() {
			g.attempt.DurationMs = max(g.attempt.DurationMs, r.UpdatedAt.Sub(r.RunStartedAt).Milliseconds())
		}
		switch {
		case r.Status != "completed":
			g.verdict = "unknown"
		case r.Conclusion == "failure" || r.Conclusion == "timed_out" || r.Conclusion == "startup_failure":
			if g.verdict != "unknown" {
				g.verdict = "fail"
			}
		case r.Conclusion == "success" || r.Conclusion == "skipped" || r.Conclusion == "neutral":
		default:
			if g.verdict == "pass" {
				g.verdict = "unknown"
			}
		}
	}
	var attempts []replay.Attempt
	for _, g := range groups {
		switch g.verdict {
		case "pass":
			g.attempt.Outcome = replay.OutcomePass
		case "fail":
			g.attempt.Outcome = replay.OutcomeFail
		default:
			continue
		}
		attempts = append(attempts, g.attempt)
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].QueuedAtMs < attempts[j].QueuedAtMs })
	return attempts
}

// listMergeGroupRuns pages through the window's merge_group runs. GitHub
// returns at most 1000 runs per query, so keep windows small.
func listMergeGroupRuns(token, repo, created string) ([]workflowRun, error) {
	var all []workflowRun
	for page := 1; page <= 10; page++ {
		q := url.Values{"event": {"merge_group"}, "created": {created}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/actions/runs?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list runs: %s: %s", resp.Status, body)
		}
		var payload struct {
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode runs: %w", err)
		}
		all = append(all, payload.WorkflowRuns...)
		if len(payload.WorkflowRuns) < 100 {
			break
		}
	}
	return all, nil
}

func writeDataset(path string, ds replay.Dataset) error {
	raw, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
