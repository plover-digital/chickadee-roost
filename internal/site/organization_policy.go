package site

const organizationOwnerSetupMessage = "Use organization setup with verified owner access to configure this organization."

const organizationLimitMessage = "Legacy setup supports one selected repository and one account managing runners. Use verified organization owner setup to update repository access; installation changes need operator reconciliation."

func organizationRank(e Enrollment) int {
	switch e.Status {
	case "active":
		return 4
	case "paused":
		return 3
	case "disconnected":
		return 2
	case "approved", "pending":
		return 1
	default:
		return 0
	}
}

func canonicalOrganization(entries []Enrollment, accountID int64) *Enrollment {
	var selected *Enrollment
	for _, entry := range entries {
		if entry.Account.Type != "Organization" || entry.Account.ID != accountID {
			continue
		}
		if selected == nil || organizationRank(entry) > organizationRank(*selected) || organizationRank(entry) == organizationRank(*selected) && entry.Created.Before(selected.Created) {
			copy := entry
			selected = &copy
		}
	}
	return selected
}

// organizationConstraint discloses no other tenant's identity or repository.
// An established scope cannot silently be replaced by another signup request.
func organizationConstraint(entries []Enrollment, user User, choice Choice) string {
	if choice.Installation.Account.Type != "Organization" {
		return ""
	}
	current := canonicalOrganization(entries, choice.Installation.Account.ID)
	if current != nil && choice.OrganizationAdmin && current.InstallationID == choice.Installation.ID {
		return ""
	}
	if current != nil && (current.User.ID != user.ID || current.InstallationID != choice.Installation.ID || current.Repository.ID != choice.Repository.ID) {
		return organizationLimitMessage
	}
	return ""
}

// Versionless forms can maintain only the exact established legacy record.
// They never create a new organization enrollment or gain an owner's broader
// exemption from the legacy one-repository constraint.
func organizationRequestConstraint(entries []Enrollment, user User, choice Choice, access *OrganizationAccess) string {
	if choice.Installation.Account.Type == "Organization" && access == nil {
		current := canonicalOrganization(entries, choice.Installation.Account.ID)
		if current == nil || current.OrganizationAccess != nil {
			return organizationOwnerSetupMessage
		}
		choice.OrganizationAdmin = false
	}
	return organizationConstraint(entries, user, choice)
}

// Every record for the same installation/account describes one organization
// scope. Consolidate failed legacy leftovers only after an explicit owner edit.
func replaceOrganizationEnrollment(entries []Enrollment, updated Enrollment) []Enrollment {
	out := make([]Enrollment, 0, len(entries))
	for _, e := range entries {
		if e.ID == updated.ID {
			out = append(out, updated)
		} else if e.InstallationID != updated.InstallationID || e.Account.ID != updated.Account.ID || e.Account.Type != "Organization" {
			out = append(out, e)
		}
	}
	return out
}
