//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

var domesticFixtureFileNames = []string{"model_pricing.json", "model_pricing.sha256", "fallback.json", "override.json"}

type domesticPricingFixture struct {
	root     string
	tempRoot string
	repoRoot string
	home     string
	files    map[string]struct{}
}

func newDomesticPricingFixture(t *testing.T) *domesticPricingFixture {
	t.Helper()
	tempRoot, err := filepath.Abs(os.TempDir())
	require.NoError(t, err)
	repoRoot := domesticPricingRepoRoot(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	home, err = filepath.Abs(home)
	require.NoError(t, err)
	root, err := os.MkdirTemp(tempRoot, "vanillaapi-domestic-pricing-")
	require.NoError(t, err)
	root, err = filepath.Abs(root)
	require.NoError(t, err)
	if err := validateDomesticFixtureRoot(root, tempRoot, repoRoot, home); err != nil {
		t.Fatalf("new fixture root failed its cleanup guard: %v", err)
	}
	f := &domesticPricingFixture{root: root, tempRoot: tempRoot, repoRoot: repoRoot, home: home, files: make(map[string]struct{})}
	t.Logf("fixture root (Windows absolute): %s", root)
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

func domesticPricingRepoRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root not found from service test working directory")
		}
		current = parent
	}
}

func validateDomesticFixtureRoot(root, tempRoot, repoRoot, home string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("fixture root is empty")
	}
	if !filepath.IsAbs(root) || filepath.VolumeName(root) == "" {
		return fmt.Errorf("fixture root is not a Windows absolute path: %q", root)
	}
	clean := filepath.Clean(root)
	if clean != root {
		return fmt.Errorf("fixture root is not resolved: %q", root)
	}
	tempRoot = filepath.Clean(tempRoot)
	rel, err := filepath.Rel(tempRoot, clean)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("fixture root is not a strict child of TEMP: %q", root)
	}
	volumeRoot := filepath.VolumeName(clean) + string(os.PathSeparator)
	forbiddenPaths := []string{volumeRoot, repoRoot, filepath.Join(repoRoot, ".git"), home}
	for _, envName := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
			if absolute, absErr := filepath.Abs(path); absErr == nil {
				forbiddenPaths = append(forbiddenPaths, absolute)
			}
		}
	}
	for _, forbidden := range forbiddenPaths {
		if forbidden != "" && domesticSameOrAncestor(clean, forbidden) {
			return fmt.Errorf("fixture root is a protected path or its ancestor: %q", root)
		}
	}
	for current := clean; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture root traverses a symlink or reparse point: %q", current)
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("resolve fixture root: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(resolved), clean) {
		return fmt.Errorf("fixture root resolves through a reparse point: %q", root)
	}
	return nil
}

