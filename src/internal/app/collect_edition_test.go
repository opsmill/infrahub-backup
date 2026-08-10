package app

import (
	"errors"
	"testing"
)

func TestClassifyInfrahubEdition(t *testing.T) {
	cases := []struct {
		reference string
		want      string
	}{
		// Community images, various registries/tags/digests.
		{"opsmill/infrahub:stable", infrahubEditionCommunity},
		{"registry.opsmill.io/opsmill/infrahub:1.5.2", infrahubEditionCommunity},
		{"infrahub", infrahubEditionCommunity},
		{"opsmill/infrahub@sha256:deadbeef", infrahubEditionCommunity},
		{"registry:5000/opsmill/infrahub:1.5.2", infrahubEditionCommunity},
		// Enterprise images.
		{"opsmill/infrahub-enterprise:stable", infrahubEditionEnterprise},
		{"registry.opsmill.io/opsmill/infrahub-enterprise:1.5.2", infrahubEditionEnterprise},
		{"infrahub-enterprise", infrahubEditionEnterprise},
		{"registry:5000/opsmill/infrahub-enterprise@sha256:abc", infrahubEditionEnterprise},
		// Helm chart names use the same signal.
		{"infrahub", infrahubEditionCommunity},
		{"infrahub-enterprise", infrahubEditionEnterprise},
		// Unrecognized references stay unknown rather than being guessed.
		{"", ""},
		{"opsmill/infrahub-enterprise-custom:1.0", ""},
		{"opsmill/some-other-image:1.0", ""},
		{"neo4j:5.20-enterprise", ""},
	}

	for _, tc := range cases {
		if got := classifyInfrahubEdition(tc.reference); got != tc.want {
			t.Errorf("classifyInfrahubEdition(%q) = %q, want %q", tc.reference, got, tc.want)
		}
	}
}

func TestImageRepository(t *testing.T) {
	cases := []struct {
		reference string
		want      string
	}{
		{"registry.opsmill.io/opsmill/infrahub-enterprise:1.5.2", "infrahub-enterprise"},
		{"opsmill/infrahub@sha256:deadbeef", "infrahub"},
		{"registry:5000/opsmill/infrahub:1.5.2", "infrahub"},
		{"infrahub", "infrahub"},
		{"  opsmill/infrahub:stable  ", "infrahub"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := imageRepository(tc.reference); got != tc.want {
			t.Errorf("imageRepository(%q) = %q, want %q", tc.reference, got, tc.want)
		}
	}
}

// editionDetectorBackend stands in for the Docker backend in populateEdition
// tests: it reports an edition directly via the editionDetector capability.
type editionDetectorBackend struct {
	bareBackend
	edition string
	err     error
}

func (e *editionDetectorBackend) InfrahubEdition() (string, error) {
	return e.edition, e.err
}

var _ editionDetector = (*editionDetectorBackend)(nil)

func TestPopulateEdition(t *testing.T) {
	t.Run("docker: detector edition is recorded", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "docker", 100)
		populateEdition(&editionDetectorBackend{
			bareBackend: bareBackend{name: "docker"},
			edition:     infrahubEditionEnterprise,
		}, manifest)
		if manifest.Edition != infrahubEditionEnterprise {
			t.Errorf("Edition = %q, want %q", manifest.Edition, infrahubEditionEnterprise)
		}
	})

	t.Run("docker: an unknown edition leaves the field unset", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "docker", 100)
		populateEdition(&editionDetectorBackend{
			bareBackend: bareBackend{name: "docker"},
			edition:     "",
		}, manifest)
		if manifest.Edition != "" {
			t.Errorf("Edition = %q, want empty when detection is inconclusive", manifest.Edition)
		}
	})

	t.Run("docker: a detection error leaves the field unset without failing", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "docker", 100)
		populateEdition(&editionDetectorBackend{
			bareBackend: bareBackend{name: "docker"},
			err:         errors.New("docker daemon unreachable"),
		}, manifest)
		if manifest.Edition != "" {
			t.Errorf("Edition = %q, want empty when detection errored", manifest.Edition)
		}
	})

	t.Run("kubernetes: edition falls back to the Helm chart name", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "kubernetes", 100)
		manifest.Helm = &HelmRelease{ReleaseName: "infrahub", Chart: "infrahub-enterprise", ChartVersion: "1.2.3"}
		populateEdition(&bareBackend{name: "kubernetes"}, manifest)
		if manifest.Edition != infrahubEditionEnterprise {
			t.Errorf("Edition = %q, want %q from the Helm chart", manifest.Edition, infrahubEditionEnterprise)
		}
	})

	t.Run("no detector and no Helm metadata leaves the field unset", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "docker", 100)
		populateEdition(&bareBackend{name: "docker"}, manifest)
		if manifest.Edition != "" {
			t.Errorf("Edition = %q, want empty when nothing can report it", manifest.Edition)
		}
	})
}

func TestEditionFromContainers(t *testing.T) {
	cases := []struct {
		name       string
		containers []composePSContainer
		want       string
	}{
		{
			name: "enterprise infrahub-server image",
			containers: []composePSContainer{
				{Service: "database", Image: "neo4j:5.20-enterprise"},
				{Service: "infrahub-server", Image: "registry.opsmill.io/opsmill/infrahub-enterprise:1.5.2"},
			},
			want: infrahubEditionEnterprise,
		},
		{
			name: "community infrahub-server image",
			containers: []composePSContainer{
				{Service: "infrahub-server", Image: "opsmill/infrahub:stable"},
			},
			want: infrahubEditionCommunity,
		},
		{
			name:       "service absent",
			containers: []composePSContainer{{Service: "cache", Image: "redis:7"}},
			want:       "",
		},
		{
			name:       "service present but image empty",
			containers: []composePSContainer{{Service: "infrahub-server", Image: ""}},
			want:       "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := editionFromContainers("infrahub-server", tc.containers); got != tc.want {
				t.Errorf("editionFromContainers() = %q, want %q", got, tc.want)
			}
		})
	}
}
