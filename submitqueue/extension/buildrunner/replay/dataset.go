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

// Package replay replays recorded CI outcomes: a buildrunner.BuildRunner that
// answers each build with the result and duration a real CI system recorded
// for the same change, and an oracle Truth backed by the same record. It
// exists for benchmarking scorers against real history and must never run a
// production queue. See README.md and doc/rfc/submitqueue/replay-benchmark.md.
package replay

import (
	"encoding/json"
	"fmt"
	"os"
)

// Outcome is the recorded CI result of one attempt.
type Outcome string

const (
	// OutcomeUnknown is the zero-value sentinel.
	OutcomeUnknown Outcome = ""
	// OutcomePass means every recorded CI run for the attempt succeeded.
	OutcomePass Outcome = "pass"
	// OutcomeFail means at least one recorded CI run for the attempt failed.
	OutcomeFail Outcome = "fail"
)

// Dataset is a window of one repository's merge-queue history.
type Dataset struct {
	// Repo is the upstream repository as owner/name.
	Repo string `json:"repo"`
	// BaseBranch is the upstream branch the queue landed onto.
	BaseBranch string `json:"base_branch"`
	// BaseSHA is the upstream commit every replayed change is rebuilt on.
	BaseSHA string `json:"base_sha"`
	// Attempts are the queue attempts in the window, oldest first.
	Attempts []Attempt `json:"attempts"`
}

// Attempt is one change's trip through the upstream merge queue.
type Attempt struct {
	// PR is the upstream pull request number.
	PR int `json:"pr"`
	// Title is the upstream merge-group commit subject.
	Title string `json:"title,omitempty"`
	// GroupSHA is the upstream merge-group commit CI tested.
	GroupSHA string `json:"group_sha"`
	// ParentSHA is GroupSHA's parent: the queue head the PR was tested on.
	ParentSHA string `json:"parent_sha"`
	// Outcome is the recorded CI result for GroupSHA.
	Outcome Outcome `json:"outcome"`
	// DurationMs is the longest recorded CI run for GroupSHA, in milliseconds.
	DurationMs int64 `json:"duration_ms"`
	// QueuedAtMs is when the first CI run for GroupSHA was created, in Unix ms.
	QueuedAtMs int64 `json:"queued_at_ms"`
	// ReplaySHA is the sandbox commit carrying only this PR's change on
	// BaseSHA. Empty until the sandbox is materialized, or if it was skipped.
	ReplaySHA string `json:"replay_sha,omitempty"`
	// Branch is the sandbox branch whose head is ReplaySHA.
	Branch string `json:"branch,omitempty"`
	// Skipped says why the attempt has no ReplaySHA, when it has none.
	Skipped string `json:"skipped,omitempty"`
	// PatchID is git's stable patch ID of the PR's own change.
	PatchID string `json:"patch_id,omitempty"`
	// FlakySuspect marks a failure whose change is identical to a later
	// passing attempt of the same PR: the failure came from something other
	// than the code, such as infrastructure or another change in its group.
	FlakySuspect bool `json:"flaky_suspect,omitempty"`
	// FailsWith lists the ReplaySHAs of changes this one fails together with,
	// though each passes alone: a conflict in the code that a textual merge
	// does not see. Symmetric in effect; recording it on one side suffices.
	FailsWith []string `json:"fails_with,omitempty"`
}

// anyFails reports whether a build holding found fails: one of them is
// recorded as failing, or two of them are recorded as failing together.
func anyFails(found []Attempt) bool {
	present := make(map[string]bool, len(found))
	for _, a := range found {
		present[a.ReplaySHA] = true
	}
	for _, a := range found {
		if a.Outcome == OutcomeFail {
			return true
		}
		for _, other := range a.FailsWith {
			if present[other] {
				return true
			}
		}
	}
	return false
}

// Load reads a dataset written by tool/replaybench.
func Load(path string) (Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Dataset{}, fmt.Errorf("read replay dataset: %w", err)
	}
	var ds Dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		return Dataset{}, fmt.Errorf("parse replay dataset %s: %w", path, err)
	}
	return ds, nil
}

// Index looks attempts up by their replayed commit.
type Index map[string]Attempt

// NewIndex indexes the materialized attempts of ds by ReplaySHA.
func NewIndex(ds Dataset) Index {
	idx := make(Index, len(ds.Attempts))
	for _, a := range ds.Attempts {
		if a.ReplaySHA != "" {
			idx[a.ReplaySHA] = a
		}
	}
	return idx
}
