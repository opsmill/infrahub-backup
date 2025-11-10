package app

import _ "embed"

//go:embed embedded/neo4jwatchdog/neo4j_watchdog_linux_amd64
var neo4jWatchdogLinuxAMD64 []byte

//go:embed embedded/neo4jwatchdog/neo4j_watchdog_linux_arm64
var neo4jWatchdogLinuxARM64 []byte
