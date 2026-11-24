package configurations

// SetSharepointEndpointsForTest points the SharePoint probe's two Microsoft
// hosts at test servers and returns the function that restores them.
func SetSharepointEndpointsForTest(login, graph string) func() {
	previousLogin, previousGraph := entraLoginBase, graphBase
	entraLoginBase, graphBase = login, graph
	return func() { entraLoginBase, graphBase = previousLogin, previousGraph }
}

// SetOpenAPITokenSchemeForTest lets the OpenAPI client-credentials probe reach
// an httptest token endpoint and returns the function that restores the rule.
func SetOpenAPITokenSchemeForTest(scheme string) func() {
	previous := openAPITokenScheme
	openAPITokenScheme = scheme
	return func() { openAPITokenScheme = previous }
}

// GuardedProjectSettingTypes returns the five guarded settings of the project
// settings guard, for the test that compares them with the central allowlist.
func GuardedProjectSettingTypes() []string {
	return append([]string(nil), guardedProjectSettingTypes...)
}