func domesticSameOrAncestor(candidate, protected string) bool {
	candidate = filepath.Clean(candidate)
	protected = filepath.Clean(protected)
	if strings.EqualFold(candidate, protected) {
		return true
	}
	rel, err := filepath.Rel(candidate, protected)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func (f *domesticPricingFixture) path(name string) string {
	if !containsDomesticFixtureName(name) {
		panic("unapproved domestic pricing fixture filename: " + name)
	}
	f.files[name] = struct{}{}
	return filepath.Join(f.root, name)
}

func containsDomesticFixtureName(name string) bool {
	if filepath.Base(name) != name {
		return false
	}
	for _, allowed := range domesticFixtureFileNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func (f *domesticPricingFixture) write(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := f.path(name)
	require.NoError(t, os.WriteFile(path, body, 0600))
	return path
}

func (f *domesticPricingFixture) cleanup(t *testing.T) {
	t.Helper()
	t.Logf("fixture cleanup target (Windows absolute): %s", f.root)
	if err := validateDomesticFixtureRoot(f.root, f.tempRoot, f.repoRoot, f.home); err != nil {
		t.Errorf("refusing fixture cleanup: %v", err)
		return
	}
	for _, name := range domesticFixtureFileNames {
		if _, ok := f.files[name]; !ok {
			continue
		}
		path := filepath.Join(f.root, name)
		t.Logf("remove registered fixture file: %s", path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove fixture file %s: %v", path, err)
			return
		}
	}
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Errorf("inspect fixture directory before non-recursive removal: %v", err)
		return
	}
	if len(entries) != 0 {
		t.Errorf("fixture directory is not empty; refusing removal: %s", f.root)
		return
	}
	if err := os.Remove(f.root); err != nil && !os.IsNotExist(err) {
		t.Errorf("remove empty fixture directory %s: %v", f.root, err)
	}
}

type domesticPricingRemoteStub struct{ body []byte }

func (c domesticPricingRemoteStub) FetchPricingJSON(context.Context, string) ([]byte, error) {
	return append([]byte(nil), c.body...), nil
}

func (domesticPricingRemoteStub) FetchHashText(context.Context, string) (string, error) {
	return "", nil
}

const domesticSentinelCatalog = `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"mode":"chat","litellm_provider":"test"}}`

func newDomesticPricingService(cfg *config.Config) *PricingService {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return NewPricingService(cfg, nil)
}

func domesticBuild(t *testing.T, svc *PricingService, body string) map[string]*LiteLLMModelPricing {
	t.Helper()
	data, _, err := svc.buildPricingData([]byte(body))
	require.NoError(t, err)
	return data
}

func domesticPlazaForPricing(pricing *PricingService) *ModelPlazaService {
	billing := NewBillingService(&config.Config{}, pricing)
	return &ModelPlazaService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
}

func TestDomesticPricingCatalog(t *testing.T) {
	body, err := domesticModelPricesFS.ReadFile("data/domestic_model_prices.json")
	require.NoError(t, err)
	require.NoError(t, validateBundledDomesticPricing(body))
	var entries map[string]map[string]any
	require.NoError(t, jsonUnmarshalDomestic(body, &entries))
	require.Len(t, entries, 2)
	require.Contains(t, entries, "glm-5.3-flash")
	require.Contains(t, entries, "glm-5.3-flashx")
	require.NotContains(t, string(body), "cache_creation_input_token_cost")

	parsed := newDomesticPricingService(nil)
	data, err := parsed.parsePricingData(body)
	require.NoError(t, err)
	require.InDelta(t, 0.15e-6, data["glm-5.3-flash"].InputCostPerToken, 1e-20)
	require.InDelta(t, 0.50e-6, data["glm-5.3-flash"].OutputCostPerToken, 1e-20)
	require.InDelta(t, 0.03e-6, data["glm-5.3-flash"].CacheReadInputTokenCost, 1e-20)
	require.InDelta(t, 0.37e-6, data["glm-5.3-flashx"].InputCostPerToken, 1e-20)
	require.InDelta(t, 1.25e-6, data["glm-5.3-flashx"].OutputCostPerToken, 1e-20)
	require.InDelta(t, 0.075e-6, data["glm-5.3-flashx"].CacheReadInputTokenCost, 1e-20)
	for _, model := range domesticSupplementModels {
		require.Equal(t, "z_ai", data[model].LiteLLMProvider)
		require.Equal(t, "chat", data[model].Mode)
		require.True(t, data[model].SupportsPromptCaching)
		require.Zero(t, data[model].CacheCreationInputTokenCost)
		require.Zero(t, data[model].CacheCreationInputTokenCostAbove1hr)
	}
	require.Error(t, validateBundledDomesticPricing([]byte(strings.Replace(string(body), `1.5e-7`, `0`, 1))), "static input prices must be positive")
	require.Error(t, validateBundledDomesticPricing([]byte(strings.Replace(string(body), `1.5e-7`, `-1.5e-7`, 1))), "negative bundled prices are invalid")
	require.Error(t, validateBundledDomesticPricing([]byte(strings.Replace(string(body), `"mode": "chat"`, `"cache_creation_input_token_cost":1e-7, "mode": "chat"`, 1))), "unpublished cache write prices must be rejected")
	var rawEntries map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &rawEntries))
	delete(rawEntries, "glm-5.3-flashx")
	missingModelBody, err := json.Marshal(rawEntries)
	require.NoError(t, err)
	require.Error(t, validateBundledDomesticPricing(missingModelBody), "both exact model keys are required")
	_, _, err = newDomesticPricingService(nil).buildPricingData([]byte(`{}`))
	require.Error(t, err, "bundled prices do not make an invalid empty upstream catalog valid")

	f := newDomesticPricingFixture(t)
	require.NoError(t, validateDomesticFixtureRoot(f.root, f.tempRoot, f.repoRoot, f.home))
	rootPath := filepath.VolumeName(f.tempRoot) + string(os.PathSeparator)
	outsidePath := filepath.Dir(f.tempRoot)
	for name, path := range map[string]string{"empty": "", "volume root": rootPath, "outside TEMP": outsidePath} {
		t.Run("cleanup guard rejects "+name, func(t *testing.T) {
			require.Error(t, validateDomesticFixtureRoot(path, f.tempRoot, f.repoRoot, f.home))
		})
	}
}

