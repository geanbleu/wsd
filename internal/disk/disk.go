// Package disk handles reflink copies (FICLONE) and sparse workspace disk creation.
package disk

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// ficloneIoctl is the FICLONE ioctl number on Linux x86_64/arm64.
// Defined in <linux/fs.h>: _IOW(0x94, 9, int)
const ficloneIoctl = 0x40049409

// Reflink copies src to dst using the FICLONE ioctl (zero-copy, CoW).
// Returns an explicit error when the filesystem does not support reflinking.
func Reflink(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("reflink open src: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("reflink create dst: %w", err)
	}
	defer out.Close()

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficloneIoctl, in.Fd())
	if errno != 0 {
		if errno == unix.EOPNOTSUPP || errno == unix.EXDEV {
			return fmt.Errorf("FICLONE non supporté sur ce filesystem "+
				"(besoin xfs+reflink=1 ou btrfs): %w", errno)
		}
		return fmt.Errorf("FICLONE ioctl: %w", errno)
	}
	return nil
}

// Hardlink creates a hard link at dst pointing to src (idempotent).
func Hardlink(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	os.Remove(dst) // remove stale link if present
	if err := os.Link(src, dst); err != nil {
		// EXDEV est le symptôme d'une erreur de déploiement, pas d'un bug :
		// un hardlink ne traverse pas les filesystems, donc data_dir et
		// jailer_dir doivent partager le même. Le dire ici évite de laisser
		// l'opérateur face à un « invalid cross-device link » nu.
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("hardlink %s → %s : les deux chemins ne sont pas "+
				"sur le même filesystem. data_dir et jailer_dir doivent le "+
				"partager (voir docs/INSTALL.md § Stockage et "+
				"deploy/setup-storage.sh) : %w", src, dst, err)
		}
		return fmt.Errorf("hardlink %s → %s: %w", src, dst, err)
	}
	return nil
}

// CreateWorkspaceDisk creates a sparse ext4 file at dst of sizeGiB gigabytes.
func CreateWorkspaceDisk(dst string, sizeGiB int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create sparse: %w", err)
	}
	if err := f.Truncate(int64(sizeGiB) << 30); err != nil {
		f.Close()
		os.Remove(dst)
		return fmt.Errorf("truncate: %w", err)
	}
	f.Close()

	// -q silencieux, -F force sur fichier (non-bloc), -L label
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-L", "workspace", dst)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("mkfs.ext4: %w", err)
	}
	return nil
}
