package app

import "os"

// openFileImpl is split out so the main test file can avoid the os
// import in its declared interface — keeps the rest of the test file
// focused on bubbletea wiring rather than filesystem plumbing.
func openFileImpl(path string) (*os.File, error) { return os.Create(path) }
