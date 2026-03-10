package bot

// IsAllowed checks whether userID is in the allowlist. If list is empty, allow all.
func IsAllowed(allowlist map[string]struct{}, userID string) bool {
	if len(allowlist) == 0 {
		return true
	}
	_, ok := allowlist[userID]
	return ok
}
