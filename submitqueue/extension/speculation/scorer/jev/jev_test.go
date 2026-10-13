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

package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/uber/submitqueue/platform/base/change"
	phttp "github.com/uber/submitqueue/platform/http"
	changesetfake "github.com/uber/submitqueue/submitqueue/core/changeset/fake"
	"github.com/uber/submitqueue/submitqueue/entity"
	changediffmock "github.com/uber/submitqueue/submitqueue/extension/changediff/mock"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer"
)

var testBatch = entity.Batch{ID: "7", Queue: "test-queue", Contains: []string{"7"}}

type staticScorer float64

func (s staticScorer) Score(context.Context, entity.Batch, entity.SpeculationPathSet) (float64, error) {
	return float64(s), nil
}

// fakeProxy serves /v1/systemone with the given handler and counts calls.
func fakeProxy(t *testing.T, handle func(w nethttp.ResponseWriter, req systemOneRequest)) (*nethttp.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		calls.Add(1)
		assert.Equal(t, DefaultPath, r.URL.Path)
		assert.Equal(t, nethttp.MethodPost, r.Method)
		var req systemOneRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		handle(w, req)
	}))
	t.Cleanup(srv.Close)
	client, err := phttp.NewClient(srv.URL)
	require.NoError(t, err)
	return client, &calls
}

func noulResponse(p float64) func(nethttp.ResponseWriter, systemOneRequest) {
	return func(w nethttp.ResponseWriter, _ systemOneRequest) {
		_, _ = fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{%q:{"type":"noul","noul":%v}}}`, questionName, p)
	}
}

func TestScore(t *testing.T) {
	tests := []struct {
		name     string
		handle   func(nethttp.ResponseWriter, systemOneRequest)
		fallback scorer.Scorer
		want     float64
		wantErr  bool
	}{
		{
			name:   "returns the model's noul",
			handle: noulResponse(0.82),
			want:   0.82,
		},
		{
			name: "non-2xx falls back",
			handle: func(w nethttp.ResponseWriter, _ systemOneRequest) {
				w.WriteHeader(nethttp.StatusUnauthorized)
			},
			fallback: staticScorer(0.5),
			want:     0.5,
		},
		{
			name: "refusal falls back",
			handle: func(w nethttp.ResponseWriter, _ systemOneRequest) {
				_, _ = fmt.Fprintf(w, `{"answers":{%q:{"type":"refusal"}}}`, questionName)
			},
			fallback: staticScorer(0.4),
			want:     0.4,
		},
		{
			name:    "out-of-range noul without fallback is an error",
			handle:  noulResponse(1.5),
			wantErr: true,
		},
		{
			name: "missing answer without fallback is an error",
			handle: func(w nethttp.ResponseWriter, _ systemOneRequest) {
				_, _ = w.Write([]byte(`{"answers":{}}`))
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := fakeProxy(t, tt.handle)
			s := New(Params{
				Config:     scorer.Config{QueueName: "test-queue"},
				HTTPClient: client,
				Model:      "jev",
				Resolver:   changesetfake.New(),
				Fallback:   tt.fallback,
			})
			got, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

func TestScoreSendsBatchStateAndModel(t *testing.T) {
	resolver := changesetfake.New().SetDetailed(testBatch.ID, entity.BatchChanges{
		BatchID: testBatch.ID,
		Queue:   testBatch.Queue,
		Changes: []entity.ChangeInfo{{
			URI: "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111",
			Details: entity.ChangeDetails{
				Author:       entity.Author{Email: "dev@example.com"},
				ChangedFiles: []entity.ChangedFile{{Path: "demo/a.go", LinesAdded: 10, LinesDeleted: 2}},
			},
		}},
	})
	var got systemOneRequest
	client, _ := fakeProxy(t, func(w nethttp.ResponseWriter, req systemOneRequest) {
		got = req
		noulResponse(0.9)(w, req)
	})
	s := New(Params{HTTPClient: client, Model: "jev", Resolver: resolver})

	_, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
	require.NoError(t, err)

	assert.Equal(t, "jev", got.Model)
	require.Contains(t, got.Questions, questionName)
	assert.Equal(t, questionTypeNoul, got.Questions[questionName].Type)
	state, err := json.Marshal(got.State)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"queue": "test-queue",
		"change_count": 1,
		"total_files_changed": 1,
		"total_lines_changed": 12,
		"changes": [{
			"uri": "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111",
			"author": "dev@example.com",
			"file_stats": [{"path": "demo/a.go", "lines_added": 10, "lines_deleted": 2}]
		}]
	}`, string(state))
}

