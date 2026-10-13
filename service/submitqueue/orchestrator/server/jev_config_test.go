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
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"
	"go.uber.org/zap/zaptest"

	"github.com/uber/submitqueue/platform/base/change"
	gitexectest "github.com/uber/submitqueue/platform/git/exectest"
	changesetfake "github.com/uber/submitqueue/submitqueue/core/changeset/fake"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer"
)

func TestLoadProfilesConfig_JevScorerDefaults(t *testing.T) {
	cfg, err := loadProfilesConfig(writeProfiles(t, "defaults:\n  scorer:\n    base: {type: jev}\n"))
	require.NoError(t, err)

	base := cfg.Defaults.Scorer.Base
	require.NotNil(t, base)
	require.NotNil(t, base.Jev)
	assert.Equal(t, defaultJevBaseURLEnv, base.Jev.BaseURLEnv)
	assert.Equal(t, defaultJevTokenEnv, base.Jev.TokenEnv)
	assert.Equal(t, defaultJevModel, base.Jev.Model)
	assert.Equal(t, "/v1/systemone", base.Jev.Path)
	require.NotNil(t, base.Fallback)
	assert.Equal(t, scorerTypeHeuristic, base.Fallback.Type)
}

func TestLoadProfilesConfig_RejectsBadJevScorers(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{name: "jev at the ranking layer", contents: "defaults:\n  scorer: {type: jev}\n"},
		{name: "jev block on evidence", contents: "defaults:\n  scorer:\n    jev: {model: jev}\n"},
		{name: "jev block under heuristic", contents: "defaults:\n  scorer:\n    base:\n      type: heuristic\n      jev: {model: jev}\n"},
		{name: "jev falling back to jev", contents: "defaults:\n  scorer:\n    base:\n      type: jev\n      fallback: {type: jev}\n"},
		{name: "negative cache size", contents: "defaults:\n  scorer:\n    base:\n      type: jev\n      jev: {cacheSize: -1}\n"},
		{name: "relative path", contents: "defaults:\n  scorer:\n    base:\n      type: jev\n      jev: {path: v1/systemone}\n"},
		{name: "bad fallback", contents: "defaults:\n  scorer:\n    base:\n      type: jev\n      fallback: {type: vibes}\n"},
		{name: "fallback under heuristic", contents: "defaults:\n  scorer:\n    base:\n      type: heuristic\n      fallback: {type: heuristic}\n"},
		{name: "oracle falling back to oracle", contents: "defaults:\n  scorer:\n    base:\n      type: oracle\n      fallback: {type: oracle}\n"},
		{name: "diff without a source", contents: "defaults:\n  scorer:\n    base:\n      type: jev\n      jev: {diff: {}}\n"},
		{name: "diff git without repoPath", contents: "defaults:\n  scorer:\n    base:\n      type: jev\n      jev: {diff: {git: {remoteUrl: /r.git, target: main}}}\n"},
		{
			name: "diff sharing a repoPath with a different remote",
			contents: "queues:\n" +
				"  - name: q\n" +
				"    changeProvider: {type: git, git: {remoteUrl: /a.git, target: main, repoPath: /copy}}\n" +
				"    scorer:\n" +
				"      base:\n" +
				"        type: jev\n" +
				"        jev: {diff: {git: {remoteUrl: /b.git, target: main, repoPath: /copy}}}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadProfilesConfig(writeProfiles(t, tt.contents))
			require.Error(t, err)
		})
	}
}

