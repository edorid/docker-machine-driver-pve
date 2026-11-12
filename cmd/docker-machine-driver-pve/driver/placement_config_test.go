package driver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

//nolint:wsl // Test branches separate expected errors from successful assertions.
func TestParsePlacementTemplates(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]int
		wantErr bool
	}{
		{name: "multiple hosts", input: "pve-a=100,pve-b=101", want: map[string]int{"pve-a": 100, "pve-b": 101}},
		{name: "duplicate host", input: "pve-a=100,pve-a=101", wantErr: true},
		{name: "malformed", input: "pve-a:100", wantErr: true},
		{name: "invalid vmid", input: "pve-a=99", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePlacementTemplates(test.input)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestSplitPlacementListTrimsEntries(t *testing.T) {
	require.Equal(t, []string{"pve-a", "pve-b"}, splitPlacementList(" pve-a, pve-b "))
	require.Nil(t, splitPlacementList("  "))
}
