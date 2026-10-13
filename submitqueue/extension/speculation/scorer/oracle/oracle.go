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

// Package oracle provides a scorer.Scorer that already knows each batch's
// outcome. It exists to measure the ceiling of speculation — the fewest builds
// and shortest time to land any scorer could reach on a workload whose
// outcomes are recorded — and must never be configured on a production queue.
// See README.md.
package oracle

import (
	"context"
	"fmt"

	"github.com/uber/submitqueue/platform/base/change"
	"github.com/uber/submitqueue/platform/fakemarker"
	"github.com/uber/submitqueue/submitqueue/core/changeset"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer"
)

// Prices for a known outcome. They stop short of 0 and 1 because the
// generator ranks by log-probability, which a certain price would make
// infinite.
const (
	PricePasses = 0.99
	PriceFails  = 0.01
)

// Truth reports the recorded outcome of a batch's changes.
type Truth interface {
	// Outcome reports whether a batch made of changes fails, and whether its
	// outcome is recorded at all. An unknown outcome is not an error.
	Outcome(ctx context.Context, changes []change.Change) (fails, known bool, err error)
}

// Params configures an oracle scorer.
type Params struct {
	// Config is the per-queue identity this scorer serves.
	Config scorer.Config
	// Resolver resolves a batch into its changes.
	Resolver changeset.Resolver
	// Truth supplies the recorded outcomes.
	Truth Truth
	// Fallback prices a batch whose outcome is not recorded. Nil returns an
	// error instead, which the generator replaces with its own default.
	Fallback scorer.Scorer
}

type oracleScorer struct {
	p Params
}

// New returns an oracle scorer.
func New(p Params) scorer.Scorer {
	return &oracleScorer{p: p}
}

func (s *oracleScorer) Score(ctx context.Context, batch entity.Batch, paths entity.SpeculationPathSet) (float64, error) {
	changes, err := s.p.Resolver.ChangesForBatch(ctx, batch)
	if err != nil {
		return 0, fmt.Errorf("resolve changes for batch %s: %w", batch.ID, err)
	}
	fails, known, err := s.p.Truth.Outcome(ctx, changes)
	if err != nil {
		return 0, fmt.Errorf("look up outcome of batch %s: %w", batch.ID, err)
	}
	switch {
	case known && fails:
		return PriceFails, nil
	case known:
		return PricePasses, nil
	case s.p.Fallback != nil:
		return s.p.Fallback.Score(ctx, batch, paths)
	default:
		return 0, fmt.Errorf("outcome of batch %s is not recorded", batch.ID)
	}
}

// markerTruth reads outcomes from the fake build runner's URI markers, so the
// oracle agrees with that runner by construction.
type markerTruth struct{}

// MarkerTruth returns a Truth that matches the fake build runner: a batch
// fails if any change URI carries "sq-fake=build-fail". Every outcome is known.
func MarkerTruth() Truth {
	return markerTruth{}
}

func (markerTruth) Outcome(_ context.Context, changes []change.Change) (bool, bool, error) {
	for _, c := range changes {
		if fakemarker.Token(c.URIs) == "build-fail" {
			return true, true, nil
		}
	}
	return false, true, nil
}
