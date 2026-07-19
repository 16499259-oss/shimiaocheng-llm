// Package provider manages upstream LLM provider data, model mappings,
// routing rules, and audit logs with encryption at rest.
package provider

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/security"
)

// ProviderTable is an atomic snapshot of all enabled providers, their mappings,
// and routing rules. It is consumed by the Router via atomic.Value.
type ProviderTable struct {
	Providers map[string]ProviderEntry     // slug -> entry
	Mappings  map[string]map[string]string // external -> providerSlug -> realModel
	Rules     []models.RoutingRule         // time-window routing rules
	Default   string                       // default provider slug
}

// ProviderEntry is an in-memory entry for a single provider in the snapshot.
type ProviderEntry struct {
	Slug     string
	Endpoint string
	APIKey   string // decrypted plaintext key (memory only, never logged or serialized)
	// ── Passthrough / MCP support ──
	AllowPassthrough bool              // provider may be used as a passthrough target
	AuthHeader       string            // upstream auth header name (default "Authorization")
	AuthScheme       string            // "bearer" | "x-api-key" | "none", default "bearer"
	ExtraHeaders     map[string]string // static extra headers (e.g. anthropic-version)
}

// ProviderStore is the data access layer for providers, model mappings,
// routing rules, and audit logs. It handles encryption/decryption transparently.
type ProviderStore struct {
	db  *sql.DB
	kek []byte
}

// NewProviderStore creates a new ProviderStore. The kek must be a valid 32-byte
// AES-256 key; if nil or wrong length, it panics.
func NewProviderStore(db *sql.DB, kek []byte) *ProviderStore {
	if len(kek) != 32 {
		panic(fmt.Sprintf("ProviderStore: KEK must be 32 bytes, got %d", len(kek)))
	}
	return &ProviderStore{db: db, kek: kek}
}

// ─────────────────────────── Provider CRUD ───────────────────────────

// ListProviders returns all providers (including disabled ones).
func (s *ProviderStore) ListProviders() ([]models.ProviderRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, name, slug, endpoint, encrypted_key, is_default, enabled,
		        allow_passthrough, auth_header, auth_scheme, extra_headers,
		        monthly_token_limit, monthly_call_limit,
		        monthly_token_low_ratio, monthly_call_low_ratio,
		        created_at, updated_at
		 FROM providers ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()

	var providers []models.ProviderRecord
	for rows.Next() {
		var p models.ProviderRecord
		var isDef, enabled, allowPassthrough int
		var authHeader, authScheme, extraHeaders string
		var monthlyTokenLimit, monthlyCallLimit int64
		var monthlyTokenLowRatio, monthlyCallLowRatio float64
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Endpoint, &p.EncryptedKey,
			&isDef, &enabled, &allowPassthrough, &authHeader, &authScheme,
			&extraHeaders, &monthlyTokenLimit, &monthlyCallLimit,
			&monthlyTokenLowRatio, &monthlyCallLowRatio, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan provider: %w", err)
		}
		p.IsDefault = isDef == 1
		p.Enabled = enabled == 1
		p.AllowPassthrough = allowPassthrough == 1
		p.AuthHeader = authHeader
		p.AuthScheme = authScheme
		p.ExtraHeaders = extraHeaders
		p.MonthlyTokenLimit = monthlyTokenLimit
		p.MonthlyCallLimit = monthlyCallLimit
		p.MonthlyTokenLowRatio = monthlyTokenLowRatio
		p.MonthlyCallLowRatio = monthlyCallLowRatio
		providers = append(providers, p)
	}
	if providers == nil {
		providers = []models.ProviderRecord{}
	}
	return providers, rows.Err()
}

