package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
)

const (
	SettingKeyChannelDetectionFeature     = "channel_detection_enabled"
	SettingKeyChannelDetectionConfig      = "channel_detection_config"
	SettingKeyChannelDetectionLatest      = "channel_detection_latest"
	channelDetectionMaxGroups             = 16
	channelDetectionMinInterval           = 15
	channelDetectionMaxInterval           = 1440
	channelDetectionRoundTimeout          = 130 * time.Second
	channelDetectionLeaseTimeout          = 190 * time.Second
	ChannelDetectionDefaultSuspendMinutes = 1440
	ChannelDetectionMaxSuspendMinutes     = 10080
	channelDetectionIdentityVersion       = 1
	channelDetectionDailySends            = 672
	channelDetectionRoundSends            = 2
)

var (
	errDetectionDisabled = infraerrors.NotFound("CHANNEL_DETECTION_DISABLED", "Channel detection is disabled")
	errDetectionConfig   = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_CONFIG", "Invalid detection schedule, suspension duration, or group selection")
	errDetectionModel    = infraerrors.BadRequest("CHANNEL_DETECTION_INVALID_MODEL", "Select an effective allowlist model in an active group")
)

type ChannelDetectionSelection struct {
	GroupID int64  `json:"group_id"`
	Model   string `json:"model"`
}

type ChannelDetectionConfig struct {
	Enabled               bool                        `json:"enabled"`
	IntervalMinutes       int                         `json:"interval_minutes"`
	SuspendMinutes        *int                        `json:"suspend_minutes"`
	AutoConfirmMissingSVG *bool                       `json:"auto_confirm_missing_svg"`
	DailyAnchorTime       *string                     `json:"daily_anchor_time"`
	Groups                []ChannelDetectionSelection `json:"groups"`
}

type ChannelDetectionGroup struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Active         bool   `json:"active"`
	HasModels      bool   `json:"has_models"`
	ModeRestricted bool   `json:"mode_restricted"`
	ClaudeCodeOnly bool   `json:"claude_code_only"`
}

type ChannelDetectionSample struct {
	CheckedAt           time.Time `json:"checked_at"`
	Pelican             string    `json:"pelican_status"`
	SVG                 string    `json:"svg,omitempty"`
	completedMissingSVG bool
}

type ChannelDetectionPublicGroup struct {
	Name   string                  `json:"name"`
	Model  string                  `json:"model,omitempty"`
	Sample *ChannelDetectionSample `json:"sample,omitempty"`
}

type channelDetectionConfigRecord struct {
	ChannelDetectionConfig
	Revision           string `json:"revision"`
	SuspensionRevision string `json:"suspension_revision,omitempty"`
	stored             string
}

type channelDetectionImage struct {
	Model string `json:"model"`
	ChannelDetectionSample
}

type channelDetectionSnapshot struct {
	IdentityVersion int                               `json:"identity_version"`
	Feature         string                            `json:"feature"`
	Revision        string                            `json:"revision"`
	Samples         map[int64]*ChannelDetectionSample `json:"samples"`
	AttemptedAt     map[int64]time.Time               `json:"attempted_at"`
	LegacyRunning   *channelDetectionRun              `json:"running,omitempty"`
	RunningByGroup  map[int64]channelDetectionRun     `json:"running_by_group,omitempty"`
	Proofs          map[int64]ChannelDetectionProof   `json:"proofs,omitempty"`
	NoSend          map[int64]bool                    `json:"no_send,omitempty"`
	Images          map[int64]channelDetectionImage   `json:"images,omitempty"`
	ScheduleSince   map[int64]time.Time               `json:"schedule_since,omitempty"`
	LastSlot        map[int64]time.Time               `json:"last_slot,omitempty"`
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
}

// ChannelDetectionSnapshotGuard identifies the settings and snapshot read by a worker.
type ChannelDetectionSnapshotGuard struct {
	Feature  string
	Config   string
	Snapshot string
}

type ChannelDetectionActiveLeases struct {
	Groups       map[int64]string
	LegacyOwner  string
	Blocked      bool
	Now          time.Time
	FeatureSince time.Time
}

