package auth

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var routingStatusTestNow = time.Date(2096, time.October, 25, 12, 0, 0, 0, time.UTC)

func newRoutingStatusManager(selector Selector, auths ...*Auth) *Manager {
	manager := NewManager(nil, selector, nil)
	manager.auths = make(map[string]*Auth, len(auths))
	for _, candidate := range auths {
		if candidate != nil {
			manager.auths[candidate.ID] = candidate.Clone()
		}
	}
	manager.scheduler.rebuild(auths)
	return manager
}

func routingStatusIndex(auth *Auth) string {
	return auth.Clone().EnsureIndex()
}

func routingStatusAccountForAuth(t *testing.T, snapshot RoutingStatusSnapshot, provider string, auth *Auth) RoutingStatusAccount {
	t.Helper()
	index := routingStatusIndex(auth)
	for _, account := range snapshot.Providers[provider] {
		if account.AuthIndex == index {
			return account
		}
	}
	t.Fatalf("provider %q has no account for auth %q: %+v", provider, auth.ID, snapshot.Providers[provider])
	return RoutingStatusAccount{}
}

func TestSoonestResetRoutingStatusMatchesSelection(t *testing.T) {
	now := routingStatusTestNow
	observed := now.Add(-time.Minute)
	low := claudeSubscription("routing-low", observed, claudeWeeklySignals(now, now.Add(24*time.Hour)))
	low.Attributes = map[string]string{"priority": "1"}
	highLater := claudeSubscription("routing-high-later", observed, claudeWeeklySignals(now, now.Add(96*time.Hour)))
	highLater.Attributes = map[string]string{"priority": "10"}
	tieB := claudeSubscription("routing-tie-b", observed, claudeWeeklySignals(now, now.Add(48*time.Hour)))
	tieB.Attributes = map[string]string{"priority": "10"}
	tieA := claudeSubscription("routing-tie-a", observed, claudeWeeklySignals(now, now.Add(48*time.Hour)))
	tieA.Attributes = map[string]string{"priority": "10"}
	auths := []*Auth{low, tieB, highLater, tieA}
	manager := newRoutingStatusManager(&SoonestResetSelector{}, auths...)

	remaining := append([]*Auth(nil), auths...)
	selector := &SoonestResetSelector{}
	wantOrder := make([]string, 0, len(auths))
	for len(remaining) > 0 {
		picked, errPick := selector.Pick(withoutSoonestResetProbe(context.Background()), "claude", "", cliproxyexecutor.Options{}, remaining)
		if errPick != nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		wantOrder = append(wantOrder, routingStatusIndex(picked))
		next := remaining[:0]
		for _, candidate := range remaining {
			if candidate.ID != picked.ID {
				next = append(next, candidate)
			}
		}
		remaining = next
	}

	snapshot := manager.RoutingStatus()
	if snapshot.Strategy != "soonest-reset" {
		t.Fatalf("strategy = %q, want soonest-reset", snapshot.Strategy)
	}
	if snapshot.GeneratedAt.IsZero() || snapshot.GeneratedAt.Location() != time.UTC {
		t.Fatalf("generated_at = %v, want a real UTC timestamp", snapshot.GeneratedAt)
	}
	accounts := snapshot.Providers["claude"]
	if len(accounts) != len(wantOrder) {
		t.Fatalf("account count = %d, want %d", len(accounts), len(wantOrder))
	}
	for index, account := range accounts {
		if account.AuthIndex != wantOrder[index] {
			t.Errorf("account %d auth_index = %q, want %q", index, account.AuthIndex, wantOrder[index])
		}
		if account.Rank != index+1 {
			t.Errorf("account %d rank = %d, want %d", index, account.Rank, index+1)
		}
	}
	if accounts[0].Priority != 10 || accounts[3].Priority != 1 {
		t.Fatalf("priority order = %d then %d, want 10 before 1", accounts[0].Priority, accounts[3].Priority)
	}
	if accounts[0].AuthIndex != routingStatusIndex(tieA) || accounts[1].AuthIndex != routingStatusIndex(tieB) {
		t.Fatalf("stable-ID tie order = %q, %q", accounts[0].AuthIndex, accounts[1].AuthIndex)
	}

	nonSoonest := newRoutingStatusManager(&RoundRobinSelector{}, low).RoutingStatus()
	if nonSoonest.Strategy != "round-robin" {
		t.Fatalf("round-robin strategy = %q", nonSoonest.Strategy)
	}
	if nonSoonest.Providers == nil || len(nonSoonest.Providers) != 0 {
		t.Fatalf("non-soonest providers = %#v, want non-nil empty map", nonSoonest.Providers)
	}
}

