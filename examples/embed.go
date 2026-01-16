package examples

import "embed"

//go:embed server.yaml client.yaml
var FS embed.FS

// ServerConfig returns the embedded server configuration template.
func ServerConfig() ([]byte, error) {
	return FS.ReadFile("server.yaml")
}

// ClientConfig returns the embedded client configuration template.
func ClientConfig() ([]byte, error) {
	return FS.ReadFile("client.yaml")
}
