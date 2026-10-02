// Package chatauthority owns actor visibility for shared chat reads.
// Project permission checks remain at the API boundary.
package chatauthority

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Scope struct {
	ActorID int64
	Admin   bool
	Support bool
	Public  bool
}

type ReadKind uint8

const (
	Detail ReadKind = iota
	Listing
	FolderListing
)

func Actor(ctx context.Context) (int64, error) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, apierr.Forbidden("chat requires an authenticated user")
	}
	id, ok := user.OwningUserID()
	if !ok {
		return 0, apierr.Forbidden("chat requires an authenticated user")
	}
	return id, nil
}

// Load derives identity and privileges from trusted context and stored roles.
// An absent optional configuration table grants no support-project exception.
// An absent role projection grants no administrator privilege.
func Load(ctx context.Context, pool *pgxpool.Pool, projectID string) (Scope, error) {
	id, err := Actor(ctx)
	if err != nil {
		return Scope{}, err
	}
	if _, err := tenantschema.Quote(projectID); err != nil {
		return Scope{}, err
	}
	if pool == nil {
		return Scope{}, fmt.Errorf("chat authority storage is unavailable")
	}
	scope := Scope{ActorID: id, Public: publicproject.IDString() == projectID}
	var assignments, roles, config bool
	err = pool.QueryRow(ctx, `SELECT
	 to_regclass('public.auth_core__project_user_role') IS NOT NULL,
	 to_regclass('public.auth_core__project_role') IS NOT NULL,
	 to_regclass('centry.platform_config') IS NOT NULL`).Scan(&assignments, &roles, &config)
	if err != nil {
		return Scope{}, fmt.Errorf("chat authority projection: %w", err)
	}
	if assignments != roles {
		return Scope{}, fmt.Errorf("chat role projection is incomplete")
	}
	if assignments {
		err = pool.QueryRow(ctx, `SELECT EXISTS (
		 SELECT 1 FROM public.auth_core__project_user_role assignment
		 JOIN public.auth_core__project_role role ON role.id=assignment.role_id AND role.project_id=assignment.project_id
		 WHERE assignment.project_id=$1 AND assignment.user_id=$2
		 AND lower(role.name) IN ('admin','super_admin'))`, projectID, id).Scan(&scope.Admin)
		if err != nil {
			return Scope{}, fmt.Errorf("chat role lookup: %w", err)
		}
	}
	if config {
		values, err := platformconfig.Load(ctx, pool, platformconfig.SectionSupportAssistant)
		if err != nil {
			return Scope{}, fmt.Errorf("chat support configuration: %w", err)
		}
		configured, _ := values.Int(platformconfig.KeySupportProjectID)
		scope.Support = strconv.FormatInt(configured, 10) == projectID
	}
	return scope, nil
}

// Predicate uses a trusted schema and alias. The returned argument binds actor identity.
// Private visibility follows user participation, not the conversation author column.
func (s Scope) Predicate(schema, alias string, parameter int, kind ReadKind) (string, []any) {
	if s.Admin && s.Support && kind == Detail {
		return fmt.Sprintf(`(%s.source='support' AND $%d::text<>'')`, alias, parameter), []any{strconv.FormatInt(s.ActorID, 10)}
	}
	member := fmt.Sprintf(`EXISTS (SELECT 1 FROM %[1]s.chat_participant_mapping access_mapping
	 JOIN %[1]s.chat_participants access_participant ON access_participant.id=access_mapping.participant_id
	 WHERE access_mapping.conversation_id=%[2]s.id AND access_participant.entity_name='user'
	 AND (access_participant.entity_meta->>'id'=$%[3]d::text`, schema, alias, parameter)
	if s.Admin && kind == Listing {
		member += ` OR TRUE`
	}
	if s.Admin && kind == Detail {
		member += fmt.Sprintf(` OR (jsonb_typeof(%[1]s.meta->'single_participant')='object' AND %[1]s.meta->'single_participant'<>'{}'::jsonb)`, alias)
	}
	member += `))`
	if kind == FolderListing && s.Support {
		if s.Admin {
			return fmt.Sprintf(`(%s.source='support' AND $%d::text<>'')`, alias, parameter), []any{strconv.FormatInt(s.ActorID, 10)}
		}
		return fmt.Sprintf(`(%s.source='support' AND %s)`, alias, member), []any{strconv.FormatInt(s.ActorID, 10)}
	}
	return fmt.Sprintf(`(%s.is_private=false OR %s)`, alias, member), []any{strconv.FormatInt(s.ActorID, 10)}
}
