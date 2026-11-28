package service

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelDetectionPlanRetainsCompletedMatchingGroups(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	_, config, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	past := time.Now().UTC().Add(-45 * time.Minute)
	for _, selection := range config.Groups {
		latest.Samples[selection.GroupID] = &ChannelDetectionSample{CheckedAt: past}
		latest.AttemptedAt[selection.GroupID] = past
	}
	latest.Proofs = map[int64]ChannelDetectionProof{
		1: {Version: channelDetectionProofVersion, SampleID: "prior-one"},
		2: {Version: channelDetectionProofVersion, SampleID: "prior-two"},
	}
	latest.AttemptedAt[1] = past.Add(20 * time.Minute)
	latest.LegacyRunning = &channelDetectionRun{GroupID: 1, Owner: "old-owner", PreviousAttemptedAt: &past}
	raw, err := json.Marshal(latest)
	require.NoError(t, err)
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionLatest, string(raw)))

	// Sorting and toggling the plan preserve completed data and restore a
	// canceled round's previous attempt time. An added group starts fresh.
	s.groupRows[3] = &Group{ID: 3, Name: "Third", Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Models: []string{"model-three"}}}
	next := ChannelDetectionConfig{Enabled: false, IntervalMinutes: 120, Groups: []ChannelDetectionSelection{
		s.cfg.Groups[1], s.cfg.Groups[0], {GroupID: 3, Model: "model-three"},
	}}
	require.NoError(t, s.Save(ctx, next))
	_, updated, kept, err := s.state(ctx, true)
	require.NoError(t, err)
	require.NotEqual(t, config.Revision, updated.Revision)
	require.Empty(t, kept.RunningByGroup)
	require.Len(t, kept.Samples, 2)
	require.Equal(t, latest.Proofs, kept.Proofs)
	require.Equal(t, map[int64]time.Time{1: past, 2: past}, kept.AttemptedAt)
	due := detectionGroupDue(updated, kept, 3, past.Add(time.Hour), time.Time{})
	require.True(t, due)

	next.Enabled = true
	require.NoError(t, s.Save(ctx, next))
	next.Groups = slices.Clone(next.Groups[:2]) // remove the unrelated third group
	next.Groups[1].Model = "updated-model"
	s.groupRows[1].ModelAllowlist.Models = append(s.groupRows[1].ModelAllowlist.Models, "updated-model")
	require.NoError(t, s.Save(ctx, next))
	_, updated, kept, err = s.state(ctx, true)
	require.NoError(t, err)
	require.Nil(t, kept.Samples[1])
	require.NotContains(t, kept.Proofs, int64(1))
	require.NotContains(t, kept.AttemptedAt, int64(1))
	require.Equal(t, past, kept.Samples[2].CheckedAt)
	require.Equal(t, "prior-two", kept.Proofs[2].SampleID)
	due = detectionGroupDue(updated, kept, 1, past.Add(45*time.Minute), time.Time{})
	require.True(t, due)

	// Removing and re-adding the same group/model cannot resurrect its data.
	next.Groups = next.Groups[1:]
	require.NoError(t, s.Save(ctx, next))
	next.Groups = append(next.Groups, s.cfg.Groups[1])
	require.NoError(t, s.Save(ctx, next))
	_, _, kept, err = s.state(ctx, true)
	require.NoError(t, err)
	require.Empty(t, kept.Samples)
	require.Empty(t, kept.Proofs)
	require.Empty(t, kept.AttemptedAt)
}

type detectionPlanRace struct {
	*detectionMemorySettings
	beforeSave func(ChannelDetectionSnapshotGuard)
}

func (m *detectionPlanRace) SavePlan(ctx context.Context, expected ChannelDetectionSnapshotGuard, config, snapshot string) (bool, error) {
	if m.beforeSave != nil {
		hook := m.beforeSave
		m.beforeSave = nil
		hook(expected)
	}
	return m.detectionMemorySettings.SavePlan(ctx, expected, config, snapshot)
}

