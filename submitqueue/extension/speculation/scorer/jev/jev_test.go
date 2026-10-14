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
	"io"
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
		name        string
		handle      func(nethttp.ResponseWriter, systemOneRequest)
		fallback    scorer.Scorer
		expectModel string
		want        float64
		wantErr     bool
	}{
		{
			name:        "answer from the expected model is the price",
			handle:      noulResponse(0.82),
			expectModel: "jev-1.13.0",
			want:        0.82,
		},
		{
			name:        "answer from another model falls back",
			handle:      noulResponse(0.82),
			expectModel: "jev-1.14.0",
			fallback:    staticScorer(0.5),
			want:        0.5,
		},
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
				Config:      scorer.Config{QueueName: "test-queue"},
				HTTPClient:  client,
				Model:       "jev",
				ExpectModel: tt.expectModel,
				Resolver:    changesetfake.New(),
				Fallback:    tt.fallback,
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

func TestScoreSendsCriteriaTrueFirst(t *testing.T) {
	var body string
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		body = string(raw)
		noulResponse(0.9)(w, systemOneRequest{})
	}))
	t.Cleanup(srv.Close)
	client, err := phttp.NewClient(srv.URL)
	require.NoError(t, err)

	_, err = New(Params{HTTPClient: client, Resolver: changesetfake.New()}).Score(context.Background(), testBatch, entity.SpeculationPathSet{})
	require.NoError(t, err)

	trueAt, falseAt := strings.Index(body, `"true":`), strings.Index(body, `"false":`)
	require.GreaterOrEqual(t, trueAt, 0)
	assert.Less(t, trueAt, falseAt)
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

// patchOf returns a patch of exactly n bytes made of whole 8-byte lines, so
// tests can place a budget boundary on or between lines.
func patchOf(n int) string {
	return strings.Repeat("+line01\n", n/8) + strings.Repeat("x", n%8)
}

func filesOf(n int) []entity.ChangedFile {
	files := make([]entity.ChangedFile, n)
	for i := range files {
		files[i] = entity.ChangedFile{Path: fmt.Sprintf("f%04d.go", i), LinesAdded: 1}
	}
	return files
}

// codeBytes is the code a state carries, split into the two budgets.
func codeBytes(doc batchStateDoc) (diff, context int) {
	for _, cs := range doc.Changes {
		diff += len(cs.Diff)
		for _, f := range cs.ModifiedFiles {
			context += len(f.Path) + len(f.Content)
		}
		if cs.RepositoryContext != nil {
			for _, c := range cs.RepositoryContext.Callers {
				context += len(c.CallSite.Text) + len(c.DeclarationBefore) + len(c.DeclarationAfter)
			}
			for _, d := range cs.RepositoryContext.Definitions {
				context += len(d.Definition.Text)
			}
		}
	}
	return diff, context
}

func TestBatchStateFileCap(t *testing.T) {
	const (
		uriA = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
		uriB = "git://example.com/repo/refs%2Fheads%2Fb/2222222222222222222222222222222222222222"
	)
	tests := []struct {
		name          string
		filesA        int
		filesB        int
		wantDescribed int
		wantTruncated bool
	}{
		{name: "under the cap describes every file", filesA: 10, filesB: 5, wantDescribed: 15},
		{name: "exactly the cap is not truncated", filesA: maxFilesInState, wantDescribed: maxFilesInState},
		{name: "one past the cap is truncated", filesA: maxFilesInState + 1, wantDescribed: maxFilesInState, wantTruncated: true},
		{name: "the cap is per batch, not per change", filesA: maxFilesInState - 1, filesB: 2, wantDescribed: maxFilesInState, wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := entity.BatchChanges{Queue: "q", Changes: []entity.ChangeInfo{
				{URI: uriA, Details: entity.ChangeDetails{ChangedFiles: filesOf(tt.filesA)}},
				{URI: uriB, Details: entity.ChangeDetails{ChangedFiles: filesOf(tt.filesB)}},
			}}
			doc := batchStateWithDefaults(changes, nil)
			described := 0
			for _, cs := range doc.Changes {
				described += len(cs.FileStats)
			}
			assert.Equal(t, tt.wantDescribed, described)
			assert.Equal(t, tt.wantTruncated, doc.FilesTruncated)
			assert.Equal(t, tt.filesA+tt.filesB, doc.TotalFiles, "totals count every file, described or not")
			assert.Len(t, doc.Changes, 2, "a change past the cap is still listed")
		})
	}
}

