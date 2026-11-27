package trace

import (
	"errors"
	"math"
)

// Tier is the verdict level. Anything but TierMatch should ask the user which model it was.
type Tier uint8

const (
	TierNone      Tier = iota // no reply had enough numbers
	TierMatch                 // one model (or merge group) clearly fits
	TierFamily                // the vendor is clear, the version is not
	TierUncertain             // neither is clear
	TierUnknown               // fits no model in the bank as well as genuine answers do
)

var tierNames = [...]string{"none", "match", "family", "uncertain", "unknown"}

func (t Tier) String() string { return tierNames[t] }

// MarshalText makes a Tier encode as its name in JSON.
func (t Tier) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// UnmarshalText decodes a Tier from its name.
func (t *Tier) UnmarshalText(text []byte) error {
	for i, name := range tierNames {
		if name == string(text) {
			*t = Tier(i)
			return nil
		}
	}
	return errors.New("trace: unknown tier " + string(text))
}

// Reply is one model reply to one challenge.
type Reply struct {
	Text          string `json:"text"`
	ExpectedCount int    `json:"expected_count"`
}

// ReplyDiag reports how one reply was read.
type ReplyDiag struct {
	Parsed   int  `json:"parsed"`
	Minimum  int  `json:"minimum"`
	Accepted bool `json:"accepted"`
	Top      int  `json:"top"` // best model index for this reply alone, -1 if not accepted
}

// Result of Categorize. Probs and FamilyProbs follow the order of Bank.Models and Bank.Families.
type Result struct {
	Used        int         `json:"used"`
	Tier        Tier        `json:"tier"`
	Top         int         `json:"top"`
	Probs       []float64   `json:"probs,omitempty"`
	FamilyTop   int         `json:"family_top"`
	FamilyProbs []float64   `json:"family_probs,omitempty"`
	Known       float64     `json:"known"`
	LowFit      bool        `json:"low_fit"` // closest model fits worse than ~99% of genuine answers
	Diagnostics []ReplyDiag `json:"diagnostics"`
}

// Categorize scores replies against the bank. It is pure and safe for concurrent use.
func Categorize(b *Bank, replies []Reply) Result {
	models, families := len(b.Models), len(b.Families)
	res := Result{Top: -1, FamilyTop: -1, Diagnostics: make([]ReplyDiag, len(replies))}
	fused := make([]float64, models)
	raw := make([]float64, models)
	logits := make([]float64, families)

	for i, reply := range replies {
		numbers := ParseNumbers(reply.Text)
		d := ReplyDiag{Parsed: len(numbers), Minimum: b.MinimumNumbers(reply.ExpectedCount), Top: -1}
		if d.Parsed >= d.Minimum {
			d.Accepted = true
			f, r, l := b.scoreReply(numbers)
			d.Top = argmax(f)
			add(fused, f)
			add(raw, r)
			add(logits, l)
			res.Used++
		}
		res.Diagnostics[i] = d
	}
	if res.Used == 0 {
		return res
	}

	q := res.Used
	if q > 3 {
		q = 3
	}
	inv := 1 / float64(res.Used)
	beta := b.beta[q-1]
	for m := range fused {
		fused[m] *= beta * inv
		raw[m] *= inv
	}
	for f := range logits {
		logits[f] *= inv
	}
	res.Probs = softmax(fused)
	res.Top = argmax(res.Probs)

	classifier := softmax(logits)
	res.FamilyProbs = make([]float64, families)
	for m, model := range b.Models {
		res.FamilyProbs[model.Family] += 0.5 * res.Probs[m]
	}
	for f := range res.FamilyProbs {
		res.FamilyProbs[f] += 0.5 * classifier[f]
	}
	res.FamilyTop = argmax(res.FamilyProbs)
	res.Known = raw[argmax(raw)]

	// A weak fit never hides a clear vendor: it downgrades a match to TierFamily, and only a weak
	// fit with no clear vendor is TierUnknown (mirrors pipeline/scoring.py tier()).
	res.LowFit = res.Known < b.unknownThreshold[q-1]
	vendorClear := res.FamilyProbs[res.FamilyTop] >= b.tierFamily
	switch {
	case res.LowFit && vendorClear:
		res.Tier = TierFamily
	case res.LowFit:
		res.Tier = TierUnknown
	case res.Probs[res.Top] >= b.tierMatch:
		res.Tier = TierMatch
	case vendorClear:
		res.Tier = TierFamily
	default:
		res.Tier = TierUncertain
	}
	return res
}