func TestScoreSendsDiffs(t *testing.T) {
	const (
		uriA = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
		uriB = "git://example.com/repo/refs%2Fheads%2Fb/2222222222222222222222222222222222222222"
	)
	newResolver := func() *changesetfake.Resolver {
		return changesetfake.New().
			Set(testBatch.ID, change.Change{URIs: []string{uriA}}, change.Change{URIs: []string{uriB}}).
			SetDetailed(testBatch.ID, entity.BatchChanges{
				BatchID: testBatch.ID,
				Queue:   testBatch.Queue,
				Changes: []entity.ChangeInfo{{URI: uriA}, {URI: uriB}},
			})
	}

	t.Run("each change carries its own diff", func(t *testing.T) {
		differ := changediffmock.NewMockProvider(gomock.NewController(t))
		differ.EXPECT().Diff(gomock.Any(), change.Change{URIs: []string{uriA}}).
			Return([]entity.ChangeDiff{{URI: uriA, Patch: "+func A() {}\n"}}, nil)
		differ.EXPECT().Diff(gomock.Any(), change.Change{URIs: []string{uriB}}).
			Return([]entity.ChangeDiff{{URI: uriB, Patch: "+func B() {}\n"}}, nil)

		var got systemOneRequest
		client, _ := fakeProxy(t, func(w nethttp.ResponseWriter, req systemOneRequest) {
			got = req
			noulResponse(0.9)(w, req)
		})
		s := New(Params{HTTPClient: client, Resolver: newResolver(), Differ: differ})
		_, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
		require.NoError(t, err)

		var state batchStateDoc
		raw, err := json.Marshal(got.State)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &state))
		require.Len(t, state.Changes, 2)
		assert.Equal(t, "+func A() {}\n", state.Changes[0].Diff)
		assert.Equal(t, "+func B() {}\n", state.Changes[1].Diff)
	})

	// Scoring without the code would silently be the heuristic the model is
	// meant to beat, so a diff failure falls back visibly instead.
	t.Run("diff error falls back without asking the model", func(t *testing.T) {
		differ := changediffmock.NewMockProvider(gomock.NewController(t))
		differ.EXPECT().Diff(gomock.Any(), gomock.Any()).Return(nil, errors.New("fetch failed"))

		client, calls := fakeProxy(t, noulResponse(0.9))
		s := New(Params{HTTPClient: client, Resolver: newResolver(), Differ: differ, Fallback: staticScorer(0.4)})
		got, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.InDelta(t, 0.4, got, 1e-9)
		assert.Zero(t, calls.Load())
	})
}

func TestBatchStateContext(t *testing.T) {
	const uri = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
	changes := entity.BatchChanges{Queue: "q", Changes: []entity.ChangeInfo{{URI: uri}}}
	caller := entity.CodeReference{
		Kind: entity.CodeReferenceKindCaller, Symbol: "Scale", Path: "report.go", Line: 4, Text: "return Scale(v)",
		DeclarationBefore: "func Scale(v int) int {", DeclarationAfter: "func Scale(v, f int) int {",
	}
	definition := entity.CodeReference{Kind: entity.CodeReferenceKindDefinition, Symbol: "ParsePort", Path: "parse.go", Line: 9, Text: "func ParsePort(s string) (int, error) {"}

	t.Run("the change and its repository context are kept apart", func(t *testing.T) {
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{uri: {
			URI:        uri,
			Patch:      "+func Scale(v, f int) int {\n",
			Files:      []entity.FileContent{{Path: "scale.go", Content: "package p\n"}},
			References: []entity.CodeReference{caller, definition},
		}})
		require.Len(t, doc.Changes, 1)
		cs := doc.Changes[0]
		assert.Equal(t, "+func Scale(v, f int) int {\n", cs.Diff)
		assert.Equal(t, []fileContentState{{Path: "scale.go", Content: "package p\n"}}, cs.ModifiedFiles)
		require.NotNil(t, cs.RepositoryContext)
		assert.Equal(t, repositoryContextDescription, cs.RepositoryContext.Description)
		assert.Equal(t, []callerState{{
			Symbol: "Scale", DeclarationBefore: "func Scale(v int) int {", DeclarationAfter: "func Scale(v, f int) int {",
			CallSite: codeLineState{Path: "report.go", Line: 4, Text: "return Scale(v)"},
		}}, cs.RepositoryContext.Callers)
		assert.Equal(t, []definitionState{{
			Symbol: "ParsePort", Definition: codeLineState{Path: "parse.go", Line: 9, Text: "func ParsePort(s string) (int, error) {"},
		}}, cs.RepositoryContext.Definitions)
		assert.False(t, cs.ContextTruncated)
	})

	t.Run("a removed declaration says so", func(t *testing.T) {
		removed := caller
		removed.DeclarationAfter = ""
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{uri: {URI: uri, References: []entity.CodeReference{removed}}})
		assert.Equal(t, removedDeclaration, doc.Changes[0].RepositoryContext.Callers[0].DeclarationAfter)
	})

	t.Run("no references means no repository context", func(t *testing.T) {
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{uri: {URI: uri, Patch: "+x\n"}})
		assert.Nil(t, doc.Changes[0].RepositoryContext)
	})

	t.Run("configured caps bound the diff", func(t *testing.T) {
		doc := batchState(changes, map[string]entity.ChangeDiff{uri: {URI: uri, Patch: "aaaa\nbbbb\ncccc\n"}}, 10, 0)
		assert.Equal(t, "aaaa\nbbbb\n", doc.Changes[0].Diff)
		assert.True(t, doc.Changes[0].DiffTruncated)
	})

	t.Run("context past the budget is cut and flagged", func(t *testing.T) {
		big := strings.Repeat("x\n", DefaultMaxContextBytes)
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{uri: {
			URI:        uri,
			Files:      []entity.FileContent{{Path: "big.go", Content: big}},
			References: []entity.CodeReference{caller},
		}})
		cs := doc.Changes[0]
		require.Len(t, cs.ModifiedFiles, 1)
		assert.True(t, cs.ModifiedFiles[0].Truncated)
		assert.LessOrEqual(t, len(cs.ModifiedFiles[0].Content), DefaultMaxContextBytes)
		assert.Nil(t, cs.RepositoryContext)
		assert.True(t, cs.ContextTruncated)
	})
}

