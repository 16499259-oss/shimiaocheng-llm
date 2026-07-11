// Package router resolves which upstream provider should serve a request,
// based on time-window routing rules stored in the database and the static
// provider / model-mapping configuration.
//
// Key invariants (also documented in AGENTS.md §6):
//   - Time windows are evaluated in Asia/Shanghai only (timeutil.ShanghaiTZ).
//   - When a window matches provider B, B is returned immediately. The router
//     NEVER falls back to provider A — if B fails upstream, the request fails
//     (502), it is never silently retried against A.
//   - Provider credentials live ONLY in memory (CredentialHolder / CredentialStore)
//     and are injected from environment variables at startup. They are never
//     written to disk or logs (see ADR-0002).
package router

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/timeutil"
)

// Provider is a resolved upstream target for a single request.
type Provider struct {
	ID       string // provider id, e.g. "zhipu" / "openai"
	Endpoint string // upstream chat-completions endpoint
	APIKey   string // in-memory API key (never logged)
}

// CredentialHolder is a thread-safe in-memory holder for a single provider's
// API key. The key is ONLY ever held in memory and injected from an environment
// variable at startup; it is never written to disk (see ADR-0002).
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
type RoutingRule struct {
	ID                int64
	ProviderID        string
	StartTime         string // "HH:MM" (Asia/Shanghai)
	EndTime           string // "HH:MM" (Asia/Shanghai)
	DaysOfWeek        string // "*" or comma list of weekday nums (0=Sun..6=Sat)
	Timezone          string
	Enabled           bool
	DefaultProviderID string // schema-reserved; global default comes from config
}

// Router selects the upstream provider for each request.
type Router struct {
	db          *sql.DB
	providers   map[string]config.ProviderConfig
	mappings    map[string]map[string]string // external -> providerID -> real model
	defaultProv config.ProviderConfig
	creds       *CredentialStore
}

// NewRouter builds a Router from the configuration and the credential store.
// The global default provider is taken from the provider flagged IsDefault
// (or, if none is flagged, the first configured provider).
func NewRouter(db *sql.DB, cfg *config.Config, creds *CredentialStore) *Router {
	providers := make(map[string]config.ProviderConfig, len(cfg.Providers))
	var def config.ProviderConfig
	defSet := false
	for _, p := range cfg.Providers {
		providers[p.ID] = p
		if p.IsDefault {
			def = p
			defSet = true
		}
	}
	if !defSet && len(cfg.Providers) > 0 {
		def = cfg.Providers[0]
	}

	return &Router{
		db:          db,
		providers:   providers,
		mappings:    buildMappings(cfg.ModelMappings),
		defaultProv: def,
		creds:       creds,
	}
}

// buildMappings flattens the config model mappings into a fast lookup map.
// Note: external uniqueness is intentionally NOT enforced this release
// (see 主理人 decisions, P2) — when a duplicate external appears, the first
// match wins.
func buildMappings(mms []config.ModelMapping) map[string]map[string]string {
	out := make(map[string]map[string]string, len(mms))
	for _, mm := range mms {
		if _, exists := out[mm.External]; exists {
			continue // first match wins
		}
		out[mm.External] = mm.PerProvider
	}
	return out
}

// ResolveProvider decides which upstream should serve a request at the given
// time. It evaluates enabled routing rules (time window + day-of-week) in
// Asia/Shanghai; the first matching rule wins and its provider is returned
// immediately (no fallback to the default). If no rule matches, the configured
// default provider is returned. It returns an error only when NO providers are
// configured at all.
func (r *Router) ResolveProvider(now time.Time) (Provider, error) {
	now = now.In(timeutil.ShanghaiTZ)

	for _, rule := range r.loadRules() {
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
		prov, ok := r.providers[rule.ProviderID]
		if !ok {
			// Strict no-fallback invariant (AGENTS.md §6): a time window that
			// matched provider B must resolve to B. If B is not configured, this
			// is an administrator misconfiguration — surface it explicitly (the
			// handler converts this error to HTTP 503) instead of silently
			// downgrading to the default provider A.
			return Provider{}, fmt.Errorf("routing rule %d targets provider %q which is not configured", rule.ID, rule.ProviderID)
		}
		return Provider{
			ID:       prov.ID,
			Endpoint: prov.Endpoint,
			APIKey:   r.creds.Get(prov.ID),
		}, nil
	}

	// No window matched -> use the configured default provider.
	return r.defaultProvider()
}

// defaultProvider returns the global default provider. It errors only when no
// providers are configured at all.
func (r *Router) defaultProvider() (Provider, error) {
	if len(r.providers) == 0 {
		return Provider{}, fmt.Errorf("no upstream providers configured")
	}
	if p, ok := r.providers[r.defaultProv.ID]; ok {
		return Provider{
			ID:       p.ID,
			Endpoint: p.Endpoint,
			APIKey:   r.creds.Get(p.ID),
		}, nil
	}
	// Defensive: default ID not found — return the first configured provider.
	for id, p := range r.providers {
		return Provider{
			ID:       p.ID,
			Endpoint: p.Endpoint,
			APIKey:   r.creds.Get(id),
		}, nil
	}
	return Provider{}, fmt.Errorf("no upstream providers configured")
}

// RewriteModel translates an external (user-facing) model name into the real
// model name for the given provider. If there is no mapping for the external
// name or for that provider, the original external name is returned unchanged
// (passthrough) — it never errors.
func (r *Router) RewriteModel(external, providerID string) string {
	if m, ok := r.mappings[external]; ok {
		if real, ok2 := m[providerID]; ok2 && real != "" {
			return real
		}
	}
	return external
}

// loadRules reads enabled routing rules from the database. A nil DB or any DB
// error yields an empty slice, which makes ResolveProvider fall back to the
// configured default provider. It never panics.
func (r *Router) loadRules() []RoutingRule {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, provider_id, start_time, end_time, days_of_week, timezone, enabled,
		        COALESCE(default_provider_id, '')
		 FROM provider_routing_rules
		 WHERE enabled = 1
		 ORDER BY id`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var rules []RoutingRule
	for rows.Next() {
		var rule RoutingRule
		var enabled int
		var tz string
		var defPID string
		if err := rows.Scan(
			&rule.ID, &rule.ProviderID, &rule.StartTime, &rule.EndTime,
			&rule.DaysOfWeek, &tz, &enabled, &defPID,
		); err != nil {
			return nil
		}
		rule.Timezone = tz
		rule.Enabled = enabled == 1
		rule.DefaultProviderID = defPID
		rules = append(rules, rule)
	}
	return rules
}