func TestChannelDetectionPlanRetriesAfterWorkerPublish(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	store := &detectionPlanRace{detectionMemorySettings: s.storage}
	s.coordinator = store
	locked, err := store.TryAcquire(ctx, 1, "worker", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	now := time.Now().UTC()
	store.beforeSave = func(guard ChannelDetectionSnapshotGuard) {
		var latest channelDetectionSnapshot
		require.NoError(t, json.Unmarshal([]byte(guard.Snapshot), &latest))
		latest.Samples[1] = &ChannelDetectionSample{CheckedAt: now}
		latest.Proofs = map[int64]ChannelDetectionProof{1: {SampleID: "newly-completed"}}
		latest.AttemptedAt[1] = now
		raw, err := json.Marshal(latest)
		require.NoError(t, err)
		published, err := store.PublishSnapshot(ctx, "worker", guard, string(raw), 1)
		require.NoError(t, err)
		require.True(t, published)
	}
	next := s.cfg
	next.IntervalMinutes = 90
	require.NoError(t, s.Save(ctx, next))
	_, _, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	require.Equal(t, "newly-completed", latest.Proofs[1].SampleID)
	require.Equal(t, now, latest.AttemptedAt[1])
	require.Equal(t, now, latest.Samples[1].CheckedAt)
}

func TestChannelDetectionPlanRemovalKeepsOtherActiveRun(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	_, config, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	old := latest.guard(config)
	remaining := channelDetectionRun{GroupID: 2, Owner: "other-worker", SampleID: "in-flight"}
	for _, run := range []channelDetectionRun{
		{GroupID: 1, Owner: "leaving-worker", SampleID: "removed"},
		remaining,
	} {
		ok, err := s.storage.TryAcquire(ctx, run.GroupID, run.Owner, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		applied, err := s.updateRunSnapshot(ctx, run, old, false, func(_ channelDetectionConfigRecord, current *channelDetectionSnapshot) bool {
			current.RunningByGroup[run.GroupID] = run
			return true
		})
		require.NoError(t, err)
		require.True(t, applied)
	}

	next := s.cfg
	next.Groups = next.Groups[1:]
	require.NoError(t, s.Save(ctx, next))
	_, _, latest, err = s.state(ctx, true)
	require.NoError(t, err)
	require.NotContains(t, latest.RunningByGroup, int64(1))
	require.True(t, latest.matchesRun(remaining))
	applied, err := s.updateRunSnapshot(ctx, remaining, old, false, func(_ channelDetectionConfigRecord, current *channelDetectionSnapshot) bool {
		delete(current.RunningByGroup, remaining.GroupID)
		current.Samples[remaining.GroupID] = &ChannelDetectionSample{Pelican: "passed"}
		return true
	})
	require.NoError(t, err)
	require.True(t, applied, "retained worker can advance with a refreshed whole-snapshot CAS")
}

func TestChannelDetectionFeatureChangeCancelsUnfinishedAttempt(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	feature, plan, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	now := time.Now().UTC()
	lock, err := s.storage.TryAcquire(ctx, 1, "worker", time.Minute)
	require.NoError(t, err)
	require.True(t, lock)
	latest.Samples[2] = &ChannelDetectionSample{CheckedAt: now}
	latest.AttemptedAt[2] = now
	latest.RunningByGroup[1] = channelDetectionRun{GroupID: 1, Owner: "worker", SampleID: "in-flight"}
	latest.AttemptedAt[1] = now
	data, err := json.Marshal(latest)
	require.NoError(t, err)
	started, err := s.storage.CompareAndSetSnapshot(ctx, 1, "worker", latest.guard(plan), string(data))
	require.NoError(t, err)
	require.True(t, started)
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionFeature, ""))
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionFeature, "enable-two"))
	reenabled, current, kept, err := s.state(ctx, true)
	require.NoError(t, err)
	require.NotEqual(t, feature, reenabled)
	require.Equal(t, plan.Revision, current.Revision)
	require.Equal(t, now, kept.Samples[2].CheckedAt)
	require.Equal(t, now, kept.AttemptedAt[2])
	require.Empty(t, kept.RunningByGroup)
	require.NotContains(t, kept.AttemptedAt, int64(1))
	applied, err := s.storage.CompareAndSetSnapshot(ctx, 1, "worker", latest.guard(plan), "stale")
	require.NoError(t, err)
	require.False(t, applied)
}

