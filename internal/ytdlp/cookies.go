package ytdlp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// yt-dlp saves its cookies file back after every run. So that runs can overlap,
// each run gets a private copy of the configured file; only a successful
// download puts its copy back (an atomic rename). The shared lock guards the
// moments the real file is read or replaced, which take milliseconds.

// cookieCopyPattern names private copies, next to the real file.
const cookieCopyPattern = ".ytdlp-cookies-*.txt"

// cookieHeader starts every cookies file yt-dlp writes.
var cookieHeader = []byte("# Netscape HTTP Cookie File")

// checkoutCookies copies the cookies file at path to a new private file
// (mode 0600) in the same directory and returns its name.
func checkoutCookies(path string, lock sync.Locker) (string, error) {
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cookies: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), cookieCopyPattern)
	if err != nil {
		return "", fmt.Errorf("copy cookies: %w", err)
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("copy cookies: %w", werr)
	}
	return f.Name(), nil
}

// checkinCookies replaces the cookies file at path with the copy, unless the
// copy doesn't look like a cookies file (then path is left alone). The copy
// is gone afterwards either way.
func checkinCookies(copyPath, path string, lock sync.Locker) error {
	data, err := os.ReadFile(copyPath)
	if err != nil {
		os.Remove(copyPath)
		return fmt.Errorf("read refreshed cookies; kept the old file: %w", err)
	}
	if !bytes.HasPrefix(data, cookieHeader) {
		os.Remove(copyPath)
		return errors.New("refreshed cookies look invalid; kept the old file")
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	if err := os.Rename(copyPath, path); err != nil {
		os.Remove(copyPath)
		return fmt.Errorf("save cookies: %w", err)
	}
	return nil
}

// RemoveStaleCookieCopies deletes private copies left next to the cookies file
// at path by a crash. Call it at startup, before any yt-dlp run.
func RemoveStaleCookieCopies(path string) error {
	stale, err := filepath.Glob(filepath.Join(filepath.Dir(path), cookieCopyPattern))
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("remove stale cookie copies: %w", errors.Join(errs...))
	}
	return nil
}
