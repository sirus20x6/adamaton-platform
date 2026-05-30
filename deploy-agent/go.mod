module github.com/sirus20x6/adamaton-platform/deploy-agent

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/sirus20x6/adamaton-core v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/creack/pty v1.1.24 // indirect

// Per-module CI needs the replace; go.work covers local dev.
replace github.com/sirus20x6/adamaton-core => ../../core
