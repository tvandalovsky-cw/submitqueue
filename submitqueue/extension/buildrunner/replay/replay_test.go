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

package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/submitqueue/platform/base/change"
	changesetfake "github.com/uber/submitqueue/submitqueue/core/changeset/fake"
	"github.com/uber/submitqueue/submitqueue/entity"
)

const (
	passSHA    = "1111111111111111111111111111111111111111"
	failSHA    = "2222222222222222222222222222222222222222"
	unknownSHA = "3333333333333333333333333333333333333333"
	// pairASHA and pairBSHA each pass alone and fail together.
	pairASHA = "4444444444444444444444444444444444444444"
	pairBSHA = "5555555555555555555555555555555555555555"
)

func uriFor(sha string) string {
	return "git://git.example.com/sandbox/refs%2Fheads%2Freplay%2Fpr-1/" + sha
}

func testIndex() Index {
	return NewIndex(Dataset{Attempts: []Attempt{
		{PR: 1, ReplaySHA: passSHA, Outcome: OutcomePass, DurationMs: 60_000},
		{PR: 2, ReplaySHA: failSHA, Outcome: OutcomeFail, DurationMs: 120_000},
		{PR: 3, Outcome: OutcomePass, Skipped: "conflict"},
		{PR: 4, ReplaySHA: pairASHA, Outcome: OutcomePass, DurationMs: 30_000},
		{PR: 5, ReplaySHA: pairBSHA, Outcome: OutcomePass, DurationMs: 30_000, FailsWith: []string{pairASHA}},
	}})
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestRunner(t *testing.T) {
	head := entity.Batch{ID: "h", Queue: "q"}
	dep := entity.Batch{ID: "d", Queue: "q"}
	tests := []struct {
		name     string
		headSHA  string
		baseSHA  string
		wantDone entity.BuildStatus
		wantWait time.Duration
	}{
		{name: "recorded pass", headSHA: passSHA, wantDone: entity.BuildStatusSucceeded, wantWait: 6 * time.Second},
		{name: "recorded failure", headSHA: failSHA, wantDone: entity.BuildStatusFailed, wantWait: 12 * time.Second},
		// A path that assumes a failing dependency succeeds builds that
		// dependency in, and so fails whatever the head is.
		{name: "failing change in the base fails the build", headSHA: passSHA, baseSHA: failSHA, wantDone: entity.BuildStatusFailed, wantWait: 12 * time.Second},
		{name: "unrecorded change passes at once", headSHA: unknownSHA, wantDone: entity.BuildStatusSucceeded},
		{name: "a pair member passes alone", headSHA: pairBSHA, wantDone: entity.BuildStatusSucceeded, wantWait: 3 * time.Second},
		{name: "a pair fails when its partner is in the base", headSHA: pairBSHA, baseSHA: pairASHA, wantDone: entity.BuildStatusFailed, wantWait: 3 * time.Second},
		{name: "the pair fails whichever side is recorded", headSHA: pairASHA, baseSHA: pairBSHA, wantDone: entity.BuildStatusFailed, wantWait: 3 * time.Second},
		{name: "a pair member with an unrelated base passes", headSHA: pairBSHA, baseSHA: passSHA, wantDone: entity.BuildStatusSucceeded, wantWait: 6 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := changesetfake.New().Set(head.ID, change.Change{URIs: []string{uriFor(tt.headSHA)}})
			var base []entity.Batch
			if tt.baseSHA != "" {
				resolver.Set(dep.ID, change.Change{URIs: []string{uriFor(tt.baseSHA)}})
				base = []entity.Batch{dep}
			}
			c := &clock{now: time.UnixMilli(1_000_000)}
			r := New(Params{Resolver: resolver, Index: testIndex(), TimeScale: 10, Now: c.Now})

			id, err := r.Trigger(context.Background(), base, head, nil)
			require.NoError(t, err)
			if tt.wantWait > 0 {
				c.now = c.now.Add(tt.wantWait - time.Millisecond)
				status, _, err := r.Status(context.Background(), id)
				require.NoError(t, err)
				assert.Equal(t, entity.BuildStatusRunning, status, "the scaled recorded duration has not elapsed")
			}
			c.now = c.now.Add(time.Millisecond)
			status, _, err := r.Status(context.Background(), id)
			require.NoError(t, err)
			assert.Equal(t, tt.wantDone, status)
		})
	}

	t.Run("a foreign build ID is an error", func(t *testing.T) {
		_, _, err := New(Params{}).Status(context.Background(), entity.BuildID{ID: "fake-ok-abc"})
		require.Error(t, err)
	})
}

func TestTruth(t *testing.T) {
	tr := Truth(testIndex())
	for name, tt := range map[string]struct {
		shas      []string
		wantFails bool
		wantKnown bool
	}{
		"recorded pass":                      {shas: []string{passSHA}, wantKnown: true},
		"recorded failure":                   {shas: []string{failSHA}, wantFails: true, wantKnown: true},
		"one failing change fails the batch": {shas: []string{passSHA, failSHA}, wantFails: true, wantKnown: true},
		"unrecorded":                         {shas: []string{unknownSHA}},
		"a pair member alone passes":         {shas: []string{pairASHA}, wantKnown: true},
		"a pair together fails":              {shas: []string{pairASHA, pairBSHA}, wantFails: true, wantKnown: true},
	} {
		t.Run(name, func(t *testing.T) {
			var changes []change.Change
			for _, sha := range tt.shas {
				changes = append(changes, change.Change{URIs: []string{uriFor(sha)}})
			}
			fails, known, err := tr.Outcome(context.Background(), changes)
			require.NoError(t, err)
			assert.Equal(t, tt.wantFails, fails)
			assert.Equal(t, tt.wantKnown, known)
		})
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ds.json")
	want := Dataset{Repo: "o/r", BaseSHA: passSHA, Attempts: []Attempt{{PR: 1, ReplaySHA: passSHA, Outcome: OutcomePass}}}
	raw, err := json.Marshal(want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o644))

	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Contains(t, NewIndex(got), passSHA)
}
