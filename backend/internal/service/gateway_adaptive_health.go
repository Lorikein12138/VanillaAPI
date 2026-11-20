package service

import (
	"slices"
	"strings"
	"time"
)

type adaptiveHealthFault uint8

const (
	adaptiveHealthy adaptiveHealthFault = iota
	adaptiveLatencyDegraded
	adaptiveUpstreamDegraded
)

const adaptiveHealthRetention = 7 * 24 * time.Hour

type adaptiveHealth struct {
	Last        time.Time
	Fault       adaptiveHealthFault
	FaultAt     time.Time
	RetryAt     time.Time
	Backoff     uint8
	Passed      uint8
	Verify      bool
	Baseline    time.Duration
	BaselineAt  time.Time
	RecoveredAt time.Time
}

func adaptiveTextStream(route adaptiveRoute) bool {
	if route.nonText {
		return false
	}
	if route.transport == "ws" {
		return strings.HasSuffix(route.operation, "/responses")
	}
	if route.transport != "sse" {
		return false
	}
	for _, suffix := range []string{"/messages", "/chat/completions", "/responses", ":streamGenerateContent"} {
		if strings.HasSuffix(route.operation, suffix) {
			return true
		}
	}
	return false
}

func adaptiveTTFTPoints(latency time.Duration) int {
	switch {
	case latency <= 10*time.Second:
		return 100
	case latency < 20*time.Second:
		return 80
	case latency <= 30*time.Second:
		return 40
	default:
		return 0
	}
}

func adaptiveLatencyRegression(latency, baseline time.Duration) bool {
	return baseline > 0 && latency-baseline >= 3*time.Second && 2*latency >= 3*baseline
}

func adaptiveMedian(values []time.Duration) time.Duration {
	slices.Sort(values)
	middle := len(values) / 2
	if len(values)%2 != 0 {
		return values[middle]
	}
	return values[middle-1] + (values[middle]-values[middle-1])/2
}

func (s *adaptiveSeries) recentSuccesses(now time.Time) []adaptiveSample {
	successes := make([]adaptiveSample, 0, 10)
	for offset := 1; offset <= len(s.successes); offset++ {
		sample := s.successes[(s.nextSuccess+len(s.successes)-offset)%len(s.successes)]
		if sample.success && !sample.at.After(now) && now.Sub(sample.at) < adaptiveWindow {
			successes = append(successes, sample)
		}
	}
	return successes
}

func (s *adaptiveSeries) failureTriggered(now time.Time) bool {
	consecutive, rapid, considered := 0, 0, 0
	for offset := 1; offset <= adaptiveSamples && considered < 10; offset++ {
		sample := s.samples[(s.next+adaptiveSamples-offset)%adaptiveSamples]
		if sample.at.After(now) {
			continue
		}
		if sample.at.IsZero() || now.Sub(sample.at) >= adaptiveWindow || !sample.at.After(s.health.RecoveredAt) {
			break
		}
		considered++
		if !sample.success {
			if considered == consecutive+1 {
				consecutive++
			}
			if now.Sub(sample.at) <= 5*time.Minute {
				rapid++
			}
		}
	}
	return consecutive >= 3 || rapid >= 3
}

func (s *adaptiveSeries) latencyHealth(now time.Time) (severe, probe, regressed bool, median time.Duration, count int) {
	successes := s.recentSuccesses(now)
	values := make([]time.Duration, 0, len(successes))
	slow, relative, moderate := 0, 0, 0
	for _, sample := range successes {
		values = append(values, sample.latency)
		if sample.latency >= 20*time.Second {
			slow++
		}
		if sample.at.After(s.health.BaselineAt) && adaptiveLatencyRegression(sample.latency, s.health.Baseline) {
			relative++
		}
		if sample.latency > 10*time.Second && sample.latency < 20*time.Second {
			moderate++
		}
	}
	if len(values) > 0 {
		median = adaptiveMedian(values)
	}
	severe = slow >= 5 || (len(successes) >= 2 && successes[0].latency > 30*time.Second && successes[1].latency > 30*time.Second)
	regressed = relative >= 5
	return severe, severe || regressed || moderate >= 5, regressed, median, len(successes)
}