func (a ChannelDetectionActiveLeases) GloballyBlocked() bool {
	return a.LegacyOwner != "" || a.Blocked
}

// ChannelDetectionCoordinator serializes probe runs and fences snapshot writes
// against the current owner, configuration, and previously read snapshot.
type ChannelDetectionCoordinator interface {
	TryAcquire(ctx context.Context, groupID int64, owner string, ttl time.Duration) (bool, error)
	ActiveLeases(ctx context.Context) (ChannelDetectionActiveLeases, error)
	Holds(ctx context.Context, groupID int64, owner string) (bool, error)
	Release(ctx context.Context, groupID int64, owner string) error
	ReserveSend(ctx context.Context, owner string, expected ChannelDetectionSnapshotGuard, groupID int64, groupRPM, dailyLimit int) (bool, error)
	CompareAndSetSnapshot(ctx context.Context, groupID int64, owner string, expected ChannelDetectionSnapshotGuard, next string) (bool, error)
	PublishSnapshot(ctx context.Context, owner string, expected ChannelDetectionSnapshotGuard, next string, completedGroupID int64) (bool, error)
	SavePlan(ctx context.Context, expected ChannelDetectionSnapshotGuard, nextConfig, nextSnapshot string) (bool, error)
}

type ChannelDetection struct {
	settings    channelDetectionSettings
	groups      channelDetectionGroups
	directory   channelDetectionGroupDirectory
	coordinator ChannelDetectionCoordinator
	lifecycle   sync.Mutex
	worker      context.Context
	gateway     http.Handler
	runs        sync.WaitGroup
	cancel      context.CancelFunc
	done        chan struct{}
	wake        chan struct{}
}

func NewChannelDetection(settings SettingRepository, groups AdminService, directory GroupRepository, coordinator ChannelDetectionCoordinator) *ChannelDetection {
	return &ChannelDetection{settings: settings, groups: groups, directory: directory, coordinator: coordinator}
}

func decodeChannelDetectionConfig(raw string) (channelDetectionConfigRecord, error) {
	config := channelDetectionConfigRecord{ChannelDetectionConfig: ChannelDetectionConfig{
		IntervalMinutes: 60,
		Groups:          []ChannelDetectionSelection{},
	}, stored: raw}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return config, err
		}
	}
	config.applyDefaults(ChannelDetectionConfig{
		SuspendMinutes:        new(ChannelDetectionDefaultSuspendMinutes),
		AutoConfirmMissingSVG: new(false),
		DailyAnchorTime:       new(""),
	})
	if !config.validPolicy() || (config.Enabled && (config.IntervalMinutes < channelDetectionMinInterval || config.IntervalMinutes > channelDetectionMaxInterval)) {
		return config, errDetectionConfig
	}
	return config, nil
}

func (c *ChannelDetectionConfig) applyDefaults(previous ChannelDetectionConfig) {
	if c.SuspendMinutes == nil {
		c.SuspendMinutes = previous.SuspendMinutes
	}
	if c.AutoConfirmMissingSVG == nil {
		c.AutoConfirmMissingSVG = previous.AutoConfirmMissingSVG
	}
	if c.DailyAnchorTime == nil {
		c.DailyAnchorTime = previous.DailyAnchorTime
	}
}

func (c ChannelDetectionConfig) validPolicy() bool {
	if *c.SuspendMinutes < 1 || *c.SuspendMinutes > ChannelDetectionMaxSuspendMinutes {
		return false
	}
	value := *c.DailyAnchorTime
	if value == "" {
		return true
	}
	parsed, err := time.Parse("15:04", value)
	return err == nil && parsed.Format("15:04") == value
}