// GetProvider returns a single provider by slug.
func (s *ProviderStore) GetProvider(slug string) (*models.ProviderRecord, error) {
	var p models.ProviderRecord
	var isDef, enabled, allowPassthrough int
	var authHeader, authScheme, extraHeaders string
	var monthlyTokenLimit, monthlyCallLimit int64
	var monthlyTokenLowRatio, monthlyCallLowRatio float64
	err := s.db.QueryRow(
		`SELECT id, name, slug, endpoint, encrypted_key, is_default, enabled,
		        allow_passthrough, auth_header, auth_scheme, extra_headers,
		        monthly_token_limit, monthly_call_limit,
		        monthly_token_low_ratio, monthly_call_low_ratio,
		        created_at, updated_at
		 FROM providers WHERE slug = ?`, slug,
	).Scan(&p.ID, &p.Name, &p.Slug, &p.Endpoint, &p.EncryptedKey,
		&isDef, &enabled, &allowPassthrough, &authHeader, &authScheme,
		&extraHeaders, &monthlyTokenLimit, &monthlyCallLimit,
		&monthlyTokenLowRatio, &monthlyCallLowRatio, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get provider %s: %w", slug, err)
	}
	p.IsDefault = isDef == 1
	p.Enabled = enabled == 1
	p.AllowPassthrough = allowPassthrough == 1
	p.AuthHeader = authHeader
	p.AuthScheme = authScheme
	p.ExtraHeaders = extraHeaders
	p.MonthlyTokenLimit = monthlyTokenLimit
	p.MonthlyCallLimit = monthlyCallLimit
	p.MonthlyTokenLowRatio = monthlyTokenLowRatio
	p.MonthlyCallLowRatio = monthlyCallLowRatio
	return &p, nil
}