func TestBatchStateDiffBudget(t *testing.T) {
	const (
		uriA = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
		uriB = "git://example.com/repo/refs%2Fheads%2Fb/2222222222222222222222222222222222222222"
		uriC = "git://example.com/repo/refs%2Fheads%2Fc/3333333333333333333333333333333333333333"
	)
	changes := entity.BatchChanges{Queue: "q", Changes: []entity.ChangeInfo{{URI: uriC}, {URI: uriA}, {URI: uriB}}}

	t.Run("a diff exactly at the budget is sent whole", func(t *testing.T) {
		patch := patchOf(DefaultMaxDiffBytes)
		doc := batchStateWithDefaults(entity.BatchChanges{Queue: "q", Changes: []entity.ChangeInfo{{URI: uriA}}},
			map[string]entity.ChangeDiff{uriA: {URI: uriA, Patch: patch}})
		assert.Equal(t, patch, doc.Changes[0].Diff)
		assert.False(t, doc.Changes[0].DiffTruncated)
	})

	t.Run("one byte past the budget is cut at a line boundary", func(t *testing.T) {
		doc := batchStateWithDefaults(entity.BatchChanges{Queue: "q", Changes: []entity.ChangeInfo{{URI: uriA}}},
			map[string]entity.ChangeDiff{uriA: {URI: uriA, Patch: patchOf(DefaultMaxDiffBytes) + "+tail\n"}})
		cs := doc.Changes[0]
		assert.True(t, cs.DiffTruncated)
		assert.LessOrEqual(t, len(cs.Diff), DefaultMaxDiffBytes)
		assert.True(t, strings.HasSuffix(cs.Diff, "\n"), "no line is sent half-cut")
	})

	// The budget is shared in URI order: the change that crosses it is cut,
	// and every change after it is listed with no code at all.
	t.Run("the budget is per batch and spent in URI order", func(t *testing.T) {
		third := DefaultMaxDiffBytes * 5 / 8
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{
			uriA: {URI: uriA, Patch: patchOf(third)},
			uriB: {URI: uriB, Patch: patchOf(third)},
			uriC: {URI: uriC, Patch: patchOf(third)},
		})
		require.Len(t, doc.Changes, 3)
		a, b, c := doc.Changes[0], doc.Changes[1], doc.Changes[2]
		assert.Equal(t, []string{uriA, uriB, uriC}, []string{a.URI, b.URI, c.URI})

		assert.Len(t, a.Diff, third)
		assert.False(t, a.DiffTruncated)
		assert.NotEmpty(t, b.Diff)
		assert.Less(t, len(b.Diff), third)
		assert.True(t, b.DiffTruncated)
		assert.Empty(t, c.Diff)
		assert.True(t, c.DiffTruncated, "a change the budget never reached is marked, not silently empty")

		diff, _ := codeBytes(doc)
		assert.LessOrEqual(t, diff, DefaultMaxDiffBytes)
	})

	t.Run("a change with no diff is not marked truncated", func(t *testing.T) {
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{
			uriA: {URI: uriA, Patch: patchOf(DefaultMaxDiffBytes * 2)},
		})
		assert.True(t, doc.Changes[0].DiffTruncated)
		assert.False(t, doc.Changes[1].DiffTruncated)
		assert.False(t, doc.Changes[2].DiffTruncated)
	})
}

