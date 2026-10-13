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

package oracle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/submitqueue/platform/base/change"
	changesetfake "github.com/uber/submitqueue/submitqueue/core/changeset/fake"
	"github.com/uber/submitqueue/submitqueue/entity"
)

type staticScorer float64

func (s staticScorer) Score(context.Context, entity.Batch, entity.SpeculationPathSet) (float64, error) {
	return float64(s), nil
}

type unknownTruth struct{}

func (unknownTruth) Outcome(context.Context, []change.Change) (bool, bool, error) {
	return false, false, nil
}

var batch = entity.Batch{ID: "1", Queue: "q", Contains: []string{"1"}}

func TestScore(t *testing.T) {
	const passing = "github://github.example.com/o/r/pull/1/1111111111111111111111111111111111111111"
	failing := passing + "?sq-fake=build-fail"

	tests := []struct {
		name     string
		uris     []string
		truth    Truth
		fallback float64
		want     float64
		wantErr  bool
	}{
		{name: "known pass", uris: []string{passing}, truth: MarkerTruth(), want: PricePasses},
		{name: "known fail", uris: []string{failing}, truth: MarkerTruth(), want: PriceFails},
		{name: "one failing change fails the batch", uris: []string{passing, failing}, truth: MarkerTruth(), want: PriceFails},
		{name: "unknown falls back", uris: []string{passing}, truth: unknownTruth{}, fallback: 0.6, want: 0.6},
		{name: "unknown without fallback is an error", uris: []string{passing}, truth: unknownTruth{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Params{Resolver: changesetfake.New().Set(batch.ID, change.Change{URIs: tt.uris}), Truth: tt.truth}
			if tt.fallback > 0 {
				p.Fallback = staticScorer(tt.fallback)
			}
			got, err := New(p).Score(context.Background(), batch, entity.SpeculationPathSet{})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}

	t.Run("resolver error", func(t *testing.T) {
		_, err := New(Params{Resolver: changesetfake.New().FailWith(errors.New("boom")), Truth: MarkerTruth()}).
			Score(context.Background(), batch, entity.SpeculationPathSet{})
		require.Error(t, err)
	})
}