// CreateProvider inserts a new provider with encrypted API key.
// monthlyTokenLimit and monthlyCallLimit are the provider's monthly usage caps
// (0 = unlimited). They are persisted as-is (a 0 value is valid and means
// "no limit"). monthlyTokenLowRatio and monthlyCallLowRatio are the provider's
// low-balance threshold overrides (remaining ratio; 0 = inherit the global
// default configured in config.ProviderQuota).
func (s *ProviderStore) CreateProvider(name, slug, endpoint, apiKey string, isDefault, allowPassthrough bool, authHeader, authScheme string, extraHeaders map[string]string, monthlyTokenLimit, monthlyCallLimit int64, monthlyTokenLowRatio, monthlyCallLowRatio float64) (*models.ProviderRecord, error) {
	now := time.Now().Format(time.RFC3339)

	// Encrypt the API key.
	encryptedKey, err := security.Encrypt(apiKey, s.kek)
	if err != nil {
		return nil, fmt.Errorf("encrypt API key: %w", err)
	}

	isDefInt := 0
	if isDefault {
		isDefInt = 1
	}
	allowInt := 0
	if allowPassthrough {
		allowInt = 1
	}
	// Default the auth scheme to chat-compatible values when unspecified.
	if authHeader == "" {
		authHeader = "Authorization"
	}
	if authScheme == "" {
		authScheme = "bearer"
	}
	extraJSON, err := json.Marshal(extraHeaders)
	if err != nil {
		return nil, fmt.Errorf("marshal extra_headers: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// If this is the default provider, clear existing defaults first.
	if isDefault {
		if _, err := tx.Exec(`UPDATE providers SET is_default = 0 WHERE is_default = 1`); err != nil {
			return nil, fmt.Errorf("clear old defaults: %w", err)
		}
	}

	result, err := tx.Exec(
		`INSERT INTO providers (name, slug, endpoint, encrypted_key, is_default, enabled,
		        allow_passthrough, auth_header, auth_scheme, extra_headers,
		        monthly_token_limit, monthly_call_limit,
		        monthly_token_low_ratio, monthly_call_low_ratio, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, slug, endpoint, encryptedKey, isDefInt, allowInt, authHeader, authScheme, string(extraJSON), monthlyTokenLimit, monthlyCallLimit, monthlyTokenLowRatio, monthlyCallLowRatio, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert provider: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	s.WriteAudit("provider.create", "provider", slug,
		fmt.Sprintf(`{"name":"%s","slug":"%s","endpoint":"%s","is_default":%v,"allow_passthrough":%v,"auth_scheme":"%s"}`,
			name, slug, endpoint, isDefault, allowPassthrough, authScheme))

	return &models.ProviderRecord{
		ID:                   id,
		Name:                 name,
		Slug:                 slug,
		Endpoint:             endpoint,
		IsDefault:            isDefault,
		Enabled:              true,
		AllowPassthrough:     allowPassthrough,
		AuthHeader:           authHeader,
		AuthScheme:           authScheme,
		ExtraHeaders:         string(extraJSON),
		MonthlyTokenLimit:    monthlyTokenLimit,
		MonthlyCallLimit:     monthlyCallLimit,
		MonthlyTokenLowRatio: monthlyTokenLowRatio,
		MonthlyCallLowRatio:  monthlyCallLowRatio,
		CreatedAt:            now,
		UpdatedAt:            now,
	}, nil
}

// UpdateProvider updates provider fields by slug. Supports partial updates.
// If apiKey is provided (non-empty), the key is re-encrypted.
func (s *ProviderStore) UpdateProvider(slug string, updates map[string]any) (*models.ProviderRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Handle is_default: clear existing default if setting new one.
	if isDef, ok := updates["is_default"]; ok {
		if b, ok := isDef.(bool); ok && b {
			if _, err := tx.Exec(`UPDATE providers SET is_default = 0 WHERE is_default = 1`); err != nil {
				return nil, fmt.Errorf("clear old defaults: %w", err)
			}
		}
	}

	// Handle api_key: encrypt new key.
	if apiKey, ok := updates["encrypted_key"]; ok {
		if keyStr, ok := apiKey.(string); ok && keyStr != "" {
			encrypted, err := security.Encrypt(keyStr, s.kek)
			if err != nil {
				return nil, fmt.Errorf("encrypt new API key: %w", err)
			}
			updates["encrypted_key"] = encrypted
		} else {
			delete(updates, "encrypted_key")
		}
	}

	// Build dynamic UPDATE.
	now := time.Now().Format(time.RFC3339)
	setClauses := []string{"updated_at = ?"}
	args := []any{now}

	if v, ok := updates["name"]; ok {
		setClauses = append(setClauses, "name = ?")
		args = append(args, v)
	}
	if v, ok := updates["endpoint"]; ok {
		setClauses = append(setClauses, "endpoint = ?")
		args = append(args, v)
	}
	if v, ok := updates["encrypted_key"]; ok {
		setClauses = append(setClauses, "encrypted_key = ?")
		args = append(args, v)
	}
	if v, ok := updates["is_default"]; ok {
		if b, ok := v.(bool); ok {
			if b {
				setClauses = append(setClauses, "is_default = 1")
			} else {
				setClauses = append(setClauses, "is_default = 0")
			}
		}
	}
	if v, ok := updates["enabled"]; ok {
		if b, ok := v.(bool); ok {
			if b {
				setClauses = append(setClauses, "enabled = 1")
			} else {
				setClauses = append(setClauses, "enabled = 0")
			}
		}
	}
	if v, ok := updates["allow_passthrough"]; ok {
		if b, ok := v.(bool); ok {
			if b {
				setClauses = append(setClauses, "allow_passthrough = 1")
			} else {
				setClauses = append(setClauses, "allow_passthrough = 0")
			}
		}
	}
	if v, ok := updates["auth_header"]; ok {
		if s, ok := v.(string); ok {
			setClauses = append(setClauses, "auth_header = ?")
			args = append(args, s)
		}
	}
	if v, ok := updates["auth_scheme"]; ok {
		if s, ok := v.(string); ok {
			setClauses = append(setClauses, "auth_scheme = ?")
			args = append(args, s)
		}
	}
	if v, ok := updates["extra_headers"]; ok {
		if m, ok := v.(map[string]string); ok {
			ej, err := json.Marshal(m)
			if err != nil {
				return nil, fmt.Errorf("marshal extra_headers: %w", err)
			}
			setClauses = append(setClauses, "extra_headers = ?")
			args = append(args, string(ej))
		}
	}
	if v, ok := updates["monthly_token_limit"]; ok {
		if n, ok := v.(int64); ok {
			setClauses = append(setClauses, "monthly_token_limit = ?")
			args = append(args, n)
		}
	}
	if v, ok := updates["monthly_call_limit"]; ok {
		if n, ok := v.(int64); ok {
			setClauses = append(setClauses, "monthly_call_limit = ?")
			args = append(args, n)
		}
	}
	if v, ok := updates["monthly_token_low_ratio"]; ok {
		if n, ok := v.(float64); ok {
			setClauses = append(setClauses, "monthly_token_low_ratio = ?")
			args = append(args, n)
		}
	}
	if v, ok := updates["monthly_call_low_ratio"]; ok {
		if n, ok := v.(float64); ok {
			setClauses = append(setClauses, "monthly_call_low_ratio = ?")
			args = append(args, n)
		}
	}

	query := "UPDATE providers SET "
	for i, clause := range setClauses {
		if i > 0 {
			query += ", "
		}
		query += clause
	}
	query += " WHERE slug = ?"
	args = append(args, slug)

	if _, err := tx.Exec(query, args...); err != nil {
		return nil, fmt.Errorf("update provider: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	s.WriteAudit("provider.update", "provider", slug, fmt.Sprintf("%v", updates))

	return s.GetProvider(slug)
}

// DeleteProvider deletes a provider by slug. Before deletion it checks whether
// the provider is referenced by routing rules, model mappings, or user fixed_provider
// settings; if so, it returns an error with details.
func (s *ProviderStore) DeleteProvider(slug string) error {
	// Check routing rules reference.
	var ruleCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM provider_routing_rules WHERE provider_id = ?`, slug,
	).Scan(&ruleCount); err != nil {
		return fmt.Errorf("check routing rules: %w", err)
	}
	if ruleCount > 0 {
		return fmt.Errorf("cannot delete provider %q: referenced by %d routing rule(s)", slug, ruleCount)
	}

	// Check model mappings reference.
	var mappingCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM model_mappings WHERE provider_id = ?`, slug,
	).Scan(&mappingCount); err != nil {
		return fmt.Errorf("check model mappings: %w", err)
	}
	if mappingCount > 0 {
		return fmt.Errorf("cannot delete provider %q: referenced by %d model mapping(s)", slug, mappingCount)
	}

	// Check fixed-provider user references (T03: prevent deletion when users pin this provider).
	usernames, err := models.GetUsersByFixedProvider(s.db, slug)
	if err != nil {
		return fmt.Errorf("check fixed users: %w", err)
	}
	if len(usernames) > 0 {
		return fmt.Errorf("cannot delete provider %q: referenced by %d user(s) as fixed provider: %s",
			slug, len(usernames), strings.Join(usernames, ", "))
	}

	// Also check if it's the last provider — refuse if it's the only one.
	var totalCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&totalCount); err != nil {
		return fmt.Errorf("count providers: %w", err)
	}
	if totalCount <= 1 {
		return fmt.Errorf("cannot delete the last provider")
	}

	_, err = s.db.Exec(`DELETE FROM providers WHERE slug = ?`, slug)
	if err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}

	s.WriteAudit("provider.delete", "provider", slug, "")
	return nil
}

// ─────────────────────── ModelMapping CRUD ───────────────────────

// ListMappings returns all model mappings.
func (s *ProviderStore) ListMappings() ([]models.ModelMappingRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, external, provider_id, real_model, created_at
		 FROM model_mappings ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list mappings: %w", err)
	}
	defer rows.Close()

	var mappings []models.ModelMappingRecord
	for rows.Next() {
		var m models.ModelMappingRecord
		if err := rows.Scan(&m.ID, &m.External, &m.ProviderID, &m.RealModel, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan mapping: %w", err)
		}
		mappings = append(mappings, m)
	}
	if mappings == nil {
		mappings = []models.ModelMappingRecord{}
	}
	return mappings, rows.Err()
}

// CreateMapping inserts a new model mapping. Returns an error if the
// (external, provider_id) pair already exists (unique constraint).
func (s *ProviderStore) CreateMapping(external, providerID, realModel string) (*models.ModelMappingRecord, error) {
	now := time.Now().Format(time.RFC3339)
	result, err := s.db.Exec(
		`INSERT INTO model_mappings (external, provider_id, real_model, created_at)
		 VALUES (?, ?, ?, ?)`,
		external, providerID, realModel, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert mapping: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}

	s.WriteAudit("mapping.create", "model_mapping", fmt.Sprintf("%d", id),
		fmt.Sprintf(`{"external":"%s","provider_id":"%s","real_model":"%s"}`, external, providerID, realModel))

	return &models.ModelMappingRecord{
		ID:         id,
		External:   external,
		ProviderID: providerID,
		RealModel:  realModel,
		CreatedAt:  now,
	}, nil
}

// DeleteMapping deletes a model mapping by ID.
func (s *ProviderStore) DeleteMapping(id int64) error {
	_, err := s.db.Exec(`DELETE FROM model_mappings WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete mapping %d: %w", id, err)
	}

	s.WriteAudit("mapping.delete", "model_mapping", fmt.Sprintf("%d", id), "")
	return nil
}

// ─────────────────────── RoutingRule CRUD ───────────────────────

// ListRoutingRules returns all routing rules (enabled and disabled).
func (s *ProviderStore) ListRoutingRules() ([]models.RoutingRule, error) {
	rows, err := s.db.Query(
		`SELECT id, provider_id, start_time, end_time, days_of_week, timezone, enabled,
		        COALESCE(default_provider_id, ''), COALESCE(priority, 0)
		 FROM provider_routing_rules
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list routing rules: %w", err)
	}
	defer rows.Close()

	var rules []models.RoutingRule
	for rows.Next() {
		var rule models.RoutingRule
		var enabled int
		if err := rows.Scan(
			&rule.ID, &rule.ProviderID, &rule.StartTime, &rule.EndTime,
			&rule.DaysOfWeek, &rule.Timezone, &enabled, &rule.DefaultProviderID, &rule.Priority,
		); err != nil {
			return nil, fmt.Errorf("scan routing rule: %w", err)
		}
		rule.Enabled = enabled == 1
		rules = append(rules, rule)
	}
	if rules == nil {
		rules = []models.RoutingRule{}
	}
	return rules, rows.Err()
}

// CreateRoutingRule inserts a new routing rule.
func (s *ProviderStore) CreateRoutingRule(rule *models.RoutingRule) error {
	enabled := 0
	if rule.Enabled {
		enabled = 1
	}
	result, err := s.db.Exec(
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rule.ProviderID, rule.StartTime, rule.EndTime, rule.DaysOfWeek, rule.Timezone, enabled, rule.Priority,
	)
	if err != nil {
		return fmt.Errorf("insert routing rule: %w", err)
	}

	id, _ := result.LastInsertId()
	rule.ID = id

	s.WriteAudit("routing_rule.create", "routing_rule", fmt.Sprintf("%d", id),
		fmt.Sprintf(`{"provider_id":"%s","start_time":"%s","end_time":"%s","priority":%d}`, rule.ProviderID, rule.StartTime, rule.EndTime, rule.Priority))
	return nil
}

// UpdateRoutingRule updates fields of a routing rule by ID.
func (s *ProviderStore) UpdateRoutingRule(id int64, updates map[string]any) error {
	now := time.Now().Format(time.RFC3339)
	setClauses := []string{}
	args := []any{}

	if v, ok := updates["provider_id"]; ok {
		setClauses = append(setClauses, "provider_id = ?")
		args = append(args, v)
	}
	if v, ok := updates["start_time"]; ok {
		setClauses = append(setClauses, "start_time = ?")
		args = append(args, v)
	}
	if v, ok := updates["end_time"]; ok {
		setClauses = append(setClauses, "end_time = ?")
		args = append(args, v)
	}
	if v, ok := updates["days_of_week"]; ok {
		setClauses = append(setClauses, "days_of_week = ?")
		args = append(args, v)
	}
	if v, ok := updates["priority"]; ok {
		// Accept both int and *int (from the admin request layer).
		switch p := v.(type) {
		case int:
			setClauses = append(setClauses, "priority = ?")
			args = append(args, p)
		case *int:
			if p != nil {
				setClauses = append(setClauses, "priority = ?")
				args = append(args, *p)
			}
		}
	}
	if v, ok := updates["enabled"]; ok {
		if b, ok := v.(bool); ok {
			if b {
				setClauses = append(setClauses, "enabled = 1")
			} else {
				setClauses = append(setClauses, "enabled = 0")
			}
		}
	}

	if len(setClauses) == 0 {
		return fmt.Errorf("no fields to update")
	}

	query := "UPDATE provider_routing_rules SET "
	for i, clause := range setClauses {
		if i > 0 {
			query += ", "
		}
		query += clause
	}
	query += " WHERE id = ?"
	args = append(args, id)

	_, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("update routing rule %d: %w", id, err)
	}

	_ = now // silence unused
	s.WriteAudit("routing_rule.update", "routing_rule", fmt.Sprintf("%d", id), fmt.Sprintf("%v", updates))
	return nil
}

// DeleteRoutingRule deletes a routing rule by ID.
func (s *ProviderStore) DeleteRoutingRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM provider_routing_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete routing rule %d: %w", id, err)
	}

	s.WriteAudit("routing_rule.delete", "routing_rule", fmt.Sprintf("%d", id), "")
	return nil
}

// ─────────────────────────── Audit ───────────────────────────

// WriteAudit writes an audit log entry. Errors are logged but not returned
// (audit failure should not block business operations).
func (s *ProviderStore) WriteAudit(action, targetType, targetID, detail string) {
	now := time.Now().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		action, targetType, targetID, detail, now,
	)
	if err != nil {
		log.Printf("WARNING: failed to write audit log: %v", err)
	}
}

// ListAuditLogs returns paginated audit logs.
func (s *ProviderStore) ListAuditLogs(page, limit int) ([]models.AuditLogRecord, int, error) {
	if page <= 0 {
		page = 1
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := (page - 1) * limit

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit logs: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT id, action, target_type, target_id, detail, created_at
		 FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit logs: %w", err)
	}
	defer rows.Close()

	var logs []models.AuditLogRecord
	for rows.Next() {
		var l models.AuditLogRecord
		if err := rows.Scan(&l.ID, &l.Action, &l.TargetType, &l.TargetID, &l.Detail, &l.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan audit log: %w", err)
		}
		logs = append(logs, l)
	}
	if logs == nil {
		logs = []models.AuditLogRecord{}
	}
	return logs, total, rows.Err()
}

// ─────────────────────── Seed / Snapshot ───────────────────────

// SeedFromConfig seeds the providers and model_mappings tables from config.yaml.
// It is idempotent: if providers table already has rows, it does nothing.
func (s *ProviderStore) SeedFromConfig(cfg *config.Config) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&count); err != nil {
		return fmt.Errorf("seed check: %w", err)
	}
	if count > 0 {
		log.Println("SeedFromConfig: providers table already populated, skipping seed")
		return nil
	}

	log.Println("SeedFromConfig: providers table empty, running seed migration...")

	for _, p := range cfg.Providers {
		apiKey := ""
		if p.APIKeyEnv != "" {
			apiKey = os.Getenv(p.APIKeyEnv)
		}
		if apiKey == "" {
			log.Printf("WARNING: seed provider %q: env var %q is empty or not set, provider key will be blank", p.ID, p.APIKeyEnv)
		}

		encryptedKey, err := security.Encrypt(apiKey, s.kek)
		if err != nil {
			return fmt.Errorf("seed encrypt key for %s: %w", p.ID, err)
		}

		isDef := 0
		if p.IsDefault {
			isDef = 1
		}
		allow := 0
		if p.AllowPassthrough {
			allow = 1
		}
		authHeader := p.AuthHeader
		if authHeader == "" {
			authHeader = "Authorization"
		}
		authScheme := p.AuthScheme
		if authScheme == "" {
			authScheme = "bearer"
		}
		extraJSON, err := json.Marshal(p.ExtraHeaders)
		if err != nil {
			return fmt.Errorf("seed marshal extra_headers for %s: %w", p.ID, err)
		}

		now := time.Now().Format(time.RFC3339)
		if _, err := s.db.Exec(
			`INSERT INTO providers (name, slug, endpoint, encrypted_key, is_default, enabled,
			        allow_passthrough, auth_header, auth_scheme, extra_headers,
			        monthly_token_limit, monthly_call_limit,
			        monthly_token_low_ratio, monthly_call_low_ratio, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 0, 0, 0, 0, ?, ?)`,
			p.ID, p.ID, p.Endpoint, encryptedKey, isDef, allow, authHeader, authScheme, string(extraJSON), now, now,
		); err != nil {
			return fmt.Errorf("seed insert provider %s: %w", p.ID, err)
		}
		log.Printf("SeedFromConfig: inserted provider %q", p.ID)
	}

	// Seed model_mappings from config.
	for _, mm := range cfg.ModelMappings {
		for providerID, realModel := range mm.PerProvider {
			now := time.Now().Format(time.RFC3339)
			if _, err := s.db.Exec(
				`INSERT INTO model_mappings (external, provider_id, real_model, created_at)
				 VALUES (?, ?, ?, ?)`,
				mm.External, providerID, realModel, now,
			); err != nil {
				log.Printf("WARNING: seed mapping external=%s provider=%s: %v", mm.External, providerID, err)
			}
		}
	}
	log.Println("SeedFromConfig: model_mappings seeded")

	return nil
}

// BuildProviderTable loads all enabled providers (decrypting their keys),
// model mappings, and routing rules from the database and builds an atomic
// ProviderTable snapshot for the Router.
func (s *ProviderStore) BuildProviderTable() (*ProviderTable, error) {
	table := &ProviderTable{
		Providers: make(map[string]ProviderEntry),
		Mappings:  make(map[string]map[string]string),
		Rules:     nil,
	}

	// Load enabled providers.
	rows, err := s.db.Query(
		`SELECT slug, endpoint, encrypted_key, is_default,
		        allow_passthrough, auth_header, auth_scheme, extra_headers
		 FROM providers WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("build table: query providers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var slug, endpoint, authHeader, authScheme, extraHeaders string
		var encryptedKey []byte
		var isDef, allowPassthrough int
		if err := rows.Scan(&slug, &endpoint, &encryptedKey, &isDef,
			&allowPassthrough, &authHeader, &authScheme, &extraHeaders); err != nil {
			return nil, fmt.Errorf("build table: scan provider: %w", err)
		}

		apiKey, err := security.Decrypt(encryptedKey, s.kek)
		if err != nil {
			return nil, fmt.Errorf("build table: decrypt key for %s: %w", slug, err)
		}

		table.Providers[slug] = ProviderEntry{
			Slug:             slug,
			Endpoint:         endpoint,
			APIKey:           apiKey,
			AllowPassthrough: allowPassthrough == 1,
			AuthHeader:       authHeader,
			AuthScheme:       authScheme,
			ExtraHeaders:     parseExtraHeaders(extraHeaders),
		}

		if isDef == 1 {
			table.Default = slug
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("build table: rows err: %w", err)
	}

	// If no default is explicitly set, pick the first provider.
	if table.Default == "" && len(table.Providers) > 0 {
		for slug := range table.Providers {
			table.Default = slug
			break
		}
	}

	// Load model mappings.
	mapRows, err := s.db.Query(
		`SELECT external, provider_id, real_model FROM model_mappings ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("build table: query mappings: %w", err)
	}
	defer mapRows.Close()

	for mapRows.Next() {
		var external, providerID, realModel string
		if err := mapRows.Scan(&external, &providerID, &realModel); err != nil {
			return nil, fmt.Errorf("build table: scan mapping: %w", err)
		}
		if table.Mappings[external] == nil {
			table.Mappings[external] = make(map[string]string)
		}
		table.Mappings[external][providerID] = realModel
	}
	if err := mapRows.Err(); err != nil {
		return nil, fmt.Errorf("build table: mappings rows err: %w", err)
	}

	// Load enabled routing rules.
	rules, err := s.ListRoutingRules()
	if err != nil {
		return nil, fmt.Errorf("build table: list rules: %w", err)
	}
	table.Rules = rules

	return table, nil
}

// DecryptKey returns the decrypted API key for a given provider slug.
// Used by admin to display the key (if needed — normally only masked).
func (s *ProviderStore) DecryptKey(slug string) (string, error) {
	var encryptedKey []byte
	err := s.db.QueryRow(
		`SELECT encrypted_key FROM providers WHERE slug = ?`, slug,
	).Scan(&encryptedKey)
	if err != nil {
		return "", fmt.Errorf("get encrypted key for %s: %w", slug, err)
	}
	return security.Decrypt(encryptedKey, s.kek)
}

// MaskProviderKey decrypts and masks the key for a provider.
func (s *ProviderStore) MaskProviderKey(slug string) (string, error) {
	plaintext, err := s.DecryptKey(slug)
	if err != nil {
		return "", err
	}
	return security.MaskKey(plaintext), nil
}

// parseExtraHeaders decodes the extra_headers JSON column into a map.
// A malformed or empty value falls back to an empty map (defensive: a
// corrupt JSON must never break the passthrough path).
func parseExtraHeaders(s string) map[string]string {
	if s == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		log.Printf("WARNING: parse extra_headers %q: %v", s, err)
		return map[string]string{}
	}
	return out
}

// BuildMaskedProviders returns all providers with masked keys for the admin list API.
func (s *ProviderStore) BuildMaskedProviders() ([]models.ProviderWithMaskedKey, error) {
	providers, err := s.ListProviders()
	if err != nil {
		return nil, err
	}

	result := make([]models.ProviderWithMaskedKey, 0, len(providers))
	for _, p := range providers {
		masked := "****"
		if len(p.EncryptedKey) > 0 {
			plaintext, decErr := security.Decrypt(p.EncryptedKey, s.kek)
			if decErr == nil {
				masked = security.MaskKey(plaintext)
			} else {
				// If decryption fails, log but still show masked.
				log.Printf("WARNING: failed to decrypt key for provider %s: %v", p.Slug, decErr)
				masked = "****"
			}
		}

		result = append(result, models.ProviderWithMaskedKey{
			ID:                   p.ID,
			Name:                 p.Name,
			Slug:                 p.Slug,
			Endpoint:             p.Endpoint,
			MaskedKey:            masked,
			IsDefault:            p.IsDefault,
			Enabled:              p.Enabled,
			AllowPassthrough:     p.AllowPassthrough,
			AuthHeader:           p.AuthHeader,
			AuthScheme:           p.AuthScheme,
			ExtraHeaders:         p.ExtraHeaders,
			MonthlyTokenLimit:    p.MonthlyTokenLimit,
			MonthlyCallLimit:     p.MonthlyCallLimit,
			MonthlyTokenLowRatio: p.MonthlyTokenLowRatio,
			MonthlyCallLowRatio:  p.MonthlyCallLowRatio,
			CreatedAt:            p.CreatedAt,
			UpdatedAt:            p.UpdatedAt,
		})
	}

	return result, nil
}

// DumpConfig serializes the current provider table as JSON for debugging.
func (s *ProviderStore) DumpConfig() string {
	table, err := s.BuildProviderTable()
	if err != nil {
		return fmt.Sprintf(`{"error":"%s"}`, err.Error())
	}

	// Build a safe version without sensitive keys.
	type safeEntry struct {
		Slug     string `json:"slug"`
		Endpoint string `json:"endpoint"`
		HasKey   bool   `json:"has_key"`
	}
	providers := make([]safeEntry, 0, len(table.Providers))
	for _, e := range table.Providers {
		providers = append(providers, safeEntry{
			Slug:     e.Slug,
			Endpoint: e.Endpoint,
			HasKey:   e.APIKey != "",
		})
	}

	out := struct {
		Providers []safeEntry                  `json:"providers"`
		Mappings  map[string]map[string]string `json:"mappings"`
		Rules     []models.RoutingRule         `json:"rules"`
		Default   string                       `json:"default"`
	}{
		Providers: providers,
		Mappings:  table.Mappings,
		Rules:     table.Rules,
		Default:   table.Default,
	}

	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}
