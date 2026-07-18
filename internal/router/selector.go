// Package router resolves which upstream provider should serve a request,
// based on time-window routing rules and the provider / model-mapping data
// loaded from the database via ProviderTable snapshots.
//
// Key invariants (also documented in AGENTS.md §6):
//   - Time windows are evaluated in Asia/Shanghai only (timeutil.ShanghaiTZ).
//   - When a window matches provider B, B is returned immediately. The router
//     NEVER falls back to provider A — if B fails upstream, the request fails
//     (502), it is never silently retried against A.
//   - Provider credentials live ONLY in memory (decrypted at load time) and are
//     NEVER written to disk or logs (see ADR-0002 / ADR-0007).
//   - The provider table is stored in an atomic.Value for zero-lock hot-path reads.
//     Reload() is called after Admin CRUD operations; it atomically swaps the table.
package router

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/timeutil"
)

// Provider is a resolved upstream target for a single request.
type Provider struct {
	ID       string // provider id, e.g. "zhipu" / "openai"
	Endpoint string // upstream chat-completions endpoint
	APIKey   string // in-memory API key (never logged)
	// ── Passthrough / MCP support (populated from the snapshot) ──
	AllowPassthrough bool              // provider may be used as a passthrough target
	AuthHeader       string            // upstream auth header name
	AuthScheme       string            // "bearer" | "x-api-key" | "none"
	ExtraHeaders     map[string]string // static extra headers
}

// CredentialHolder is a thread-safe in-memory holder for a single provider's
// API key. The key is ONLY ever held in memory and injected from an environment
// variable at startup; it is never written to disk (see ADR-0002).
// Deprecated: Provider credentials are now managed via ProviderStore/Router
// atomic snapshots. CredentialHolder is retained for backward compatibility
// with existing code that may reference it.
type CredentialHolder struct {
	mu  sync.RWMutex
	key string
}

// NewCredentialHolder creates a holder pre-seeded with an (optional) key.
func NewCredentialHolder(initial string) *CredentialHolder {
	return &CredentialHolder{key: initial}
}

// Get returns the current key.
func (h *CredentialHolder) Get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.key
}

// Set replaces the key (e.g. admin panel hot-update). Memory only.
func (h *CredentialHolder) Set(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.key = key
}

// CredentialStore holds one CredentialHolder per provider ID. The same holder
// instance is returned for a given provider ID, so admin updates and router
// reads share one source of truth.
// Deprecated: Provider credentials are now managed via ProviderStore/Router
// atomic snapshots. CredentialStore is retained for backward compatibility.
type CredentialStore struct {
	mu      sync.RWMutex
	holders map[string]*CredentialHolder
}

// NewCredentialStore creates an empty credential store.
func NewCredentialStore() *CredentialStore {
	return &CredentialStore{holders: make(map[string]*CredentialHolder)}
}

// Holder returns the credential holder for a provider, creating it if needed.
func (s *CredentialStore) Holder(providerID string) *CredentialHolder {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.holders[providerID]
	if !ok {
		h = NewCredentialHolder("")
		s.holders[providerID] = h
	}
	return h
}

// Get returns the credential for a provider (empty string if unset).
func (s *CredentialStore) Get(providerID string) string {
	return s.Holder(providerID).Get()
}

// Set updates the credential for a provider.
func (s *CredentialStore) Set(providerID, key string) {
	s.Holder(providerID).Set(key)
}

// RoutingRule mirrors a row in the provider_routing_rules table.
// Deprecated: Use models.RoutingRule instead. This alias is kept for backward
// compatibility with code that references router.RoutingRule.
type RoutingRule = models.RoutingRule

// Router selects the upstream provider for each request.
// Uses an atomic.Value to hold a ProviderTable snapshot for zero-lock reads on
// the hot path. The snapshot is rebuilt by Reload() after Admin CRUD operations.
type Router struct {
	db    *sql.DB
	store *provider.ProviderStore
	table atomic.Value // *provider.ProviderTable
}

