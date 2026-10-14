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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"os"
	"path/filepath"
	"sync"
)

// modelCall is one request the jev scorer sent while scoring a row.
type modelCall struct {
	// RequestBytes is the size of the request body.
	RequestBytes int `json:"request_bytes"`
	// Status is the HTTP status of the response.
	Status int `json:"status"`
	// Model is the model the response names; empty on failure.
	Model string `json:"model,omitempty"`
	// InputTokens is the response's token count; zero when not reported.
	InputTokens int `json:"input_tokens,omitempty"`
	// Cached is whether the response came from the response cache.
	Cached bool `json:"cached,omitempty"`
}

type modelCallLog struct {
	mu    sync.Mutex
	calls []modelCall
}

type modelCallLogKey struct{}

// withModelCallLog returns a context under which every request through a
// modelCallTransport is recorded in the returned log, so calls made by a
// shared client can be attributed to the row being scored.
func withModelCallLog(ctx context.Context) (context.Context, *modelCallLog) {
	log := &modelCallLog{}
	return context.WithValue(ctx, modelCallLogKey{}, log), log
}

func (l *modelCallLog) add(c modelCall) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
}

func (l *modelCallLog) list() []modelCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]modelCall(nil), l.calls...)
}

// modelCallTransport records each request into the context's modelCallLog and,
// with a cache directory, answers a request whose exact bytes were answered
// successfully before from disk. The key covers host, path and body, and the
// body names the proxy model and carries the whole question and state, so any
// change to the request is a miss. Credentials are added below this transport
// and are not part of the key.
type modelCallTransport struct {
	base     nethttp.RoundTripper
	cacheDir string
}

func (t *modelCallTransport) RoundTrip(req *nethttp.Request) (*nethttp.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
	}
	call := modelCall{RequestBytes: len(body)}
	log, _ := req.Context().Value(modelCallLogKey{}).(*modelCallLog)
	record := func() {
		if log != nil {
			log.add(call)
		}
	}

	path := t.cachePath(req, body)
	if path != "" {
		if cached, err := os.ReadFile(path); err == nil {
			call.Status, call.Cached = nethttp.StatusOK, true
			call.Model, call.InputTokens = describeResponse(cached)
			record()
			return cachedResponse(req, cached), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read response cache: %w", err)
		}
	}

	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		record()
		return nil, err
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		record()
		return nil, fmt.Errorf("read response body: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	call.Status = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		call.Model, call.InputTokens = describeResponse(respBody)
		if path != "" {
			if err := writeFileAtomic(path, respBody); err != nil {
				record()
				return nil, fmt.Errorf("write response cache: %w", err)
			}
		}
	}
	record()
	return resp, nil
}

func (t *modelCallTransport) cachePath(req *nethttp.Request, body []byte) string {
	if t.cacheDir == "" {
		return ""
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s %s%s\n", req.Method, req.URL.Host, req.URL.Path)
	_, _ = h.Write(body)
	return filepath.Join(t.cacheDir, hex.EncodeToString(h.Sum(nil))+".json")
}

// describeResponse reads the answering model and token count from a System One
// response, leaving the answers to the scorer.
func describeResponse(body []byte) (string, int) {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return "", 0
	}
	return resp.Model, resp.Usage.InputTokens
}

func cachedResponse(req *nethttp.Request, body []byte) *nethttp.Response {
	return &nethttp.Response{
		Status:        "200 OK",
		StatusCode:    nethttp.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        nethttp.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// writeFileAtomic writes through a temporary file so a concurrent reader or an
// interrupted run never sees a partial response.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
