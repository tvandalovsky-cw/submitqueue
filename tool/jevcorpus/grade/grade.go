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

// Package grade scores probability predictions against binary outcomes.
//
// AUC measures ranking alone: the chance a random landing batch is priced above
// a random failing one. Brier score and log loss measure the probabilities
// themselves, log loss punishing confident mistakes far harder. Calibration
// bins compare mean prediction with the observed landing rate, and ECE
// summarises their gaps. A scorer can rank perfectly and be badly calibrated,
// or be perfectly calibrated and rank no better than chance, so a report
// carries all of them.
package grade

import (
	"math"
	"sort"
)

// logLossEpsilon clips predictions away from 0 and 1 so one certain mistake
// is a large finite penalty rather than infinity.
const logLossEpsilon = 1e-6

// Prediction is one scored outcome.
type Prediction struct {
	// P is the predicted probability that the batch lands, in [0, 1].
	P float64 `json:"p"`
	// Lands is the observed outcome.
	Lands bool `json:"lands"`
}

// Bin is one calibration bucket over [Lo, Hi).
type Bin struct {
	// Lo is the inclusive lower bound of predictions in the bin.
	Lo float64 `json:"lo"`
	// Hi is the upper bound: exclusive, except 1 in the last bin.
	Hi float64 `json:"hi"`
	// Count is the number of predictions in the bin.
	Count int `json:"count"`
	// MeanP is the mean prediction in the bin; zero when Count is zero.
	MeanP float64 `json:"mean_p"`
	// LandRate is the observed fraction that landed; zero when Count is zero.
	LandRate float64 `json:"land_rate"`
}

// Report grades one set of predictions.
type Report struct {
	// N is the number of predictions.
	N int `json:"n"`
	// Lands is how many of them landed.
	Lands int `json:"lands"`
	// AUC is the ROC area; meaningful only when AUCDefined.
	AUC float64 `json:"auc"`
	// AUCDefined is false when every outcome is the same, which leaves
	// ranking unmeasurable.
	AUCDefined bool `json:"auc_defined"`
	// Brier is the mean squared error of the predictions; 0 is perfect and
	// 0.25 is a constant 0.5.
	Brier float64 `json:"brier"`
	// LogLoss is the mean negative log-likelihood of the outcomes.
	LogLoss float64 `json:"log_loss"`
	// ECE is the count-weighted mean gap between MeanP and LandRate across
	// Bins.
	ECE float64 `json:"ece"`
	// MeanLands and MeanFails are the mean prediction for each outcome; zero
	// when it has none. Their gap is the separation a one-outcome slice, where
	// AUC is undefined, can still show.
	MeanLands float64 `json:"mean_lands"`
	MeanFails float64 `json:"mean_fails"`
	// Bins is the calibration table.
	Bins []Bin `json:"bins"`
}

// Grade returns the report for preds with the given number of equal-width
// calibration bins. Fewer than one bin means one.
func Grade(preds []Prediction, bins int) Report {
	r := Report{N: len(preds)}
	if len(preds) == 0 {
		return r
	}
	var sumLands, sumFails float64
	for _, p := range preds {
		if p.Lands {
			r.Lands++
			sumLands += p.P
		} else {
			sumFails += p.P
		}
	}
	if r.Lands > 0 {
		r.MeanLands = sumLands / float64(r.Lands)
	}
	if fails := r.N - r.Lands; fails > 0 {
		r.MeanFails = sumFails / float64(fails)
	}
	r.AUC, r.AUCDefined = AUC(preds)
	r.Brier = Brier(preds)
	r.LogLoss = LogLoss(preds)
	r.Bins = Calibration(preds, bins)
	r.ECE = ECE(r.Bins)
	return r
}

// AUC returns the probability that a random landing prediction is priced above
// a random failing one, counting ties as half. It is false when preds lack
// either outcome.
func AUC(preds []Prediction) (float64, bool) {
	sorted := append([]Prediction(nil), preds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].P < sorted[j].P })
	var pos, neg int
	var rankSum float64
	for i := 0; i < len(sorted); {
		j := i
		for j < len(sorted) && sorted[j].P == sorted[i].P {
			j++
		}
		// Tied predictions share the mean of the ranks they span (1-based).
		meanRank := float64(i+1+j) / 2
		for k := i; k < j; k++ {
			if sorted[k].Lands {
				pos++
				rankSum += meanRank
			} else {
				neg++
			}
		}
		i = j
	}
	if pos == 0 || neg == 0 {
		return 0, false
	}
	return (rankSum - float64(pos)*float64(pos+1)/2) / (float64(pos) * float64(neg)), true
}

// Brier returns the mean of (p - outcome)².
func Brier(preds []Prediction) float64 {
	if len(preds) == 0 {
		return 0
	}
	var sum float64
	for _, p := range preds {
		d := p.P - outcome(p)
		sum += d * d
	}
	return sum / float64(len(preds))
}

// LogLoss returns the mean of -[y·ln(p) + (1-y)·ln(1-p)], with p clipped to
// [ε, 1-ε].
func LogLoss(preds []Prediction) float64 {
	if len(preds) == 0 {
		return 0
	}
	var sum float64
	for _, p := range preds {
		q := math.Min(math.Max(p.P, logLossEpsilon), 1-logLossEpsilon)
		if p.Lands {
			sum -= math.Log(q)
		} else {
			sum -= math.Log(1 - q)
		}
	}
	return sum / float64(len(preds))
}

// Calibration buckets preds into n equal-width bins over [0, 1].
func Calibration(preds []Prediction, n int) []Bin {
	if n < 1 {
		n = 1
	}
	bins := make([]Bin, n)
	sumP := make([]float64, n)
	landed := make([]int, n)
	for i := range bins {
		bins[i].Lo = float64(i) / float64(n)
		bins[i].Hi = float64(i+1) / float64(n)
	}
	for _, p := range preds {
		i := int(p.P * float64(n))
		i = min(max(i, 0), n-1)
		bins[i].Count++
		sumP[i] += p.P
		if p.Lands {
			landed[i]++
		}
	}
	for i := range bins {
		if bins[i].Count > 0 {
			bins[i].MeanP = sumP[i] / float64(bins[i].Count)
			bins[i].LandRate = float64(landed[i]) / float64(bins[i].Count)
		}
	}
	return bins
}

// ECE returns the count-weighted mean |MeanP - LandRate| across bins.
func ECE(bins []Bin) float64 {
	total := 0
	var sum float64
	for _, b := range bins {
		total += b.Count
		sum += float64(b.Count) * math.Abs(b.MeanP-b.LandRate)
	}
	if total == 0 {
		return 0
	}
	return sum / float64(total)
}

func outcome(p Prediction) float64 {
	if p.Lands {
		return 1
	}
	return 0
}
