package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	SettingKeyChannelDetectionFeature = "channel_detection_enabled"
	SettingKeyChannelDetectionConfig  = "channel_detection_config"
	SettingKeyChannelDetectionLatest  = "channel_detection_latest"
	channelDetectionMaxGroups         = 16
	channelDetectionMinInterval       = 15
	channelDetectionMaxInterval       = 1440
	channelDetectionRoundTimeout      = 5 * time.Minute
)

var (
	errDetectionDisabled = infraerrors.NotFound("CHANNEL_DETECTION_DISABLED", "Channel detection is disabled")
	errDetectionConfig   = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_CONFIG", "Invalid detection interval or group selection")
	errDetectionModel    = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_MODEL", "Select an effective allowlist model in an active group")
	errDetectionKey      = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_KEY", "Select an active limited API key assigned to this group")
)

type ChannelDetectionSelection struct {
	GroupID  int64  `json:"group_id"`
	Model    string `json:"model"`
	APIKeyID int64  `json:"api_key_id"`
}

type ChannelDetectionConfig struct {
	Enabled         bool                        `json:"enabled"`
	IntervalMinutes int                         `json:"interval_minutes"`
	Groups          []ChannelDetectionSelection `json:"groups"`
}

type ChannelDetectionGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type ChannelDetectionCounts struct {
	Passed       int `json:"passed"`
	Failed       int `json:"failed"`
	Errors       int `json:"errors"`
	Unattributed int `json:"unattributed"`
	Untested     int `json:"untested"`
}

type ChannelDetectionSample struct {
	CheckedAt   time.Time              `json:"checked_at"`
	Reasoning   ChannelDetectionCounts `json:"reasoning"`
	Fingerprint ChannelDetectionCounts `json:"fingerprint"`
	Pelican     string                 `json:"pelican_status"`
	SVG         string                 `json:"svg,omitempty"`
}

type ChannelDetectionPublicGroup struct {
	Name   string                  `json:"name"`
	Model  string                  `json:"model,omitempty"`
	Sample *ChannelDetectionSample `json:"sample,omitempty"`
}

type channelDetectionConfigRecord struct {
	ChannelDetectionConfig
	Revision string `json:"revision"`
	stored   string
}

type channelDetectionSnapshot struct {
	Feature     string                            `json:"feature"`
	Revision    string                            `json:"revision"`
	Samples     map[int64]*ChannelDetectionSample `json:"samples"`
	AttemptedAt map[int64]time.Time               `json:"attempted_at"`
	stored      string
}

type channelDetectionGroups interface {
	GetGroup(context.Context, int64) (*Group, error)
}

type channelDetectionGroupDirectory interface {
	ListActive(context.Context) ([]Group, error)
}

type ChannelDetectionKeyRepository interface {
	GetChannelDetectionKey(ctx context.Context, id int64, includeSecret bool) (*APIKey, error)
}

type channelDetectionSettings interface {
	GetMultiple(context.Context, []string) (map[string]string, error)
	Set(context.Context, string, string) error
}

// ChannelDetectionSnapshotGuard identifies the settings and snapshot read by a worker.
type ChannelDetectionSnapshotGuard struct {
	Feature  string
	Config   string
	Snapshot string
}

// ChannelDetectionCoordinator serializes probe runs and fences snapshot writes
// against the current owner, configuration, and previously read snapshot.
type ChannelDetectionCoordinator interface {
	TryAcquire(ctx context.Context, owner string, ttl time.Duration) (bool, error)
	Holds(ctx context.Context, owner string) (bool, error)
	Release(ctx context.Context, owner string) error
	CompareAndSetSnapshot(ctx context.Context, owner string, expected ChannelDetectionSnapshotGuard, next string) (bool, error)
}

