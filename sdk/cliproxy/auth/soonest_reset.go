package auth

import (
	"context"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector picks the available credential whose weekly quota
// window resets soonest, so quota that would otherwise expire unused is spent
// first. Credentials without a known future reset are picked before the rest
// so their reset time gets learned. Ties keep the stable ID order.
//
// The scheduler treats it as a custom selector, so every pick goes through
// Pick with the full candidate list.
type SoonestResetSelector struct{}

// Pick selects the available auth with the soonest weekly quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	best := available[0]
	bestReset := weeklyQuotaReset(best, now)
	for _, candidate := range available[1:] {
		reset := weeklyQuotaReset(candidate, now)
		if resetsSooner(reset, bestReset) {
			best, bestReset = candidate, reset
		}
	}
	return best, nil
}

// resetsSooner orders reset times, with unknown (zero) first.
func resetsSooner(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() && !b.IsZero()
	}
	return a.Before(b)
}

// weeklyQuotaReset returns when the credential's weekly quota window resets,
// from its latest observed quota signals. It returns zero when unknown or
// already passed.
func weeklyQuotaReset(auth *Auth, now time.Time) time.Time {
	if auth == nil {
		return time.Time{}
	}
	signals := auth.Quota.Signals
	var reset time.Time
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		reset = parseSignalUnix(signals["Anthropic-Ratelimit-Unified-7d-Reset"])
	case "codex":
		reset = parseSignalUnix(signals["X-Codex-Secondary-Reset-At"])
		if reset.IsZero() {
			if seconds, ok := parseSignalFloat(signals["X-Codex-Secondary-Reset-After-Seconds"]); ok && seconds > 0 {
				observedAt := auth.Quota.ObservedAt
				if observedAt.IsZero() {
					observedAt = now
				}
				reset = observedAt.Add(time.Duration(seconds) * time.Second)
			}
		}
	}
	if !reset.After(now) {
		return time.Time{}
	}
	return reset
}
