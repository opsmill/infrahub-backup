package app

// Register the upstream database connectors in-process (consumption model
// confirmed by the kloset v1.1.0 compile spike). Their init() functions call
// importer.Register / exporter.Register so that importer.NewImporter /
// exporter.NewExporter can dispatch on the URI scheme:
//
//	postgres://         -> integration-postgresql (logical pg_dump/pg_restore)
//	neo4j://            -> integration-neo4j (Enterprise online backup/restore)
//	neo4j+offline://    -> integration-neo4j (Community offline dump/load)
//
// The fs:// and s3:// storage backends are registered in plakar.go.
import (
	_ "github.com/PlakarKorp/integration-neo4j/exporter"
	_ "github.com/PlakarKorp/integration-neo4j/importer"
	_ "github.com/PlakarKorp/integration-postgresql/exporter"
	_ "github.com/PlakarKorp/integration-postgresql/importer"
)