// NewRouter creates a Router and performs an initial load from the store.
// If db is nil, an empty table is stored (useful for tests that don't need a DB).
func NewRouter(db *sql.DB, store *provider.ProviderStore) *Router {
	r := &Router{
		db:    db,
		store: store,
	}
	// Perform initial load if we have a valid DB; otherwise store an empty table.
	if db != nil {
		if err := r.Reload(); err != nil {
			// Store an empty table on initial load failure.
			r.table.Store(&provider.ProviderTable{
				Providers: make(map[string]provider.ProviderEntry),
				Mappings:  make(map[string]map[string]string),
				Rules:     nil,
				Default:   "",
			})
		}
	} else {
		r.table.Store(&provider.ProviderTable{
			Providers: make(map[string]provider.ProviderEntry),
			Mappings:  make(map[string]map[string]string),
			Rules:     nil,
			Default:   "",
		})
	}
	return r
}

// Reload rebuilds the provider table from the database and atomically swaps it.
// It is called after every Admin CRUD operation to ensure routing picks up
// changes immediately. On failure, the current snapshot is preserved.
// If db is nil, it returns an error.
func (r *Router) Reload() error {
	if r.db == nil {
		return fmt.Errorf("cannot reload: no database connection")
	}
	table, err := r.store.BuildProviderTable()
	if err != nil {
		return fmt.Errorf("reload provider table: %w", err)
	}
	r.table.Store(table)
	return nil
}

// ResolveProvider decides which upstream should serve a request at the given
// time. It evaluates enabled routing rules (time window + day-of-week) in
// Asia/Shanghai; the first matching rule wins and its provider is returned
// immediately (no fallback to the default). If no rule matches, the configured
// default provider is returned. It returns an error only when NO providers are
// configured at all.
//
// Precedence when multiple enabled rules overlap in time:
//  1. higher Priority wins (explicit administrative control),
//  2. on equal priority, the narrower (more specific) window wins — so a
//     later-added override rule that covers a sub-window automatically takes
//     precedence over a broader base rule without manual reordering,
//  3. on equal priority AND equal width, the lower id (older) wins for stability.
func (r *Router) ResolveProvider(now time.Time) (Provider, error) {
	now = now.In(timeutil.ShanghaiTZ)
	table := r.table.Load().(*provider.ProviderTable)

	rules := sortRoutingRules(table.Rules)
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		// Time-window check (locked to Asia/Shanghai).
		if !timeutil.IsInRange(rule.StartTime, rule.EndTime, now) {
			continue
		}
		// Day-of-week check.
		if !timeutil.MatchDay(rule.DaysOfWeek, now) {
			continue
		}

		// Window hit -> return this provider. NEVER fall back to the default.
		prov, ok := table.Providers[rule.ProviderID]
		if !ok {
			// Strict no-fallback invariant (AGENTS.md §6): a time window that
			// matched provider B must resolve to B. If B is not configured, this
			// is an administrator misconfiguration — surface it explicitly.
			return Provider{}, fmt.Errorf("routing rule %d targets provider %q which is not configured", rule.ID, rule.ProviderID)
		}
		return Provider{
			ID:               prov.Slug,
			Endpoint:         prov.Endpoint,
			APIKey:           prov.APIKey,
			AllowPassthrough: prov.AllowPassthrough,
			AuthHeader:       prov.AuthHeader,
			AuthScheme:       prov.AuthScheme,
			ExtraHeaders:     prov.ExtraHeaders,
		}, nil
	}

	// No window matched -> use the configured default provider.
	return r.defaultProvider(table)
}

// defaultProvider returns the global default provider from the table snapshot.
func (r *Router) defaultProvider(table *provider.ProviderTable) (Provider, error) {
	if len(table.Providers) == 0 {
		return Provider{}, fmt.Errorf("no upstream providers configured")
	}
	if prov, ok := table.Providers[table.Default]; ok {
		return Provider{
			ID:               prov.Slug,
			Endpoint:         prov.Endpoint,
			APIKey:           prov.APIKey,
			AllowPassthrough: prov.AllowPassthrough,
			AuthHeader:       prov.AuthHeader,
			AuthScheme:       prov.AuthScheme,
			ExtraHeaders:     prov.ExtraHeaders,
		}, nil
	}
	// Defensive: default slug not found — return the first configured provider.
	for _, prov := range table.Providers {
		return Provider{
			ID:               prov.Slug,
			Endpoint:         prov.Endpoint,
			APIKey:           prov.APIKey,
			AllowPassthrough: prov.AllowPassthrough,
			AuthHeader:       prov.AuthHeader,
			AuthScheme:       prov.AuthScheme,
			ExtraHeaders:     prov.ExtraHeaders,
		}, nil
	}
	return Provider{}, fmt.Errorf("no upstream providers configured")
}