type ChannelDetection struct {
	settings    channelDetectionSettings
	groups      channelDetectionGroups
	directory   channelDetectionGroupDirectory
	keys        ChannelDetectionKeyRepository
	coordinator ChannelDetectionCoordinator
	runtime     *config.Config
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewChannelDetection(settings SettingRepository, groups AdminService, directory GroupRepository, keys ChannelDetectionKeyRepository, coordinator ChannelDetectionCoordinator, runtime *config.Config) *ChannelDetection {
	return &ChannelDetection{settings: settings, groups: groups, directory: directory, keys: keys, coordinator: coordinator, runtime: runtime}
}

func (s *ChannelDetection) state(ctx context.Context, withSamples bool) (string, channelDetectionConfigRecord, channelDetectionSnapshot, error) {
	config := channelDetectionConfigRecord{ChannelDetectionConfig: ChannelDetectionConfig{
		IntervalMinutes: 60,
		Groups:          []ChannelDetectionSelection{},
	}}
	var latest channelDetectionSnapshot
	keys := []string{SettingKeyChannelDetectionFeature, SettingKeyChannelDetectionConfig}
	if withSamples {
		keys = append(keys, SettingKeyChannelDetectionLatest)
	}
	values, err := s.settings.GetMultiple(ctx, keys)
	if err != nil {
		return "", config, latest, err
	}
	feature := values[SettingKeyChannelDetectionFeature]
	config.stored = values[SettingKeyChannelDetectionConfig]
	if value := config.stored; value != "" {
		if err := json.Unmarshal([]byte(value), &config); err != nil {
			return "", config, latest, err
		}
	}
	storedSnapshot := values[SettingKeyChannelDetectionLatest]
	if value := storedSnapshot; value != "" && withSamples {
		if err := json.Unmarshal([]byte(value), &latest); err != nil {
			return "", config, latest, err
		}
	}
	if latest.Feature != feature || latest.Revision != config.Revision {
		latest = channelDetectionSnapshot{Feature: feature, Revision: config.Revision}
	}
	latest.stored = storedSnapshot
	if latest.Samples == nil {
		latest.Samples = make(map[int64]*ChannelDetectionSample)
	}
	if latest.AttemptedAt == nil {
		latest.AttemptedAt = make(map[int64]time.Time)
	}
	return feature, config, latest, nil
}

func (s *ChannelDetection) PublicEnabled(ctx context.Context) (bool, error) {
	feature, config, _, err := s.state(ctx, false)
	return err == nil && feature != "" && config.Enabled, err
}

func (s *ChannelDetection) Config(ctx context.Context) (ChannelDetectionConfig, error) {
	feature, config, _, err := s.state(ctx, false)
	if err == nil && feature == "" {
		err = errDetectionDisabled
	}
	return config.ChannelDetectionConfig, err
}

func (s *ChannelDetection) Save(ctx context.Context, input ChannelDetectionConfig) error {
	feature, current, _, err := s.state(ctx, false)
	if err != nil {
		return err
	}
	if feature == "" {
		return errDetectionDisabled
	}
	if input.IntervalMinutes < channelDetectionMinInterval || input.IntervalMinutes > channelDetectionMaxInterval ||
		len(input.Groups) > channelDetectionMaxGroups || (input.Enabled && len(input.Groups) == 0) {
		return errDetectionConfig
	}
	seen := make(map[int64]bool, len(input.Groups))
	parkedSelections := !input.Enabled && slices.Equal(current.Groups, input.Groups)
	for _, selection := range input.Groups {
		if selection.GroupID <= 0 || seen[selection.GroupID] {
			return errDetectionConfig
		}
		seen[selection.GroupID] = true
		// Pausing must remain possible after a group, model, or key has changed.
		if parkedSelections {
			continue
		}
		if _, err := s.probeGroup(ctx, selection); err != nil {
			return err
		}
		if _, err := s.probeKey(ctx, selection, false); err != nil {
			return err
		}
	}
	if input.Groups == nil {
		input.Groups = []ChannelDetectionSelection{}
	}
	if current.Enabled == input.Enabled && current.IntervalMinutes == input.IntervalMinutes && slices.Equal(current.Groups, input.Groups) {
		return nil
	}
	data, err := json.Marshal(channelDetectionConfigRecord{
		ChannelDetectionConfig: input,
		Revision:               uuid.NewString(),
	})
	if err != nil {
		return err
	}
	return s.settings.Set(ctx, SettingKeyChannelDetectionConfig, string(data))
}

func (s *ChannelDetection) GroupOptions(ctx context.Context) ([]ChannelDetectionGroup, error) {
	if _, err := s.Config(ctx); err != nil {
		return nil, err
	}
	groups, err := s.directory.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	options := make([]ChannelDetectionGroup, 0, len(groups))
	for i := range groups {
		group := &groups[i]
		if !group.IsActive() {
			continue
		}
		options = append(options, ChannelDetectionGroup{ID: group.ID, Name: group.Name})
	}
	return options, nil
}

func (s *ChannelDetection) ModelOptions(ctx context.Context, groupID int64) ([]string, error) {
	if _, err := s.Config(ctx); err != nil {
		return nil, err
	}
	group, err := s.groups.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group == nil || !group.IsActive() {
		return nil, errDetectionModel
	}
	models := make([]string, 0, len(group.ModelAllowlist.Models))
	for _, model := range group.ModelAllowlist.Models {
		if concreteDetectionModel(model) {
			models = append(models, model)
		}
	}
	return models, nil
}

func (s *ChannelDetection) PublicGroups(ctx context.Context) ([]ChannelDetectionPublicGroup, error) {
	feature, config, latest, err := s.state(ctx, true)
	if err != nil {
		return nil, err
	}
	if feature == "" || !config.Enabled {
		return nil, errDetectionDisabled
	}
	results := make([]ChannelDetectionPublicGroup, 0, len(config.Groups))
	for _, selection := range config.Groups {
		group, err := s.probeGroup(ctx, selection)
		if errors.Is(err, errDetectionModel) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result := ChannelDetectionPublicGroup{Name: group.Name, Model: selection.Model}
		// Public reads need key eligibility only. The credential is excluded
		// from the repository query even when a sample exists.
		if _, err := s.probeKey(ctx, selection, false); err == nil {
			result.Sample = latest.Samples[selection.GroupID]
			if result.Sample != nil && result.Sample.SVG != "" {
				result.Sample.SVG = sanitizeDetectionSVG(result.Sample.SVG)
				if result.Sample.SVG == "" {
					result.Sample.Pelican = "error"
				}
			}
		} else if !errors.Is(err, errDetectionKey) {
			return nil, err
		}
		results = append(results, result)
	}
	// Group resolution can outlive a switch/config update on another instance.
	matches, err := s.current(ctx, feature, config.Revision)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, errDetectionDisabled
	}
	return results, nil
}

