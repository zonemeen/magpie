package provider

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

// A plugin's model lists must survive both a failed read and a restart:
// the whole card stays, and routing and usage still count the same models
// in each window (#4168). An aside window must not cost the card its cache.
func TestKeepLastPluginWindows(t *testing.T) {
	pool := []string{"Grok-4.7@256k", "Composer-2.5"}
	for _, tc := range []struct {
		name              string
		models, notModels []string
		aside             bool
	}{
		{name: "all"},
		{name: "models", models: pool},
		{name: "notModels", notModels: pool},
		{name: "models win", models: pool, notModels: pool},
		{name: "aside models", models: pool, aside: true},
		{name: "aside notModels", notModels: pool, aside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			restart := func() {
				lastQuotas.Lock()
				lastQuotas.m, lastQuotas.loaded = nil, false
				lastQuotas.Unlock()
			}
			restart()
			t.Cleanup(restart)
			now := time.Now()
			reset := now.Add(time.Hour).UTC().Truncate(time.Second)
			q := quotaOfPlugin(SubscriptionQuota{Provider: "acme-plugin", User: "A@x.com"}, plugin.Usage{Windows: []plugin.UsageWindow{
				{Name: "Day", Used: fptr(25), Span: 24 * 3600},
				{Name: "Pool", Used: fptr(100), Span: 5 * 3600, ResetsAt: reset.Format(time.RFC3339),
					Models: tc.models, NotModels: tc.notModels, Aside: tc.aside},
			}})
			failed := SubscriptionQuota{Provider: q.Provider, User: "a@x.com", Error: "HTTP 503 service unavailable"}
			check := func(q SubscriptionQuota, kept bool) {
				t.Helper()
				if q.Error != "" || (q.AsOf != nil) != kept || len(q.Windows) != 2 {
					t.Fatalf("card lost on a failed read: %+v", q)
				}
				day, w := q.Windows[0], q.Windows[1]
				if day.Name != "Day" || day.Used != 25 || day.Span != 24*time.Hour ||
					w.Name != "Pool" || w.Used != 100 || w.Span != 5*time.Hour || w.Aside != tc.aside ||
					w.ResetsAt == nil || !w.ResetsAt.Equal(reset) {
					t.Fatalf("window lost its reading: %+v", q.Windows)
				}
				a, known := routedAllowance("plugin:acme", q.User, q, now)
				if !known {
					t.Fatal("routing lost the account's allowance")
				}
				for model, inPool := range map[string]bool{
					"grok-4.7":        true,
					"GROK-4.7@256K":   true,
					"grok-4.7@500k":   false,
					"composer-2.5@1m": true,
					"grok-4.7-Trial":  false,
					"claude-opus-5-5": false,
				} {
					counts := true
					if len(tc.models) != 0 {
						counts = inPool
					} else if len(tc.notModels) != 0 {
						counts = !inPool
					}
					if w.Counts(model) != counts {
						t.Errorf("usage: Pool counts %s = %v, want %v", model, w.Counts(model), counts)
					}
					want := 25.0
					if counts && !tc.aside {
						want = 100
					}
					if used, _ := a.For(model, now); used != want {
						t.Errorf("routing: %s used %v, want %v", model, used, want)
					}
				}
			}
			check(keepLast(q, ""), false)
			check(keepLast(failed, ""), true)
			restart()
			check(keepLast(failed, ""), true)
			// Keeping a restored reading again must not lose its model lists.
			keepLast(keepLast(failed, ""), "")
			restart()
			check(keepLast(failed, ""), true)
		})
	}
}

// Old cache files have no model lists; their ordinary and single-model
// windows must keep the scope and routing metadata they already stored.
func TestKeepLastLegacyWindows(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := lastQuotasT{}
	if err := os.MkdirAll(filepath.Dir(lastQuotasPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastQuotasPath(), []byte(`{
		"claude/a@x.com": {
			"at": "2026-10-28T00:00:00Z",
			"quota": {"provider": "claude", "user": "a@x.com", "windows": [
				{"name": "Day", "used": 25}, {"name": "Opus", "used": 100}, {"name": "Extra", "used": 100}
			]},
			"spans": [{"span": 86400000000000}, {"model": "opus"}, {"aside": true}]
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.load()
	q, ok := c.reading("claude/a@x.com")
	if !ok || len(q.Windows) != 3 || q.Windows[0].Span != 24*time.Hour || !q.Windows[2].Aside {
		t.Fatalf("legacy reading: %+v", q)
	}
	a := allowanceOf(q.Windows, time.Now())
	for model, want := range map[string]float64{"claude-sonnet-4-6": 25, "claude-opus-5-5": 100} {
		if used, _ := a.For(model, time.Now()); used != want {
			t.Errorf("%s: used %v, want %v", model, used, want)
		}
	}
}

// The host's real usage path writes the snapshot that startup routing
// reads, including both lists and an aside window with a list of its own.
func TestPluginLastAllowance(t *testing.T) {
	restart := func() {
		lastQuotas.Lock()
		lastQuotas.m, lastQuotas.loaded = nil, false
		lastQuotas.Unlock()
	}
	restart()
	t.Cleanup(restart)
	windows := filepath.Join(t.TempDir(), "windows.json")
	if err := os.WriteFile(windows, []byte(`{
		"a@fake": [
			{"name": "Pool", "used": 20, "models": ["fake-claude"]},
			{"name": "Others", "used": 100, "notModels": ["fake-claude"]},
			{"name": "Extra", "used": 100, "aside": true, "models": ["fake-claude"]}
		],
		"full@fake": [{"name": "Day", "used": 75}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	u := fakePluginUsage(t, windows)
	if q := u["a@fake"]; q.Error != "" || len(q.Windows) != 3 {
		t.Fatalf("plugin reading: %+v", q)
	}
	restart()
	a := lastAllowances("plugin:fakeco")
	now := time.Now()
	for model, want := range map[string]float64{"fake-claude": 20, "fake-gpt": 100} {
		if used, _ := a["a@fake"].For(model, now); used != want {
			t.Errorf("startup routing: %s used %v, want %v", model, used, want)
		}
	}
	if used, _ := a["full@fake"].For("fake-claude", now); used != 75 {
		t.Errorf("other account: used %v, want 75", used)
	}
}

// A predicate with no lists to restore is still not safe to persist:
// dropping it on restart would turn a model's pool into every model's.
func TestKeepLastUnstoredMatcher(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	restart := func() {
		lastQuotas.Lock()
		lastQuotas.m, lastQuotas.loaded = nil, false
		lastQuotas.Unlock()
	}
	restart()
	t.Cleanup(restart)
	q := SubscriptionQuota{Provider: "acme", User: "u", Windows: []QuotaWindow{
		{Name: "Pool", Used: 100, matches: func(model string) bool { return model == "one" }},
	}}
	keepLast(q, "")
	restart()
	q = keepLast(SubscriptionQuota{Provider: "acme", User: "u", Error: "HTTP 503 service unavailable"}, "")
	if q.Error == "" || q.AsOf != nil || len(q.Windows) != 0 {
		t.Fatalf("an unrestorable matcher was kept: %+v", q)
	}
}
