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

package grade

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func preds(pairs ...any) []Prediction {
	var out []Prediction
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Prediction{P: pairs[i].(float64), Lands: pairs[i+1].(bool)})
	}
	return out
}

func TestAUC(t *testing.T) {
	tests := []struct {
		name        string
		preds       []Prediction
		want        float64
		wantDefined bool
	}{
		{name: "perfect ranking", preds: preds(0.9, true, 0.8, true, 0.2, false, 0.1, false), want: 1, wantDefined: true},
		{name: "inverted ranking", preds: preds(0.1, true, 0.9, false), want: 0, wantDefined: true},
		{name: "constant prediction is chance", preds: preds(0.5, true, 0.5, false, 0.5, true, 0.5, false), want: 0.5, wantDefined: true},
		{name: "one swapped pair of four", preds: preds(0.9, true, 0.4, true, 0.6, false, 0.1, false), want: 0.75, wantDefined: true},
		{name: "ranking ignores scale", preds: preds(0.52, true, 0.51, false), want: 1, wantDefined: true},
		{name: "single outcome is undefined", preds: preds(0.9, true, 0.1, true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, defined := AUC(tt.preds)
			assert.Equal(t, tt.wantDefined, defined)
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

func TestBrier(t *testing.T) {
	assert.InDelta(t, 0, Brier(preds(1.0, true, 0.0, false)), 1e-9)
	assert.InDelta(t, 0.25, Brier(preds(0.5, true, 0.5, false)), 1e-9)
	assert.InDelta(t, 0.81, Brier(preds(0.9, false)), 1e-9)
}

func TestLogLoss(t *testing.T) {
	assert.InDelta(t, -math.Log(0.6), LogLoss(preds(0.4, false)), 1e-9)
	assert.Greater(t, LogLoss(preds(0.99, false)), LogLoss(preds(0.6, false)), "confident mistakes cost more")
	assert.False(t, math.IsInf(LogLoss(preds(1.0, false)), 0), "a certain mistake is clipped, not infinite")
}

func TestCalibration(t *testing.T) {
	bins := Calibration(preds(0.05, false, 0.75, true, 0.75, false, 1.0, true), 4)
	assert.Len(t, bins, 4)
	assert.Equal(t, 1, bins[0].Count)
	assert.Equal(t, 0, bins[1].Count)
	assert.Equal(t, 3, bins[3].Count, "1.0 joins the last bin rather than overflowing it")
	assert.InDelta(t, 2.0/3, bins[3].LandRate, 1e-9)
}

func TestECE(t *testing.T) {
	calibrated := Calibration(preds(0.75, true, 0.75, true, 0.75, true, 0.75, false), 10)
	assert.InDelta(t, 0, ECE(calibrated), 1e-9)
	overconfident := Calibration(preds(0.95, true, 0.95, false), 10)
	assert.InDelta(t, 0.45, ECE(overconfident), 1e-9)
}

func TestGrade(t *testing.T) {
	r := Grade(preds(0.9, true, 0.2, false), 10)
	assert.Equal(t, 2, r.N)
	assert.Equal(t, 1, r.Lands)
	assert.True(t, r.AUCDefined)
	assert.Len(t, r.Bins, 10)
	assert.InDelta(t, 0.9, r.MeanLands, 1e-9)
	assert.InDelta(t, 0.2, r.MeanFails, 1e-9)
	assert.Equal(t, Report{}, Grade(nil, 10))
}
