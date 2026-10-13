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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
)

func TestAttemptsFromRuns(t *testing.T) {
	const parent = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	t0 := time.Unix(1_700_000_000, 0)
	run := func(sha string, pr string, conclusion string, at time.Time, minutes int) workflowRun {
		return workflowRun{
			HeadSHA: sha, HeadBranch: "gh-readonly-queue/main/pr-" + pr + "-" + parent,
			Status: "completed", Conclusion: conclusion,
			CreatedAt: at, RunStartedAt: at, UpdatedAt: at.Add(time.Duration(minutes) * time.Minute),
		}
	}
	runs := []workflowRun{
		run("pass1", "1", "success", t0, 10),
		run("pass1", "1", "skipped", t0, 1),
		run("fail2", "2", "success", t0.Add(time.Minute), 5),
		run("fail2", "2", "failure", t0.Add(time.Minute), 20),
		// GitHub cancels the groups behind a failure; they say nothing about
		// their own PR.
		run("cancel3", "3", "cancelled", t0.Add(2*time.Minute), 1),
		{HeadSHA: "other", HeadBranch: "gh-readonly-queue/release/pr-4-" + parent, Status: "completed", Conclusion: "success", CreatedAt: t0},
		{HeadSHA: "pending5", HeadBranch: "gh-readonly-queue/main/pr-5-" + parent, Status: "in_progress", CreatedAt: t0},
	}

	got := attemptsFromRuns(runs, "main")
	require.Len(t, got, 2)
	assert.Equal(t, replay.Attempt{PR: 1, GroupSHA: "pass1", ParentSHA: parent, Outcome: replay.OutcomePass, DurationMs: 10 * 60_000, QueuedAtMs: t0.UnixMilli()}, got[0])
	assert.Equal(t, replay.Attempt{PR: 2, GroupSHA: "fail2", ParentSHA: parent, Outcome: replay.OutcomeFail, DurationMs: 20 * 60_000, QueuedAtMs: t0.Add(time.Minute).UnixMilli()}, got[1])
}
