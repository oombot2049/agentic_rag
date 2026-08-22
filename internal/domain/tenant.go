package domain

import "time"

// Tenant is the root isolation owner for every business aggregate.
type Tenant struct {
	id        TenantID
	createdAt time.Time
}

func NewTenant(id TenantID, createdAt time.Time) (*Tenant, error) {
	if err := validateTenantID(id); err != nil {
		return nil, err
	}
	normalized, err := normalizeTime("created_at", createdAt)
	if err != nil {
		return nil, err
	}
	return &Tenant{id: id, createdAt: normalized}, nil
}

func (t *Tenant) ID() TenantID         { return t.id }
func (t *Tenant) CreatedAt() time.Time { return t.createdAt }
