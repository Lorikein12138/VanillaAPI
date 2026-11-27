// Package trace scores model replies against a ModelTrace fingerprint bank.
//
// Categorize is a pure function: no I/O, no globals, no goroutines, no reflection. The same
// package is compiled natively for the API and with TinyGo to WebAssembly for the browser.
// pipeline/scoring.py is its Python twin; testdata/golden.json holds the outputs both must match.
package trace

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/modeltrace/internal/mtfb"
)

// Model is one entry of the bank (a merge group counts as one entry).
type Model struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Family      int      `json:"family"`
	Members     []string `json:"members"`
	Source      string   `json:"source"` // official | partner | host
	Providers   []string `json:"providers"`
	Collected   string   `json:"collected"`
	Responses   int      `json:"responses"`
	Acc1        float64  `json:"acc1"`
	Acc3        float64  `json:"acc3"`
	Fam3        float64  `json:"fam3"`        // vendor correct, 3 replies, setup held out
	Fam3Unseen  float64  `json:"fam3_unseen"` // vendor correct with the model left out of the bank
}

// Family is a model vendor.
type Family struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type block struct {
	dim, rank, envCount     int
	mean, scale, basis      []float64
	centroids, envCentroids []float64
}

type familyModel struct {
	dim                          int
	mean, scale, coef, intercept []float64
}

// Bank is a decoded, read-only fingerprint bank. It is safe to share between goroutines.
type Bank struct {
	Version          string
	BuiltAt          string
	Models           []Model
	Families         []Family
	valueMax         int
	minNumbers       int
	alpha            float64
	orderedWeight    float64
	beta             []float64
	unknownThreshold []float64
	tierMatch        float64
	tierFamily       float64
	hellinger        block
	ordered          block
	family           familyModel
}

// Open decodes a bank written by pipeline/export.py.
func Open(data []byte) (*Bank, error) {
	if len(data) < 8 || !mtfb.BankBufferHasIdentifier(data) {
		return nil, errors.New("trace: not a ModelTrace bank (identifier MTB2 missing)")
	}
	fb := mtfb.GetRootAsBank(data, 0)
	b := &Bank{
		Version:       string(fb.Version()),
		BuiltAt:       string(fb.BuiltAt()),
		valueMax:      int(fb.ValueMax()),
		minNumbers:    int(fb.MinNumbers()),
		alpha:         float64(fb.Alpha()),
		orderedWeight: float64(fb.OrderedWeight()),
		tierMatch:     float64(fb.TierMatch()),
		tierFamily:    float64(fb.TierFamily()),
	}
	b.beta = floats(fb.BetaLength(), fb.Beta)
	b.unknownThreshold = floats(fb.UnknownThresholdLength(), fb.UnknownThreshold)
	if len(b.beta) != 3 || len(b.unknownThreshold) != 3 {
		return nil, errors.New("trace: bank needs 3 temperatures and 3 thresholds")
	}

	var family mtfb.Family
	for i := 0; i < fb.FamiliesLength(); i++ {
		fb.Families(&family, i)
		b.Families = append(b.Families, Family{ID: string(family.Id()), Name: string(family.Name())})
	}
	var model mtfb.Model
	for i := 0; i < fb.ModelsLength(); i++ {
		fb.Models(&model, i)
		m := Model{
			ID: string(model.Id()), DisplayName: string(model.DisplayName()), Family: int(model.Family()),
			Source: string(model.Source()), Collected: string(model.Collected()), Responses: int(model.Responses()),
			Acc1: float64(model.Acc1()), Acc3: float64(model.Acc3()),
			Fam3: float64(model.Fam3()), Fam3Unseen: float64(model.Fam3Unseen()),
		}
		for j := 0; j < model.MembersLength(); j++ {
			m.Members = append(m.Members, string(model.Members(j)))
		}
		for j := 0; j < model.ProvidersLength(); j++ {
			m.Providers = append(m.Providers, string(model.Providers(j)))
		}
		b.Models = append(b.Models, m)
	}

	var err error
	if b.hellinger, err = readBlock(fb.Hellinger(nil), len(b.Models)); err != nil {
		return nil, err
	}
	if b.ordered, err = readBlock(fb.Ordered(nil), len(b.Models)); err != nil {
		return nil, err
	}
	fm := fb.FamilyModel(nil)
	if fm == nil {
		return nil, errors.New("trace: bank has no family model")
	}
	b.family = familyModel{
		dim: int(fm.Dim()), mean: floats(fm.MeanLength(), fm.Mean), scale: floats(fm.ScaleLength(), fm.Scale),
		coef: floats(fm.CoefLength(), fm.Coef), intercept: floats(fm.InterceptLength(), fm.Intercept),
	}
	if b.family.dim != b.hellinger.dim+b.ordered.dim || len(b.family.coef) != len(b.Families)*b.family.dim {
		return nil, errors.New("trace: family model does not match the feature blocks")
	}
	return b, nil
}

func readBlock(fb *mtfb.Block, models int) (block, error) {
	if fb == nil {
		return block{}, errors.New("trace: bank is missing a feature block")
	}
	bl := block{
		dim: int(fb.Dim()), rank: int(fb.Rank()), envCount: int(fb.EnvCount()),
		mean: floats(fb.MeanLength(), fb.Mean), scale: floats(fb.ScaleLength(), fb.Scale),
		basis: floats(fb.BasisLength(), fb.Basis), centroids: floats(fb.CentroidsLength(), fb.Centroids),
		envCentroids: floats(fb.EnvCentroidsLength(), fb.EnvCentroids),
	}
	if len(bl.mean) != bl.dim || len(bl.basis) != bl.rank*bl.dim || len(bl.centroids) != models*bl.dim ||
		len(bl.envCentroids) != bl.envCount*models*bl.dim {
		return block{}, errors.New("trace: feature block has inconsistent sizes")
	}
	return bl, nil
}

func floats(n int, at func(int) float32) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(at(i))
	}
	return out
}
