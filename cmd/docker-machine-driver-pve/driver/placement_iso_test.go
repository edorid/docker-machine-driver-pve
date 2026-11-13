package driver

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/stretchr/testify/require"
)

func TestBuildPlacementISO(t *testing.T) {
	isoPath, err := buildPlacementISO(t.TempDir(), "#cloud-config\nusers: []\n", "instance-id: machine-123\n", "version: 2\nethernets: {}\n")
	require.NoError(t, err)
	backend, err := file.OpenFromPath(isoPath, true)
	require.NoError(t, err)
	defer backend.Close()
	fs, err := iso9660.Read(backend, 0, 0, 2048)
	require.NoError(t, err)
	require.Equal(t, "cidata", strings.TrimRight(fs.Label(), "\x00 "))
	for name, expected := range map[string]string{
		"user-data": "#cloud-config\nusers: []\n", "meta-data": "instance-id: machine-123\n", "network-config": "version: 2\nethernets: {}\n",
	} {
		f, err := fs.OpenFile("/"+name, os.O_RDONLY)
		require.NoError(t, err)
		data, err := io.ReadAll(f)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		require.Equal(t, expected, string(data))
	}
}