func TestBatchStateContextBudget(t *testing.T) {
	const (
		uriA = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
		uriB = "git://example.com/repo/refs%2Fheads%2Fb/2222222222222222222222222222222222222222"
	)
	changes := entity.BatchChanges{Queue: "q", Changes: []entity.ChangeInfo{{URI: uriA}, {URI: uriB}}}
	caller := entity.CodeReference{
		Kind: entity.CodeReferenceKindCaller, Symbol: "Scale", Path: "report.go", Line: 4, Text: "return Scale(v)",
		DeclarationBefore: "func Scale(v int) int {", DeclarationAfter: "func Scale(v, f int) int {",
	}

	t.Run("context has its own budget beside a full diff budget", func(t *testing.T) {
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{
			uriA: {URI: uriA, Patch: patchOf(DefaultMaxDiffBytes), Files: []entity.FileContent{{Path: "a.go", Content: "package p\n"}}},
		})
		cs := doc.Changes[0]
		assert.False(t, cs.DiffTruncated)
		assert.Equal(t, []fileContentState{{Path: "a.go", Content: "package p\n"}}, cs.ModifiedFiles)
		assert.False(t, cs.ContextTruncated)
	})

	t.Run("context is spent only after every diff", func(t *testing.T) {
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{
			uriA: {URI: uriA, Patch: "+a\n", Files: []entity.FileContent{{Path: "big.go", Content: patchOf(DefaultMaxContextBytes * 2)}}},
			uriB: {URI: uriB, Patch: "+b\n", Files: []entity.FileContent{{Path: "b.go", Content: "package p\n"}}},
		})
		a, b := doc.Changes[0], doc.Changes[1]
		assert.Equal(t, "+b\n", b.Diff, "an earlier change's context never displaces a later change's diff")
		assert.False(t, b.DiffTruncated)
		require.Len(t, a.ModifiedFiles, 1)
		assert.True(t, a.ModifiedFiles[0].Truncated)
		assert.Empty(t, b.ModifiedFiles, "the first change's context spent the budget")
		assert.True(t, b.ContextTruncated)
	})

	t.Run("references past the budget are dropped and flagged", func(t *testing.T) {
		refs := make([]entity.CodeReference, 2*DefaultMaxContextBytes/len(caller.Text))
		for i := range refs {
			refs[i] = caller
		}
		doc := batchStateWithDefaults(changes, map[string]entity.ChangeDiff{uriA: {URI: uriA, References: refs}})
		cs := doc.Changes[0]
		require.NotNil(t, cs.RepositoryContext)
		assert.NotEmpty(t, cs.RepositoryContext.Callers)
		assert.Less(t, len(cs.RepositoryContext.Callers), len(refs))
		assert.True(t, cs.ContextTruncated)
		_, context := codeBytes(doc)
		assert.LessOrEqual(t, context, DefaultMaxContextBytes)
	})
}

// With every option on and every part over its limit, a request carries at
// most the two budgets of code; there is no separate overall cap.
func TestBatchStateCodeIsBoundedByBothBudgets(t *testing.T) {
	caller := entity.CodeReference{
		Kind: entity.CodeReferenceKindCaller, Symbol: "Scale", Path: "report.go", Line: 4, Text: "return Scale(v)",
		DeclarationBefore: "func Scale(v int) int {", DeclarationAfter: "func Scale(v, f int) int {",
	}
	var changes entity.BatchChanges
	content := map[string]entity.ChangeDiff{}
	for i := range 5 {
		uri := fmt.Sprintf("git://example.com/repo/refs%%2Fheads%%2Fc%d/%040d", i, i)
		changes.Changes = append(changes.Changes, entity.ChangeInfo{URI: uri, Details: entity.ChangeDetails{ChangedFiles: filesOf(maxFilesInState)}})
		refs := make([]entity.CodeReference, 500)
		for j := range refs {
			refs[j] = caller
		}
		content[uri] = entity.ChangeDiff{
			URI:        uri,
			Patch:      patchOf(DefaultMaxDiffBytes),
			Files:      []entity.FileContent{{Path: fmt.Sprintf("f%d.go", i), Content: patchOf(DefaultMaxContextBytes)}},
			References: refs,
		}
	}

	doc := batchStateWithDefaults(changes, content)

	diff, context := codeBytes(doc)
	assert.LessOrEqual(t, diff, DefaultMaxDiffBytes)
	assert.LessOrEqual(t, context, DefaultMaxContextBytes)
	assert.Greater(t, diff, DefaultMaxDiffBytes*9/10, "the diff budget is used, not just respected")
	assert.Greater(t, context, DefaultMaxContextBytes*9/10, "the context budget is used, not just respected")
	assert.True(t, doc.FilesTruncated)

	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	code := DefaultMaxDiffBytes + DefaultMaxContextBytes
	assert.Less(t, len(raw), code+code/2, "metadata and JSON escaping add to the code, not multiply it")
}

