package site

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// OrganizationAccess is explicit desired/applied policy. Its absence preserves
// legacy repository enrollment; a missing field never broadens an existing ACL.
type OrganizationAccess struct {
	RepositorySelection string   `json:"repository_selection"`
	RepositoryIDs       []int64  `json:"repository_ids,omitempty"`
	Workflows           []string `json:"workflows,omitempty"`
}

var workflowReference = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}\.ya?ml@refs/(heads|tags)/[A-Za-z0-9_./-]+$`)

func validOrganizationAccess(a *OrganizationAccess) bool {
	if a == nil {
		return true
	}
	if a.RepositorySelection != "all" && a.RepositorySelection != "selected" || len(a.RepositoryIDs) > 100 || len(a.Workflows) > 50 {
		return false
	}
	if a.RepositorySelection == "all" && len(a.RepositoryIDs) != 0 || a.RepositorySelection == "selected" && len(a.RepositoryIDs) == 0 {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range a.RepositoryIDs {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	refs := map[string]bool{}
	for _, ref := range a.Workflows {
		if len(ref) > 300 || !workflowReference.MatchString(ref) || refs[ref] {
			return false
		}
		refs[ref] = true
	}
	return true
}
func (s *Server) organizationAdmin(ctx context.Context, v session, account Account) bool {
	var membership struct {
		State, Role  string
		Organization Account
		User         User
	}
	if !regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`).MatchString(account.Login) {
		return false
	}
	err := s.fetch(ctx, "/user/memberships/orgs/"+account.Login, v.Token, &membership)
	return err == nil && membership.State == "active" && membership.Role == "admin" && membership.Organization.ID == account.ID && membership.User.ID == v.User.ID
}
func (c Choice) authorizes(e Enrollment) bool {
	return c.Installation.ID == e.InstallationID && (e.OrganizationAccess == nil && c.Repository.ID == e.Repository.ID || e.OrganizationAccess != nil && c.Installation.Account.ID == e.Account.ID && c.OrganizationAdmin)
}
func requestedOrganizationAccess(form url.Values, c Choice) (*OrganizationAccess, error) {
	if form.Get("organization_access_version") == "" {
		return nil, nil
	}
	if form.Get("organization_access_version") != "1" || c.Installation.Account.Type != "Organization" || !c.OrganizationAdmin {
		return nil, errors.New("Verified organization owner access and App Members read permission are required")
	}
	a := &OrganizationAccess{RepositorySelection: form.Get("repository_access")}
	allowed := map[int64]bool{}
	names := map[string]int64{}
	for _, r := range c.RepositoryOptions {
		allowed[r.ID] = true
		names[r.Name] = r.ID
	}
	for _, raw := range form["selected_repository"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || !allowed[id] {
			return nil, errors.New("Repository selection is outside this App installation")
		}
		a.RepositoryIDs = append(a.RepositoryIDs, id)
	}
	if a.RepositorySelection == "all" {
		a.RepositoryIDs = nil
	}
	for _, line := range strings.Split(form.Get("allowed_workflows"), "\n") {
		if ref := strings.TrimSpace(line); ref != "" {
			a.Workflows = append(a.Workflows, ref)
		}
	}
	if !validOrganizationAccess(a) {
		return nil, errors.New("Choose all repositories or at least one selected repository, and valid workflow references")
	}
	for _, ref := range a.Workflows {
		parts := strings.Split(ref, "/")
		id := names[strings.Join(parts[:2], "/")]
		if id == 0 {
			return nil, errors.New("Workflow repository is outside this App installation")
		}
		if a.RepositorySelection == "selected" {
			found := false
			for _, selected := range a.RepositoryIDs {
				if selected == id {
					found = true
				}
			}
			if !found {
				return nil, errors.New("Workflow repository must be selected")
			}
		}
	}
	return a, nil
}
func (c Choice) AccessSelection() string {
	if c.OrganizationAccess != nil {
		return c.OrganizationAccess.RepositorySelection
	}
	if len(c.SelectedQueues) > 0 {
		return "selected"
	}
	return "all"
}
func (c Choice) RepositorySelected(id int64) bool {
	if c.OrganizationAccess == nil {
		if c.LegacyRepository.ID > 0 {
			return c.LegacyRepository.ID == id
		}
		return c.Repository.ID == id
	}
	for _, selected := range c.OrganizationAccess.RepositoryIDs {
		if id == selected {
			return true
		}
	}
	return false
}
func (c Choice) WorkflowReferences() string {
	if c.OrganizationAccess != nil {
		return strings.Join(c.OrganizationAccess.Workflows, "\n")
	}
	if c.WorkflowPath != "" && c.EnabledWorkflowAccess != "repository" {
		repo := c.Repository
		if c.LegacyRepository.ID > 0 {
			repo = c.LegacyRepository
		}
		return fmt.Sprintf("%s/%s@refs/heads/main", repo.Name, c.WorkflowPath)
	}
	return ""
}
func (a OrganizationAccess) Description() string {
	if a.RepositorySelection == "all" {
		return "All current and future organization repositories"
	}
	return fmt.Sprintf("%d selected repositories", len(a.RepositoryIDs))
}

func organizationOwner(choices []Choice, e Enrollment) bool {
	for _, c := range choices {
		if c.OrganizationAdmin && c.Installation.ID == e.InstallationID && c.Installation.Account.ID == e.Account.ID {
			return true
		}
	}
	return false
}
