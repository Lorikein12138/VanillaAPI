package service

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

const (
	adaptiveProbeInterval = time.Minute
	adaptiveProbeCooldown = 30 * time.Minute
	adaptiveProbePoolTTL  = 24 * time.Hour
	adaptiveProbeRequests = 100
)

type adaptiveProbeMember struct {
	id       int64
	position int
}

type adaptiveProbeScope struct {
	groupID int64
	model   string
	route   adaptiveRoute
}

type adaptiveProbePool struct {
	lastSeen time.Time
	lastID   int64
	requests int
}

type adaptiveExploration struct {
	mu         sync.Mutex
	lastProbe  time.Time
	nextPrune  time.Time
	candidates map[int64]time.Time
	pools      map[adaptiveProbeScope]adaptiveProbePool
}

func (e *adaptiveExploration) pick(scope adaptiveProbeScope, members []adaptiveProbeMember, now time.Time, opportunity func() bool) int {
	if len(members) == 0 || !opportunity() {
		return 0
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if !now.Before(e.nextPrune) {
		for id, eligibleAt := range e.candidates {
			if !now.Before(eligibleAt) {
				delete(e.candidates, id)
			}
		}
		for key, pool := range e.pools {
			if now.Sub(pool.lastSeen) >= adaptiveProbePoolTTL {
				delete(e.pools, key)
			}
		}
		e.nextPrune = now.Add(adaptiveProbeInterval)
	}
	pool, tracked := e.pools[scope]
	if tracked {
		pool.lastSeen = now
		pool.requests = min(pool.requests+1, adaptiveProbeRequests)
		e.pools[scope] = pool
		if pool.requests < adaptiveProbeRequests {
			return 0
		}
	} else if len(e.pools) >= adaptiveCapacity {
		return 0
	}
	if now.Sub(e.lastProbe) < adaptiveProbeInterval || len(e.candidates) >= adaptiveCapacity {
		return 0
	}
	// Advance from the previous candidate ID and wrap once. Expiring a
	// candidate's cooldown does not reset its place in the rotation.
	slices.SortFunc(members, func(a, b adaptiveProbeMember) int { return cmp.Compare(a.id, b.id) })
	start := 0
	for start < len(members) && members[start].id <= pool.lastID {
		start++
	}
	for i := range members {
		member := members[(start+i)%len(members)]
		if now.Before(e.candidates[member.id]) {
			continue
		}
		if e.pools == nil {
			e.pools = make(map[adaptiveProbeScope]adaptiveProbePool)
			e.candidates = make(map[int64]time.Time)
		}
		e.lastProbe = now
		e.pools[scope] = adaptiveProbePool{lastSeen: now, lastID: member.id}
		e.candidates[member.id] = now.Add(adaptiveProbeCooldown)
		return member.position
	}
	return 0
}