func TestScoreAppliesDefaultBudgets(t *testing.T) {
	const uri = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
	resolver := changesetfake.New().
		Set(testBatch.ID, change.Change{URIs: []string{uri}}).
		SetDetailed(testBatch.ID, entity.BatchChanges{BatchID: testBatch.ID, Queue: testBatch.Queue, Changes: []entity.ChangeInfo{{URI: uri}}})
	differ := changediffmock.NewMockProvider(gomock.NewController(t))
	differ.EXPECT().Diff(gomock.Any(), gomock.Any()).Return([]entity.ChangeDiff{{
		URI:   uri,
		Patch: patchOf(DefaultMaxDiffBytes * 2),
		Files: []entity.FileContent{{Path: "a.go", Content: patchOf(DefaultMaxContextBytes * 2)}},
	}}, nil)

	var got systemOneRequest
	client, _ := fakeProxy(t, func(w nethttp.ResponseWriter, req systemOneRequest) {
		got = req
		noulResponse(0.9)(w, req)
	})
	s := New(Params{HTTPClient: client, Resolver: resolver, Differ: differ})
	_, err := s.Score(context.Background(), testBatch, entity.SpeculationPathSet{})
	require.NoError(t, err)

	var state batchStateDoc
	raw, err := json.Marshal(got.State)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &state))
	cs := state.Changes[0]
	assert.True(t, cs.DiffTruncated)
	assert.LessOrEqual(t, len(cs.Diff), DefaultMaxDiffBytes, "zero MaxDiffBytes means the default, not unlimited")
	require.Len(t, cs.ModifiedFiles, 1)
	assert.True(t, cs.ModifiedFiles[0].Truncated)
	assert.LessOrEqual(t, len(cs.ModifiedFiles[0].Content), DefaultMaxContextBytes, "zero MaxContextBytes means the default, not unlimited")
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

type batchLoaderFunc func(ctx context.Context, queue, id string) (entity.Batch, error)

func (f batchLoaderFunc) Batch(ctx context.Context, queue, id string) (entity.Batch, error) {
	return f(ctx, queue, id)
}