func TestTruncatePatch(t *testing.T) {
	tests := []struct {
		name          string
		patch         string
		limit         int
		want          string
		wantTruncated bool
	}{
		{name: "fits", patch: "a\nb\n", limit: 10, want: "a\nb\n"},
		{name: "cut at a line boundary", patch: "aaa\nbbb\nccc\n", limit: 9, want: "aaa\nbbb\n", wantTruncated: true},
		{name: "no whole line fits", patch: "aaaaaa\n", limit: 3, want: "", wantTruncated: true},
		{name: "budget spent", patch: "a\n", limit: 0, want: "", wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := truncatePatch(tt.patch, tt.limit)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantTruncated, truncated)
		})
	}
}

func TestScoreUsesConfiguredPath(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.URL.Path != "/typesafe/v1/systemone" {
			w.WriteHeader(nethttp.StatusNotFound)
			return
		}
		noulResponse(0.61)(w, systemOneRequest{})
	}))
	t.Cleanup(srv.Close)
	client, err := phttp.NewClient(srv.URL)
	require.NoError(t, err)

	s := New(Params{HTTPClient: client, Path: "/typesafe/v1/systemone", Resolver: changesetfake.New()})
	got, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
	require.NoError(t, err)
	assert.InDelta(t, 0.61, got, 1e-9)
}

func TestScoreCachesSuccessesOnly(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	client, calls := fakeProxy(t, func(w nethttp.ResponseWriter, req systemOneRequest) {
		if fail.Load() {
			w.WriteHeader(nethttp.StatusBadGateway)
			return
		}
		noulResponse(0.7)(w, req)
	})
	cache := NewCache(10)
	newScorer := func() scorer.Scorer {
		return New(Params{HTTPClient: client, Model: "jev", Resolver: changesetfake.New(), Cache: cache, Fallback: staticScorer(0.5)})
	}

	got, err := newScorer().Score(context.Background(), testBatch, entity.SpeculationPathSet{})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, got, 1e-9)

	fail.Store(false)
	for range 3 {
		got, err = newScorer().Score(context.Background(), testBatch, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.InDelta(t, 0.7, got, 1e-9)
	}
	assert.Equal(t, int32(2), calls.Load())
}

func TestScoreWithoutEndpoint(t *testing.T) {
	t.Run("falls back", func(t *testing.T) {
		s := New(Params{Resolver: changesetfake.New(), Fallback: staticScorer(0.6)})
		got, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.InDelta(t, 0.6, got, 1e-9)
	})
	t.Run("errors without fallback", func(t *testing.T) {
		s := New(Params{Resolver: changesetfake.New()})
		_, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
		require.Error(t, err)
	})
}

func TestScoreResolverError(t *testing.T) {
	client, calls := fakeProxy(t, noulResponse(0.9))
	s := New(Params{HTTPClient: client, Resolver: changesetfake.New().FailWith(errors.New("boom")), Fallback: staticScorer(0.3)})
	got, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
	require.NoError(t, err)
	assert.InDelta(t, 0.3, got, 1e-9)
	assert.Zero(t, calls.Load())
}

func TestCacheEvictsOldest(t *testing.T) {
	c := NewCache(2)
	c.put("a", 0.1)
	c.put("b", 0.2)
	c.put("c", 0.3)

	_, ok := c.get("a")
	assert.False(t, ok)
	for key, want := range map[string]float64{"b": 0.2, "c": 0.3} {
		got, ok := c.get(key)
		require.True(t, ok, key)
		assert.InDelta(t, want, got, 1e-9)
	}
}

func batchStateWithDefaults(changes entity.BatchChanges, content map[string]entity.ChangeDiff) batchStateDoc {
	return batchState(changes, content, DefaultMaxDiffBytes, DefaultMaxContextBytes)
}
