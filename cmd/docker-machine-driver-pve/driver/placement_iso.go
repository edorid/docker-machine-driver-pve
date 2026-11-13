package driver

import (
	"errors"
	"fmt"
	"os"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// buildPlacementISO creates a private, uniquely named NoCloud ISO. Its caller
// owns the resulting file and must remove it after the upload attempt.
func buildPlacementISO(dir, userdata, metadata, network string) (path string, err error) {
	if userdata == "" || metadata == "" || network == "" {
		return "", errors.New("cloud-init user, metadata and network configuration are required")
	}
	tmp, err := os.CreateTemp(dir, "pve-cloud-init-*.iso")
	if err != nil {
		return "", fmt.Errorf("create cloud-init ISO: %w", err)
	}
	path = tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	backend, err := file.OpenFromPath(path, false)
	if err != nil {
		return path, err
	}
	defer func() {
		if closeErr := backend.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	fs, err := iso9660.Create(backend, 0, 0, 2048, "")
	if err != nil {
		return path, err
	}
	if err := fs.Mkdir("/"); err != nil {
		return path, err
	}
	for name, content := range map[string]string{"user-data": userdata, "meta-data": metadata, "network-config": network} {
		f, openErr := fs.OpenFile("/"+name, os.O_CREATE|os.O_RDWR)
		if openErr != nil {
			return path, openErr
		}
		_, writeErr := f.Write([]byte(content))
		closeErr := f.Close()
		if writeErr != nil {
			return path, writeErr
		}
		if closeErr != nil {
			return path, closeErr
		}
	}
	if err := fs.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: "cidata"}); err != nil {
		return path, err
	}
	return path, nil
}
