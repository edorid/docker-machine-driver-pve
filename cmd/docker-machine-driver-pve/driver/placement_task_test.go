package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func TestCloneTaskIdentityIsFullUPID(t *testing.T) {
	const upid = "UPID:drc-pve01:00001234:00000042:67300000:qmclone:9001:root@pam:"
	task := proxmox.NewTask(proxmox.UPID(upid), nil)
	require.Equal(t, "9001", task.ID)
	stored, err := cloneTaskIdentity(task, "drc-pve01")
	require.NoError(t, err)
	require.Equal(t, upid, stored)
	resumed, err := recordedCloneTask(stored, "drc-pve01", nil)
	require.NoError(t, err)
	require.Equal(t, "drc-pve01", resumed.Node)
	require.Equal(t, proxmox.UPID(upid), resumed.UPID)
}

func TestRecordedCloneTaskPollsFullUPIDOnItsNode(t *testing.T) {
	const upid = "UPID:drc-pve01:001A8E0D:02388B5C:6AF6D272:qmclone:9001:user@pve!token:"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api2/json/nodes/drc-pve01/tasks/"+upid+"/status", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "stopped", "exitstatus": "OK"}})
	}))
	defer server.Close()
	driver := NewDriver("worker", t.TempDir())
	driver.URL, driver.TokenID, driver.TokenSecret, driver.InsecureTLS = server.URL, "user@pve!test", "test-secret", true
	original := proxmox.NewTask(proxmox.UPID(upid), nil)
	stored, err := cloneTaskIdentity(original, "drc-pve01")
	require.NoError(t, err)
	resumed, err := recordedCloneTask(stored, "drc-pve01", driver.getPVEClient())
	require.NoError(t, err)
	require.NoError(t, resumed.Ping(t.Context()))
	require.True(t, resumed.IsCompleted)
	require.True(t, resumed.IsSuccessful)
}

func TestCloneTaskRejectsInvalidAndWrongNodeIDs(t *testing.T) {
	for _, id := range []string{"", "9001", "UPID:short", "UPID:other:1:2:3:qmclone:9001:root@pam:"} {
		_, err := recordedCloneTask(id, "drc-pve01", nil)
		require.Error(t, err, id)
	}
}