func TestNewProfiles_JevScorer(t *testing.T) {
	batch := entity.Batch{ID: "1", Queue: "q", Contains: []string{"1"}}
	profilesYAML := "" +
		"defaults:\n" +
		"  scorer:\n" +
		"    base:\n" +
		"      type: jev\n" +
		"      jev: {baseUrlEnv: TEST_JEV_URL, tokenEnv: TEST_JEV_KEY, model: test-jev, path: /typesafe/v1/systemone}\n" +
		"      fallback:\n" +
		"        type: heuristic\n" +
		"        buckets: [{min: 0, max: 2147483647, score: 0.25}]\n"

	scoreThroughProfiles := func(t *testing.T) float64 {
		t.Helper()
		cfg, err := loadProfilesConfig(writeProfiles(t, profilesYAML))
		require.NoError(t, err)
		profiles, err := newProfiles(context.Background(), zaptest.NewLogger(t), tally.NoopScope, changesetfake.New(), nil, cfg)
		require.NoError(t, err)
		s, err := profiles.ScorerFactory().For(scorer.Config{QueueName: "q"})
		require.NoError(t, err)
		got, err := s.Score(context.Background(), batch, entity.SpeculationPathSet{})
		require.NoError(t, err)
		return got
	}

	t.Run("asks the proxy with the configured key and model", func(t *testing.T) {
		srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
			assert.Equal(t, "/typesafe/v1/systemone", r.URL.Path)
			assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"model":"test-jev","answers":{"batch_lands":{"type":"noul","noul":0.8}}}`))
		}))
		t.Cleanup(srv.Close)
		t.Setenv("TEST_JEV_URL", srv.URL)
		t.Setenv("TEST_JEV_KEY", "sk-test")

		assert.InDelta(t, 0.8, scoreThroughProfiles(t), 1e-9)
	})

	t.Run("falls back without credentials", func(t *testing.T) {
		t.Setenv("TEST_JEV_URL", "")
		t.Setenv("TEST_JEV_KEY", "")

		assert.InDelta(t, 0.25, scoreThroughProfiles(t), 1e-9)
	})
}

func TestNewProfiles_JevScorerSendsDiffsFromTheChangeRepository(t *testing.T) {
	git := gitexectest.Git(t)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	run("", "init", "--bare", "-b", "main", remote)
	run("", "clone", remote, work)
	run(work, "config", "user.name", "Test")
	run(work, "config", "user.email", "test@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed\n"), 0o644))
	run(work, "add", ".")
	run(work, "commit", "-m", "seed")
	run(work, "push", "origin", "main")
	run(work, "checkout", "-B", "feature/x", "main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "one.go"), []byte("package one\n\nfunc Broken( {\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed, edited\n"), 0o644))
	run(work, "add", "-A")
	run(work, "commit", "-m", "change")
	run(work, "push", "origin", "feature/x")
	uri := fmt.Sprintf("git://git.example.com/demo/%s/%s", url.PathEscape("refs/heads/feature/x"), run(work, "rev-parse", "HEAD"))

	var sentDiff string
	var sentFiles int
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		var body struct {
			State struct {
				Changes []struct {
					Diff         string            `json:"diff"`
					FileContents []json.RawMessage `json:"modified_files_after_change"`
				} `json:"changes"`
			} `json:"state"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if assert.Len(t, body.State.Changes, 1) {
			sentDiff = body.State.Changes[0].Diff
			sentFiles = len(body.State.Changes[0].FileContents)
		}
		_, _ = w.Write([]byte(`{"answers":{"batch_lands":{"type":"noul","noul":0.1}}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TEST_JEV_URL", srv.URL)
	t.Setenv("TEST_JEV_KEY", "sk-test")

	gitBlock := fmt.Sprintf("{remoteUrl: %s, target: main, repoPath: %s}", remote, filepath.Join(root, "copy.git"))
	cfg, err := loadProfilesConfig(writeProfiles(t, fmt.Sprintf(`
queues:
  - name: q
    changeProvider: {type: git, git: %s}
    scorer:
      base:
        type: jev
        jev: {baseUrlEnv: TEST_JEV_URL, tokenEnv: TEST_JEV_KEY, diff: {git: %s, files: true}}
`, gitBlock, gitBlock)))
	require.NoError(t, err)

	batch := entity.Batch{ID: "1", Queue: "q", Contains: []string{"1"}}
	resolver := changesetfake.New().
		Set(batch.ID, change.Change{URIs: []string{uri}}).
		SetDetailed(batch.ID, entity.BatchChanges{BatchID: batch.ID, Queue: batch.Queue, Changes: []entity.ChangeInfo{{URI: uri}}})
	profiles, err := newProfiles(context.Background(), zaptest.NewLogger(t), tally.NoopScope, resolver, nil, cfg)
	require.NoError(t, err)
	s, err := profiles.ScorerFactory().For(scorer.Config{QueueName: "q"})
	require.NoError(t, err)

	got, err := s.Score(context.Background(), batch, entity.SpeculationPathSet{})
	require.NoError(t, err)
	assert.InDelta(t, 0.1, got, 1e-9)
	assert.Contains(t, sentDiff, "+func Broken( {", "the model must see the change's code")
	assert.Equal(t, 1, sentFiles, "files: true sends the modified file in full; the added one is already whole in the diff")
}
