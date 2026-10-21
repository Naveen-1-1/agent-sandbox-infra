package depcache

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Stats counts what an archive holds.
type Stats struct {
	Files int
	Bytes int64 // uncompressed file contents
}

// Limits bound what Extract accepts, so a hostile archive can't fill the
// disk. Zero means the default.
type Limits struct {
	MaxEntries int   // default 2,000,000
	MaxBytes   int64 // default 16 GiB
}

func (l Limits) withDefaults() Limits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = 2_000_000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 16 << 30
	}
	return l
}

// Pack writes dir as a zstd-compressed tar to w, with names relative to
// dir. It keeps regular files, directories and symlinks with their
// permission bits and modification times; anything else is skipped. Reads
// go through an os.Root on dir, so a symlink inside never leads Pack out.
func Pack(ctx context.Context, dir string, w io.Writer) (Stats, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Stats{}, err
	}
	defer root.Close()
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return Stats{}, err
	}
	tw := tar.NewWriter(zw)
	var st Stats
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		var link string
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = root.Readlink(rel); err != nil {
				return err
			}
		case !info.Mode().IsRegular() && !info.IsDir():
			return nil // sockets, devices and pipes aren't dependencies
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", "" // ownership is the extractor's
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(tw, f)
		st.Files++
		st.Bytes += n
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return st, err
}

// ErrUnsafe means an archive tried to do something no dependency directory
// needs: escape the target, hard-link, create a device, or set setuid bits.
var ErrUnsafe = errors.New("depcache: unsafe archive")

// Extract unpacks an archive made by Pack into dst, which must not exist
// yet. Every write goes through an os.Root on dst, so no entry, and no
// symlink planted by an earlier entry, can reach outside it. Relative
// symlinks must also point inside dst; absolute ones are kept (a virtualenv
// links to the image's interpreter) but can't be written through here.
// setuid, setgid and sticky bits are dropped.
func Extract(ctx context.Context, r io.Reader, dst string, lim Limits) (Stats, error) {
	lim = lim.withDefaults()
	if err := os.Mkdir(dst, 0o755); err != nil {
		return Stats{}, err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return Stats{}, err
	}
	defer root.Close()
	zr, err := zstd.NewReader(r)
	if err != nil {
		return Stats{}, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	var st Stats
	for entries := 0; ; entries++ {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return st, nil
		}
		if err != nil {
			return st, err
		}
		if entries >= lim.MaxEntries {
			return st, fmt.Errorf("%w: more than %d entries", ErrUnsafe, lim.MaxEntries)
		}
		name, err := cleanName(hdr.Name)
		if err != nil {
			return st, err
		}
		mode := fs.FileMode(hdr.Mode & 0o777) //nolint:gosec // masked to permission bits: no setuid, setgid or sticky
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, mode|0o700); err != nil {
				return st, err
			}
		case tar.TypeReg:
			if st.Bytes+hdr.Size > lim.MaxBytes {
				return st, fmt.Errorf("%w: more than %d bytes", ErrUnsafe, lim.MaxBytes)
			}
			if err := writeFile(root, name, mode, tr); err != nil {
				return st, err
			}
			_ = root.Chtimes(name, hdr.ModTime, hdr.ModTime) // keeps Python's bytecode caches valid
			st.Files++
			st.Bytes += hdr.Size
		case tar.TypeSymlink:
			// This is the traversal check; writes are confined by root anyway.
			if !path.IsAbs(hdr.Linkname) && !inside(path.Join(path.Dir(name), hdr.Linkname)) { //nolint:gosec // see above
				return st, fmt.Errorf("%w: %s links outside the archive (%s)", ErrUnsafe, name, hdr.Linkname)
			}
			if err := mkParent(root, name); err != nil {
				return st, err
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return st, err
			}
		case tar.TypeXGlobalHeader:
		default:
			return st, fmt.Errorf("%w: %s has entry type %q", ErrUnsafe, name, hdr.Typeflag)
		}
	}
}

func writeFile(root *os.Root, name string, mode fs.FileMode, r io.Reader) error {
	if err := mkParent(root, name); err != nil {
		return err
	}
	// O_EXCL: a second entry with the same name is refused, not merged.
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func mkParent(root *os.Root, name string) error {
	if dir := path.Dir(name); dir != "." {
		return root.MkdirAll(dir, 0o755)
	}
	return nil
}

// cleanName returns an entry name as a clean relative path, or an error if
// it is absolute or climbs out with "..".
func cleanName(name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if path.IsAbs(clean) || !inside(clean) || clean == "." {
		return "", fmt.Errorf("%w: entry name %q", ErrUnsafe, name)
	}
	return clean, nil
}

func inside(rel string) bool {
	rel = path.Clean(rel)
	return rel != ".." && !strings.HasPrefix(rel, "../")
}