func TestChannelDetectionFixedScheduleBeijingSlots(t *testing.T) {
	beijing := func(day, hour, minute int) time.Time {
		return time.Date(2026, 11, day, hour, minute, 0, 0, channelDetectionBeijing)
	}
	for _, tc := range []struct {
		minutes   int
		now, slot time.Time
	}{
		{60, beijing(28, 7, 59), beijing(28, 7, 0)},
		{60, beijing(28, 9, 10), beijing(28, 9, 0)},
		{1000, beijing(28, 0, 41), beijing(28, 0, 40)},
		{1000, beijing(28, 8, 0), beijing(28, 8, 0)},
		{1439, beijing(28, 7, 59), beijing(28, 7, 59)},
		{1440, beijing(28, 7, 59), beijing(27, 8, 0)},
		{1440, beijing(28, 8, 0), beijing(28, 8, 0)},
	} {
		require.True(t, channelDetectionSlot("08:00", tc.minutes, tc.now.UTC()).Equal(tc.slot), "%d minutes at %s", tc.minutes, tc.now)
	}

	config := channelDetectionConfigRecord{ChannelDetectionConfig: ChannelDetectionConfig{IntervalMinutes: 60, DailyAnchorTime: new("08:00")}}
	latest := channelDetectionSnapshot{
		ScheduleSince: map[int64]time.Time{1: beijing(28, 8, 30)},
		LastSlot:      map[int64]time.Time{1: beijing(27, 8, 0)},
		AttemptedAt:   map[int64]time.Time{1: beijing(28, 8, 55)},
	}
	require.False(t, detectionGroupDue(config, latest, 1, beijing(28, 8, 59), time.Time{}))
	require.True(t, detectionGroupDue(config, latest, 1, beijing(28, 9, 0), time.Time{}), "manual attempts do not push a fixed slot back")
	require.False(t, detectionGroupDue(config, latest, 1, beijing(28, 9, 30), beijing(28, 9, 7)), "feature re-enable does not backfill")
	latest.LastSlot[1] = beijing(28, 9, 0)
	require.False(t, detectionGroupDue(config, latest, 1, beijing(28, 9, 50), time.Time{}))
	require.True(t, detectionGroupDue(config, latest, 1, beijing(30, 11, 32), time.Time{}), "restart considers the latest slot")
}

func TestChannelDetectionFixedSchedulePlanKeepsSlots(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	input := s.cfg
	input.DailyAnchorTime = new("08:00")
	require.NoError(t, s.Save(ctx, input))
	_, config, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	start := latest.ScheduleSince[1]
	require.False(t, start.IsZero())
	slot := channelDetectionSlot("08:00", input.IntervalMinutes, start).Add(time.Hour)
	latest.LastSlot = map[int64]time.Time{1: slot}
	latest.AttemptedAt[1] = slot.Add(7 * time.Minute)
	latest.Samples[1] = &ChannelDetectionSample{CheckedAt: start, Pelican: "generated", SVG: testDetectionSVG}
	encoded, err := json.Marshal(latest)
	require.NoError(t, err)
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionLatest, string(encoded)))

	input.SuspendMinutes = new(15)
	input.AutoConfirmMissingSVG = new(true)
	require.NoError(t, s.Save(ctx, input))
	_, policy, kept, err := s.state(ctx, true)
	require.NoError(t, err)
	require.NotEqual(t, config.SuspensionRevision, policy.SuspensionRevision)
	require.Equal(t, start, kept.ScheduleSince[1])
	require.Equal(t, slot, kept.LastSlot[1])
	require.Equal(t, start, kept.Images[1].CheckedAt)

	input.IntervalMinutes = 1439
	require.NoError(t, s.Save(ctx, input))
	_, later, changed, err := s.state(ctx, true)
	require.NoError(t, err)
	require.Equal(t, policy.SuspensionRevision, later.SuspensionRevision)
	require.False(t, changed.ScheduleSince[1].Before(start))
	require.Empty(t, changed.LastSlot)
	require.Equal(t, kept.Images[1], changed.Images[1])
	otherGroupActivation := changed.ScheduleSince[2]

	input.Groups = slices.Clone(input.Groups)
	input.Groups[0].Model = "another"
	s.groupRows[1].ModelAllowlist.Models = append(s.groupRows[1].ModelAllowlist.Models, "another")
	require.NoError(t, s.Save(ctx, input))
	_, _, changed, err = s.state(ctx, true)
	require.NoError(t, err)
	require.NotContains(t, changed.Images, int64(1))
	require.NotContains(t, changed.Proofs, int64(1))
	require.Equal(t, otherGroupActivation, changed.ScheduleSince[2])
}
