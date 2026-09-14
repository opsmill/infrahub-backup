package app

// Register the upstream database connectors in-process (consumption model
// confirmed by the kloset v1.1.0 compile spike). Their init() functions call
// importer.Register / exporter.Register so that importer.NewImporter /
// exporter.NewExporter can dispatch on the URI scheme:
//
//	postgres://         -> integration-postgresql (logical pg_dump/pg_restore)
//	neo4j://            -> plakar-integration-neo4j (Enterprise online backup/restore)
//	neo4j+offline://    -> plakar-integration-neo4j (Community offline dump/load)
//
// The Neo4j integration is OpsMill-maintained and consumed as an ordinary tagged
// module (github.com/opsmill/plakar-integration-neo4j); its SDK plugin binaries
// exist for external `plakar pkg` users and are irrelevant here, since these
// blank imports register the same code in-process.
//
// The fs:// and s3:// storage backends are registered in plakar.go.
import (
	_ "github.com/PlakarKorp/integration-postgresql/exporter"
	_ "github.com/PlakarKorp/integration-postgresql/importer"
	_ "github.com/opsmill/plakar-integration-neo4j/exporter"
	_ "github.com/opsmill/plakar-integration-neo4j/importer"
)