func concreteDetectionModel(model string) bool {
	return model != "" && !strings.Contains(model, "*")
}

func validDetectionModel(group *Group, model string) bool {
	return group != nil && group.IsActive() && concreteDetectionModel(model) &&
		slices.Contains(group.ModelAllowlist.Models, model)
}

func (s *ChannelDetection) probeGroup(ctx context.Context, selection ChannelDetectionSelection) (*Group, error) {
	group, err := s.groups.GetGroup(ctx, selection.GroupID)
	if errors.Is(err, ErrGroupNotFound) || infraerrors.Reason(err) == "SIMPLE_MODE_GROUP_NOT_BINDABLE" {
		return nil, errDetectionModel
	}
	if err != nil {
		return nil, err
	}
	if !validDetectionModel(group, selection.Model) {
		return nil, errDetectionModel
	}
	return group, nil
}

func (s *ChannelDetection) probeKey(ctx context.Context, selection ChannelDetectionSelection, includeSecret bool) (*APIKey, error) {
	if selection.APIKeyID <= 0 {
		return nil, errDetectionKey
	}
	key, err := s.keys.GetChannelDetectionKey(ctx, selection.APIKeyID, includeSecret)
	if errors.Is(err, ErrAPIKeyNotFound) {
		return nil, errDetectionKey
	}
	if err != nil {
		return nil, err
	}
	if key == nil || key.ID != selection.APIKeyID || !key.IsActive() || key.IsExpired() || (includeSecret && key.Key == "") ||
		key.User == nil || !key.User.IsActive() || key.GroupID == nil || *key.GroupID != selection.GroupID ||
		key.Group == nil || key.Group.ID != selection.GroupID || !key.Group.IsActive() ||
		!s.probeKeyLimitsAvailable(key) {
		return nil, errDetectionKey
	}
	return key, nil
}

func (s *ChannelDetection) probeKeyLimitsAvailable(key *APIKey) bool {
	if s.runtime != nil && s.runtime.RunMode == config.RunModeSimple {
		if !simpleModeKeyRateLimitBillingEnabled(s.runtime, key) {
			return false
		}
	} else if key.IsQuotaExhausted() || !(key.Quota > 0 || key.HasRateLimits()) {
		return false
	}
	return (key.RateLimit5h <= 0 || key.EffectiveUsage5h() < key.RateLimit5h) &&
		(key.RateLimit1d <= 0 || key.EffectiveUsage1d() < key.RateLimit1d) &&
		(key.RateLimit7d <= 0 || key.EffectiveUsage7d() < key.RateLimit7d)
}

