package configurations

// SetSharepointEndpointsForTest points the SharePoint probe's two Microsoft
// hosts at test servers and returns the function that restores them.
func SetSharepointEndpointsForTest(login, graph string) func() {
	previousLogin, previousGraph := entraLoginBase, graphBase
	entraLoginBase, graphBase = login, graph
	return func() { entraLoginBase, graphBase = previousLogin, previousGraph }
}
