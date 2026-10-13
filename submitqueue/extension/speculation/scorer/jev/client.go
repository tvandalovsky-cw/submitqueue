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
	"fmt"
	nethttp "net/http"

	phttp "github.com/uber/submitqueue/platform/http"
)

// DefaultPath is LiteLLM's model-routed System One route. Proxies without it
// may expose the TypeSafe pass-through, "/typesafe/v1/systemone", instead;
// both take the same body and return the same answers.
const DefaultPath = "/v1/systemone"

// Question types and answer types of the System One format.
const (
	questionTypeNoul = "noul"
	answerTypeNoul   = "noul"
)

// systemOneRequest is a POST /v1/systemone body.
type systemOneRequest struct {
	// Model is the proxy's model name, not the provider's.
	Model string `json:"model"`
	// State is the thing being judged. Any JSON value is accepted.
	State any `json:"state"`
	// Questions are keyed by name; each key comes back as a key in answers.
	Questions map[string]systemOneQuestion `json:"questions"`
}

// systemOneQuestion is one named question in a System One request.
type systemOneQuestion struct {
	// Type is the question type, e.g. "noul".
	Type string `json:"type"`
	// Instructions is the question put to the model.
	Instructions string `json:"instructions,omitempty"`
	// Criteria describes the answers. For a noul it maps "true" and "false" to
	// what each means.
	Criteria map[string]string `json:"criteria,omitempty"`
}

// systemOneResponse is the body of a successful POST /v1/systemone.
type systemOneResponse struct {
	// Model is the concrete model version that answered.
	Model string `json:"model"`
	// Answers holds one answer per question name.
	Answers map[string]systemOneAnswer `json:"answers"`
}

// systemOneAnswer is one answer in a System One response. A question the model
// declines comes back with Type "refusal" and no value.
type systemOneAnswer struct {
	// Type is the answer type, e.g. "noul" or "refusal".
	Type string `json:"type"`
	// Noul is the probability that a noul question's answer is true.
	Noul *float64 `json:"noul,omitempty"`
}

// askNoul sends one noul question about state and returns the probability the
// answer is true. client must be rooted at the proxy base URL and authenticated.
func askNoul(ctx context.Context, client *nethttp.Client, path, model, name string, question systemOneQuestion, state any) (float64, string, error) {
	body, err := json.Marshal(systemOneRequest{
		Model:     model,
		State:     state,
		Questions: map[string]systemOneQuestion{name: question},
	})
	if err != nil {
		return 0, "", fmt.Errorf("marshal system one request: %w", err)
	}

	status, respBody, err := phttp.SendRequest(ctx, client, nethttp.MethodPost, path, body, func(r *nethttp.Request) {
		r.Header.Set("Content-Type", "application/json")
	})
	if err != nil {
		return 0, "", fmt.Errorf("post %s: %w", path, err)
	}
	if status < 200 || status >= 300 {
		return 0, "", fmt.Errorf("post %s: %w", path, phttp.NewStatusError(status, respBody))
	}

	var resp systemOneResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0, "", fmt.Errorf("decode system one response: %w", err)
	}
	answer, ok := resp.Answers[name]
	if !ok {
		return 0, resp.Model, fmt.Errorf("system one response has no answer for %q", name)
	}
	if answer.Type != answerTypeNoul || answer.Noul == nil {
		return 0, resp.Model, fmt.Errorf("system one answer for %q is %q, not a noul", name, answer.Type)
	}
	p := *answer.Noul
	if !(p >= 0 && p <= 1) {
		return 0, resp.Model, fmt.Errorf("system one noul for %q is %v, not a probability", name, p)
	}
	return p, resp.Model, nil
}
