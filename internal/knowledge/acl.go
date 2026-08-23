package knowledge

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type PrincipalKind string

const (
	PrincipalUser  PrincipalKind = "USER"
	PrincipalGroup PrincipalKind = "GROUP"
	PrincipalRole  PrincipalKind = "ROLE"
)

type Permission uint8

const (
	PermissionRead  Permission = 1
	PermissionWrite Permission = 2
	PermissionAdmin Permission = 3
)

type Grant struct {
	Kind        PrincipalKind
	PrincipalID string
	Permission  Permission
}

// ACL is a canonical immutable policy snapshot. Tenant is always evaluated
// before grants, making a matching principal from another tenant insufficient.
type ACL struct {
	tenantID domain.TenantID
	revision uint64
	grants   []Grant
}

func NewACL(tenantID domain.TenantID, revision uint64, grants []Grant) (ACL, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return ACL{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if revision == 0 || len(grants) == 0 || len(grants) > 10_000 {
		return ACL{}, fmt.Errorf("%w: ACL revision/grant count", ErrInvalid)
	}
	owned := append([]Grant(nil), grants...)
	for _, grant := range owned {
		if !validKind(grant.Kind) || !validPrincipal(grant.PrincipalID) || grant.Permission < PermissionRead || grant.Permission > PermissionAdmin {
			return ACL{}, fmt.Errorf("%w: ACL grant", ErrInvalid)
		}
	}
	slices.SortFunc(owned, func(left, right Grant) int {
		if value := strings.Compare(string(left.Kind), string(right.Kind)); value != 0 {
			return value
		}
		return strings.Compare(left.PrincipalID, right.PrincipalID)
	})
	for index := 1; index < len(owned); index++ {
		if owned[index-1].Kind == owned[index].Kind && owned[index-1].PrincipalID == owned[index].PrincipalID {
			return ACL{}, fmt.Errorf("%w: duplicate ACL principal", ErrInvalid)
		}
	}
	return ACL{tenantID: tenantID, revision: revision, grants: owned}, nil
}
func (a ACL) TenantID() domain.TenantID { return a.tenantID }
func (a ACL) Revision() uint64          { return a.revision }
func (a ACL) Grants() []Grant           { return append([]Grant(nil), a.grants...) }
func (a ACL) Allows(tenantID domain.TenantID, kind PrincipalKind, principalID string, required Permission) bool {
	if tenantID != a.tenantID || !validKind(kind) || !validPrincipal(principalID) || required < PermissionRead || required > PermissionAdmin {
		return false
	}
	index, found := slices.BinarySearchFunc(a.grants, Grant{Kind: kind, PrincipalID: principalID}, func(left, right Grant) int {
		if value := strings.Compare(string(left.Kind), string(right.Kind)); value != 0 {
			return value
		}
		return strings.Compare(left.PrincipalID, right.PrincipalID)
	})
	return found && a.grants[index].Permission >= required
}
func validKind(value PrincipalKind) bool {
	return value == PrincipalUser || value == PrincipalGroup || value == PrincipalRole
}
func validPrincipal(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}
