package auth

import (
	"strconv"
	"strings"
	"time"
)

// PromoteBurnHoursMetadataKey is the auth file field that lets a credential
// with a negative priority join the default priority tier (0) when its weekly
// quota would otherwise expire unused. The value is roughly how many hours of
// heavy use it takes to burn a full week of that plan. The credential is
// promoted while
//
//	hours until weekly reset <= remaining weekly fraction * promote_burn_hours
//
// and also while its weekly reset or usage is unknown (after a restart or once
// the window rolled over), so one request learns them.
const PromoteBurnHoursMetadataKey = "promote_burn_hours"

// authPromoteBurnHours returns the configured burn hours, or 0 when unset or invalid.
func authPromoteBurnHours(auth *Auth) float64 {
	if auth == nil || auth.Metadata == nil {
		return 0
	}
	var hours float64
	switch value := auth.Metadata[PromoteBurnHoursMetadataKey].(type) {
	case float64:
		hours = value
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if errParse != nil {
			return 0
		}
		hours = parsed
	default:
		return 0
	}
	if hours <= 0 {
		return 0
	}
	return hours
}

// weeklyQuotaRemaining returns the unused fraction (0-1) of the credential's
// weekly quota from its latest observed quota signals.
func weeklyQuotaRemaining(auth *Auth) (float64, bool) {
	if auth == nil {
		return 0, false
	}
	signals := auth.Quota.Signals
	var used float64
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		value, ok := parseSignalFloat(signals["Anthropic-Ratelimit-Unified-7d-Utilization"])
		if !ok {
			return 0, false
		}
		used = value
	case "codex":
		value, ok := parseSignalFloat(signals["X-Codex-Secondary-Used-Percent"])
		if !ok {
			return 0, false
		}
		used = value / 100
	default:
		return 0, false
	}
	return min(max(1-used, 0), 1), true
}

// isPromotedBeforeReset reports whether a credential with promote_burn_hours
// should currently be treated as priority 0.
func isPromotedBeforeReset(auth *Auth, now time.Time) bool {
	burnHours := authPromoteBurnHours(auth)
	if burnHours == 0 {
		return false
	}
	reset := weeklyQuotaReset(auth, now)
	remaining, ok := weeklyQuotaRemaining(auth)
	if reset.IsZero() || !ok {
		return true
	}
	return reset.Sub(now).Hours() <= remaining*burnHours
}

// dropDemotedAuths removes credentials with promote_burn_hours that are not in
// the highest available priority tier, so a session bound to one of them moves
// to the top tier once its promotion ends or a higher tier recovers.
func dropDemotedAuths(available []*Auth) []*Auth {
	top := highestPriorityAuths(available)
	if len(top) == len(available) {
		return available
	}
	inTop := make(map[string]struct{}, len(top))
	for _, auth := range top {
		inTop[auth.ID] = struct{}{}
	}
	kept := make([]*Auth, 0, len(available))
	for _, auth := range available {
		if _, ok := inTop[auth.ID]; ok || authPromoteBurnHours(auth) == 0 {
			kept = append(kept, auth)
		}
	}
	return kept
}
