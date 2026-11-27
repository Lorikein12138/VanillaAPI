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
	latest.Running = &channelDetectionRun{GroupID: 1, Owner: "old-owner", PreviousAttemptedAt: &past}
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
	require.Nil(t, kept.Running)
	require.Len(t, kept.Samples, 2)
	require.Equal(t, latest.Proofs, kept.Proofs)
	require.Equal(t, map[int64]time.Time{1: past, 2: past}, kept.AttemptedAt)
	selection, due := dueDetectionGroup(updated, kept, past.Add(time.Hour))
	require.True(t, due)
	require.EqualValues(t, 3, selection.GroupID)

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
	selection, due = dueDetectionGroup(updated, kept, past.Add(45*time.Minute))
	require.True(t, due)
	require.EqualValues(t, 1, selection.GroupID)

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
	locked, err := store.TryAcquire(ctx, "worker", time.Minute)
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

func TestChannelDetectionFeatureChangeCancelsUnfinishedAttempt(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	feature, plan, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	now := time.Now().UTC()
	lock, err := s.storage.TryAcquire(ctx, "worker", time.Minute)
	require.NoError(t, err)
	require.True(t, lock)
	latest.Samples[2] = &ChannelDetectionSample{CheckedAt: now}
	latest.AttemptedAt[2] = now
	run, started := s.startRun(ctx, "worker", plan, &latest, s.cfg.Groups[0], "automatic")
	require.True(t, started)
	require.EqualValues(t, 1, run.GroupID)
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionFeature, ""))
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionFeature, "enable-two"))
	reenabled, current, kept, err := s.state(ctx, true)
	require.NoError(t, err)
	require.NotEqual(t, feature, reenabled)
	require.Equal(t, plan.Revision, current.Revision)
	require.Equal(t, now, kept.Samples[2].CheckedAt)
	require.Equal(t, now, kept.AttemptedAt[2])
	require.Nil(t, kept.Running)
	require.NotContains(t, kept.AttemptedAt, int64(1))
	applied, err := s.storage.CompareAndSetSnapshot(ctx, "worker", latest.guard(plan), "stale")
	require.NoError(t, err)
	require.False(t, applied)
}