func TestDomesticPricingUpstreamWins(t *testing.T) {
	svc := newDomesticPricingService(nil)
	body := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"mode":"chat","litellm_provider":"test"},` +
		`"glm-5.3-flash":{"input_cost_per_token":0,"output_cost_per_token":0.9e-6,"cache_read_input_token_cost":0.7e-6,"mode":"chat","litellm_provider":"z_ai"},` +
		`"glm-5.3-flashx":{"input_cost_per_token":0.8e-6,"output_cost_per_token":0,"cache_read_input_token_cost":0.6e-6,"mode":"chat","litellm_provider":"z_ai"}}`
	data := domesticBuild(t, svc, body)
	require.Zero(t, data["glm-5.3-flash"].InputCostPerToken, "explicit upstream zero wins")
	require.InDelta(t, 0.9e-6, data["glm-5.3-flash"].OutputCostPerToken, 1e-20)
	require.InDelta(t, 0.7e-6, data["glm-5.3-flash"].CacheReadInputTokenCost, 1e-20)
	require.InDelta(t, 0.8e-6, data["glm-5.3-flashx"].InputCostPerToken, 1e-20)
	require.Zero(t, data["glm-5.3-flashx"].OutputCostPerToken, "explicit upstream zero wins")
	require.InDelta(t, 0.6e-6, data["glm-5.3-flashx"].CacheReadInputTokenCost, 1e-20)

	svc.pricingData = data
	plaza := domesticPlazaForPricing(svc)
	flash := plaza.lookupOfficialPricing(context.Background(), "glm-5.3-flash", make(map[string]*PlazaOfficialPricing))
	flashx := plaza.lookupOfficialPricing(context.Background(), "glm-5.3-flashx", make(map[string]*PlazaOfficialPricing))
	require.NotNil(t, flash)
	require.Nil(t, flash.InputPrice, "existing DTO keeps explicit zero as absent display price")
	require.InDelta(t, 0.9e-6, *flash.OutputPrice, 1e-20)
	require.NotNil(t, flashx)
	require.InDelta(t, 0.8e-6, *flashx.InputPrice, 1e-20)
	require.Nil(t, flashx.OutputPrice)
}

func TestDomesticPricingUpstreamSparseEntry(t *testing.T) {
	for _, model := range domesticSupplementModels {
		t.Run(model+" sparse upstream remains sparse", func(t *testing.T) {
			body := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"mode":"chat","litellm_provider":"test"},` +
				fmt.Sprintf("%q:{\"input_cost_per_token\":2e-6,\"output_cost_per_token\":4e-6,\"mode\":\"chat\",\"litellm_provider\":\"z_ai\"}}", model)
			svc := newDomesticPricingService(nil)
			data := domesticBuild(t, svc, body)
			require.InDelta(t, 2e-6, data[model].InputCostPerToken, 1e-20)
			require.InDelta(t, 4e-6, data[model].OutputCostPerToken, 1e-20)
			require.Zero(t, data[model].CacheReadInputTokenCost, "cache read must not be filled from the bundle")
			svc.pricingData = data
			billing := NewBillingService(&config.Config{}, svc)
			priced, err := billing.GetModelPricing(model)
			require.NoError(t, err)
			require.InDelta(t, 2e-6, priced.InputPricePerToken, 1e-20)
			require.Zero(t, priced.CacheReadPricePerToken)
			plaza := domesticPlazaForPricing(svc).lookupOfficialPricing(context.Background(), model, make(map[string]*PlazaOfficialPricing))
			require.NotNil(t, plaza)
			require.Nil(t, plaza.CacheReadPrice, "missing upstream cache read stays missing in official display")
		})
	}

	t.Run("unusable upstream entries remain reserved and fail closed", func(t *testing.T) {
		for _, raw := range []string{
			`{"mode":"chat","litellm_provider":"z_ai"}`,
			`{"output_cost_per_image":0.25,"mode":"image_generation","litellm_provider":"z_ai"}`,
			`17`,
		} {
			body := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"mode":"chat","litellm_provider":"test"},"glm-5.3-flash":` + raw + `}`
			svc := newDomesticPricingService(nil)
			data := domesticBuild(t, svc, body)
			require.True(t, data["glm-5.3-flash"].TokenPricingAbsent)
			svc.pricingData = data
			billing := NewBillingService(&config.Config{}, svc)
			_, err := billing.GetModelPricing("glm-5.3-flash")
			require.ErrorIs(t, err, ErrModelPricingUnavailable)
			_, warned := billing.fallbackWarnSeen.Load("glm-5.3-flash")
			require.False(t, warned, "reserved upstream entry must not trigger hard fallback")
		}
	})

	t.Run("canonical and provider aliases choose one complete source; dates are not aliases", func(t *testing.T) {
		canonicalBody := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},` +
			`"z_ai/glm-5.3-flash":{"input_cost_per_token":4e-6,"output_cost_per_token":8e-6},` +
			`"glm-5.3-flash":{"input_cost_per_token":3e-6,"output_cost_per_token":6e-6}}`
		data := domesticBuild(t, newDomesticPricingService(nil), canonicalBody)
		require.InDelta(t, 3e-6, data["glm-5.3-flash"].InputCostPerToken, 1e-20)
		require.InDelta(t, 6e-6, data["glm-5.3-flash"].OutputCostPerToken, 1e-20)

		providerBody := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},` +
			`"z-provider/glm-5.3-flash":{"input_cost_per_token":9e-6,"output_cost_per_token":18e-6},` +
			`"a-provider/glm-5.3-flash":{"input_cost_per_token":5e-6,"output_cost_per_token":10e-6}}`
		data = domesticBuild(t, newDomesticPricingService(nil), providerBody)
		require.InDelta(t, 5e-6, data["glm-5.3-flash"].InputCostPerToken, 1e-20)
		require.InDelta(t, 10e-6, data["glm-5.3-flash"].OutputCostPerToken, 1e-20)

		dateBody := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},` +
			`"z_ai/glm-5.3-flash-20260101":{"input_cost_per_token":9e-6,"output_cost_per_token":18e-6}}`
		data = domesticBuild(t, newDomesticPricingService(nil), dateBody)
		require.InDelta(t, 0.15e-6, data["glm-5.3-flash"].InputCostPerToken, 1e-20, "an arbitrary date suffix does not suppress the exact supplement")
	})
}

