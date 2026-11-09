package agentteams

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

func members(n int) []Member {
	out := make([]Member, n)
	for i := range out {
		out[i] = Member{ApplicationID: int64(i + 1), ProjectID: 1, VersionID: int64(10 + i), Name: " Agent "}
	}
	return out
}

func TestNormalizeAcceptsAValidTeam(t *testing.T) {
	team := Team{Name: "  Release crew ", Members: members(2)}
	if err := team.Normalize(); err != nil {
		t.Fatal(err)
	}
	if team.Name != "Release crew" || team.Policy != turntaking.PolicyAddressed || team.Members[0].Name != "Agent" {
		t.Fatalf("normalized = %+v", team)
	}
	lead := Team{Name: "Lead crew", Policy: turntaking.PolicyLead, Lead: 2, Members: members(2)}
	if err := lead.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeRefusesEveryBrokenRule(t *testing.T) {
	cases := map[string]Team{
		"no name":             {Name: " ", Members: members(1)},
		"long name":           {Name: strings.Repeat("a", MaxName+1), Members: members(1)},
		"long description":    {Name: "x", Description: strings.Repeat("d", MaxDesc+1), Members: members(1)},
		"no members":          {Name: "x"},
		"too many":            {Name: "x", Members: members(MaxMembers + 1)},
		"zero id":             {Name: "x", Members: []Member{{ApplicationID: 0, ProjectID: 1, VersionID: 1}}},
		"twice":               {Name: "x", Members: []Member{{ApplicationID: 1, ProjectID: 1, VersionID: 1}, {ApplicationID: 1, ProjectID: 1, VersionID: 2}}},
		"lead without policy": {Name: "x", Lead: 1, Members: members(1)},
		"lead out of range":   {Name: "x", Policy: turntaking.PolicyLead, Lead: 3, Members: members(2)},
		"lead missing":        {Name: "x", Policy: turntaking.PolicyLead, Members: members(2)},
		"unknown policy":      {Name: "x", Policy: "router", Members: members(1)},
	}
	for name, team := range cases {
		t.Run(name, func(t *testing.T) {
			if err := team.Normalize(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestTheSameAgentFromTwoProjectsIsTwoMembers(t *testing.T) {
	team := Team{Name: "x", Members: []Member{{ApplicationID: 1, ProjectID: 1, VersionID: 1}, {ApplicationID: 1, ProjectID: 2, VersionID: 1}}}
	if err := team.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestMemberUnavailableUnwraps(t *testing.T) {
	err := error(&MemberUnavailable{Position: 2})
	if !errors.Is(err, ErrMemberUnavailable) || !strings.Contains(err.Error(), "agent 2") {
		t.Fatalf("err = %v", err)
	}
}
