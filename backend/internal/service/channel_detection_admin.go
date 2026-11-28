package service

import (
	"context"
	"errors"
	"slices"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrDetectionBusy         = infraerrors.Conflict("CHANNEL_DETECTION_BUSY", "A detection is running. Refresh and try again.")
	ErrDetectionChanged      = infraerrors.Conflict("CHANNEL_DETECTION_CHANGED", "The group, sample, or account has changed. Refresh and try again.")
	ErrDetectionScopeChanged = infraerrors.Conflict("CHANNEL_DETECTION_SCOPE_CHANGED", "Account group membership has changed. Refresh and review the scope.")
	ErrDetectionProtected    = infraerrors.Conflict("CHANNEL_DETECTION_ALREADY_PROTECTED", "This account already has an active detection suspension.")
)

type channelDetectionRun struct {
	GroupID             int64      `json:"group_id"`
	SampleID            string     `json:"sample_id"`
	Owner               string     `json:"owner"`
	Source              string     `json:"source"`
	PreviousAttemptedAt *time.Time `json:"previous_attempted_at,omitempty"`
	SuspensionRevision  string     `json:"suspension_revision,omitempty"`
	GroupPlatform       string     `json:"group_platform,omitempty"`
}

const (
	channelDetectionProofVersion = 2
	// Existing version 2 proofs could cover all 14 attempts of the old round.
	channelDetectionProofMaxSends = 14
)