func TestDomesticPricingLayers(t *testing.T) {
	f := newDomesticPricingFixture(t)
	f.write(t, "fallback.json", []byte(`{"glm-5.3-flash":{"input_cost_per_token":4e-6,"output_cost_per_token":8e-6,"mode":"chat","litellm_provider":"operator"},"fallback-only":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6}}`))
	f.write(t, "override.json", []byte(`{"glm-5.3-flashx":{"input_cost_per_token":0,"output_cost_per_token":9e-6,"cache_read_input_token_cost":null}}`))
	cfg := &config.Config{Pricing: config.PricingConfig{FallbackFile: f.path("fallback.json"), OverrideFile: f.path("override.json")}}
	data := domesticBuild(t, newDomesticPricingService(cfg), domesticSentinelCatalog)
	require.InDelta(t, 4e-6, data["glm-5.3-flash"].InputCostPerToken, 1e-20, "configured fallback precedes bundled supplement")
	require.NotNil(t, data["fallback-only"])
	require.Zero(t, data["glm-5.3-flashx"].InputCostPerToken, "operator explicit zero overrides the bundle")
	require.InDelta(t, 9e-6, data["glm-5.3-flashx"].OutputCostPerToken, 1e-20)
	require.Zero(t, data["glm-5.3-flashx"].CacheReadInputTokenCost, "operator null removes a bundled field")
}

