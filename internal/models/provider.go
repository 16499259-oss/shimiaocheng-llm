package models

// ProviderRecord is a row from the providers table.
// EncryptedKey is excluded from JSON serialization (json:"-").
type ProviderRecord struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Endpoint     string `json:"endpoint"`
	EncryptedKey []byte `json:"-"` // AES-256-GCM ciphertext (never serialized to JSON)
	IsDefault    bool   `json:"is_default"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// ModelMappingRecord is a row from the model_mappings table.
// (external, provider_id) is a unique composite key.
type ModelMappingRecord struct {
	ID         int64  `json:"id"`
	External   string `json:"external"`
	ProviderID string `json:"provider_id"`
	RealModel  string `json:"real_model"`
	CreatedAt  string `json:"created_at"`
}

// AuditLogRecord is a row from the audit_logs table.
type AuditLogRecord struct {
	ID         int64  `json:"id"`
	Action     string `json:"action"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
}

// ProviderWithMaskedKey is a ProviderRecord with the plaintext key masked for
// frontend display. Used in list API responses.
type ProviderWithMaskedKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Endpoint  string `json:"endpoint"`
	MaskedKey string `json:"masked_key"`
	IsDefault bool   `json:"is_default"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// RoutingRule mirrors a row in the provider_routing_rules table.
// Defined here (not in router package) to avoid circular imports between
// the router and provider packages.
type RoutingRule struct {
	ID                int64  `json:"id"`
	ProviderID        string `json:"provider_id"`
	StartTime         string `json:"start_time"`   // "HH:MM" (Asia/Shanghai)
	EndTime           string `json:"end_time"`     // "HH:MM" (Asia/Shanghai)
	DaysOfWeek        string `json:"days_of_week"` // "*" or comma list of weekday nums
	Timezone          string `json:"timezone"`
	Enabled           bool   `json:"enabled"`
	Priority          int    `json:"priority"`                      // higher = evaluated first on overlap; tie-break by narrower window then id
	DefaultProviderID string `json:"default_provider_id,omitempty"` // schema-reserved
}