func (s *ChannelDetection) state(ctx context.Context, withSamples bool) (string, channelDetectionConfigRecord, channelDetectionSnapshot, error) {
	var latest channelDetectionSnapshot
	keys := []string{SettingKeyChannelDetectionFeature, SettingKeyChannelDetectionConfig}
	if withSamples {
		keys = append(keys, SettingKeyChannelDetectionLatest)
	}
	values, err := s.settings.GetMultiple(ctx, keys)
	if err != nil {
		return "", channelDetectionConfigRecord{}, latest, err
	}
	feature := values[SettingKeyChannelDetectionFeature]
	config, err := decodeChannelDetectionConfig(values[SettingKeyChannelDetectionConfig])
	if err != nil {
		return "", config, latest, err
	}
	storedSnapshot := values[SettingKeyChannelDetectionLatest]
	if value := storedSnapshot; value != "" && withSamples {
		if err := json.Unmarshal([]byte(value), &latest); err != nil {
			return "", config, latest, err
		}
	}
	if latest.Revision != config.Revision {
		latest = channelDetectionSnapshot{Feature: feature, Revision: config.Revision}
	}
	latest.stored = storedSnapshot
	if latest.IdentityVersion != channelDetectionIdentityVersion || latest.Samples == nil {
		latest.IdentityVersion = channelDetectionIdentityVersion
		latest.Samples = make(map[int64]*ChannelDetectionSample)
		latest.Proofs = nil
	}
	if latest.AttemptedAt == nil {
		latest.AttemptedAt = make(map[int64]time.Time)
	}
	if latest.RunningByGroup == nil {
		latest.RunningByGroup = make(map[int64]channelDetectionRun)
	}
	if old := latest.LegacyRunning; old != nil {
		if _, exists := latest.RunningByGroup[old.GroupID]; !exists {
			latest.RunningByGroup[old.GroupID] = *old
		}
		latest.LegacyRunning = nil
	}
	if latest.Images == nil {
		// Upgrade the previous public image while the matching plan still
		// identifies the model. Its original check time survives later failures.
		latest.Images = make(map[int64]channelDetectionImage)
		for _, selection := range config.Groups {
			latest.rememberImage(selection, latest.Samples[selection.GroupID])
		}
	}
	if latest.Feature != feature {
		latest.cancelRuns()
		latest.Feature = feature
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
	feature, current, latest, err := s.state(ctx, true)
	if err != nil {
		return err
	}
	if feature == "" {
		return errDetectionDisabled
	}
	input.applyDefaults(current.ChannelDetectionConfig)
	if input.IntervalMinutes < channelDetectionMinInterval || input.IntervalMinutes > channelDetectionMaxInterval ||
		!input.validPolicy() ||
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
	scheduleChanged := current.Enabled != input.Enabled || current.IntervalMinutes != input.IntervalMinutes ||
		*current.DailyAnchorTime != *input.DailyAnchorTime
	policyChanged := *current.SuspendMinutes != *input.SuspendMinutes || *current.AutoConfirmMissingSVG != *input.AutoConfirmMissingSVG
	if !scheduleChanged && !policyChanged && slices.Equal(current.Groups, input.Groups) {
		return nil
	}
	next := channelDetectionConfigRecord{
		ChannelDetectionConfig: input,
		Revision:               uuid.NewString(),
		SuspensionRevision:     current.SuspensionRevision,
	}
	if policyChanged {
		next.SuspensionRevision = uuid.NewString()
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if s.coordinator == nil {
		return errors.New("detection coordinator unavailable")
	}
	keep := make(map[int64]bool, len(input.Groups))
	for _, selection := range input.Groups {
		if slices.Contains(current.Groups, selection) {
			keep[selection.GroupID] = true
		}
	}
	for attempt := range 3 {
		if attempt != 0 {
			currentFeature, plan, snapshot, err := s.state(ctx, true)
			if err != nil {
				return err
			}
			if feature != currentFeature || current.stored != plan.stored {
				return ErrDetectionChanged
			}
			latest = snapshot
		}
		guard := latest.guard(current)
		if scheduleChanged {
			latest.cancelRuns()
		}
		latest.Revision = next.Revision
		maps.DeleteFunc(latest.RunningByGroup, func(id int64, _ channelDetectionRun) bool { return !keep[id] })
		maps.DeleteFunc(latest.Samples, func(id int64, _ *ChannelDetectionSample) bool { return !keep[id] })
		maps.DeleteFunc(latest.Proofs, func(id int64, _ ChannelDetectionProof) bool { return !keep[id] })
		maps.DeleteFunc(latest.AttemptedAt, func(id int64, _ time.Time) bool { return !keep[id] })
		maps.DeleteFunc(latest.NoSend, func(id int64, _ bool) bool { return !keep[id] })
		maps.DeleteFunc(latest.Images, func(id int64, _ channelDetectionImage) bool { return !keep[id] })
		maps.DeleteFunc(latest.ScheduleSince, func(id int64, _ time.Time) bool { return !keep[id] || scheduleChanged })
		maps.DeleteFunc(latest.LastSlot, func(id int64, _ time.Time) bool { return !keep[id] || scheduleChanged })
		if *next.DailyAnchorTime != "" && next.Enabled {
			if latest.ScheduleSince == nil {
				latest.ScheduleSince = make(map[int64]time.Time)
			}
			for _, selection := range next.Groups {
				if scheduleChanged || !keep[selection.GroupID] {
					// SavePlan replaces this sentinel with the database activation time.
					latest.ScheduleSince[selection.GroupID] = time.Time{}
				}
			}
		}
		snapshot, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		saved, err := s.coordinator.SavePlan(ctx, guard, string(data), string(snapshot))
		if err != nil || saved {
			return err
		}
	}
	return ErrDetectionChanged
}

func (s *channelDetectionSnapshot) cancelRuns() {
	for id, run := range s.RunningByGroup {
		switch {
		case run.PreviousAttemptedAt != nil:
			s.AttemptedAt[id] = *run.PreviousAttemptedAt
		case s.Samples[id] != nil:
			// Older runtime markers did not retain the previous attempt time.
			s.AttemptedAt[id] = s.Samples[id].CheckedAt
		default:
			delete(s.AttemptedAt, id)
		}
	}
	clear(s.RunningByGroup)
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
	ranked, err := s.resultGroups(ctx, feature, config, latest)
	if err != nil {
		return nil, err
	}
	results := make([]ChannelDetectionPublicGroup, 0, len(ranked))
	for _, group := range ranked {
		results = append(results, group.result)
	}
	return results, nil
}

type channelDetectionResultGroup struct {
	id             int64
	result         ChannelDetectionPublicGroup
	rateMultiplier float64
}

func (s *ChannelDetection) resultGroups(ctx context.Context, feature string, config channelDetectionConfigRecord, latest channelDetectionSnapshot) ([]channelDetectionResultGroup, error) {
	if feature == "" || !config.Enabled {
		return nil, errDetectionDisabled
	}
	ranked := make([]channelDetectionResultGroup, 0, len(config.Groups))
	for _, selection := range config.Groups {
		group, err := s.probeGroup(ctx, selection)
		if errors.Is(err, errDetectionModel) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result := ChannelDetectionPublicGroup{Name: group.Name, Model: selection.Model}
		if image := latest.Images[selection.GroupID]; image.Model == selection.Model {
			if svg := sanitizeDetectionSVG(image.SVG); svg != "" {
				sample := image.ChannelDetectionSample
				sample.SVG = svg
				result.Sample = &sample
			}
		}
		ranked = append(ranked, channelDetectionResultGroup{id: group.ID, result: result, rateMultiplier: group.RateMultiplier})
	}
	// Group resolution can outlive a switch/config update on another instance.
	matches, err := s.current(ctx, feature, config.Revision)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, errDetectionDisabled
	}
	slices.SortStableFunc(ranked, func(a, b channelDetectionResultGroup) int {
		return cmp.Compare(a.rateMultiplier, b.rateMultiplier)
	})
	return ranked, nil
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
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.worker != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.worker = ctx
	s.gateway = gateway
	s.cancel = cancel
	s.done = make(chan struct{})
	s.wake = make(chan struct{}, 1)
	go func() {
		defer func() {
			s.runs.Wait()
			close(s.done)
		}()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			s.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-s.wake:
			}
		}
	}()
}