// GetProviderBySlug looks up a provider from the current atomic snapshot by slug.
// Returns the Provider and true if found, or zero Provider and false if the slug
// is not present (e.g. provider is disabled, deleted, or never existed).
// This is used by the proxy hot path for fixed-route-mode users.
func (r *Router) GetProviderBySlug(slug string) (Provider, bool) {
	table := r.table.Load().(*provider.ProviderTable)
	prov, ok := table.Providers[slug]
	if !ok {
		return Provider{}, false
	}
	return Provider{
		ID:               prov.Slug,
		Endpoint:         prov.Endpoint,
		APIKey:           prov.APIKey,
		AllowPassthrough: prov.AllowPassthrough,
		AuthHeader:       prov.AuthHeader,
		AuthScheme:       prov.AuthScheme,
		ExtraHeaders:     prov.ExtraHeaders,
	}, true
}

// RewriteModel translates an external (user-facing) model name into the real
// model name for the given provider. If there is no mapping for the external
// name or for that provider, the original external name is returned unchanged
// (passthrough) — it never errors.
//
// Matching is case-insensitive: exact match is tried first (hot path), and if
// it fails, a case-insensitive scan across all mapping keys is performed as a
// fallback. This handles clients such as Cursor that may send model names with
// different casing (e.g. "GLM-5.2" vs the stored "glm-5.2").
func (r *Router) RewriteModel(external, providerID string) string {
	table := r.table.Load().(*provider.ProviderTable)
	if m, ok := table.Mappings[external]; ok {
		if real, ok2 := m[providerID]; ok2 && real != "" {
			return real
		}
	}
	// Case-insensitive fallback for clients that send model names with
	// different casing (e.g. Cursor sends "GLM-5.2" but mapping is "glm-5.2").
	for key, m := range table.Mappings {
		if strings.EqualFold(key, external) {
			if real, ok := m[providerID]; ok && real != "" {
				return real
			}
			// Found a case-insensitive match for the external model name
			// but no per-provider override — passthrough.
			break
		}
	}
	return external
}

// sortRoutingRules returns a precedence-ordered copy of the rules snapshot.
// It does NOT mutate the shared snapshot (which lives in an atomic.Value and
// may be read concurrently), so a fresh copy is made each call.
//
// Order: Priority DESC, then narrower window first (more specific), then id ASC.
func sortRoutingRules(rules []models.RoutingRule) []models.RoutingRule {
	out := make([]models.RoutingRule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority // higher priority first
		}
		da, db := windowMinutes(a.StartTime, a.EndTime), windowMinutes(b.StartTime, b.EndTime)
		if da != db {
			return da < db // narrower (more specific) window first
		}
		return a.ID < b.ID // stable: older rule first
	})
	return out
}

// windowMinutes returns the duration of a "HH:MM"-"HH:MM" time window in minutes,
// evaluated in Asia/Shanghai semantics (the router always locks to that zone).
// Overnight ranges (start > end, e.g. "22:00"-"06:00") wrap past midnight.
// An unparseable endpoint yields the widest possible window (24*60) so it sorts
// last among equal-priority rules rather than panicking.
func windowMinutes(start, end string) int {
	const day = 24 * 60
	s := parseHHMM(start)
	e := parseHHMM(end)
	if s < 0 || e < 0 {
		return day
	}
	if s <= e {
		return e - s
	}
	// Overnight.
	return (day - s) + e
}

// parseHHMM parses an "HH:MM" string into minutes since midnight.
// Returns -1 if the format is invalid.
func parseHHMM(v string) int {
	if len(v) != 5 || v[2] != ':' {
		return -1
	}
	h := int(v[0]-'0')*10 + int(v[1]-'0')
	m := int(v[3]-'0')*10 + int(v[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return -1
	}
	return h*60 + m
}
