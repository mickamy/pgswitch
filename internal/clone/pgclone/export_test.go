package pgclone

// WithStagingNames makes a build its copies under the names next returns.
func WithStagingNames(a Admin, next func() string) Admin {
	a.newStagingName = next

	return a
}