func (s *ChannelDetection) Stop(ctx context.Context) error {
	s.lifecycle.Lock()
	if s.cancel == nil {
		s.lifecycle.Unlock()
		return nil
	}
	s.cancel()
	s.lifecycle.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ChannelDetection) tick(ctx context.Context) {
	if s.coordinator == nil {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	feature, config, latest, err := s.state(checkCtx, true)
	if err != nil || feature == "" || !config.Enabled {
		return
	}
	active, err := s.coordinator.ActiveLeases(checkCtx)
	if err != nil || active.GloballyBlocked() {
		return
	}
	groups := slices.Clone(config.Groups)
	slices.SortStableFunc(groups, func(a, b ChannelDetectionSelection) int {
		return latest.AttemptedAt[a.GroupID].Compare(latest.AttemptedAt[b.GroupID])
	})
	for _, selection := range groups {
		if checkCtx.Err() != nil {
			return
		}
		if active.Groups[selection.GroupID] != "" || !detectionGroupDue(config, latest, selection.GroupID, active.Now, active.FeatureSince) {
			continue
		}
		_, _ = s.admit(checkCtx, selection, latest.guard(config), "automatic")
	}
}

func (s *ChannelDetection) admit(ctx context.Context, selection ChannelDetectionSelection, expected ChannelDetectionSnapshotGuard, source string) (string, error) {
	s.lifecycle.Lock()
	worker, gateway, wake := s.worker, s.gateway, s.wake
	if worker == nil || worker.Err() != nil || gateway == nil || s.coordinator == nil {
		s.lifecycle.Unlock()
		return "", ErrDetectionBusy
	}
	s.runs.Add(1)
	s.lifecycle.Unlock()

	id, owner := selection.GroupID, uuid.NewString()
	handedOff := false
	defer func() {
		if !handedOff {
			s.runs.Done()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	stop := context.AfterFunc(worker, cancel)
	defer stop()

	acquired, err := s.coordinator.TryAcquire(ctx, id, owner, channelDetectionLeaseTimeout)
	if err != nil {
		_ = s.release(ctx, id, owner)
		return "", err
	}
	if !acquired {
		return "", ErrDetectionBusy
	}
	defer func() {
		if !handedOff {
			_ = s.release(ctx, id, owner)
		}
	}()

	group, err := s.probeGroup(ctx, selection)
	if err != nil {
		if errors.Is(err, errDetectionModel) {
			err = ErrDetectionChanged
		}
		return "", err
	}
	run := channelDetectionRun{GroupID: id, Owner: owner, SampleID: uuid.NewString(), Source: source}
	active, err := s.coordinator.ActiveLeases(ctx)
	if err != nil {
		return "", err
	}
	started, err := s.updateRunSnapshot(ctx, run, expected, false, func(config channelDetectionConfigRecord, latest *channelDetectionSnapshot) bool {
		now := active.Now.UTC()
		if !slices.Contains(config.Groups, selection) ||
			(source == "automatic" && !detectionGroupDue(config, *latest, id, now, active.FeatureSince)) {
			return false
		}
		if source == "automatic" && *config.DailyAnchorTime != "" {
			if latest.LastSlot == nil {
				latest.LastSlot = make(map[int64]time.Time)
			}
			latest.LastSlot[id] = channelDetectionSlot(*config.DailyAnchorTime, config.IntervalMinutes, now)
		}
		if *config.AutoConfirmMissingSVG {
			run.SuspensionRevision = config.SuspensionRevision
			run.GroupPlatform = group.Platform
		}
		run.PreviousAttemptedAt = nil
		if previous, exists := latest.AttemptedAt[id]; exists {
			run.PreviousAttemptedAt = &previous
		}
		latest.RunningByGroup[id] = run
		latest.AttemptedAt[id] = now
		return true
	})
	if err != nil {
		return "", err
	}
	if !started {
		return "", ErrDetectionChanged
	}

	// Stop and handoff must agree on whether this run was accepted.
	s.lifecycle.Lock()
	if worker.Err() != nil {
		s.lifecycle.Unlock()
		// The gateway has not been called. Undo only this run while we still
		// hold its lease, even though shutdown canceled the admission context.
		undoCtx, stopUndo := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stopUndo()
		_, err := s.updateRunSnapshot(undoCtx, run, expected, false, func(_ channelDetectionConfigRecord, latest *channelDetectionSnapshot) bool {
			marker := latest.RunningByGroup[id]
			if marker.Owner != owner || marker.SampleID != run.SampleID {
				return false
			}
			delete(latest.RunningByGroup, id)
			if marker.PreviousAttemptedAt == nil {
				delete(latest.AttemptedAt, id)
			} else {
				latest.AttemptedAt[id] = *marker.PreviousAttemptedAt
			}
			return true
		})
		if err != nil {
			slog.Warn("channel_detection.admission_undo_failed", "group_id", id, "error", err)
		}
		return "", ErrDetectionBusy
	}
	handedOff = true
	go func() {
		defer s.runs.Done()
		s.run(worker, gateway, run, expected, selection)
		if err := s.release(worker, id, owner); err == nil && worker.Err() == nil {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	s.lifecycle.Unlock()
	return run.SampleID, nil
}

func (s *ChannelDetection) release(ctx context.Context, groupID int64, owner string) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.coordinator.Release(releaseCtx, groupID, owner)
}

func (s *ChannelDetection) run(ctx context.Context, gateway http.Handler, run channelDetectionRun, expected ChannelDetectionSnapshotGuard, selection ChannelDetectionSelection) {
	id, owner := run.GroupID, run.Owner
	roundCtx, cancel := context.WithTimeout(ctx, channelDetectionRoundTimeout)
	defer cancel()
	currentGuard := func(ctx context.Context) (ChannelDetectionSnapshotGuard, bool, error) {
		feature, config, latest, err := s.state(ctx, true)
		if err != nil || feature != expected.Feature || !config.Enabled ||
			!slices.Contains(config.Groups, selection) || !latest.matchesRun(run) {
			return ChannelDetectionSnapshotGuard{}, false, err
		}
		held, err := s.coordinator.Holds(ctx, id, owner)
		if err != nil || !held {
			return ChannelDetectionSnapshotGuard{}, false, err
		}
		_, err = s.probeGroup(ctx, selection)
		return latest.guard(config), err == nil, err
	}
	current := func(ctx context.Context) bool {
		_, ok, _ := currentGuard(ctx)
		return ok
	}
	go watchDetectionConfiguration(roundCtx, current, cancel)
	budget := &detectionRoundBudget{trackCompletion: run.SuspensionRevision != "", reserve: func(ctx context.Context, group *Group) (bool, error) {
		guard, current, err := currentGuard(ctx)
		if err != nil || !current {
			return false, err
		}
		for range 3 {
			ok, err := s.coordinator.ReserveSend(ctx, owner, guard, group.ID, group.RPMLimit, channelDetectionDailySends)
			if ok || err != nil {
				return ok, err
			}
			next, current, err := currentGuard(ctx)
			if err != nil || !current || next.Config == guard.Config {
				return false, err
			}
			guard = next
		}
		return false, nil
	}}
	sample := s.runSample(roundCtx, gateway, selection, current, budget)

	// A question timeout can be published. Shutdown and superseded rounds
	// cannot write, even if an upstream ignores cancellation.
	if errors.Is(roundCtx.Err(), context.Canceled) || ctx.Err() != nil {
		return
	}
	writeCtx, stopWrite := context.WithTimeout(ctx, 4*time.Second)
	defer stopWrite()
	if !current(writeCtx) {
		return
	}
	proof, entered := budget.proof(run)
	if roundCtx.Err() == nil && sample.completedMissingSVG && budget.autoConfirmable() {
		proof.SuspensionRevision = run.SuspensionRevision
	}
	noSend := !entered && sample.Pelican != "generated"
	_, err := s.updateRunSnapshot(writeCtx, run, expected, true, func(_ channelDetectionConfigRecord, latest *channelDetectionSnapshot) bool {
		if !latest.matchesRun(run) {
			return false
		}
		delete(latest.RunningByGroup, id)
		delete(latest.NoSend, id)
		if noSend {
			sample.Pelican = "untested"
			if latest.NoSend == nil {
				latest.NoSend = make(map[int64]bool)
			}
			latest.NoSend[id] = true
		}
		latest.rememberImage(selection, sample)
		latest.Samples[id] = &ChannelDetectionSample{CheckedAt: sample.CheckedAt, Pelican: sample.Pelican}
		if latest.Proofs == nil {
			latest.Proofs = make(map[int64]ChannelDetectionProof)
		}
		latest.Proofs[id] = proof
		return true
	})
	if err != nil && ctx.Err() == nil {
		slog.Warn("channel_detection.publish_failed", "group_id", id, "error", err)
	}
}

func (latest channelDetectionSnapshot) guard(config channelDetectionConfigRecord) ChannelDetectionSnapshotGuard {
	return ChannelDetectionSnapshotGuard{Feature: latest.Feature, Config: config.stored, Snapshot: latest.stored}
}

func (latest channelDetectionSnapshot) matchesRun(run channelDetectionRun) bool {
	marker := latest.RunningByGroup[run.GroupID]
	return marker.Owner == run.Owner && marker.SampleID == run.SampleID
}

func (s *ChannelDetection) updateRunSnapshot(ctx context.Context, run channelDetectionRun, expected ChannelDetectionSnapshotGuard, publish bool, update func(channelDetectionConfigRecord, *channelDetectionSnapshot) bool) (bool, error) {
	for ctx.Err() == nil {
		feature, config, latest, err := s.state(ctx, true)
		if err != nil || feature != expected.Feature || !config.Enabled ||
			(config.stored != expected.Config && !latest.matchesRun(run)) {
			return false, err
		}
		if !update(config, &latest) {
			return false, nil
		}
		encoded, err := json.Marshal(latest)
		if err != nil {
			return false, err
		}
		guard := latest.guard(config)
		var applied bool
		if publish {
			applied, err = s.coordinator.PublishSnapshot(ctx, run.Owner, guard, string(encoded), run.GroupID)
		} else {
			applied, err = s.coordinator.CompareAndSetSnapshot(ctx, run.GroupID, run.Owner, guard, string(encoded))
		}
		if err != nil || applied {
			return applied, err
		}
		if held, err := s.coordinator.Holds(ctx, run.GroupID, run.Owner); err != nil || !held {
			return false, err
		}
	}
	return false, ctx.Err()
}

func (latest *channelDetectionSnapshot) rememberImage(selection ChannelDetectionSelection, sample *ChannelDetectionSample) {
	if sample == nil || sample.Pelican != "generated" {
		return
	}
	svg := sanitizeDetectionSVG(sample.SVG)
	if svg == "" {
		return
	}
	latest.Images[selection.GroupID] = channelDetectionImage{Model: selection.Model, ChannelDetectionSample: ChannelDetectionSample{
		CheckedAt: sample.CheckedAt, Pelican: "generated", SVG: svg,
	}}
}

var channelDetectionBeijing = time.FixedZone("UTC+8", 8*60*60)

func channelDetectionSlot(anchorTime string, minutes int, now time.Time) time.Time {
	anchor, _ := time.Parse("15:04", anchorTime)
	local := now.In(channelDetectionBeijing)
	start := time.Date(local.Year(), local.Month(), local.Day(), anchor.Hour(), anchor.Minute(), 0, 0, channelDetectionBeijing)
	if local.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	interval := time.Duration(minutes) * time.Minute
	return start.Add(local.Sub(start) / interval * interval).UTC()
}

func detectionGroupDue(config channelDetectionConfigRecord, latest channelDetectionSnapshot, id int64, now, featureSince time.Time) bool {
	if *config.DailyAnchorTime == "" {
		return !latest.AttemptedAt[id].Add(time.Duration(config.IntervalMinutes) * time.Minute).After(now)
	}
	slot := channelDetectionSlot(*config.DailyAnchorTime, config.IntervalMinutes, now)
	return slot.After(featureSince) && slot.After(latest.ScheduleSince[id]) && slot.After(latest.LastSlot[id])
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
