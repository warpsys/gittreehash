package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/serum-errors/go-serum"
	"github.com/warpfork/go-fsx"
	"github.com/warpfork/go-fsx/osfs"
)

// Note that .gitignore files and other special behaviors of git are not treated here.
func main() {
	startPath := "."
	if len(os.Args) > 1 {
		startPath = filepath.Clean(os.Args[1])
	}
	fsys := osfs.DirFS(".")

	hash, _, err := hashSomething(fsys, startPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", serum.ToJSONString(err))
		os.Exit(9)
	}
	var hashHex [64]byte
	hex.Encode(hashHex[:], hash[:])
	fmt.Printf("%s\n", hashHex)
}

const (
	ErrUnsupportedFileType = "gittreehash-error-unsupported-file-type"
	ErrIO                  = "gittreehash-error-io"
	ErrConcurrentIO        = "gittreehash-error-concurrent-io"
)

// hashSomething figures out what kind of file the given parameters point to,
// hashes it appropriately, and writes the raw hash bytes to the given writer.
//
// It returns the filemode of what was encountered, because the caller tends to
// need that information again when composing tree objects.
//
// Errors:
//
//   - gittreehash-error-unsupported-file-type -- if the filesystem contains
//       files that git doesn't have a description of: sockets, device nodes, etc.
//   - gittreehash-error-io -- if any raw IO barfs while we're scanning the filesystem.
//   - gittreehash-error-concurrent-io -- if any inconsistencies are detected which
//       likely arose from concurrent filesystem changes during the hashing.
//       May also be triggered if a filesystem incorrectly reports file size.
//
func hashSomething(fsys fsx.FS, pth string) ([32]byte, fs.FileMode, error) {
	fi, err := fsx.Lstat(fsys, pth)
	if err != nil {
		return [32]byte{}, 0, serum.Errorf(ErrIO, "%w", err)
	}
	mode := fi.Mode()
	switch mode & fs.ModeType {
	case 0: // https://git-scm.com/book/en/v2/Git-Internals-Git-Objects
		claimedSize := fi.Size()
		var preamble bytes.Buffer
		preamble.WriteString("blob ")
		preamble.WriteString(strconv.Itoa(int(claimedSize)))
		preamble.WriteByte(0)
		preambleLen := preamble.Len()

		f, err := fsys.Open(pth)
		if err != nil {
			return [32]byte{}, mode, serum.Errorf(ErrIO, "%w", err)
		}
		defer f.Close()
		hash, coveredSize, err := hashStream(io.MultiReader(&preamble, f))
		if err != nil {
			return [32]byte{}, mode, err
		}

		contentSize := coveredSize - int64(preambleLen)
		if contentSize != claimedSize {
			return hash, mode, serum.Errorf(ErrConcurrentIO, "expected file size %d but read %d bytes at path %q", claimedSize, contentSize, pth)
		}

		return hash, mode, nil
	case fs.ModeSymlink: // the target is treated as a blob; only the way they're written into the parent tree differs.
		claimedSize := fi.Size()
		var preamble bytes.Buffer
		preamble.WriteString("blob ")
		preamble.WriteString(strconv.Itoa(int(claimedSize)))
		preamble.WriteByte(0)
		preambleLen := preamble.Len()

		target, err := fsx.Readlink(fsys, pth)
		if err != nil {
			return [32]byte{}, mode, serum.Errorf(ErrConcurrentIO, "found symlink at path %q but readlink failed: %w", pth, err)
		}
		hash, coveredSize, err := hashStream(io.MultiReader(&preamble, strings.NewReader(target)))
		if err != nil {
			panic("unreachable; all data already in memory")
		}

		contentSize := coveredSize - int64(preambleLen)
		if contentSize != claimedSize {
			return hash, mode, serum.Errorf(ErrConcurrentIO, "expected file size %d but read %d bytes at path %q", claimedSize, contentSize, pth)
		}

		return hash, mode, nil
	case fs.ModeDir: // https://stackoverflow.com/questions/14790681/what-is-the-internal-format-of-a-git-tree-object
		dirEnts, err := fsx.ReadDir(fsys, pth)
		if err != nil {
			return [32]byte{}, mode, serum.Errorf(ErrIO, "%w", err)
		}
		// TODO: check if the sorting is correct, here.
		var buf bytes.Buffer // Buffer to accumulate all the child object info and hashes, first.  Need this so we can compute the length of the whole tree object body.
		for _, dirEnt := range dirEnts {
			hash, dirEntMode, err := hashSomething(fsys, filepath.Join(pth, dirEnt.Name()))
			if err != nil {
				return [32]byte{}, mode, err
			}
			switch dirEntMode & fs.ModeType {
			case 0:
				if dirEntMode&0o111 != 0 {
					buf.Write([]byte("100755 "))
				} else {
					buf.Write([]byte("100644 "))
				}
			case fs.ModeSymlink:
				buf.Write([]byte("120000 "))
			case fs.ModeDir:
				buf.Write([]byte("40000 "))
			default:
				panic("unreachable?  other types should've error earlier")
			}
			buf.Write([]byte(dirEnt.Name()))
			buf.Write([]byte{0})
			buf.Write(hash[:])
			// Somewhat shockingly, there's no delimiter here.  The hash length is necessary hardcoded by this absense.
		}

		var preamble bytes.Buffer
		preamble.WriteString("tree ")
		preamble.WriteString(strconv.Itoa(buf.Len()))
		preamble.WriteByte(0)
		hash, _, err := hashStream(io.MultiReader(&preamble, &buf))
		if err != nil {
			panic("unreachable; all data already in memory")
		}

		return hash, mode, nil
	case fs.ModeNamedPipe:
		return [32]byte{}, mode, NewErrUnsupportedFileType("pipe", pth)
	case fs.ModeSocket:
		return [32]byte{}, mode, NewErrUnsupportedFileType("socket", pth)
	case fs.ModeDevice, fs.ModeCharDevice:
		return [32]byte{}, mode, NewErrUnsupportedFileType("device", pth)
	case fs.ModeIrregular:
		return [32]byte{}, mode, NewErrUnsupportedFileType("irregular", pth)
	default:
		panic("unreachable?  'irregular' should be the catch-all here")
	}
}

func hashStream(data io.Reader) (hash [32]byte, contentSize int64, err error) {
	h := sha256.New()
	contentSize, err = io.Copy(h, data)
	if err != nil {
		err = serum.Errorf(ErrIO, "%w", err)
		return
	}
	h.Sum(hash[:0])
	return
}

func NewErrUnsupportedFileType(typ string, pth string) error {
	return serum.Error(
		ErrUnsupportedFileType,
		serum.WithMessageTemplate("git hashes can not describe {{type}} files; found one at {{path}}"),
		serum.WithDetail("type", typ),
		serum.WithDetail("path", pth),
	)
}
