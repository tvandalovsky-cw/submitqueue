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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"
	"go.uber.org/zap/zaptest"

	"github.com/uber/submitqueue/platform/base/change"
	changesetfake "github.com/uber/submitqueue/submitqueue/core/changeset/fake"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/buildrunner"
	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer/oracle"
)

func TestNewProfiles_OracleScorer(t *testing.T) {
	const uri = "github://github.example.com/o/r/pull/1/1111111111111111111111111111111111111111"
	cfg, err := loadProfilesConfig(writeProfiles(t, "defaults:\n  scorer:\n    base: {type: oracle}\n"))
	require.NoError(t, err)

	for name, tt := range map[string]struct {
		uri  string
		want float64
	}{
		"a marked failure is priced as one":      {uri: uri + "?sq-fake=build-fail", want: oracle.PriceFails},
		"an unmarked change is priced as a pass": {uri: uri, want: oracle.PricePasses},
	} {
		t.Run(name, func(t *testing.T) {
			batch := entity.Batch{ID: "1", Queue: "q", Contains: []string{"1"}}
			resolver := changesetfake.New().Set(batch.ID, change.Change{URIs: []string{tt.uri}})
			profiles, err := newProfiles(context.Background(), zaptest.NewLogger(t), tally.NoopScope, resolver, nil, cfg)
			require.NoError(t, err)
			s, err := profiles.ScorerFactory().For(scorer.Config{QueueName: "q"})
			require.NoError(t, err)
			got, err := s.Score(context.Background(), batch, entity.SpeculationPathSet{})
			require.NoError(t, err)
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

func TestNewProfiles_ReplayRunnerAndOracleShareADataset(t *testing.T) {
	const (
		passSHA = "1111111111111111111111111111111111111111"
		failSHA = "2222222222222222222222222222222222222222"
	)
	dir := t.TempDir()
	dsPath := filepath.Join(dir, "ds.json")
	raw, err := json.Marshal(replay.Dataset{Attempts: []replay.Attempt{
		{PR: 1, ReplaySHA: passSHA, Outcome: replay.OutcomePass},
		{PR: 2, ReplaySHA: failSHA, Outcome: replay.OutcomeFail},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dsPath, raw, 0o644))

	cfg, err := loadProfilesConfig(writeProfiles(t, fmt.Sprintf(`
defaults:
  buildRunner: {type: replay, dataset: %s}
  scorer:
    base: {type: oracle, oracle: {dataset: %s}}
`, dsPath, dsPath)))
	require.NoError(t, err)

	for name, tt := range map[string]struct {
		sha        string
		wantPrice  float64
		wantStatus entity.BuildStatus
	}{
		"recorded pass":    {sha: passSHA, wantPrice: oracle.PricePasses, wantStatus: entity.BuildStatusSucceeded},
		"recorded failure": {sha: failSHA, wantPrice: oracle.PriceFails, wantStatus: entity.BuildStatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			batch := entity.Batch{ID: "1", Queue: "q", Contains: []string{"1"}}
			uri := "git://git.example.com/sandbox/refs%2Fheads%2Freplay%2Fpr-1/" + tt.sha
			resolver := changesetfake.New().Set(batch.ID, change.Change{URIs: []string{uri}})
			profiles, err := newProfiles(context.Background(), zaptest.NewLogger(t), tally.NoopScope, resolver, nil, cfg)
			require.NoError(t, err)

			s, err := profiles.ScorerFactory().For(scorer.Config{QueueName: "q"})
			require.NoError(t, err)
			price, err := s.Score(context.Background(), batch, entity.SpeculationPathSet{})
			require.NoError(t, err)
			assert.InDelta(t, tt.wantPrice, price, 1e-9)

			runner, err := profiles.BuildRunnerFactory().For(buildrunner.Config{QueueName: "q"})
			require.NoError(t, err)
			id, err := runner.Trigger(context.Background(), nil, batch, nil)
			require.NoError(t, err)
			status, _, err := runner.Status(context.Background(), id)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, status)
		})
	}
}

func TestLoadProfilesConfig_RejectsBadReplayAndOracle(t *testing.T) {
	for name, contents := range map[string]string{
		"replay without a dataset":  "defaults:\n  buildRunner: {type: replay}\n",
		"negative time scale":       "defaults:\n  buildRunner: {type: replay, dataset: /d.json, timeScale: -1}\n",
		"oracle block on heuristic": "defaults:\n  scorer:\n    base: {type: heuristic, oracle: {dataset: /d.json}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadProfilesConfig(writeProfiles(t, contents))
			require.Error(t, err)
		})
	}
}
