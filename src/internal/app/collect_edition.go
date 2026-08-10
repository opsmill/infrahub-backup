package app

import (
	"strings"

	"github.com/sirupsen/logrus"
)

// Infrahub edition identifiers recorded in the bundle manifest. These are the
// Infrahub product editions, distinct from the Neo4j edition tracked by the
// backup metadata.
const (
	infrahubEditionCommunity  = "community"
	infrahubEditionEnterprise = "enterprise"
)

// editionDetector is an optional backend capability: report the Infrahub
// edition (community or enterprise) of the running deployment for the bundle
// manifest. The Docker backend implements it by classifying the infrahub-server
// container image; on Kubernetes the edition is derived from the Helm chart name
// instead (see populateEdition), so the Kubernetes backend does not implement
// this interface.
type editionDetector interface {
	// InfrahubEdition returns the detected edition, or "" when it cannot be
	// determined (e.g. the server is absent or its image is unrecognized).
	InfrahubEdition() (string, error)
}

// populateEdition best-effort fills the manifest's edition field. It prefers a
// backend that can report the edition directly (Docker, via the infrahub-server
// image), and otherwise falls back to the Helm chart name already resolved into
// the manifest (Kubernetes). Edition is deployment provenance, not a collector,
// so a detection failure is logged at debug and simply leaves the field unset
// rather than being recorded as a collector outcome.
func populateEdition(backend EnvironmentBackend, manifest *BundleManifest) {
	if detector, ok := backend.(editionDetector); ok {
		edition, err := detector.InfrahubEdition()
		if err != nil {
			logrus.Debugf("Could not detect Infrahub edition: %v", err)
		} else if edition != "" {
			manifest.Edition = edition
			return
		}
	}

	// Kubernetes: the Helm chart name (infrahub / infrahub-enterprise) is the
	// same signal, already fetched into the manifest by populateHelmRelease.
	if manifest.Helm != nil {
		if edition := classifyInfrahubEdition(manifest.Helm.Chart); edition != "" {
			manifest.Edition = edition
		}
	}
}

// classifyInfrahubEdition maps an infrahub-server image reference — or a Helm
// chart name — to the Infrahub edition it identifies. The enterprise artifact is
// published as ".../infrahub-enterprise[:tag]" and the community one as
// ".../infrahub[:tag]", so the final repository segment is the signal; a
// mirrored or privately-tagged image keeps that segment. The match is exact so
// an unrecognized artifact yields "" (edition unknown) rather than being
// misclassified.
func classifyInfrahubEdition(reference string) string {
	switch imageRepository(reference) {
	case "infrahub-enterprise":
		return infrahubEditionEnterprise
	case "infrahub":
		return infrahubEditionCommunity
	default:
		return ""
	}
}

// imageRepository extracts the final repository segment of a container image
// reference (or bare name), dropping the registry host, any tag, and any
// digest: "registry.opsmill.io/opsmill/infrahub-enterprise:1.5.2" and
// "opsmill/infrahub@sha256:..." reduce to "infrahub-enterprise" and "infrahub".
func imageRepository(reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ""
	}
	// Drop a digest suffix (@sha256:...) before anything else.
	if at := strings.IndexByte(reference, '@'); at >= 0 {
		reference = reference[:at]
	}
	// The repository name is the final path segment; splitting on '/' first
	// ensures a registry "host:port" colon is never mistaken for a tag colon.
	if slash := strings.LastIndexByte(reference, '/'); slash >= 0 {
		reference = reference[slash+1:]
	}
	if colon := strings.IndexByte(reference, ':'); colon >= 0 {
		reference = reference[:colon]
	}
	return reference
}
