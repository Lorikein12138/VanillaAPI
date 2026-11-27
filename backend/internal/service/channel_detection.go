package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
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
	channelDetectionIdentityVersion   = 1
	channelDetectionDailySends        = 672
	channelDetectionRoundSends        = 14
)

var (
	errDetectionDisabled = infraerrors.NotFound("CHANNEL_DETECTION_DISABLED", "Channel detection is disabled")
	errDetectionConfig   = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_CONFIG", "Invalid detection interval or group selection")
	errDetectionModel    = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_MODEL", "Select an effective allowlist model in an active group")
)

type ChannelDetectionSelection struct {
	GroupID int64  `json:"group_id"`
	Model   string `json:"model"`
}

type ChannelDetectionConfig struct {
	Enabled         bool                        `json:"enabled"`
	IntervalMinutes int                         `json:"interval_minutes"`
	Groups          []ChannelDetectionSelection `json:"groups"`
}

type ChannelDetectionGroup struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Active         bool   `json:"active"`
	HasModels      bool   `json:"has_models"`
	ModeRestricted bool   `json:"mode_restricted"`
	ClaudeCodeOnly bool   `json:"claude_code_only"`
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
	IdentityVersion int                               `json:"identity_version"`
	Feature         string                            `json:"feature"`
	Revision        string                            `json:"revision"`
	Samples         map[int64]*ChannelDetectionSample `json:"samples"`
	AttemptedAt     map[int64]time.Time               `json:"attempted_at"`
	stored          string
}

type channelDetectionGroups interface {
	GetGroup(context.Context, int64) (*Group, error)
}

type channelDetectionGroupDirectory interface {
	List(context.Context, pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error)
}

type channelDetectionDirectoryModels interface {
	resolveAdminGroupModelAllowlist(context.Context, *Group) error
	validateSimpleModeGroupAccess(*Group) error
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
	ReserveSend(ctx context.Context, owner string, expected ChannelDetectionSnapshotGuard, groupID int64, groupRPM, dailyLimit int) (bool, error)
	CompareAndSetSnapshot(ctx context.Context, owner string, expected ChannelDetectionSnapshotGuard, next string) (bool, error)
}

type ChannelDetection struct {
	settings    channelDetectionSettings
	groups      channelDetectionGroups
	directory   channelDetectionGroupDirectory
	coordinator ChannelDetectionCoordinator
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewChannelDetection(settings SettingRepository, groups AdminService, directory GroupRepository, coordinator ChannelDetectionCoordinator) *ChannelDetection {
	return &ChannelDetection{settings: settings, groups: groups, directory: directory, coordinator: coordinator}
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
	if latest.IdentityVersion != channelDetectionIdentityVersion || latest.Samples == nil {
		latest.IdentityVersion = channelDetectionIdentityVersion
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
	for _, selection := range input.Groups {
		if selection.GroupID <= 0 || seen[selection.GroupID] {
			return errDetectionConfig
		}
		seen[selection.GroupID] = true
		// Preserve existing selections even after their eligibility changes.
		if slices.Contains(current.Groups, selection) {
			continue
		}
		if _, err := s.probeGroup(ctx, selection); err != nil {
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
	resolver, ok := s.groups.(channelDetectionDirectoryModels)
	if !ok {
		return nil, errors.New("detection group directory resolver unavailable")
	}
	options := make([]ChannelDetectionGroup, 0)
	for page := 1; ; page++ {
		groups, result, err := s.directory.List(ctx, pagination.PaginationParams{
			Page: page, PageSize: 100, SortBy: "id", SortOrder: "asc",
		})
		if err != nil {
			return nil, err
		}
		for i := range groups {
			group := &groups[i]
			if strings.EqualFold(group.Status, "deleted") {
				continue
			}
			if !group.ClaudeCodeOnly {
				if err := resolver.resolveAdminGroupModelAllowlist(ctx, group); err != nil {
					return nil, err
				}
			}
			options = append(options, ChannelDetectionGroup{
				ID: group.ID, Name: group.Name, Active: group.IsActive(),
				HasModels:      !group.ClaudeCodeOnly && slices.ContainsFunc(group.ModelAllowlist.Models, concreteDetectionModel),
				ClaudeCodeOnly: group.ClaudeCodeOnly,
				ModeRestricted: resolver.validateSimpleModeGroupAccess(group) != nil,
			})
		}
		if len(groups) == 0 || int64(page*100) >= result.Total {
			return options, nil
		}
	}
}

func (s *ChannelDetection) ModelOptions(ctx context.Context, groupID int64) ([]string, error) {
	if _, err := s.Config(ctx); err != nil {
		return nil, err
	}
	group, err := s.groups.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if !detectionGroupEligible(group) {
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
		result.Sample = latest.Samples[selection.GroupID]
		if result.Sample != nil && result.Sample.SVG != "" {
			result.Sample.SVG = sanitizeDetectionSVG(result.Sample.SVG)
			if result.Sample.SVG == "" {
				result.Sample.Pelican = "error"
			}
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

func detectionGroupEligible(group *Group) bool {
	return group != nil && group.IsActive() && !group.ClaudeCodeOnly
}

func validDetectionModel(group *Group, model string) bool {
	return detectionGroupEligible(group) && concreteDetectionModel(model) &&
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
	if err == nil && enabled && s.coordinator != nil {
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
	if err != nil || feature == "" || !config.Enabled {
		stopLoad()
		return
	}
	eligible := config
	eligible.Groups = make([]ChannelDetectionSelection, 0, len(config.Groups))
	for _, selection := range config.Groups {
		if _, err := s.probeGroup(loadCtx, selection); err == nil {
			eligible.Groups = append(eligible.Groups, selection)
		} else if !errors.Is(err, errDetectionModel) {
			stopLoad()
			return
		}
	}
	stopLoad()
	selection, due := dueDetectionGroup(eligible, latest, time.Now())
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
		if err != nil || !held {
			return false
		}
		_, err = s.probeGroup(ctx, selection)
		return err == nil
	}
	go watchDetectionConfiguration(roundCtx, current, cancel)
	budget := &detectionRoundBudget{reserve: func(ctx context.Context, group *Group) (bool, error) {
		return s.coordinator.ReserveSend(ctx, token, ChannelDetectionSnapshotGuard{
			Feature: feature, Config: config.stored,
		}, group.ID, group.RPMLimit, channelDetectionDailySends)
	}}
	sample := s.runSample(roundCtx, gateway, selection, current, budget)

	// A timed-out sample can record partial results. A canceled or superseded
	// sample cannot write, even if an upstream ignores request cancellation.
	if errors.Is(roundCtx.Err(), context.Canceled) || ctx.Err() != nil {
		return
	}
	writeCtx, stopWrite := context.WithTimeout(ctx, 4*time.Second)
	defer stopWrite()
	if !current(writeCtx) {
		return
	}
	latest.Samples[selection.GroupID] = sample
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
