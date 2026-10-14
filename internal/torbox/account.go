package torbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// User is the slice of GET /v1/api/user/me the startup account check reads.
// Field names are taken from TorBox's official SDK (@torbox/torbox-api), whose
// documentation lists plans as 0 = Free, 1 = Essential, 2 = Pro, 3 = Standard.
type User struct {
	ID               int    `json:"id"`
	Plan             int    `json:"plan"`
	IsSubscribed     bool   `json:"is_subscribed"`
	PremiumExpiresAt string `json:"premium_expires_at"`
	CooldownUntil    string `json:"cooldown_until"`
}

// TorBox plan ids as reported in User.Plan.
const (
	PlanFree      = 0
	PlanEssential = 1
	PlanPro       = 2
	PlanStandard  = 3
)

// PlanInfo describes a TorBox plan: its name and how many concurrent download
// slots it includes. Slots is 0 when unknown. Accounts can buy extra slots on
// top of a plan, so the count is the plan's floor, not a hard ceiling.
type PlanInfo struct {
	Name  string
	Slots int
}

// planInfo maps a plan id to what it includes. The slot counts (3/5/10) are
// TorBox's published per-plan download slots; the free plan's is not
// documented, so it's left unknown rather than guessed.
func planInfo(plan int) PlanInfo {
	switch plan {
	case PlanFree:
		return PlanInfo{Name: "Free"}
	case PlanEssential:
		return PlanInfo{Name: "Essential", Slots: 3}
	case PlanStandard:
		return PlanInfo{Name: "Standard", Slots: 5}
	case PlanPro:
		return PlanInfo{Name: "Pro", Slots: 10}
	default:
		return PlanInfo{Name: fmt.Sprintf("unknown (%d)", plan)}
	}
}

// Me returns the authenticated account's details. A rejected API key surfaces
// as an *APIError (401/403, or a BAD_TOKEN/NO_AUTH/AUTH_ERROR code).
func (c *Client) Me(ctx context.Context) (*User, error) {
	q := url.Values{}
	q.Set("settings", "false")
	env, err := c.do(ctx, http.MethodGet, "user/me", q, nil, "", true)
	if err != nil {
		return nil, err
	}
	var u User
	if err := unmarshalData(env, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// IsAuthError reports whether TorBox rejected the request's credentials.
func (e *APIError) IsAuthError() bool {
	switch e.Code {
	case "BAD_TOKEN", "NO_AUTH", "AUTH_ERROR":
		return true
	}
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// expiryWarning is how far ahead of a subscription's end the check starts
// warning about it.
const expiryWarning = 7 * 24 * time.Hour

// CheckAccount verifies the API key against TorBox once and logs what it
// learns, so a bad key, a lapsed subscription or a slot setting that doesn't
// match the plan shows up in the log at startup — not hours later as
// submissions that quietly sit in the queue. Best-effort: problems are logged,
// never returned, and nothing is disabled (TorBox may simply be unreachable
// right now; the engine retries on its own schedule).
func CheckAccount(ctx context.Context, c *Client, configuredSlots int, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	u, err := c.Me(ctx)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsAuthError() {
			log.Error("TorBox rejected the API key; nothing will be submitted until it is fixed (check TORBOX_API_KEY)",
				"err", err)
			return
		}
		log.Warn("TorBox account check failed; continuing", "err", err)
		return
	}
	checkUser(u, configuredSlots, time.Now(), log)
}

// checkUser logs the account findings for u. Split from CheckAccount so the
// rules can be tested without an HTTP round trip.
func checkUser(u *User, configuredSlots int, now time.Time, log *slog.Logger) {
	plan := planInfo(u.Plan)
	expires, hasExpiry := parseTorBoxTime(u.PremiumExpiresAt)
	attrs := []any{"plan", plan.Name, "max_active_slots", configuredSlots}
	if hasExpiry {
		attrs = append(attrs, "premium_expires_at", expires.Format(time.RFC3339))
	}
	log.Info("TorBox account verified", attrs...)

	switch {
	case u.Plan == PlanFree:
		log.Warn("TorBox account is on the Free plan; downloads are heavily limited — a paid plan is expected")
	case hasExpiry && !expires.After(now):
		log.Warn("TorBox subscription has expired; submissions will be rejected until it is renewed",
			"premium_expires_at", expires.Format(time.RFC3339))
	case hasExpiry && expires.Sub(now) < expiryWarning:
		log.Warn("TorBox subscription expires soon",
			"premium_expires_at", expires.Format(time.RFC3339), "in", expires.Sub(now).Round(time.Hour))
	}

	if until, ok := parseTorBoxTime(u.CooldownUntil); ok && until.After(now) {
		log.Warn("TorBox account is in a cooldown; new downloads may be refused until it ends",
			"cooldown_until", until.Format(time.RFC3339))
	}

	if plan.Slots == 0 {
		return
	}
	switch {
	case configuredSlots < plan.Slots:
		log.Info("torbox.max_active_slots is below what the plan includes; raise it to use the spare slots",
			"plan", plan.Name, "plan_slots", plan.Slots, "max_active_slots", configuredSlots)
	case configuredSlots > plan.Slots:
		// Not necessarily wrong — extra slots can be bought — but if they weren't,
		// every submission past the plan's cap gets queued by TorBox, cancelled,
		// and retried after a cooldown, burning createtorrent quota.
		log.Warn("torbox.max_active_slots is above what the plan includes; unless extra slots were purchased, lower it",
			"plan", plan.Name, "plan_slots", plan.Slots, "max_active_slots", configuredSlots)
	}
}

// parseTorBoxTime parses a TorBox timestamp. TorBox emits ISO-8601, sometimes
// with fractional seconds and sometimes without a zone (UTC is assumed then).
func parseTorBoxTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
