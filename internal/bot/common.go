package bot

// IsAllowed checks whether userID is in the allowlist.
func IsAllowed(allowlist map[string]struct{}, userID string) bool {
	_, ok := allowlist[userID]
	return ok
}
