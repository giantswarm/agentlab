package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRefusesGHCRImages(t *testing.T) {
	const (
		dexField = "dexImage"
		registry = "gsoci.azurecr.io"
	)
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, testCase := range []struct {
		name, fields, field, replacement string
	}{
		{
			name:   "Dex tag",
			fields: "dexImage: ghcr.io/dexidp/dex:v2.45.1\n",
			field:  dexField, replacement: "gsoci.azurecr.io/giantswarm/dex:v2.45.1",
		},
		{
			name:   "Dex digest",
			fields: "dexImage: ghcr.io/dexidp/dex" + digest + "\n",
			field:  dexField, replacement: "gsoci.azurecr.io/giantswarm/dex" + digest,
		},
		{
			name:   "custom Dex image",
			fields: "dexImage: ghcr.io/example/custom-dex:v1\n",
			field:  dexField, replacement: registry,
		},
		{
			name:   "dev image",
			fields: "platform:\n  devImages:\n    muster: ghcr.io/giantswarm/muster:dev\n",
			field:  "platform.devImages.muster", replacement: registry,
		},
		{
			name:   "coding image",
			fields: "platform:\n  coding:\n    images:\n      omp: ghcr.io/example/omp" + digest + "\n",
			field:  "platform.coding.images.omp", replacement: registry,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			raw := []byte("clusterName: lab\n" + testCase.fields)
			require.NoError(t, os.WriteFile(File, raw, 0o600))

			_, err := Load()
			require.ErrorContains(t, err, testCase.field)
			require.ErrorContains(t, err, testCase.replacement)
			after, err := os.ReadFile(File)
			require.NoError(t, err)
			require.Equal(t, raw, after)
		})
	}
}

func TestConfigureCanRepairGHCRDexImage(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(File, []byte("clusterName: lab\ndexImage: ghcr.io/dexidp/dex:v2.45.1\n"), 0o600))

	configuration, err := LoadForConfigure()
	require.NoError(t, err)
	require.ErrorContains(t, configuration.Validate(), "gsoci.azurecr.io/giantswarm/dex:v2.45.1")
	configuration.DexImage = "gsoci.azurecr.io/giantswarm/dex:v2.45.1"
	require.NoError(t, configuration.Save())

	loaded, err := Load()
	require.NoError(t, err)
	require.Equal(t, configuration.DexImage, loaded.DexImage)
}

func TestDexImageValidation(t *testing.T) {
	for _, image := range []string{
		Default().DexImage,
		"gsoci.azurecr.io/giantswarm/dex@sha256:" + strings.Repeat("a", 64),
		"dex:dev",
		"localhost:5000/dex:dev",
	} {
		t.Run(image, func(t *testing.T) {
			configuration := Default()
			configuration.DexImage = image
			require.NoError(t, configuration.Validate())
		})
	}
	for _, image := range []string{"", "dex", "dex:bad tag"} {
		t.Run(image, func(t *testing.T) {
			configuration := Default()
			configuration.DexImage = image
			require.ErrorContains(t, configuration.Validate(), "dexImage")
		})
	}
}
