package dbreuse

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrIncompleteMedia means at least one symlink or file could not be copied.
// Files that were copied are left in place.
var ErrIncompleteMedia = errors.New("dbreuse: media incomplete")

// ErrMediaConflict means the destination already has different bytes.
var ErrMediaConflict = errors.New("dbreuse: destination media differs")

// MediaFile is one materialized file. Bodies are not included.
type MediaFile struct {
	LogicalPath string `json:"logical_path"`
	SHA256      string `json:"sha256,omitempty"`
	Size        int64  `json:"size"`
	Symlink     bool   `json:"symlink"`
	LinkTarget  string `json:"link_target,omitempty"`
	Missing     bool   `json:"missing"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// MediaManifest is the result of copying a media tree and dereferencing symlinks.
type MediaManifest struct {
	Source    string      `json:"source"`
	Dest      string      `json:"dest"`
	Files     []MediaFile `json:"files"`
	Complete  bool        `json:"complete"`
	Copied    int         `json:"copied"`
	Present   int         `json:"present"`
	Missing   int         `json:"missing"`
	Conflicts int         `json:"conflicts"`
}

// MaterializeMedia copies src to dest. Symlinks are stored as regular files
// containing the target bytes. Existing destination files with the same SHA-256
// are left alone. Different bytes are not overwritten.
func MaterializeMedia(src, dest string) (*MediaManifest, error) {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(srcAbs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("media source %s is not a directory", srcAbs)
	}
	if srcAbs == destAbs || strings.HasPrefix(destAbs, srcAbs+string(os.PathSeparator)) {
		return nil, fmt.Errorf("media destination must be outside the source tree")
	}
	if err := os.MkdirAll(destAbs, 0o700); err != nil {
		return nil, err
	}
	manifest := &MediaManifest{Source: srcAbs, Dest: destAbs, Complete: true}
	seen := map[string]struct{}{}
	err = walkMedia(srcAbs, destAbs, "", 0, false, "", seen, manifest)
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].LogicalPath < manifest.Files[j].LogicalPath
	})
	if err != nil {
		manifest.Complete = false
		return manifest, err
	}
	if manifest.Missing > 0 || manifest.Conflicts > 0 {
		manifest.Complete = false
		if manifest.Conflicts > 0 {
			return manifest, ErrMediaConflict
		}
		return manifest, ErrIncompleteMedia
	}
	return manifest, nil
}

func walkMedia(dir, dest, logical string, depth int, viaSymlink bool, viaTarget string, seen map[string]struct{}, manifest *MediaManifest) error {
	if depth > 32 {
		return fmt.Errorf("media walk exceeded depth at %s", logical)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var walkErr error
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}
		disk := filepath.Join(dir, name)
		rel := name
		if logical != "" {
			rel = logical + "/" + name
		}
		if strings.Contains(rel, "..") {
			return fmt.Errorf("refusing media path %s", rel)
		}
		linkInfo, err := os.Lstat(disk)
		if err != nil {
			walkErr = errors.Join(walkErr, err)
			continue
		}
		symlink := linkInfo.Mode()&os.ModeSymlink != 0
		linkTarget := ""
		resolved := disk
		if symlink {
			linkTarget, err = os.Readlink(disk)
			if err != nil {
				walkErr = errors.Join(walkErr, err)
				continue
			}
			if !filepath.IsAbs(linkTarget) {
				resolved = filepath.Join(dir, linkTarget)
			} else {
				resolved = linkTarget
			}
			if _, err := os.Stat(resolved); err != nil {
				manifest.Files = append(manifest.Files, MediaFile{
					LogicalPath: rel,
					Symlink:     true,
					LinkTarget:  linkTarget,
					Missing:     true,
					Status:      "missing",
					Error:       err.Error(),
				})
				manifest.Missing++
				continue
			}
		}
		info, err := os.Stat(resolved)
		if err != nil {
			manifest.Files = append(manifest.Files, MediaFile{
				LogicalPath: rel,
				Symlink:     symlink,
				LinkTarget:  linkTarget,
				Missing:     true,
				Status:      "missing",
				Error:       err.Error(),
			})
			manifest.Missing++
			continue
		}
		if info.IsDir() {
			key, ok := inodeKey(info)
			if ok {
				if _, dup := seen[key]; dup {
					return fmt.Errorf("symlink cycle at %s", rel)
				}
				seen[key] = struct{}{}
			}
			nextVia := viaSymlink
			nextTarget := viaTarget
			if symlink {
				nextVia = true
				nextTarget = linkTarget
			}
			if err := walkMedia(resolved, dest, rel, depth+1, nextVia, nextTarget, seen, manifest); err != nil {
				walkErr = errors.Join(walkErr, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			manifest.Files = append(manifest.Files, MediaFile{
				LogicalPath: rel,
				Symlink:     symlink,
				LinkTarget:  linkTarget,
				Status:      "skipped",
				Error:       "not a regular file",
			})
			continue
		}
		file, err := copyMaterialized(resolved, filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			if errors.Is(err, ErrMediaConflict) {
				manifest.Conflicts++
				manifest.Files = append(manifest.Files, MediaFile{
					LogicalPath: rel,
					Symlink:     symlink,
					LinkTarget:  linkTarget,
					Status:      "conflict",
					Error:       err.Error(),
				})
				continue
			}
			walkErr = errors.Join(walkErr, err)
			continue
		}
		file.LogicalPath = rel
		file.Symlink = symlink || viaSymlink
		file.LinkTarget = linkTarget
		if file.LinkTarget == "" {
			file.LinkTarget = viaTarget
		}
		manifest.Files = append(manifest.Files, file)
		if file.Status == "copied" {
			manifest.Copied++
		} else if file.Status == "present" {
			manifest.Present++
		}
	}
	return walkErr
}

func copyMaterialized(src, dst string) (MediaFile, error) {
	in, err := os.Open(src)
	if err != nil {
		return MediaFile{}, err
	}
	defer in.Close()
	sum, size, err := hashReader(in)
	if err != nil {
		return MediaFile{}, err
	}
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return MediaFile{}, fmt.Errorf("%w: %s is a symlink", ErrMediaConflict, dst)
		}
		existing, err := os.Open(dst)
		if err != nil {
			return MediaFile{}, err
		}
		defer existing.Close()
		existingSum, _, err := hashReader(existing)
		if err != nil {
			return MediaFile{}, err
		}
		if existingSum != sum {
			return MediaFile{}, fmt.Errorf("%w: %s", ErrMediaConflict, dst)
		}
		return MediaFile{SHA256: sum, Size: size, Status: "present"}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return MediaFile{}, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return MediaFile{}, err
	}
	defer out.Close()
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return MediaFile{}, err
	}
	if _, err := io.Copy(out, in); err != nil {
		return MediaFile{}, err
	}
	return MediaFile{SHA256: sum, Size: size, Status: "copied"}, nil
}

func hashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func inodeKey(info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), true
}