func TestSoonestResetRoutingStatusExhaustionAndUnknown(t *testing.T) {
	now := routingStatusTestNow
	observed := now.Add(-time.Minute)
	exhaustedSignals := claudeWeeklySignals(now, now.Add(24*time.Hour))
	exhaustedSignals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
	exhaustedSignals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "1"
	exhausted := claudeSubscription("routing-exhausted", observed, exhaustedSignals)
	usable := claudeSubscription("routing-usable", observed, claudeWeeklySignals(now, now.Add(96*time.Hour)))
	unknown := claudeSubscription("routing-unknown", time.Time{}, nil)
	manager := newRoutingStatusManager(&SoonestResetSelector{}, exhausted, usable, unknown)

	snapshot := manager.RoutingStatus()
	exhaustedAccount := routingStatusAccountForAuth(t, snapshot, "claude", exhausted)
	usableAccount := routingStatusAccountForAuth(t, snapshot, "claude", usable)
	unknownAccount := routingStatusAccountForAuth(t, snapshot, "claude", unknown)
	if usableAccount.Rank == 0 || exhaustedAccount.Rank <= usableAccount.Rank {
		t.Fatalf("ranks usable=%d exhausted=%d, want usable first", usableAccount.Rank, exhaustedAccount.Rank)
	}
	if exhaustedAccount.State != "exhausted" || usableAccount.State != "eligible" {
		t.Fatalf("states exhausted=%q usable=%q", exhaustedAccount.State, usableAccount.State)
	}
	if exhaustedAccount.ExhaustedUntil == nil || !exhaustedAccount.ExhaustedUntil.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("exhausted_until = %v, want %v", exhaustedAccount.ExhaustedUntil, now.Add(24*time.Hour))
	}
	if unknownAccount.WeeklyReset != nil || unknownAccount.ExhaustedUntil != nil || unknownAccount.ObservedAt != nil {
		t.Fatalf("unknown timestamps = weekly %v, exhausted %v, observed %v; want all omitted", unknownAccount.WeeklyReset, unknownAccount.ExhaustedUntil, unknownAccount.ObservedAt)
	}

	t.Run("carried scheduler facts", func(t *testing.T) {
		windowless := claudeSubscription("routing-carried", time.Time{}, nil)
		carriedManager := newRoutingStatusManager(&SoonestResetSelector{}, windowless)
		carriedReset := now.Add(72 * time.Hour)
		carriedManager.scheduler.mu.Lock()
		meta := carriedManager.scheduler.providers["claude"].auths[windowless.ID]
		meta.soonestReset = soonestResetFacts{subscription: true, windowed: true, weeklyReset: carriedReset}
		carriedManager.scheduler.mu.Unlock()

		account := routingStatusAccountForAuth(t, carriedManager.RoutingStatus(), "claude", windowless)
		if account.Reason != "known" || account.WeeklyReset == nil || !account.WeeklyReset.Equal(carriedReset) {
			t.Fatalf("carried account = %+v, want known reset %v", account, carriedReset)
		}
	})
}

func cloneRoutingStatusProbes(probes soonestResetProbes) soonestResetProbes {
	cloned := make(soonestResetProbes, len(probes))
	for id, pickedAt := range probes {
		cloned[id] = pickedAt
	}
	return cloned
}

func TestSoonestResetRoutingStatusProbePendingPaths(t *testing.T) {
	unknown := claudeSubscription("routing-probe", time.Time{}, nil)
	pickedAt := time.Now().Add(-time.Minute)

	t.Run("scheduler ledger", func(t *testing.T) {
		selector := &SoonestResetSelector{}
		manager := newRoutingStatusManager(selector, unknown)
		manager.scheduler.mu.Lock()
		manager.scheduler.providers["claude"].soonestResetProbes = soonestResetProbes{unknown.ID: pickedAt}
		before := cloneRoutingStatusProbes(manager.scheduler.providers["claude"].soonestResetProbes)
		manager.scheduler.mu.Unlock()

		for range 2 {
			account := routingStatusAccountForAuth(t, manager.RoutingStatus(), "claude", unknown)
			if !account.ProbePending || account.Reason != "subscription" {
				t.Fatalf("account = %+v, want pending subscription", account)
			}
		}
		manager.scheduler.mu.Lock()
		after := cloneRoutingStatusProbes(manager.scheduler.providers["claude"].soonestResetProbes)
		manager.scheduler.mu.Unlock()
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("scheduler probes mutated: before %v, after %v", before, after)
		}
		selector.mu.Lock()
		legacyAfter := cloneRoutingStatusProbes(selector.probes)
		selector.mu.Unlock()
		if len(legacyAfter) != 0 {
			t.Fatalf("legacy probes mutated: %v", legacyAfter)
		}
	})

	t.Run("legacy affinity ledger", func(t *testing.T) {
		affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetSelector{}, TTL: time.Hour})
		t.Cleanup(affinity.Stop)
		fallback := affinity.fallback.(*SoonestResetSelector)
		manager := newRoutingStatusManager(affinity, unknown)
		fallback.mu.Lock()
		fallback.probes = soonestResetProbes{unknown.ID: pickedAt}
		before := cloneRoutingStatusProbes(fallback.probes)
		fallback.mu.Unlock()

		for range 2 {
			account := routingStatusAccountForAuth(t, manager.RoutingStatus(), "claude", unknown)
			if !account.ProbePending || account.Reason != "subscription" {
				t.Fatalf("account = %+v, want pending subscription", account)
			}
		}
		fallback.mu.Lock()
		after := cloneRoutingStatusProbes(fallback.probes)
		fallback.mu.Unlock()
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("legacy probes mutated: before %v, after %v", before, after)
		}
		manager.scheduler.mu.Lock()
		schedulerAfter := cloneRoutingStatusProbes(manager.scheduler.providers["claude"].soonestResetProbes)
		manager.scheduler.mu.Unlock()
		if len(schedulerAfter) != 0 {
			t.Fatalf("scheduler probes mutated: %v", schedulerAfter)
		}
	})
}

