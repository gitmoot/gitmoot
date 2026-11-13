package cli

import (
	"context"

	dashboard "github.com/gitmoot/gitmoot-dashboard"
)

// Skills satisfies the pinned dashboard module's DataSource contract for the
// Learning page. Its data source, the agent_templates / agent_template_versions
// tables, was dropped with agent templates in #2589, so there is nothing to list;
// the method goes away when the module drops the Learning page (#2593).
func (d *webDataSource) Skills(context.Context) (dashboard.Skills, error) {
	return dashboard.Skills{Templates: []dashboard.SkillTemplate{}}, nil
}