// ChannelDetectionProof is only persisted with the private worker state.
type ChannelDetectionProof struct {
	Version            int        `json:"version"`
	SampleID           string     `json:"sample_id"`
	Source             string     `json:"source"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	AccountID          int64      `json:"account_id,omitempty"`
	RouteFingerprint   string     `json:"route_fingerprint,omitempty"`
	Sends              int        `json:"sends,omitempty"`
	SuspensionRevision string     `json:"suspension_revision,omitempty"`
}

func (p ChannelDetectionProof) Attributable() bool {
	return p.Version == channelDetectionProofVersion && p.Sends > 0 && p.Sends <= channelDetectionProofMaxSends &&
		p.AccountID > 0 && len(p.RouteFingerprint) == 64 && p.CompletedAt != nil
}

type ChannelDetectionImpactGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type ChannelDetectionScope struct {
	AccountName    string                        `json:"account_name"`
	Groups         []ChannelDetectionImpactGroup `json:"groups"`
	Version        string                        `json:"scope_version"`
	SuspendMinutes int                           `json:"suspend_minutes"`
}

type ChannelDetectionAdminGroup struct {
	ChannelDetectionPublicGroup
	Latest       *ChannelDetectionLatestResult `json:"latest,omitempty"`
	GroupID      int64                         `json:"group_id"`
	TriggerState string                        `json:"trigger_state"`
	SampleID     string                        `json:"sample_id,omitempty"`
	Confirmation *ChannelDetectionScope        `json:"confirmation,omitempty"`
}

type ChannelDetectionLatestResult struct {
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Pelican   string    `json:"pelican_status"`
}

type ChannelDetectionConfirmation struct {
	Until       time.Time `json:"until"`
	SyncPending bool      `json:"sync_pending"`
}

type ChannelDetectionConfirmationRequest struct {
	Guard         ChannelDetectionSnapshotGuard
	Selection     ChannelDetectionSelection
	Proof         ChannelDetectionProof
	ActorID       int64
	ScopeVersion  string
	GroupPlatform string
}

type channelDetectionActions interface {
	ConfirmationScope(context.Context, ChannelDetectionSelection, ChannelDetectionProof, string) (*ChannelDetectionScope, error)
	Confirm(context.Context, ChannelDetectionConfirmationRequest) (ChannelDetectionConfirmation, error)
}

func (s *ChannelDetection) AdminGroups(ctx context.Context) ([]ChannelDetectionAdminGroup, error) {
	feature, config, latest, err := s.state(ctx, true)
	if err != nil {
		return nil, err
	}
	ranked, err := s.resultGroups(ctx, feature, config, latest)
	if err != nil {
		return nil, err
	}
	active, err := s.coordinator.ActiveLeases(ctx)
	if err != nil {
		return nil, err
	}
	actions, _ := s.coordinator.(channelDetectionActions)
	results := make([]ChannelDetectionAdminGroup, 0, len(ranked))
	for _, group := range ranked {
		result := ChannelDetectionAdminGroup{ChannelDetectionPublicGroup: group.result, GroupID: group.id, TriggerState: "idle"}
		switch {
		case active.Groups[group.id] != "" || (active.LegacyOwner != "" && latest.RunningByGroup[group.id].Owner == active.LegacyOwner):
			result.TriggerState = "running"
		case active.GloballyBlocked():
			result.TriggerState = "busy"
		case latest.NoSend[group.id]:
			result.TriggerState = "no_send"
		}
		proof := latest.Proofs[group.id]
		sample := latest.Samples[group.id]
		if latest.NoSend[group.id] && (sample == nil || sample.Pelican != "untested") {
			// Old no-send rounds left the sample and proof untouched.
			attemptedAt := latest.AttemptedAt[group.id]
			if run := latest.RunningByGroup[group.id]; run.Owner != "" {
				attemptedAt = time.Time{}
				if run.PreviousAttemptedAt != nil {
					attemptedAt = *run.PreviousAttemptedAt
				}
			}
			result.Latest = &ChannelDetectionLatestResult{CheckedAt: attemptedAt, Pelican: "untested"}
		} else if sample != nil {
			result.Latest = &ChannelDetectionLatestResult{CheckedAt: sample.CheckedAt, Pelican: sample.Pelican}
			if proof.CompletedAt != nil {
				result.Latest.CheckedAt = *proof.CompletedAt
			}
			result.SampleID = proof.SampleID
			if actions != nil && !latest.NoSend[group.id] && proof.Attributable() {
				result.Confirmation, err = actions.ConfirmationScope(ctx, ChannelDetectionSelection{GroupID: group.id, Model: group.result.Model}, proof, config.stored)
				if err != nil {
					return nil, err
				}
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *ChannelDetection) Trigger(ctx context.Context, groupID int64) (string, error) {
	if s.coordinator == nil {
		return "", ErrDetectionBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	selection, config, latest, _, err := s.actionSelection(ctx, groupID, false)
	if err != nil {
		return "", err
	}
	return s.admit(ctx, selection, latest.guard(config), "manual")
}

func (s *ChannelDetection) Confirm(ctx context.Context, groupID int64, sampleID, scopeVersion string, actorID int64) (ChannelDetectionConfirmation, error) {
	actions, ok := s.coordinator.(channelDetectionActions)
	if !ok {
		return ChannelDetectionConfirmation{}, errors.New("detection confirmation store unavailable")
	}
	selection, config, latest, group, err := s.actionSelection(ctx, groupID, true)
	if err != nil {
		return ChannelDetectionConfirmation{}, err
	}
	proof := latest.Proofs[groupID]
	if latest.NoSend[groupID] || latest.Samples[groupID] == nil || sampleID != proof.SampleID || !proof.Attributable() {
		return ChannelDetectionConfirmation{}, ErrDetectionChanged
	}
	return actions.Confirm(ctx, ChannelDetectionConfirmationRequest{
		Guard: latest.guard(config), Selection: selection, Proof: proof, ActorID: actorID, ScopeVersion: scopeVersion, GroupPlatform: group.Platform,
	})
}

func (s *ChannelDetection) actionSelection(ctx context.Context, groupID int64, withSamples bool) (ChannelDetectionSelection, channelDetectionConfigRecord, channelDetectionSnapshot, *Group, error) {
	feature, config, latest, err := s.state(ctx, withSamples)
	if err != nil {
		return ChannelDetectionSelection{}, config, latest, nil, err
	}
	if feature == "" || !config.Enabled {
		return ChannelDetectionSelection{}, config, latest, nil, errDetectionDisabled
	}
	index := slices.IndexFunc(config.Groups, func(selection ChannelDetectionSelection) bool { return selection.GroupID == groupID })
	if index < 0 {
		return ChannelDetectionSelection{}, config, latest, nil, ErrDetectionChanged
	}
	selection := config.Groups[index]
	group, err := s.probeGroup(ctx, selection)
	if err != nil {
		if errors.Is(err, errDetectionModel) {
			err = ErrDetectionChanged
		}
		return ChannelDetectionSelection{}, config, latest, nil, err
	}
	return selection, config, latest, group, nil
}