// scoreReply returns, per model, the fused z-score and the unscaled similarity, and the family logits.
func (b *Bank) scoreReply(numbers []int) (fused, raw, familyLogits []float64) {
	models := len(b.Models)
	w := b.orderedWeight

	h := hellingerFeature(numbers, b.alpha)
	hRaw := b.hellinger.similarities(b.hellinger.project(h))

	o := orderedFeature(numbers)
	oStd := make([]float64, len(o))
	for i := range o {
		oStd[i] = (o[i] - b.ordered.mean[i]) / b.ordered.scale[i]
	}
	unitInPlace(oStd)
	template := make([]float64, models)
	for m := range template {
		template[m] = math.Inf(-1)
	}
	for e := 0; e < b.ordered.envCount; e++ {
		sims := b.ordered.envSimilarities(oStd, e)
		for m, s := range sims {
			if s > template[m] {
				template[m] = s
			}
		}
	}
	oRaw := b.ordered.similarities(b.ordered.project(o))

	templateZ, oRawZ, hZ := standardize(template), standardize(oRaw), standardize(hRaw)
	mix := make([]float64, models)
	for m := range mix {
		mix[m] = 0.5*templateZ[m] + 0.5*oRawZ[m]
	}
	orderedScore := standardize(mix)

	fused = make([]float64, models)
	raw = make([]float64, models)
	for m := range fused {
		fused[m] = (1-w)*hZ[m] + w*orderedScore[m]
		raw[m] = (1-w)*hRaw[m] + w*oRaw[m]
	}

	fm := b.family
	x := make([]float64, 0, fm.dim)
	x = append(append(x, h...), o...)
	familyLogits = make([]float64, len(b.Families))
	for f := range familyLogits {
		row := fm.coef[f*fm.dim : (f+1)*fm.dim]
		s := fm.intercept[f]
		for i, v := range x {
			s += row[i] * (v - fm.mean[i]) / fm.scale[i]
		}
		familyLogits[f] = s
	}
	return fused, raw, familyLogits
}

// project standardizes a feature, removes the prompt-setup directions and scales it to unit length.
func (bl *block) project(feature []float64) []float64 {
	x := make([]float64, bl.dim)
	for i := range x {
		x[i] = (feature[i] - bl.mean[i]) / bl.scale[i]
	}
	coeffs := make([]float64, bl.rank)
	for k := range coeffs {
		coeffs[k] = dot(x, bl.basis[k*bl.dim:(k+1)*bl.dim])
	}
	for k, c := range coeffs {
		row := bl.basis[k*bl.dim : (k+1)*bl.dim]
		for i := range x {
			x[i] -= c * row[i]
		}
	}
	unitInPlace(x)
	return x
}

func (bl *block) similarities(x []float64) []float64 {
	models := len(bl.centroids) / bl.dim
	out := make([]float64, models)
	for m := range out {
		out[m] = dot(x, bl.centroids[m*bl.dim:(m+1)*bl.dim])
	}
	return out
}

func (bl *block) envSimilarities(x []float64, env int) []float64 {
	models := len(bl.centroids) / bl.dim
	base := env * models * bl.dim
	out := make([]float64, models)
	for m := range out {
		out[m] = dot(x, bl.envCentroids[base+m*bl.dim:base+(m+1)*bl.dim])
	}
	return out
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func unitInPlace(x []float64) {
	norm := math.Sqrt(dot(x, x))
	if norm < 1e-12 {
		norm = 1e-12
	}
	for i := range x {
		x[i] /= norm
	}
}

// standardize: z-scores with the population standard deviation (numpy's default).
func standardize(v []float64) []float64 {
	mean := 0.0
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	variance := 0.0
	for _, x := range v {
		variance += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(variance / float64(len(v)))
	if sd < 1e-12 {
		sd = 1e-12
	}
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = (x - mean) / sd
	}
	return out
}

func softmax(v []float64) []float64 {
	peak := v[argmax(v)]
	out := make([]float64, len(v))
	total := 0.0
	for i, x := range v {
		out[i] = math.Exp(x - peak)
		total += out[i]
	}
	for i := range out {
		out[i] /= total
	}
	return out
}

func argmax(v []float64) int {
	best := 0
	for i := range v {
		if v[i] > v[best] {
			best = i
		}
	}
	return best
}

func add(dst, src []float64) {
	for i := range dst {
		dst[i] += src[i]
	}
}