func TestRoutingStatusSelectionStates(t *testing.T) {
	now := routingStatusTestNow
	observed := now.Add(-time.Minute)
	eligible := claudeSubscription("state-eligible", observed, claudeWeeklySignals(now, now.Add(48*time.Hour)))
	exhaustedSignals := claudeWeeklySignals(now, now.Add(24*time.Hour))
	exhaustedSignals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "1"
	exhausted := claudeSubscription("state-exhausted", observed, exhaustedSignals)
	cooldown := claudeSubscription("state-cooldown", observed, claudeWeeklySignals(now, now.Add(48*time.Hour)))
	cooldown.Quota.Exceeded = true
	cooldown.Quota.Reason = "credential_quota"
	cooldown.Quota.NextRecoverAt = now.Add(time.Hour)
	unavailable := claudeSubscription("state-unavailable", observed, claudeWeeklySignals(now, now.Add(48*time.Hour)))
	unavailable.Unavailable = true
	disabled := claudeSubscription("state-disabled", observed, claudeWeeklySignals(now, now.Add(48*time.Hour)))
	disabled.Disabled = true
	manager := newRoutingStatusManager(&SoonestResetSelector{}, eligible, exhausted, cooldown, unavailable, disabled)
	snapshot := manager.RoutingStatus()

	tests := []struct {
		auth       *Auth
		state      string
		reason     string
		wantRanked bool
	}{
		{auth: eligible, state: "eligible", reason: "known", wantRanked: true},
		{auth: exhausted, state: "exhausted", reason: "known", wantRanked: true},
		{auth: cooldown, state: "cooldown", reason: "cooldown"},
		{auth: unavailable, state: "unavailable", reason: "unavailable"},
		{auth: disabled, state: "disabled", reason: "disabled"},
	}
	for _, test := range tests {
		account := routingStatusAccountForAuth(t, snapshot, "claude", test.auth)
		if account.State != test.state || account.Reason != test.reason {
			t.Errorf("auth %q state/reason = %q/%q, want %q/%q", test.auth.ID, account.State, account.Reason, test.state, test.reason)
		}
		if (account.Rank > 0) != test.wantRanked {
			t.Errorf("auth %q rank = %d, want ranked %v", test.auth.ID, account.Rank, test.wantRanked)
		}
	}
}

func TestRoutingStatusSessionAffinitySummary(t *testing.T) {
	subagents := false
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:         &SoonestResetSelector{},
		TTL:              17 * time.Minute,
		SubagentAffinity: &subagents,
		RebindOn:         SessionAffinityRebindOnQuota,
	})
	t.Cleanup(affinity.Stop)
	manager := newRoutingStatusManager(affinity)
	snapshot := manager.RoutingStatus()
	if snapshot.Strategy != "soonest-reset" {
		t.Fatalf("strategy = %q, want soonest-reset", snapshot.Strategy)
	}
	want := RoutingSessionAffinityStatus{Enabled: true, TTL: "17m0s", Subagents: false, RebindOn: "quota"}
	if !reflect.DeepEqual(snapshot.SessionAffinity, want) {
		t.Fatalf("session affinity = %+v, want %+v", snapshot.SessionAffinity, want)
	}
}

