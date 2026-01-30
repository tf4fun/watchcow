// Package fpkgen provides fnOS package generation functionality.
// Types are now defined in the app package for unified usage.
package fpkgen

import "watchcow/internal/app"

// Type aliases for backward compatibility and convenience within fpkgen package.
// All types are defined in the app package.
type (
	App           = app.App
	AppConfig     = app.App // Deprecated: use App instead
	Entry         = app.Entry
	EntryControl  = app.EntryControl
	VolumeMapping = app.VolumeMapping
)
