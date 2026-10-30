package auth

import (
	"strconv"
	"strings"
	"time"
)

// SoftQuotaThresholdMetadataKey is the auth file field that pauses a credential
// once its observed plan usage reaches the given fraction (for example 0.95),
// whether or not the credential can bill paid overage. It overrides the global
// routing.soft-quota-threshold for that credential.
const SoftQuotaThresholdMetadataKey = "soft_quota_threshold"

// softQuotaFallbackPause is used when the upstream response carries no usable reset time.
const softQuotaFallbackPause = time.Hour

// authSoftQuotaThreshold returns the configured threshold in (0, 1], or 0 when unset or invalid.
func authSoftQuotaThreshold(auth *Auth) float64 {
	if auth == nil || auth.Metadata == nil {
		return 0
	}
	var threshold float64
	switch value := auth.Metadata[SoftQuotaThresholdMetadataKey].(type) {
	case float64:
		threshold = value
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if errParse != nil {
			return 0
		}
		threshold = parsed
	default:
		return 0
	}
	return validSoftQuotaThreshold(threshold)
}

func validSoftQuotaThreshold(threshold float64) float64 {
	if threshold <= 0 || threshold > 1 {
		return 0
	}
	return threshold
}

// effectiveSoftQuotaThreshold returns the threshold for a credential: its own
// auth file value if set, otherwise the global threshold when the latest
// response shows the credential can bill paid overage. 0 means never pause.
func effectiveSoftQuotaThreshold(auth *Auth, globalThreshold float64) float64 {
	if threshold := authSoftQuotaThreshold(auth); threshold > 0 {
		return threshold
	}
	globalThreshold = validSoftQuotaThreshold(globalThreshold)
	if globalThreshold == 0 || auth == nil || !canBillPaidOverage(auth.Provider, auth.Quota.Signals) {
		return 0
	}
	return globalThreshold
}

// canBillPaidOverage reports whether the quota signals show the credential keeps
// working past its plan limit by billing paid usage: Claude extra usage or
// Codex credits.
func canBillPaidOverage(provider string, signals map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		status := strings.ToLower(strings.TrimSpace(signals["Anthropic-Ratelimit-Unified-Overage-Status"]))
		return strings.HasPrefix(status, "allowed")
	case "codex":
		return signalTrue(signals["X-Codex-Credits-Has-Credits"]) || signalTrue(signals["X-Codex-Credits-Unlimited"])
	default:
		return false
	}
}

func signalTrue(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "true")
}

// softQuotaRecoverAt reports whether the observed quota signals crossed the
// threshold and, if so, when the credential may be used again.
func softQuotaRecoverAt(provider string, signals map[string]string, threshold float64, now time.Time) (time.Time, bool) {
	if threshold <= 0 || len(signals) == 0 {
		return time.Time{}, false
	}
	var recoverAt time.Time
	crossed := false
	cross := func(reset time.Time) {
		crossed = true
		if reset.After(recoverAt) {
			recoverAt = reset
		}
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		// Utilization is a fraction (0.0-1.0); resets are unix seconds.
		for _, window := range []string{"5h", "7d"} {
			prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
			if used, ok := parseSignalFloat(signals[prefix+"Utilization"]); ok && used >= threshold {
				cross(parseSignalUnix(signals[prefix+"Reset"]))
			}
		}
		// Overage in use means the plan limit is already exhausted.
		if signalTrue(signals["Anthropic-Ratelimit-Unified-Overage-In-Use"]) {
			cross(parseSignalUnix(signals["Anthropic-Ratelimit-Unified-Reset"]))
		}
	case "codex":
		// Used percent is 0-100; resets are unix seconds or seconds from now.
		for _, window := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + window + "-"
			used, ok := parseSignalFloat(signals[prefix+"Used-Percent"])
			if !ok || used/100 < threshold {
				continue
			}
			reset := parseSignalUnix(signals[prefix+"Reset-At"])
			if reset.IsZero() {
				if seconds, okSeconds := parseSignalFloat(signals[prefix+"Reset-After-Seconds"]); okSeconds && seconds > 0 {
					reset = now.Add(time.Duration(seconds) * time.Second)
				}
			}
			cross(reset)
		}
	default:
		return time.Time{}, false
	}
	if !crossed {
		return time.Time{}, false
	}
	if !recoverAt.After(now) {
		recoverAt = now.Add(softQuotaFallbackPause)
	}
	return recoverAt, true
}

// applySoftQuota pauses the credential until recovery when its latest quota
// snapshot crossed its effective threshold. It reuses the credential-wide
// quota cooldown, which the selector and scheduler already skip and which a
// later success does not clear.
func applySoftQuota(auth *Auth, globalThreshold float64, now time.Time) (time.Time, bool) {
	threshold := effectiveSoftQuotaThreshold(auth, globalThreshold)
	if threshold == 0 {
		return time.Time{}, false
	}
	recoverAt, crossed := softQuotaRecoverAt(auth.Provider, auth.Quota.Signals, threshold, now)
	if !crossed {
		return time.Time{}, false
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(recoverAt) {
		recoverAt = auth.Quota.NextRecoverAt
	}
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "credential_quota"
	auth.Quota.NextRecoverAt = recoverAt
	if recoverAt.After(auth.NextRetryAfter) {
		auth.NextRetryAfter = recoverAt
	}
	return recoverAt, true
}

func parseSignalFloat(raw string) (float64, bool) {
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if errParse != nil {
		return 0, false
	}
	return value, true
}

func parseSignalUnix(raw string) time.Time {
	seconds, errParse := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}
