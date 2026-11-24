package service

import "os"

// PrepareStateDir makes the folder the module keeps its tokens in, for the service alone.
// MkdirAll does not tighten a folder that was already there, so the mode is set again; a
// failure of that is passed to warn and does not stop the start.
func PrepareStateDir(path string, warn func(error)) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		warn(err)
	}
	return nil
}
