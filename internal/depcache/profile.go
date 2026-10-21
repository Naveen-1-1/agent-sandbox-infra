// Package depcache restores dependency directories (node_modules, .venv)
// from a shared cache instead of reinstalling them on every wake. An archive
// is keyed by everything that determines the directory's contents: the
// package manager's lockfile, the workload image (which pins the language
// runtime and package manager) and the CPU architecture.
package depcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Profile is one way of installing dependencies.
type Profile struct {
	Name string
	// Lockfiles are tried in order; the first present one is used.
	Lockfiles []string
	// Dir is the directory the install produces, relative to the workspace.
	Dir string
	// Install is the shell command that produces Dir from the lockfile.
	Install string
	// Commands are programs that need Dir. Commands that run them wait for
	// the restore; everything else runs at once.
	Commands []string
}

// Profiles are tried in order.
var Profiles = []Profile{
	{
		Name: "uv", Lockfiles: []string{"uv.lock"}, Dir: ".venv",
		Install:  "uv sync --frozen",
		Commands: []string{"uv", "python", "python3", "pip", "pip3", "pytest"},
	},
	{
		Name: "npm", Lockfiles: []string{"package-lock.json", "npm-shrinkwrap.json"}, Dir: "node_modules",
		Install:  "npm ci --no-audit --no-fund",
		Commands: []string{"npm", "npx", "node"},
	},
	{
		Name: "pip", Lockfiles: []string{"requirements.txt"}, Dir: ".venv",
		Install:  "python3 -m venv .venv && .venv/bin/pip install --disable-pip-version-check -r requirements.txt",
		Commands: []string{"python", "python3", "pip", "pip3", "pytest"},
	},
}

// maxLockfile bounds how much of a lockfile is read.
const maxLockfile = 64 << 20

// Lookup finds a profile by name.
func Lookup(name string) (Profile, bool) {
	i := slices.IndexFunc(Profiles, func(p Profile) bool { return p.Name == name })
	if i < 0 {
		return Profile{}, false
	}
	return Profiles[i], true
}

// Detect picks the profile whose lockfile is in root and hashes that
// lockfile. ok is false when the workspace has no dependencies we know.
func Detect(root string) (p Profile, lockSHA string, ok bool, err error) {
	for _, p := range Profiles {
		for _, name := range p.Lockfiles {
			sum, err := hashLockfile(filepath.Join(root, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return Profile{}, "", false, err
			}
			return p, sum, true, nil
		}
	}
	return Profile{}, "", false, nil
}

// LockSHA hashes p's lockfile in root again, to see whether it changed.
func (p Profile) LockSHA(root string) (string, error) {
	for _, name := range p.Lockfiles {
		sum, err := hashLockfile(filepath.Join(root, name))
		if !errors.Is(err, os.ErrNotExist) {
			return sum, err
		}
	}
	return "", os.ErrNotExist
}

func hashLockfile(name string) (string, error) {
	st, err := os.Lstat(name)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	if st.Size() > maxLockfile {
		return "", fmt.Errorf("%s is over %d bytes", name, maxLockfile)
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxLockfile)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Key identifies the archive for a profile's directory built from the
// lockfile with this hash, in this image, on this architecture.
func Key(profile, lockSHA, image, arch string) string {
	h := sha256.New()
	fmt.Fprintf(h, "sbx-deps-v1\x00%s\x00%s\x00%s\x00%s", profile, lockSHA, image, arch)
	return hex.EncodeToString(h.Sum(nil))
}

// ValidKey reports whether s looks like a Key.
func ValidKey(s string) bool { return validKey.MatchString(s) }

var validKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NeedsDeps reports whether a command line runs one of p's programs, or
// anything from inside p.Dir, anywhere in it: "npm test", "sh -c 'cd app
// && npx jest'", "./node_modules/.bin/tsc". It errs towards waiting, which
// only costs time; ls, cat and git never match.
func (p Profile) NeedsDeps(argv []string) bool {
	for _, word := range strings.FieldsFunc(strings.Join(argv, " "), isSeparator) {
		if name, _, ok := strings.Cut(word, "="); ok && !strings.Contains(name, "/") {
			continue // FOO=bar in front of a command, or --flag=value
		}
		if slices.Contains(p.Commands, path.Base(word)) {
			return true
		}
		if strings.Contains("/"+word+"/", "/"+p.Dir+"/") {
			return true
		}
	}
	return false
}

func isSeparator(r rune) bool {
	switch r {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '`', '\'', '"', '$', '{', '}', '<', '>':
		return true
	}
	return false
}
