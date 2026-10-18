// Package auth implements login, JWT signing/parsing, bcrypt password
// hashing, and the UserStore used by the admin dashboard. Per design.md
// §18.1, two roles are recognised: admin and viewer. Higher tiers belong to
// P2 and are intentionally rejected at validation time.
package auth

import "errors"

// Role is a typed permission band. Stored on users and attached to JWT
// claims so middleware can authorise requests without re-reading the DB.
type Role string

// Sentinel error returned by ParseRole for unknown values. Callers use
// errors.Is(err, ErrUnknownRole) to surface a 422 from admin/user handlers.
var ErrUnknownRole = errors.New("auth: unknown role")

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// ParseRole returns the typed Role for the supplied string. Unknown values
// produce ErrUnknownRole so handler layers can reject misconfigured input
// before touching the store.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleAdmin, RoleViewer:
		return Role(s), nil
	}
	return "", ErrUnknownRole
}

// String renders the role using its underlying string form.
func (r Role) String() string { return string(r) }

// IsValid reports whether r is one of the recognised roles.
func (r Role) IsValid() bool {
	switch r {
	case RoleAdmin, RoleViewer:
		return true
	}
	return false
}

// CanManageUsers reports whether the role may mutate /admin/users.
func (r Role) CanManageUsers() bool { return r == RoleAdmin }

// CanMutateTasks reports whether the role may create/update/delete tasks.
func (r Role) CanMutateTasks() bool { return r == RoleAdmin }