package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoftQuotaRecoverAt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(72 * time.Hour)
	unix := func(ts time.Time) string { return strconv.FormatInt(ts.Unix(), 10) }

	tests := []struct {
		name      string
		provider  string
		signals   map[string]string
		threshold float64
		wantPause bool
		wantAt    time.Time
	}{
		{
			name:     "claude below threshold",
			provider: "claude",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.94",
				"Anthropic-Ratelimit-Unified-5h-Reset":       unix(reset5h),
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
				"Anthropic-Ratelimit-Unified-7d-Reset":       unix(reset7d),
			},
			threshold: 0.95,
		},
		{
			name:     "claude 5h window crossed",
			provider: "claude",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.95",
				"Anthropic-Ratelimit-Unified-5h-Reset":       unix(reset5h),
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
				"Anthropic-Ratelimit-Unified-7d-Reset":       unix(reset7d),
			},
			threshold: 0.95,
			wantPause: true,
			wantAt:    reset5h,
		},
		{
			name:     "claude both windows crossed waits for the later reset",
			provider: "claude",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.99",
				"Anthropic-Ratelimit-Unified-5h-Reset":       unix(reset5h),
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.97",
				"Anthropic-Ratelimit-Unified-7d-Reset":       unix(reset7d),
			},
			threshold: 0.95,
			wantPause: true,
			wantAt:    reset7d,
		},
		{
			name:     "claude overage in use",
			provider: "claude",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization":       "0.2",
				"Anthropic-Ratelimit-Unified-Overage-In-Use":       "true",
				"Anthropic-Ratelimit-Unified-Reset":                unix(reset5h),
				"Anthropic-Ratelimit-Unified-Overage-Status":       "allowed",
				"Anthropic-Ratelimit-Unified-Overage-Reset":        unix(reset7d),
				"Anthropic-Ratelimit-Unified-7d-Utilization":       "0.1",
				"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
			},
			threshold: 0.95,
			wantPause: true,
			wantAt:    reset5h,
		},
		{
			name:     "codex primary crossed with reset-at",
			provider: "codex",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "96",
				"X-Codex-Primary-Reset-At":       unix(reset7d),
				"X-Codex-Secondary-Used-Percent": "0",
			},
			threshold: 0.95,
			wantPause: true,
			wantAt:    reset7d,
		},
		{
			name:     "codex falls back to reset-after-seconds",
			provider: "codex",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "100",
				"X-Codex-Primary-Reset-After-Seconds": "3600",
			},
			threshold: 0.95,
			wantPause: true,
			wantAt:    now.Add(time.Hour),
		},
		{
			name:     "codex below threshold",
			provider: "codex",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "52",
				"X-Codex-Primary-Reset-At":     unix(reset7d),
			},
			threshold: 0.95,
		},
		{
			name:     "missing reset time pauses for the fallback duration",
			provider: "claude",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "1.0",
			},
			threshold: 0.95,
			wantPause: true,
			wantAt:    now.Add(softQuotaFallbackPause),
		},
		{
			name:     "unset threshold never pauses",
			provider: "claude",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "1.0",
			},
		},
		{
			name:     "unsupported provider never pauses",
			provider: "gemini",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "100",
			},
			threshold: 0.95,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAt, gotPause := softQuotaRecoverAt(tt.provider, tt.signals, tt.threshold, now)
			if gotPause != tt.wantPause {
				t.Fatalf("paused = %v, want %v", gotPause, tt.wantPause)
			}
			if tt.wantPause && !gotAt.Equal(tt.wantAt) {
				t.Fatalf("recoverAt = %s, want %s", gotAt, tt.wantAt)
			}
		})
	}
}

func TestAuthSoftQuotaThreshold(t *testing.T) {
	tests := []struct {
		value any
		want  float64
	}{
		{value: nil, want: 0},
		{value: 0.95, want: 0.95},
		{value: "0.9", want: 0.9},
		{value: 1.0, want: 1.0},
		{value: 0.0, want: 0},
		{value: 95.0, want: 0},
		{value: "abc", want: 0},
		{value: true, want: 0},
	}
	for _, tt := range tests {
		auth := &Auth{Metadata: map[string]any{}}
		if tt.value != nil {
			auth.Metadata[SoftQuotaThresholdMetadataKey] = tt.value
		}
		if got := authSoftQuotaThreshold(auth); got != tt.want {
			t.Fatalf("authSoftQuotaThreshold(%v) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestMarkResultSoftQuotaPausesCredentialAndSelectorSkipsIt(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	business, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "claude-business",
		Provider: "claude",
		Metadata: map[string]any{SoftQuotaThresholdMetadataKey: 0.95},
	})
	if errRegister != nil {
		t.Fatalf("Register(business) error = %v", errRegister)
	}
	personal, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "claude-personal",
		Provider: "claude",
	})
	if errRegister != nil {
		t.Fatalf("Register(personal) error = %v", errRegister)
	}

	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	markSuccess := func(authID, utilization string) {
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": []string{utilization},
			"Anthropic-Ratelimit-Unified-5h-Reset":       []string{strconv.FormatInt(reset.Unix(), 10)},
			"Anthropic-Ratelimit-Unified-Status":         []string{"allowed"},
		})
		manager.MarkResult(ctx, Result{AuthID: authID, Provider: "claude", Model: "claude-sonnet-5-5", Success: true})
	}

	// Without a threshold, a credential at 99% stays available.
	markSuccess(personal.ID, "0.99")
	if updated, _ := manager.GetByID(personal.ID); updated.Quota.Exceeded {
		t.Fatalf("credential without threshold was paused: %#v", updated.Quota)
	}

	markSuccess(business.ID, "0.96")
	updated, _ := manager.GetByID(business.ID)
	if !updated.Quota.Exceeded || updated.Quota.Reason != "credential_quota" || !updated.Quota.NextRecoverAt.Equal(reset) {
		t.Fatalf("business quota after crossing threshold = %#v", updated.Quota)
	}

	// A later in-flight success with low usage must not resume the credential.
	markSuccess(business.ID, "0.10")
	updated, _ = manager.GetByID(business.ID)
	if !updated.Quota.Exceeded || !updated.Quota.NextRecoverAt.Equal(reset) {
		t.Fatalf("later success cleared the soft quota pause: %#v", updated.Quota)
	}

	businessNow, _ := manager.GetByID(business.ID)
	personalNow, _ := manager.GetByID(personal.ID)
	selector := &RoundRobinSelector{}
	for i := 0; i < 4; i++ {
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{businessNow, personalNow})
		if errPick != nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		if picked.ID != personal.ID {
			t.Fatalf("Pick() = %s, want %s", picked.ID, personal.ID)
		}
	}
}
