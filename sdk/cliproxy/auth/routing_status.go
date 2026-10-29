package auth

import (
	"sort"
	"time"
)

// RoutingStatusSnapshot describes the installed routing strategy and its accounts.
type RoutingStatusSnapshot struct {
	Strategy        string                            `json:"strategy"`
	GeneratedAt     time.Time                         `json:"generated_at"`
	SessionAffinity RoutingSessionAffinityStatus      `json:"session_affinity"`
	Providers       map[string][]RoutingStatusAccount `json:"providers"`
}

// RoutingSessionAffinityStatus summarizes session affinity without exposing bindings.
type RoutingSessionAffinityStatus struct {
	Enabled   bool   `json:"enabled"`
	TTL       string `json:"ttl,omitempty"`
	Subagents bool   `json:"subagents,omitempty"`
	RebindOn  string `json:"rebind_on,omitempty"`
}

// RoutingStatusAccount describes one account's selection state and quota observations.
type RoutingStatusAccount struct {
	AuthIndex      string     `json:"auth_index"`
	Rank           int        `json:"rank,omitempty"`
	Priority       int        `json:"priority"`
	State          string     `json:"state"`
	Reason         string     `json:"reason"`
	WeeklyReset    *time.Time `json:"weekly_reset,omitempty"`
	ExhaustedUntil *time.Time `json:"exhausted_until,omitempty"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
	ProbePending   bool       `json:"probe_pending"`
}

type routingStatusCandidate struct {
	id      string
	account RoutingStatusAccount
	rank    soonestResetRank
	blocked bool
}

type routingStatusSchedulerSnapshot struct {
	facts  map[string]soonestResetFacts
	probes map[string]soonestResetProbes
}

// RoutingStatus returns a side-effect-free snapshot of the current routing state.
func (m *Manager) RoutingStatus() RoutingStatusSnapshot {
	now := time.Now().UTC()
	snapshot := RoutingStatusSnapshot{
		GeneratedAt: now,
		Providers:   make(map[string][]RoutingStatusAccount),
	}
	if m == nil {
		return snapshot
	}

	m.mu.RLock()
	auths := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if cloned := candidate.Clone(); cloned != nil {
			auths = append(auths, cloned)
		}
	}
	selector := m.selector
	scheduler := m.scheduler
	m.mu.RUnlock()

	strategy := selector
	var affinity *SessionAffinitySelector
	if wrapped, ok := selector.(*SessionAffinitySelector); ok && wrapped != nil {
		affinity = wrapped
		strategy = wrapped.fallback
		snapshot.SessionAffinity = RoutingSessionAffinityStatus{
			Enabled:   true,
			Subagents: wrapped.subagentAffinity,
			RebindOn:  SessionAffinityRebindOnAny,
		}
		if wrapped.cache != nil {
			snapshot.SessionAffinity.TTL = wrapped.cache.ttl.String()
		}
		if wrapped.rebindOnQuotaOnly {
			snapshot.SessionAffinity.RebindOn = SessionAffinityRebindOnQuota
		}
	}
	snapshot.Strategy = routingStatusStrategyName(strategy)
	soonest, ok := strategy.(*SoonestResetSelector)
	if !ok || soonest == nil {
		return snapshot
	}

	schedulerState := snapshotRoutingStatusScheduler(scheduler)
	var legacyProbes soonestResetProbes
	if affinity != nil || scheduler == nil {
		soonest.mu.Lock()
		legacyProbes = cloneSoonestResetProbes(soonest.probes)
		soonest.mu.Unlock()
	}

	candidatesByProvider := make(map[string][]routingStatusCandidate)
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		provider := canonicalSchedulingProvider(executorKeyFromAuth(candidate))
		facts, carried := schedulerState.facts[candidate.ID]
		if !carried {
			facts = soonestResetFactsFor(candidate)
		}
		probes := schedulerState.probes[provider]
		if affinity != nil || scheduler == nil {
			probes = legacyProbes
		}
		probePending := probes.pending(candidate.ID, now)
		rank := facts.rank(now, probePending)
		account := RoutingStatusAccount{
			AuthIndex:      candidate.EnsureIndex(),
			Priority:       authPriority(candidate),
			WeeklyReset:    routingStatusTimestamp(facts.weeklyReset),
			ExhaustedUntil: routingStatusTimestamp(facts.exhaustedUntil),
			ObservedAt:     routingStatusTimestamp(candidate.Quota.ObservedAt),
			ProbePending:   probePending,
		}
		blocked, blockReason, _ := isAuthBlockedForModel(candidate, "", now)
		if blocked {
			account.State, account.Reason = routingStatusBlockReason(blockReason)
		} else {
			account.Reason = routingStatusRankReason(rank.class)
			if rank.exhausted {
				account.State = "exhausted"
			} else {
				account.State = "eligible"
			}
		}
		candidatesByProvider[provider] = append(candidatesByProvider[provider], routingStatusCandidate{
			id:      candidate.ID,
			account: account,
			rank:    rank,
			blocked: blocked,
		})
	}

	for provider, candidates := range candidatesByProvider {
		sort.Slice(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			if left.blocked != right.blocked {
				return !left.blocked
			}
			if left.account.Priority != right.account.Priority {
				return left.account.Priority > right.account.Priority
			}
			if !left.blocked {
				return soonestResetPrefers(left.rank, left.id, right.rank, right.id)
			}
			return left.id < right.id
		})
		accounts := make([]RoutingStatusAccount, 0, len(candidates))
		rank := 0
		for _, candidate := range candidates {
			if !candidate.blocked {
				rank++
				candidate.account.Rank = rank
			}
			accounts = append(accounts, candidate.account)
		}
		snapshot.Providers[provider] = accounts
	}
	return snapshot
}

func routingStatusStrategyName(selector Selector) string {
	switch selector.(type) {
	case *RoundRobinSelector:
		return "round-robin"
	case *WeightedRoundRobinSelector:
		return "weighted-round-robin"
	case *FillFirstSelector:
		return "fill-first"
	case *SoonestResetSelector:
		return "soonest-reset"
	case nil:
		return ""
	default:
		return "custom"
	}
}

func snapshotRoutingStatusScheduler(scheduler *authScheduler) routingStatusSchedulerSnapshot {
	state := routingStatusSchedulerSnapshot{
		facts:  make(map[string]soonestResetFacts),
		probes: make(map[string]soonestResetProbes),
	}
	if scheduler == nil {
		return state
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	for provider, providerState := range scheduler.providers {
		if providerState == nil {
			continue
		}
		provider = canonicalSchedulingProvider(provider)
		state.probes[provider] = cloneSoonestResetProbes(providerState.soonestResetProbes)
		for id, meta := range providerState.auths {
			if meta != nil {
				state.facts[id] = meta.soonestReset
			}
		}
	}
	return state
}

func cloneSoonestResetProbes(probes soonestResetProbes) soonestResetProbes {
	cloned := make(soonestResetProbes, len(probes))
	for id, pickedAt := range probes {
		cloned[id] = pickedAt
	}
	return cloned
}

func routingStatusTimestamp(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func routingStatusRankReason(class int) string {
	switch class {
	case soonestResetClassProbe:
		return "probe"
	case soonestResetClassKnown:
		return "known"
	case soonestResetClassSubscription:
		return "subscription"
	default:
		return "other"
	}
}

func routingStatusBlockReason(reason blockReason) (string, string) {
	switch reason {
	case blockReasonCooldown:
		return "cooldown", "cooldown"
	case blockReasonDisabled:
		return "disabled", "disabled"
	default:
		return "unavailable", "unavailable"
	}
}