func TestScoreSendsChangesAhead(t *testing.T) {
	const (
		ownURI   = "git://example.com/repo/refs%2Fheads%2Fb/2222222222222222222222222222222222222222"
		aheadURI = "git://example.com/repo/refs%2Fheads%2Fa/1111111111111111111111111111111111111111"
		doneURI  = "git://example.com/repo/refs%2Fheads%2Fz/3333333333333333333333333333333333333333"
	)
	head := entity.Batch{ID: "9", Queue: "test-queue", Contains: []string{"9"}, Dependencies: []string{"8", "5"}}
	batches := map[string]entity.Batch{
		"8": {ID: "8", Queue: "test-queue", Contains: []string{"8"}, State: entity.BatchStateSpeculating},
		"5": {ID: "5", Queue: "test-queue", Contains: []string{"5"}, State: entity.BatchStateSucceeded},
	}
	loader := batchLoaderFunc(func(_ context.Context, _, id string) (entity.Batch, error) {
		b, ok := batches[id]
		if !ok {
			return entity.Batch{}, errors.New("no such batch")
		}
		return b, nil
	})
	resolver := func() *changesetfake.Resolver {
		return changesetfake.New().
			Set("9", change.Change{URIs: []string{ownURI}}).
			SetDetailed("9", entity.BatchChanges{BatchID: "9", Queue: "test-queue", Changes: []entity.ChangeInfo{{URI: ownURI}}}).
			Set("8", change.Change{URIs: []string{aheadURI}}).
			SetDetailed("8", entity.BatchChanges{BatchID: "8", Queue: "test-queue", Changes: []entity.ChangeInfo{{URI: aheadURI}}}).
			Set("5", change.Change{URIs: []string{doneURI}}).
			SetDetailed("5", entity.BatchChanges{BatchID: "5", Queue: "test-queue", Changes: []entity.ChangeInfo{{URI: doneURI}}})
	}
	newDiffer := func(t *testing.T, aheadPatch string) *changediffmock.MockProvider {
		differ := changediffmock.NewMockProvider(gomock.NewController(t))
		differ.EXPECT().Diff(gomock.Any(), change.Change{URIs: []string{ownURI}}).
			Return([]entity.ChangeDiff{{URI: ownURI, Patch: "+return scale(v)\n"}}, nil).AnyTimes()
		differ.EXPECT().Diff(gomock.Any(), change.Change{URIs: []string{aheadURI}}).
			Return([]entity.ChangeDiff{{
				URI: aheadURI, Patch: aheadPatch,
				Files:      []entity.FileContent{{Path: "scale.go", Content: "package p\n"}},
				References: []entity.CodeReference{{Kind: entity.CodeReferenceKindDefinition, Symbol: "x", Path: "x.go", Text: "func x() {}"}},
			}}, nil).AnyTimes()
		return differ
	}
	capture := func(t *testing.T) (*nethttp.Client, *systemOneRequest) {
		var got systemOneRequest
		client, _ := fakeProxy(t, func(w nethttp.ResponseWriter, req systemOneRequest) {
			got = req
			noulResponse(0.2)(w, req)
		})
		return client, &got
	}
	decode := func(t *testing.T, req *systemOneRequest) batchStateDoc {
		raw, err := json.Marshal(req.State)
		require.NoError(t, err)
		var doc batchStateDoc
		require.NoError(t, json.Unmarshal(raw, &doc))
		return doc
	}

	t.Run("unfinished dependencies are sent as changes ahead", func(t *testing.T) {
		client, got := capture(t)
		s := New(Params{HTTPClient: client, Resolver: resolver(), Differ: newDiffer(t, "-func scale(v int) int {\n+func scale(v, f int) int {\n"), Ahead: loader})
		p, err := s.Score(context.Background(), head, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.InDelta(t, 0.2, p, 1e-9)

		doc := decode(t, got)
		require.Len(t, doc.Changes, 1, "the changes ahead are not part of the batch")
		assert.Equal(t, ownURI, doc.Changes[0].URI)
		require.NotNil(t, doc.ChangesAhead)
		assert.Equal(t, changesAheadDescription, doc.ChangesAhead.Description)
		require.Len(t, doc.ChangesAhead.Changes, 1, "a dependency that already landed is skipped")
		ahead := doc.ChangesAhead.Changes[0]
		assert.Equal(t, aheadURI, ahead.URI)
		assert.Equal(t, "-func scale(v int) int {\n+func scale(v, f int) int {\n", ahead.Diff)
		assert.Empty(t, ahead.ModifiedFiles, "changes ahead carry their diff only")
		assert.Nil(t, ahead.RepositoryContext)
		assert.Equal(t, landsOnTopQuestion, got.Questions[questionName])
	})

	t.Run("the ahead budget is its own", func(t *testing.T) {
		client, got := capture(t)
		s := New(Params{HTTPClient: client, Resolver: resolver(), Differ: newDiffer(t, patchOf(DefaultMaxAheadBytes*2)), Ahead: loader})
		_, err := s.Score(context.Background(), head, entity.SpeculationPathSet{})
		require.NoError(t, err)
		doc := decode(t, got)
		assert.Equal(t, "+return scale(v)\n", doc.Changes[0].Diff, "context from the queue never displaces the batch's own code")
		ahead := doc.ChangesAhead.Changes[0]
		assert.True(t, ahead.DiffTruncated)
		assert.LessOrEqual(t, len(ahead.Diff), DefaultMaxAheadBytes)
	})

	t.Run("without a loader the request is unchanged", func(t *testing.T) {
		client, got := capture(t)
		s := New(Params{HTTPClient: client, Resolver: resolver(), Differ: newDiffer(t, "+x\n")})
		_, err := s.Score(context.Background(), head, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.Nil(t, decode(t, got).ChangesAhead)
		assert.Equal(t, landsQuestion, got.Questions[questionName])
	})

	t.Run("only finished dependencies means no changes ahead", func(t *testing.T) {
		client, got := capture(t)
		landed := head
		landed.Dependencies = []string{"5"}
		s := New(Params{HTTPClient: client, Resolver: resolver(), Differ: newDiffer(t, "+x\n"), Ahead: loader})
		_, err := s.Score(context.Background(), landed, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.Nil(t, decode(t, got).ChangesAhead)
		assert.Equal(t, landsQuestion, got.Questions[questionName])
	})

	t.Run("a batch ahead that cannot be loaded falls back", func(t *testing.T) {
		client, calls := fakeProxy(t, noulResponse(0.9))
		missing := head
		missing.Dependencies = []string{"404"}
		s := New(Params{HTTPClient: client, Resolver: resolver(), Differ: newDiffer(t, "+x\n"), Ahead: loader, Fallback: staticScorer(0.5)})
		p, err := s.Score(context.Background(), missing, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.InDelta(t, 0.5, p, 1e-9)
		assert.Zero(t, calls.Load())
	})

	t.Run("the cache keys on the dependencies", func(t *testing.T) {
		client, calls := fakeProxy(t, noulResponse(0.4))
		cache := NewCache(10)
		s := New(Params{HTTPClient: client, Resolver: resolver(), Differ: newDiffer(t, "+x\n"), Ahead: loader, Cache: cache})
		_, err := s.Score(context.Background(), head, entity.SpeculationPathSet{})
		require.NoError(t, err)
		_, err = s.Score(context.Background(), head, entity.SpeculationPathSet{})
		require.NoError(t, err)
		fewer := head
		fewer.Dependencies = []string{"8"}
		_, err = s.Score(context.Background(), fewer, entity.SpeculationPathSet{})
		require.NoError(t, err)
		assert.Equal(t, int32(2), calls.Load(), "the same dependencies hit the cache; different ones ask again")
	})
}
