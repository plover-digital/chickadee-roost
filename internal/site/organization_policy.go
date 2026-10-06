package site

const organizationLimitMessage = "This beta supports one selected repository and one account managing runners per organization. An existing organization pool needs operator reconciliation before changing its repository or manager."

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
	if current != nil && (current.User.ID != user.ID || current.InstallationID != choice.Installation.ID || current.Repository.ID != choice.Repository.ID) {
		return organizationLimitMessage
	}
	return ""
}
