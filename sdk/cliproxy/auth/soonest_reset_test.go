package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeAuthResettingAt(id string, reset time.Time) *Auth {
	auth := &Auth{ID: id, Provider: "claude"}
	if !reset.IsZero() {
		auth.Quota.Signals = map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(reset.Unix(), 10),
		}
	}
	return auth
}

func TestSoonestResetSelectorPick(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{
			name: "soonest known reset wins",
			auths: []*Auth{
				claudeAuthResettingAt("a", now.Add(96*time.Hour)),
				claudeAuthResettingAt("b", now.Add(20*time.Hour)),
				claudeAuthResettingAt("c", now.Add(50*time.Hour)),
			},
			want: "b",
		},
		{
			name: "unknown reset is tried first",
			auths: []*Auth{
				claudeAuthResettingAt("a", now.Add(time.Hour)),
				claudeAuthResettingAt("b", time.Time{}),
			},
			want: "b",
		},
		{
			name: "past reset counts as unknown",
			auths: []*Auth{
				claudeAuthResettingAt("a", now.Add(time.Hour)),
				claudeAuthResettingAt("b", now.Add(-time.Hour)),
			},
			want: "b",
		},
		{
			name: "ties keep ID order",
			auths: []*Auth{
				claudeAuthResettingAt("b", time.Time{}),
				claudeAuthResettingAt("a", time.Time{}),
			},
			want: "a",
		},
		{
			name: "cooling credential is skipped",
			auths: []*Auth{
				func() *Auth {
					auth := claudeAuthResettingAt("a", now.Add(time.Hour))
					auth.Quota.Exceeded = true
					auth.Quota.Reason = "credential_quota"
					auth.Quota.NextRecoverAt = now.Add(time.Hour)
					return auth
				}(),
				claudeAuthResettingAt("b", now.Add(90*time.Hour)),
			},
			want: "b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&SoonestResetSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, tt.auths)
			if err != nil {
				t.Fatalf("Pick() error = %v", err)
			}
			if got.ID != tt.want {
				t.Fatalf("Pick() = %q, want %q", got.ID, tt.want)
			}
		})
	}
}

func TestWeeklyQuotaResetCodex(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(3 * 24 * time.Hour)
	withResetAt := &Auth{Provider: "codex", Quota: QuotaState{Signals: map[string]string{
		"X-Codex-Secondary-Reset-At": strconv.FormatInt(resetAt.Unix(), 10),
	}}}
	if got := weeklyQuotaReset(withResetAt, now); !got.Equal(resetAt) {
		t.Fatalf("reset-at: got %v, want %v", got, resetAt)
	}

	observedAt := now.Add(-time.Hour)
	withResetAfter := &Auth{Provider: "codex", Quota: QuotaState{ObservedAt: observedAt, Signals: map[string]string{
		"X-Codex-Secondary-Reset-After-Seconds": "7200",
	}}}
	if got, want := weeklyQuotaReset(withResetAfter, now), observedAt.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("reset-after-seconds: got %v, want %v", got, want)
	}
}

func TestSoonestResetSelectorThroughManager(t *testing.T) {
	selectors := map[string]Selector{
		"plain":            &SoonestResetSelector{},
		"session affinity": NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetSelector{}}),
	}
	for name, selector := range selectors {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			model := "soonest-reset-test-model"
			later := claudeAuthResettingAt("soonest-reset-later", now.Add(96*time.Hour))
			sooner := claudeAuthResettingAt("soonest-reset-sooner", now.Add(20*time.Hour))

			m := NewManager(nil, selector, nil)
			m.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			reg := registry.GetGlobalRegistry()
			for _, auth := range []*Auth{later, sooner} {
				reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
				authID := auth.ID
				t.Cleanup(func() { reg.UnregisterClient(authID) })
				if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("register %s: %v", auth.ID, errRegister)
				}
			}

			picked, _, errPick := m.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext() error = %v", errPick)
			}
			if picked.ID != sooner.ID {
				t.Fatalf("pickNext() = %q, want %q", picked.ID, sooner.ID)
			}
		})
	}
}
