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
	"fmt"

	"github.com/uber/submitqueue/platform/base/change"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/changeprovider"
)

// corpusResolver is a changeset.Resolver over a fixed set of single-change
// requests, standing in for the orchestrator's request and change stores. It
// asks the change provider on every call, as the orchestrator's stores are
// filled from the provider at admission.
type corpusResolver struct {
	queue    string
	provider changeprovider.ChangeProvider
	// uriByRequest maps a request ID to its one change URI.
	uriByRequest map[string]string
}

func (r corpusResolver) ChangesForBatch(_ context.Context, batch entity.Batch) ([]change.Change, error) {
	out := make([]change.Change, 0, len(batch.Contains))
	for _, id := range batch.Contains {
		uri, ok := r.uriByRequest[id]
		if !ok {
			return nil, fmt.Errorf("batch %s names unknown request %s", batch.ID, id)
		}
		out = append(out, change.Change{URIs: []string{uri}})
	}
	return out, nil
}

func (r corpusResolver) DetailedForBatch(ctx context.Context, batch entity.Batch) (entity.BatchChanges, error) {
	changes, err := r.ChangesForBatch(ctx, batch)
	if err != nil {
		return entity.BatchChanges{}, err
	}
	out := entity.BatchChanges{BatchID: batch.ID, Queue: batch.Queue}
	for i, c := range changes {
		infos, err := r.provider.Get(ctx, entity.Request{ID: batch.Contains[i], Queue: r.queue, Change: c})
		if err != nil {
			return entity.BatchChanges{}, err
		}
		out.Changes = append(out.Changes, infos...)
	}
	return out, nil
}
