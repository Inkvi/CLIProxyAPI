package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// claudeWeeklyAuth builds a Claude credential with a weekly reset and usage.
// A zero reset leaves the quota signals unknown.
func claudeWeeklyAuth(id string, priority string, burnHours any, reset time.Time, used float64) *Auth {
	auth := &Auth{ID: id, Provider: "claude", Attributes: map[string]string{}, Metadata: map[string]any{}}
	if priority != "" {
		auth.Attributes["priority"] = priority
	}
	if burnHours != nil {
		auth.Metadata[PromoteBurnHoursMetadataKey] = burnHours
	}
	if !reset.IsZero() {
		auth.Quota.Signals = map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(reset.Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(used, 'f', -1, 64),
		}
	}
	return auth
}

func TestAuthPriorityPromoteBeforeReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name string
		auth *Auth
		want int
	}{
		{
			name: "no burn hours keeps configured priority",
			auth: claudeWeeklyAuth("a", "-10", nil, now.Add(time.Hour), 0),
			want: -10,
		},
		{
			name: "far from reset stays demoted",
			auth: claudeWeeklyAuth("a", "-10", float64(30), now.Add(96*time.Hour), 0.63),
			want: -10,
		},
		{
			name: "remaining quota cannot be burned before reset is promoted",
			// 74% left * 30h = 22.2h >= 7h until reset.
			auth: claudeWeeklyAuth("a", "-10", float64(30), now.Add(7*time.Hour), 0.26),
			want: 0,
		},
		{
			name: "little quota left waits until very close to reset",
			// 5% left * 30h = 1.5h < 7h until reset.
			auth: claudeWeeklyAuth("a", "-10", float64(30), now.Add(7*time.Hour), 0.95),
			want: -10,
		},
		{
			name: "burn hours as string",
			auth: claudeWeeklyAuth("a", "-10", "30", now.Add(7*time.Hour), 0.26),
			want: 0,
		},
		{
			name: "unknown quota is promoted so it gets learned",
			auth: claudeWeeklyAuth("a", "-10", float64(30), time.Time{}, 0),
			want: 0,
		},
		{
			name: "non-negative priority is never changed",
			auth: claudeWeeklyAuth("a", "5", float64(30), now.Add(time.Hour), 0),
			want: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := authPriority(tt.auth); got != tt.want {
				t.Fatalf("authPriority() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWeeklyQuotaRemainingCodex(t *testing.T) {
	t.Parallel()

	auth := &Auth{Provider: "codex", Quota: QuotaState{Signals: map[string]string{
		"X-Codex-Secondary-Used-Percent": "63",
	}}}
	got, ok := weeklyQuotaRemaining(auth)
	if !ok || got < 0.369 || got > 0.371 {
		t.Fatalf("weeklyQuotaRemaining() = %v, %v, want 0.37, true", got, ok)
	}
}

func TestSoonestResetPromotesTeamOnlyNearReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	individual := claudeWeeklyAuth("max", "", nil, now.Add(31*time.Hour), 0.88)
	team := claudeWeeklyAuth("team", "-10", float64(30), now.Add(96*time.Hour), 0.26)
	auths := []*Auth{individual, team}
	selector := &SoonestResetSelector{}

	available, errAvailable := getAvailableAuths(auths, "claude", "", now)
	if errAvailable != nil {
		t.Fatalf("getAvailableAuths() error = %v", errAvailable)
	}
	picked, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, available)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked.ID != individual.ID {
		t.Fatalf("far from reset: Pick() = %q, want %q", picked.ID, individual.ID)
	}

	team.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(7*time.Hour).Unix(), 10)
	available, errAvailable = getAvailableAuths(auths, "claude", "", now)
	if errAvailable != nil {
		t.Fatalf("getAvailableAuths() error = %v", errAvailable)
	}
	picked, errPick = selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, available)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked.ID != team.ID {
		t.Fatalf("near reset: Pick() = %q, want %q", picked.ID, team.ID)
	}
}

func TestSessionAffinityLeavesTeamWhenPromotionEnds(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	defer selector.Stop()

	now := time.Now()
	individual := claudeWeeklyAuth("max", "", nil, now.Add(31*time.Hour), 0.88)
	team := claudeWeeklyAuth("team", "-10", float64(30), now.Add(7*time.Hour), 0.26)
	auths := []*Auth{individual, team}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_promote-ends"}}`)}

	first, errFirst := selector.Pick(context.Background(), "claude", "claude-3", opts, auths)
	if errFirst != nil {
		t.Fatalf("first Pick() error = %v", errFirst)
	}
	if first.ID != team.ID {
		t.Fatalf("first Pick() = %q, want promoted %q", first.ID, team.ID)
	}

	// Team's window rolled over: next reset is a week out and usage is fresh.
	team.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(168*time.Hour).Unix(), 10)
	team.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0"
	second, errSecond := selector.Pick(context.Background(), "claude", "claude-3", opts, auths)
	if errSecond != nil {
		t.Fatalf("second Pick() error = %v", errSecond)
	}
	if second.ID != individual.ID {
		t.Fatalf("second Pick() = %q, want %q after promotion ended", second.ID, individual.ID)
	}
}

func TestSessionAffinityKeepsTeamWhenItIsTheOnlyTier(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	defer selector.Stop()

	now := time.Now()
	individual := claudeWeeklyAuth("max", "", nil, now.Add(31*time.Hour), 1)
	individual.Quota.Exceeded = true
	individual.Quota.Reason = "credential_quota"
	individual.Quota.NextRecoverAt = now.Add(31 * time.Hour)
	team := claudeWeeklyAuth("team", "-10", float64(30), now.Add(168*time.Hour), 0)
	auths := []*Auth{individual, team}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_only-tier"}}`)}

	for i := 0; i < 2; i++ {
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-3", opts, auths)
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		if picked.ID != team.ID {
			t.Fatalf("Pick() #%d = %q, want fallback %q", i, picked.ID, team.ID)
		}
	}
}
