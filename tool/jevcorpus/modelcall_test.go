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
	"bytes"
	"context"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const answered = `{"model":"jev-1.13.0","answers":{},"usage":{"input_tokens":42}}`

func TestModelCallTransport(t *testing.T) {
	var hits atomic.Int32
	status := nethttp.StatusOK
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answered))
	}))
	t.Cleanup(srv.Close)
	client := &nethttp.Client{Transport: &modelCallTransport{base: nethttp.DefaultTransport, cacheDir: t.TempDir()}}

	post := func(body string) ([]modelCall, string) {
		ctx, log := withModelCallLog(context.Background())
		req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, srv.URL+"/v1/systemone", bytes.NewReader([]byte(body)))
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return log.list(), string(got)
	}

	t.Run("records an answered request", func(t *testing.T) {
		calls, body := post(`{"a":1}`)
		assert.Equal(t, answered, body)
		assert.Equal(t, []modelCall{{RequestBytes: 7, Status: 200, Model: "jev-1.13.0", InputTokens: 42}}, calls)
		assert.Equal(t, int32(1), hits.Load())
	})

	t.Run("repeated request is answered from the cache", func(t *testing.T) {
		calls, body := post(`{"a":1}`)
		assert.Equal(t, answered, body)
		assert.Equal(t, []modelCall{{RequestBytes: 7, Status: 200, Model: "jev-1.13.0", InputTokens: 42, Cached: true}}, calls)
		assert.Equal(t, int32(1), hits.Load())
	})

	t.Run("different request bytes miss the cache", func(t *testing.T) {
		calls, _ := post(`{"a":2}`)
		require.Len(t, calls, 1)
		assert.False(t, calls[0].Cached)
		assert.Equal(t, int32(2), hits.Load())
	})

	t.Run("failed request is recorded and not cached", func(t *testing.T) {
		status = nethttp.StatusBadGateway
		first, _ := post(`{"a":3}`)
		second, _ := post(`{"a":3}`)
		assert.Equal(t, []modelCall{{RequestBytes: 7, Status: 502}}, first)
		assert.Equal(t, first, second)
		assert.Equal(t, int32(4), hits.Load())
	})
}
