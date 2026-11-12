module github.com/tctien342/pig-cursor-sdk

go 1.26

// Pig resolves this requirement to the version-matched staged SDK at build time.
require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0

require (
	connectrpc.com/connect v1.21.1
	google.golang.org/protobuf v1.36.12
)