// Reject a stale authenticated snapshot when its limits differ from the ones
// that made the key eligible. The gateway bills using the authenticated key.
func (s *ChannelDetection) sameProbeKeyLimits(a, b *APIKey) bool {
	simple := s.runtime != nil && s.runtime.RunMode == config.RunModeSimple
	return a.UserID == b.UserID && (simple || a.Quota == b.Quota) &&
		a.RateLimit5h == b.RateLimit5h && a.RateLimit1d == b.RateLimit1d && a.RateLimit7d == b.RateLimit7d
}

func (s *ChannelDetection) current(ctx context.Context, feature, revision string) (bool, error) {
	currentFeature, config, _, err := s.state(ctx, false)
	return err == nil && feature != "" && feature == currentFeature && config.Enabled && revision == config.Revision, err
}

func (s *ChannelDetection) Start(gateway http.Handler) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			s.tick(ctx, gateway)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *ChannelDetection) Stop(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ChannelDetection) tick(ctx context.Context, gateway http.Handler) {
	checkCtx, stopCheck := context.WithTimeout(ctx, 4*time.Second)
	enabled, err := s.PublicEnabled(checkCtx)
	token := uuid.NewString()
	var locked bool
	if err == nil && enabled {
		locked, err = s.coordinator.TryAcquire(checkCtx, token, channelDetectionRoundTimeout+time.Minute)
	}
	stopCheck()
	if err != nil || !locked {
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = s.coordinator.Release(releaseCtx, token)
	}()

	loadCtx, stopLoad := context.WithTimeout(ctx, 5*time.Second)
	feature, config, latest, err := s.state(loadCtx, true)
	stopLoad()
	if err != nil || feature == "" || !config.Enabled {
		return
	}
	selection, due := dueDetectionGroup(config, latest, time.Now())
	if !due {
		return
	}
	// Reserve the interval before the first billable request. If an instance
	// stops or cannot persist the result, the next tick respects this attempt.
	latest.AttemptedAt[selection.GroupID] = time.Now().UTC()
	reserveCtx, stopReserve := context.WithTimeout(ctx, 4*time.Second)
	reserved := s.writeSnapshot(reserveCtx, token, config, &latest)
	stopReserve()
	if !reserved {
		return
	}
	roundCtx, cancel := context.WithTimeout(ctx, channelDetectionRoundTimeout)
	defer cancel()
	current := func(ctx context.Context) bool {
		same, err := s.current(ctx, feature, config.Revision)
		if err != nil || !same {
			return false
		}
		held, err := s.coordinator.Holds(ctx, token)
		return err == nil && held
	}
	go watchDetectionConfiguration(roundCtx, current, cancel)
	latest.Samples[selection.GroupID] = s.runSample(roundCtx, gateway, selection, current)

	// A timed-out sample can record partial results. A canceled or superseded
	// sample cannot write, even if an upstream ignores request cancellation.
	if errors.Is(roundCtx.Err(), context.Canceled) || ctx.Err() != nil {
		return
	}
	writeCtx, stopWrite := context.WithTimeout(ctx, 4*time.Second)
	defer stopWrite()
	s.writeSnapshot(writeCtx, token, config, &latest)
}

func (s *ChannelDetection) writeSnapshot(ctx context.Context, owner string, config channelDetectionConfigRecord, latest *channelDetectionSnapshot) bool {
	encoded, err := json.Marshal(latest)
	if err != nil {
		return false
	}
	next := string(encoded)
	applied, err := s.coordinator.CompareAndSetSnapshot(ctx, owner, ChannelDetectionSnapshotGuard{
		Feature: latest.Feature, Config: config.stored, Snapshot: latest.stored,
	}, next)
	if err != nil || !applied {
		return false
	}
	latest.stored = next
	return true
}

func dueDetectionGroup(config channelDetectionConfigRecord, latest channelDetectionSnapshot, now time.Time) (ChannelDetectionSelection, bool) {
	var selected ChannelDetectionSelection
	var oldest time.Time
	for _, group := range config.Groups {
		lastAttempt := latest.AttemptedAt[group.GroupID]
		if lastAttempt.IsZero() {
			return group, true
		}
		if selected.GroupID == 0 || lastAttempt.Before(oldest) {
			selected, oldest = group, lastAttempt
		}
	}
	return selected, selected.GroupID != 0 && !oldest.Add(time.Duration(config.IntervalMinutes)*time.Minute).After(now)
}

func watchDetectionConfiguration(ctx context.Context, current func(context.Context) bool, cancel context.CancelFunc) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, done := context.WithTimeout(ctx, 2*time.Second)
			valid := current(checkCtx)
			done()
			if !valid {
				cancel()
				return
			}
		}
	}
}
