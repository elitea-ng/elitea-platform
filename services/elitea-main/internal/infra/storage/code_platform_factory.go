package storage

import (
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/codeplatform"
	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

// CodePlatformNativeFactory binds existing product owners to one current claim.
// It cannot create an actor/project scope or bypass native permissions.
type CodePlatformNativeFactory struct {
	journal      app.Journal
	content      *CodePlatformContent
	signer       app.ReplySigner
	authorizer   AgentRuntimeContextAuthorizer
	permissions  auth.PermissionResolver
	secrets      *RuntimeCodeSecretService
	applications CodeApplicationCatalog
	toolkits     CodeToolkitCatalog
	runs         *toolkit.RunService
	children     app.ToolkitChildren
}

func NewCodePlatformNativeFactory(journal app.Journal, content *CodePlatformContent, signer app.ReplySigner, authorizer AgentRuntimeContextAuthorizer, permissions auth.PermissionResolver, secrets *RuntimeCodeSecretService, applications CodeApplicationCatalog, toolkits CodeToolkitCatalog, runs *toolkit.RunService, children app.ToolkitChildren) (*CodePlatformNativeFactory, error) {
	if journal == nil || content == nil || signer == nil || authorizer == nil || permissions == nil || secrets == nil || applications == nil || toolkits == nil || runs == nil || children == nil {
		return nil, domain.ErrUnavailable
	}
	return &CodePlatformNativeFactory{journal: journal, content: content, signer: signer, authorizer: authorizer, permissions: permissions, secrets: secrets, applications: applications, toolkits: toolkits, runs: runs, children: children}, nil
}
func (f *CodePlatformNativeFactory) ForCodePlatformCall(claim ContentClaim, a domain.Admission) (CodePlatformCallService, error) {
	if f == nil {
		return nil, domain.ErrUnavailable
	}
	reader, err := NewCodeNativeReader(claim, a, f.authorizer, f.permissions, f.secrets, f.applications, f.toolkits, f.runs)
	if err != nil {
		return nil, err
	}
	operations, err := app.NewNativeOperations(f.runs, f.children, reader, reader)
	if err != nil {
		return nil, err
	}
	return app.NewService(f.journal, f.content, operations, f.signer)
}

var _ CodePlatformCallFactory = (*CodePlatformNativeFactory)(nil)
