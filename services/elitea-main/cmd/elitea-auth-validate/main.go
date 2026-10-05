package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	exitValid         = 0
	exitInvalid       = 1
	exitInvalidUsage  = 2
	formUsersFlag     = "form-users-file"
	invalidUsageText  = "invalid arguments\n"
	invalidConfigText = "Form configuration validation failed\n"
	// missingEmailWarningFormat is a warning, not a failure: the exit status
	// stays valid.
	missingEmailWarningFormat = "warning: %d Form user(s) have no usable email address " +
		"(top-level \"email\" or attributes.email, outside the reserved @centry.user domain); " +
		"their sign-in will be refused\n"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run intentionally has no stdout writer. A valid secret-bearing snapshot is
// reported only through exit status; failures never include a path, parser
// detail, login, password, or provider attribute.
func run(arguments []string, stderr io.Writer) int {
	if countFlag(arguments, formUsersFlag) != 1 {
		_, _ = io.WriteString(stderr, invalidUsageText)
		return exitInvalidUsage
	}

	flags := flag.NewFlagSet("elitea-auth-validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String(formUsersFlag, "", "absolute path to the resolved Form users snapshot")
	if err := flags.Parse(arguments); err != nil || *input == "" || flags.NArg() != 0 {
		_, _ = io.WriteString(stderr, invalidUsageText)
		return exitInvalidUsage
	}

	provider, valid := resolvedFormProvider(*input)
	if !valid {
		_, _ = io.WriteString(stderr, invalidConfigText)
		return exitInvalid
	}
	// A user with no usable address loads (one bad entry must not lock every
	// other user out) but is refused at sign-in. Say so here, where the
	// operator is looking, without naming a login: this tool's output never
	// carries any configured value.
	if missing := len(provider.MisconfiguredLogins()); missing > 0 {
		_, _ = fmt.Fprintf(stderr, missingEmailWarningFormat, missing)
	}
	return exitValid
}

func countFlag(arguments []string, name string) int {
	count := 0
	short := "-" + name
	long := "--" + name
	for _, argument := range arguments {
		if argument == short || argument == long || strings.HasPrefix(argument, short+"=") ||
			strings.HasPrefix(argument, long+"=") {
			count++
		}
	}
	return count
}

func resolvedFormProvider(path string) (*browserapp.FormProvider, bool) {
	raw, err := securefile.Read(
		path,
		browserapp.MaxFormConfigurationBytes,
		securefile.PrivateMaterial,
	)
	if err != nil {
		return nil, false
	}
	defer clear(raw)

	provider, err := browserapp.NewFormProvider(raw)
	return provider, err == nil
}
