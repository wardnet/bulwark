package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// httpTimeout bounds a single toolchain download. A language toolchain is
// tens of megabytes, so this is generous — but unbounded is not an option: a
// hung download would hang the whole scan with no output, which is
// indistinguishable from bulwark being broken.
const httpTimeout = 10 * time.Minute

// maxArchiveBytes caps what will be written out of one archive. A toolchain
// tarball expands to a few hundred MB at most; anything past this is a
// decompression bomb or a wrong URL, and filling the runner's disk is a worse
// failure than refusing to install.
const maxArchiveBytes = 2 << 30 // 2 GiB

// cacheRoot is the version-keyed directory layout every bulwark-managed
// install already uses (internal/golang's gobin-<tool>-<version>,
// internal/rust's <tool>-<version>, internal/typescript's
// biome-toolchain-<versions>). Language toolchains join it rather than
// inventing a second location, so one cache key in CI covers all of them —
// which is exactly what wardnet's workflow already caches by path.
func cacheRoot(elem ...string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{base, "bulwark"}, elem...)...), nil
}

// fetch GETs url and returns its body, bounded by httpTimeout.
func fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes))
}

// downloadVerified fetches url and checks its SHA-256 against want before
// returning a single byte of it to a caller.
//
// Verifying is not optional and the digest is never derived from the download
// itself: bulwark is about to put this archive's contents on PATH and execute
// them, which makes an unverified toolchain download a straight supply-chain
// hole. Both publishers offer a digest out of band — Go in its release index,
// Node in a SHASUMS256.txt — and each provisioner reads it from there. This
// matches what scripts/install.sh already does for bulwark's own binary.
func downloadVerified(ctx context.Context, url, want string) ([]byte, error) {
	data, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, want)
	}
	return data, nil
}

// extractTarGz unpacks a .tar.gz into dest, dropping the archive's single
// leading path component (both Go's and Node's tarballs wrap everything in
// one top-level directory, and callers want its contents at dest).
func extractTarGz(data []byte, dest string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(io.LimitReader(zr, maxArchiveBytes))
	var written int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := stripLeading(hdr.Name)
		if rel == "" {
			continue
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			// Remove any existing entry before writing. Opening with O_CREATE
			// follows an existing symlink and writes through it, so an
			// archive that plants a symlink and then writes a regular file at
			// the same name would write wherever the link points. Since dest
			// is a directory this function just created, anything already at
			// target came from this same archive — dropping it is safe and is
			// what stops that two-step.
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			n, err := writeFile(target, tr, hdr.FileInfo().Mode(), maxArchiveBytes-written)
			if err != nil {
				return err
			}
			written += n
			if written >= maxArchiveBytes {
				return fmt.Errorf("archive exceeds %d bytes; refusing to continue", maxArchiveBytes)
			}
		case tar.TypeSymlink:
			// Node's tarball ships symlinks (npm/npx into lib/node_modules),
			// so they cannot simply be skipped — but they are the sharpest
			// edge in an archive and need two separate checks.
			//
			// An absolute target is rejected outright rather than
			// containment-checked, because the obvious check does not work on
			// one: filepath.Join("bin", "/etc/passwd") is "bin/etc/passwd",
			// so joining a link target against its own directory silently
			// reinterprets an absolute path as a relative one and every
			// escape passes. A toolchain tarball has no legitimate use for an
			// absolute symlink anyway — it would point outside the install
			// either way.
			if path.IsAbs(filepath.ToSlash(hdr.Linkname)) {
				return fmt.Errorf("archive entry %q is a symlink to the absolute path %q", rel, hdr.Linkname)
			}
			// A relative target still has to land inside dest once resolved
			// against the link's own directory. Slash semantics throughout:
			// tar names are slash-separated regardless of host.
			if _, err := safeJoin(dest, path.Join(path.Dir(filepath.ToSlash(rel)), filepath.ToSlash(hdr.Linkname))); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Character devices, FIFOs and hard links have no business in a
			// toolchain tarball; skipping is safer than materialising them.
			continue
		}
	}
}

// writeFile streams one archive entry to disk, bounded by remaining.
func writeFile(target string, r io.Reader, mode os.FileMode, remaining int64) (int64, error) {
	// #nosec G304 -- target is the output of safeJoin, which has already
	// confirmed it stays within the destination directory.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()&0o755)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, io.LimitReader(r, remaining))
	if err != nil {
		return n, err
	}
	return n, f.Close()
}

// stripLeading removes the archive's single top-level directory from a path.
//
// Deliberately does NOT clean the path first. Cleaning "go/../../escaped"
// collapses it to "../escaped", after which dropping the leading component
// eats the traversal and yields a benign-looking "escaped" — an entry that
// lands inside the destination under a name bearing no relation to what the
// archive asked for. Splitting the raw name instead leaves "../../escaped"
// intact for safeJoin to reject, which is the correct outcome: an archive
// that tries to escape is refused, not quietly rewritten.
func stripLeading(name string) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	_, rest, found := strings.Cut(name, "/")
	if !found {
		return ""
	}
	return rest
}

// safeJoin resolves rel inside dest and refuses anything that escapes it.
//
// This is the zip-slip guard. An archive entry named "../../.ssh/authorized_keys"
// would otherwise be written wherever it pleases, and bulwark unpacks these
// archives with the user's own privileges. The check is on the cleaned,
// absolute result rather than on the input string, so "a/../../b" and a
// symlinked dest are both caught.
func safeJoin(dest, rel string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(rel))
	cleanDest := filepath.Clean(dest) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) {
		return "", fmt.Errorf("archive entry %q escapes the destination directory", rel)
	}
	return target, nil
}

// installOnce runs install into a fresh temp directory and moves the result
// into place atomically, so a download interrupted halfway can never leave a
// half-populated toolchain directory that the next run mistakes for a
// complete one. If dir already exists it is taken as a finished install and
// install is not called at all.
//
// The same-parent temp directory matters: os.Rename cannot cross filesystems,
// and os.TempDir is routinely a different mount from the user cache dir.
func installOnce(dir string, install func(staging string) error) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".staging-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := install(staging); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		// A concurrent bulwark (two jobs sharing a cache volume) may have
		// won the race and created dir first. Its content is the same
		// verified archive, so that is a success, not a conflict.
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}
