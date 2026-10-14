package torbox

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// captureLog returns a logger writing text records into the returned buffer.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestMe(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/user/me" {
			t.Errorf("path = %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"success":true,"error":null,"detail":"ok","data":{"id":7,"plan":1,"is_subscribed":true,"premium_expires_at":"2026-12-01T00:00:00Z","cooldown_until":"2024-01-01T00:00:00Z"}}`))
	})
	u, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.ID != 7 || u.Plan != PlanEssential || !u.IsSubscribed || u.PremiumExpiresAt != "2026-12-01T00:00:00Z" {
		t.Fatalf("unexpected user: %+v", u)
	}
}

func TestCheckAccountRejectedKey(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"BAD_TOKEN","detail":"Invalid token","data":null}`))
	})
	log, buf := captureLog()
	CheckAccount(context.Background(), c, 3, log)
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "rejected the API key") {
		t.Fatalf("want an error about the rejected key, got:\n%s", out)
	}
}

func TestCheckUser(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		user    User
		slots   int
		want    []string
		notWant []string
	}{
		{
			name:    "healthy essential",
			user:    User{Plan: PlanEssential, IsSubscribed: true, PremiumExpiresAt: "2027-01-01T00:00:00Z"},
			slots:   3,
			want:    []string{"TorBox account verified", "plan=Essential"},
			notWant: []string{"level=WARN"},
		},
		{
			name:  "expired",
			user:  User{Plan: PlanEssential, PremiumExpiresAt: "2026-10-01T00:00:00"},
			slots: 3,
			want:  []string{"subscription has expired"},
		},
		{
			name:  "expiring soon",
			user:  User{Plan: PlanPro, PremiumExpiresAt: "2026-10-16 12:00:00.123456"},
			slots: 10,
			want:  []string{"expires soon"},
		},
		{
			name:  "too many slots for plan",
			user:  User{Plan: PlanEssential, PremiumExpiresAt: "2027-01-01T00:00:00Z"},
			slots: 5,
			want:  []string{"above what the plan includes", "plan_slots=3"},
		},
		{
			name:    "spare slots",
			user:    User{Plan: PlanStandard, PremiumExpiresAt: "2027-01-01T00:00:00Z"},
			slots:   3,
			want:    []string{"below what the plan includes", "plan_slots=5"},
			notWant: []string{"level=WARN"},
		},
		{
			name:  "cooldown",
			user:  User{Plan: PlanEssential, PremiumExpiresAt: "2027-01-01T00:00:00Z", CooldownUntil: "2026-10-14T13:00:00Z"},
			slots: 3,
			want:  []string{"in a cooldown"},
		},
		{
			name:  "free plan",
			user:  User{Plan: PlanFree},
			slots: 3,
			want:  []string{"Free plan"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, buf := captureLog()
			checkUser(&tt.user, tt.slots, now, log)
			out := buf.String()
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("log missing %q:\n%s", w, out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(out, w) {
					t.Errorf("log unexpectedly contains %q:\n%s", w, out)
				}
			}
		})
	}
}
