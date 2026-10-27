package credentialgrant

// The five governed credential permissions (ADR-0039 decision 8). They are
// resolved in default mode against the tenant project. No role holds them by
// default: an operator grants them from the Roles matrix, which lists them
// because the admin catalogue declares Permissions().
const (
	PermissionUse     = "models.credentials.governed.use"
	PermissionReveal  = "models.credentials.governed.reveal"
	PermissionManage  = "models.credentials.governed.manage"
	PermissionProbe   = "models.credentials.governed.probe"
	PermissionApprove = "models.credentials.governed.approve"
)

// Permissions returns the five permission names, in capability order.
func Permissions() []string {
	return []string{PermissionUse, PermissionReveal, PermissionManage, PermissionProbe, PermissionApprove}
}

// Permission is the permission that capability c requires, or "" for an
// unknown capability.
func Permission(c Capability) string {
	switch c {
	case CapUse:
		return PermissionUse
	case CapReveal:
		return PermissionReveal
	case CapManage:
		return PermissionManage
	case CapProbe:
		return PermissionProbe
	case CapApprove:
		return PermissionApprove
	}
	return ""
}
