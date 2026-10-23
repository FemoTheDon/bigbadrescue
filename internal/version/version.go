// Package version holds the app's name and version. Breadbaskets versions
// are numbered v1.0, v1.01, v1.02 and so on.
package version

const (
	App     = "BigBadRescue"
	Company = "Breadbaskets"
	Version = "v1.01"
)

// Title is the full name shown in the window and the README.
func Title() string { return App + " by " + Company + " " + Version }
