package platform

import "runtime"

// IsWindows reports whether the current operating system is Windows.
// This is used throughout the codebase for platform-specific file operations.
var IsWindows = runtime.GOOS == "windows"