func TestRoutingStatusSecretExclusion(t *testing.T) {
	secrets := []string{
		"fake-api-key-secret",
		"fake-access-token-secret",
		"fake-refresh-token-secret",
		"https://user:fake-proxy-secret@example.com:8443",
		"fake-metadata-secret",
		"fake-attribute-secret",
		"fake-raw-signal-secret",
		"fake-auth-filename-secret.json",
	}
	auth := claudeSubscription("fake-credential-id-secret", routingStatusTestNow, map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": "fake-raw-signal-secret",
	})
	auth.FileName = "fake-auth-filename-secret.json"
	auth.ProxyURL = secrets[3]
	auth.Attributes = map[string]string{
		AttributeAPIKey: "fake-api-key-secret",
		"custom":        "fake-attribute-secret",
	}
	auth.Metadata = map[string]any{
		"access_token":  "fake-access-token-secret",
		"refresh_token": "fake-refresh-token-secret",
		"nested":        map[string]any{"value": "fake-metadata-secret"},
	}
	manager := newRoutingStatusManager(&SoonestResetSelector{}, auth)
	snapshot := manager.RoutingStatus()
	encoded, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	for _, secret := range append(secrets, auth.ID) {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("snapshot JSON contains secret %q: %s", secret, encoded)
		}
	}
	account := routingStatusAccountForAuth(t, snapshot, "claude", auth)
	if account.AuthIndex == "" || account.AuthIndex != routingStatusIndex(auth) {
		t.Fatalf("auth_index = %q, want stable EnsureIndex %q", account.AuthIndex, routingStatusIndex(auth))
	}
	if original := manager.auths[auth.ID]; original.Index != "" || original.indexAssigned {
		t.Fatalf("RoutingStatus mutated manager auth index: Index=%q assigned=%v", original.Index, original.indexAssigned)
	}
	var generic map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &generic); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal() error = %v", errUnmarshal)
	}
	providers := generic["providers"].(map[string]any)
	accounts := providers["claude"].([]any)
	accountJSON := accounts[0].(map[string]any)
	for _, field := range []string{"id", "filename", "metadata", "attributes", "signals", "proxy_url"} {
		if _, found := accountJSON[field]; found {
			t.Errorf("snapshot JSON unexpectedly exposes field %q", field)
		}
	}
}

func TestSoonestResetRoutingStatusAliasProbeSharedWithScheduler(t *testing.T) {
	const routeModel = "routing-status-alias"
	const targetModel = "routing-status-target"
	authA := claudeSubscription("routing-status-alias-a", time.Time{}, nil)
	authB := claudeSubscription("routing-status-alias-b", time.Time{}, nil)
	registerSoonestResetModels(t, "claude", []string{authA.ID, authB.ID}, targetModel)
	selector := &SoonestResetSelector{}
	manager := newRoutingStatusManager(selector, authA, authB)
	manager.executors["claude"] = schedulerTestExecutor{provider: "claude"}
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: targetModel, Alias: routeModel, Fork: true}},
	})

	first, _, errPick := manager.pickNext(context.Background(), "claude", routeModel, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("alias pickNext() error = %v", errPick)
	}
	if first.ID != authA.ID {
		t.Fatalf("alias pick = %q, want first probe %q", first.ID, authA.ID)
	}
	account := routingStatusAccountForAuth(t, manager.RoutingStatus(), "claude", authA)
	if !account.ProbePending {
		t.Errorf("alias probe is absent from RoutingStatus: %+v", account)
	}

	second, _, errPick := manager.pickNext(context.Background(), "claude", targetModel, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("non-alias pickNext() error = %v", errPick)
	}
	if second.ID != authB.ID {
		t.Errorf("non-alias pick = %q, want %q while %q has a probe pending", second.ID, authB.ID, authA.ID)
	}
}

func TestSoonestResetRoutingStatusPluginFallbackProbeSharedWithScheduler(t *testing.T) {
	authA := claudeSubscription("routing-status-plugin-a", time.Time{}, nil)
	authB := claudeSubscription("routing-status-plugin-b", time.Time{}, nil)
	selector := &SoonestResetSelector{}
	manager := newRoutingStatusManager(selector, authA, authB)
	manager.executors["claude"] = schedulerTestExecutor{provider: "claude"}
	manager.SetPluginScheduler(&fakePluginScheduler{handled: false})

	first, _, errPick := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("plugin fallback pickNext() error = %v", errPick)
	}
	if first.ID != authA.ID {
		t.Fatalf("plugin fallback pick = %q, want first probe %q", first.ID, authA.ID)
	}
	account := routingStatusAccountForAuth(t, manager.RoutingStatus(), "claude", authA)
	if !account.ProbePending {
		t.Errorf("plugin fallback probe is absent from RoutingStatus: %+v", account)
	}

	manager.SetPluginScheduler(nil)
	second, _, errPick := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler pickNext() error = %v", errPick)
	}
	if second.ID != authB.ID {
		t.Errorf("scheduler pick = %q, want %q while %q has a probe pending", second.ID, authB.ID, authA.ID)
	}
}