func (s *adaptiveSeries) updateHealth(key adaptiveSampleKey, sample adaptiveSample, probe bool) {
	h := &s.health
	if sample.at.Before(h.Last) {
		return
	}
	if !h.Last.IsZero() && sample.at.Sub(h.Last) >= adaptiveHealthRetention {
		*h = adaptiveHealth{}
	}
	stale := !h.Last.IsZero() && sample.at.Sub(h.Last) >= adaptiveWindow
	if stale {
		h.Baseline, h.BaselineAt = 0, time.Time{}
		h.Passed = 0
		h.Verify = true
		h.Fault, h.FaultAt = adaptiveHealthy, time.Time{}
	}
	h.Last = sample.at
	text := adaptiveTextStream(key.route)
	if probe && (h.Fault != adaptiveHealthy || h.Verify) {
		if sample.success && (!text || sample.latency < 20*time.Second && !adaptiveLatencyRegression(sample.latency, h.Baseline)) {
			h.Passed++
			h.RetryAt = sample.at
			if h.Passed >= 3 {
				*h = adaptiveHealth{Last: sample.at, RecoveredAt: sample.at}
			}
			return
		}
		fault := h.Fault
		if !sample.success {
			fault = adaptiveUpstreamDegraded
		} else if fault == adaptiveHealthy {
			fault = adaptiveLatencyDegraded
		}
		h.degrade(fault, sample.at)
		return
	}
	if s.failureTriggered(sample.at) && h.Fault != adaptiveUpstreamDegraded {
		h.degrade(adaptiveUpstreamDegraded, sample.at)
		return
	}
	if probe && h.Fault == adaptiveHealthy && !stale {
		h.RetryAt = sample.at.Add(30 * time.Minute)
	}
	if h.Fault != adaptiveHealthy || !text {
		return
	}
	severe, _, _, median, count := s.latencyHealth(sample.at)
	if severe && h.RecoveredAt.IsZero() {
		h.degrade(adaptiveLatencyDegraded, sample.at)
	} else if severe {
		successes := s.recentSuccesses(sample.at)
		slow := 0
		for _, recent := range successes {
			if recent.at.After(h.RecoveredAt) && recent.latency >= 20*time.Second {
				slow++
			}
		}
		if slow >= 5 || (len(successes) >= 2 && successes[1].at.After(h.RecoveredAt) &&
			successes[0].latency > 30*time.Second && successes[1].latency > 30*time.Second) {
			h.degrade(adaptiveLatencyDegraded, sample.at)
		}
	} else if h.Baseline == 0 && count == 10 && median < 20*time.Second && sample.success {
		successes := s.recentSuccesses(sample.at)
		stable := true
		for _, recent := range successes {
			if recent.latency >= 20*time.Second || adaptiveLatencyRegression(recent.latency, median) {
				stable = false
				break
			}
		}
		if stable {
			h.Baseline, h.BaselineAt = median, sample.at
		}
	}
}

func (h *adaptiveHealth) degrade(fault adaptiveHealthFault, now time.Time) {
	delays := [...]time.Duration{5, 10, 20, 30}
	h.RetryAt = now.Add(delays[h.Backoff] * time.Minute)
	h.Backoff = min(h.Backoff+1, uint8(len(delays)-1))
	h.Fault, h.FaultAt, h.Passed = fault, now, 0
}

func (s adaptiveScore) canRetest(now time.Time) bool {
	if s.health.Last.IsZero() || now.Before(s.health.RetryAt) {
		return false
	}
	if s.health.Fault != adaptiveHealthy || s.health.Verify || now.Sub(s.health.Last) >= adaptiveWindow {
		return true
	}
	return s.probe
}

func adaptiveHealthPrefers(current, candidate *Account, a, b adaptiveScore, sameModel bool) bool {
	if b.health.Fault != adaptiveHealthy || b.health.Verify {
		return false
	}
	switch a.health.Fault {
	case adaptiveUpstreamDegraded:
		return true
	case adaptiveLatencyDegraded:
		if candidate.Priority <= current.Priority && candidate.BillingRateMultiplier() <= current.BillingRateMultiplier() {
			return true
		}
		return adaptiveProvenFaster(a, b, sameModel)
	default:
		return a.regressed && adaptiveProvenFaster(a, b, sameModel)
	}
}

func adaptiveComparisonProbe(a, b adaptiveScore, sameModel bool) bool {
	return sameModel && (a.health.Fault == adaptiveLatencyDegraded || a.regressed) &&
		b.health.Fault == adaptiveHealthy && !b.health.Verify && b.comparable < 3
}

func adaptiveProvenFaster(a, b adaptiveScore, sameModel bool) bool {
	return sameModel && a.comparable >= 3 && b.comparable >= 3 &&
		b.median < 20*time.Second && a.median-b.median >= 3*time.Second
}

func adaptiveSamePriorityPrefers(current, candidate *Account, a, b adaptiveScore, sameModel bool) bool {
	if current.Priority != candidate.Priority || !a.known || !b.known ||
		a.health.Fault != adaptiveHealthy || b.health.Fault != adaptiveHealthy || a.health.Verify || b.health.Verify {
		return false
	}
	if b.reliability != a.reliability {
		return b.reliability > a.reliability
	}
	return b.latency < a.latency && (candidate.BillingRateMultiplier() <= current.BillingRateMultiplier() ||
		adaptiveProvenFaster(a, b, sameModel))
}