func TestDomesticPricingReload(t *testing.T) {
	f := newDomesticPricingFixture(t)
	f.write(t, "fallback.json", []byte(`{}`))
	mainPath := f.write(t, "model_pricing.json", []byte(domesticSentinelCatalog))
	cfg := &config.Config{Pricing: config.PricingConfig{
		DataDir: f.root, FallbackFile: f.path("fallback.json"),
		RemoteURL: "https://example.invalid/model_pricing.json",
	}}
	service := NewPricingService(cfg, domesticPricingRemoteStub{body: []byte(domesticSentinelCatalog)})
	require.NoError(t, service.loadPricingData(mainPath))
	require.InDelta(t, 0.15e-6, service.pricingData["glm-5.3-flash"].InputCostPerToken, 1e-20)

	remoteBody := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},"glm-5.3-flash":{"input_cost_per_token":4e-6,"output_cost_per_token":8e-6,"mode":"chat","litellm_provider":"z_ai"}}`
	f.path("model_pricing.json")
	f.path("model_pricing.sha256")
	service.remoteClient = domesticPricingRemoteStub{body: []byte(remoteBody)}
	require.NoError(t, service.downloadPricingData())
	require.InDelta(t, 4e-6, service.pricingData["glm-5.3-flash"].InputCostPerToken, 1e-20, "download path uses the common builder")
	require.InDelta(t, 0.37e-6, service.pricingData["glm-5.3-flashx"].InputCostPerToken, 1e-20)

	reloadBody := `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},"glm-5.3-flashx":{"input_cost_per_token":7e-6,"output_cost_per_token":14e-6,"mode":"chat","litellm_provider":"z_ai"}}`
	require.NoError(t, os.WriteFile(mainPath, []byte(reloadBody), 0600))
	require.NoError(t, service.reloadCustomPricingLayers())
	require.InDelta(t, 0.15e-6, service.pricingData["glm-5.3-flash"].InputCostPerToken, 1e-20, "custom reload uses the common builder")
	require.InDelta(t, 7e-6, service.pricingData["glm-5.3-flashx"].InputCostPerToken, 1e-20)

	withdrawnBody := domesticSentinelCatalog
	require.NoError(t, os.WriteFile(mainPath, []byte(withdrawnBody), 0600))
	require.NoError(t, os.WriteFile(f.path("fallback.json"), []byte("{} "), 0600))
	service.reloadIfCustomFilesChanged()
	require.InDelta(t, 0.15e-6, service.pricingData["glm-5.3-flash"].InputCostPerToken, 1e-20, "withdrawal re-enables the supplement")
	require.InDelta(t, 0.37e-6, service.pricingData["glm-5.3-flashx"].InputCostPerToken, 1e-20)

	previousFlash := service.pricingData["glm-5.3-flash"]
	require.NoError(t, os.WriteFile(f.path("fallback.json"), []byte("{"), 0600))
	err := service.reloadCustomPricingLayers()
	require.Error(t, err)
	require.Same(t, previousFlash, service.pricingData["glm-5.3-flash"], "failed reload leaves the published snapshot untouched")
}

func TestDomesticBillingRecognized(t *testing.T) {
	pricing := newDomesticPricingService(nil)
	pricing.pricingData = domesticBuild(t, pricing, domesticSentinelCatalog)
	billing := NewBillingService(&config.Config{}, pricing)
	expected := map[string]struct{ input, output, cacheRead float64 }{
		"glm-5.3-flash":  {input: 0.15e-6, output: 0.50e-6, cacheRead: 0.03e-6},
		"glm-5.3-flashx": {input: 0.37e-6, output: 1.25e-6, cacheRead: 0.075e-6},
	}
	for _, model := range domesticSupplementModels {
		got, err := billing.GetModelPricing(model)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.InDelta(t, expected[model].input, got.InputPricePerToken, 1e-20)
		require.InDelta(t, expected[model].output, got.OutputPricePerToken, 1e-20)
		require.InDelta(t, expected[model].cacheRead, got.CacheReadPricePerToken, 1e-20)
		_, warned := billing.fallbackWarnSeen.Load(model)
		require.False(t, warned, "bundled recognition must avoid hard fallback warnings")
	}
	plaza := &ModelPlazaService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
	for _, model := range domesticSupplementModels {
		got := plaza.lookupOfficialPricing(context.Background(), model, make(map[string]*PlazaOfficialPricing))
		require.NotNil(t, got)
		require.NotNil(t, got.InputPrice)
		require.NotNil(t, got.OutputPrice)
		require.NotNil(t, got.CacheReadPrice)
		require.InDelta(t, expected[model].input, *got.InputPrice, 1e-20)
		require.InDelta(t, expected[model].output, *got.OutputPrice, 1e-20)
		require.InDelta(t, expected[model].cacheRead, *got.CacheReadPrice, 1e-20)
		require.Nil(t, got.CacheWritePrice)
		require.Nil(t, got.CacheWrite1hPrice)
	}
}

func TestDomesticChannelPricingPreserved(t *testing.T) {
	pricing := newDomesticPricingService(nil)
	pricing.pricingData = domesticBuild(t, pricing, domesticSentinelCatalog)
	channelPrices := []ChannelModelPricing{{
		Platform: "z_ai", Models: []string{"glm-5.3-flash"}, BillingMode: BillingModeToken,
		InputPrice: testPtrFloat64(3e-6), OutputPrice: testPtrFloat64(7e-6),
	}}
	channelBefore := channelPrices[0].Clone()
	billing, resolver := newTokenCostTestEnv(t, "z_ai", channelPrices, pricing)
	group := enabledGroup("z_ai")
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "glm-5.3-flash", GroupID: &group.ID, Group: group})
	require.Equal(t, PricingSourceChannel, resolved.Source)
	cost, err := billing.CalculateCostUnified(billing.tokenCostInput(TokenCostRequest{
		Ctx: context.Background(), Model: "glm-5.3-flash", Group: group,
		Tokens: UsageTokens{InputTokens: 1000}, RateMultiplier: 1, Resolver: resolver,
	}, resolved))
	require.NoError(t, err)
	require.InDelta(t, 1000*3e-6, cost.ActualCost, 1e-12)
	require.Equal(t, channelBefore, channelPrices[0], "pricing and billing never mutate channel configuration")

	groupCard := ChannelModelPricing{Models: []string{"glm-5.3-flash"}, BillingMode: BillingModeToken,
		InputPrice: testPtrFloat64(5e-6), OutputPrice: testPtrFloat64(11e-6)}
	groupCardGroup := &Group{ID: 100, Platform: "z_ai", LongContextPricingEnabled: true, ModelPricing: []ChannelModelPricing{groupCard}}
	groupBefore := groupCardGroup.ModelPricing[0].Clone()
	groupResolved := resolver.Resolve(context.Background(), PricingInput{Model: "glm-5.3-flash", GroupID: &groupCardGroup.ID, Group: groupCardGroup})
	require.Equal(t, PricingSourceGroup, groupResolved.Source)
	groupCost, err := billing.CalculateCostUnified(billing.tokenCostInput(TokenCostRequest{
		Ctx: context.Background(), Model: "glm-5.3-flash", Group: groupCardGroup,
		Tokens: UsageTokens{InputTokens: 1000}, RateMultiplier: 1, Resolver: resolver,
	}, groupResolved))
	require.NoError(t, err)
	require.InDelta(t, 1000*5e-6, groupCost.ActualCost, 1e-12)
	require.Equal(t, groupBefore, groupCardGroup.ModelPricing[0], "pricing and billing never mutate group configuration")
}

func jsonUnmarshalDomestic(body []byte, target any) error {
	return json.Unmarshal(body, target)
}
