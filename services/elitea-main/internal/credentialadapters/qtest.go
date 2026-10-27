package credentialadapters

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// QTest: static_secret only (qTest has no delegated OAuth). Its API tokens
// are long lived, so the class needs a rotation policy before it can be
// qualified (gate D5). The target is the site base URL.
func QTest() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       "qtest",
		AuthClasses:        []credentialgrant.CredentialClass{staticSecret},
		TargetKind:         TargetBaseURL,
		CanonicalizeTarget: CanonicalBaseURL,
		Operations: []OperationSpec{
			op("qtest.project.read", OperationRead, "project", 1),
			op("qtest.test_case.read", OperationRead, "test_case", 1),
			op("qtest.test_case.search", OperationRead, "project", 1),
			op("qtest.test_case.write", OperationWrite, "test_case", 1),
		},
		Probe: probe("qtest.project.read"),
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			staticSecret: {"token"},
		},
		Qualification: map[credentialgrant.CredentialClass]string{
			staticSecret: QualificationFixture,
		},
		RotationRequired: []credentialgrant.CredentialClass{staticSecret},
	}
}
