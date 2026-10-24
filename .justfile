# This file is the project's own — add recipes below. Keep the import: it
# mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# The binary links Virtualization.framework through forkcloser/vz: cgo is
# mandatory, and the build is darwin/arm64 only.
export GO_CGO := '1'
# The baseline builds with the osusergo tag, the pure-Go os/user that reads
# /etc/passwd; on macOS users live in Open Directory, which only libc's
# getpwnam reaches, and the guest agent runs commands as the provisioned
# user. Both binaries are darwin-only, so the tag list is netgo alone: a
# later -tags wins, which is what BUILD_GO_FLAGS is for.
export BUILD_GO_FLAGS := '-tags=netgo'

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::go::default do::lint::go::deadcode do::lint::default
fix: do::fix::go::default do::fix::default
test: do::test::go::unit do::test::go::race
# The security workflow runs `just security`.
security: do::security::default

# The binary is finished under a scratch name and swapped in with ONE rename:
# macOS kills a running process whose mapped code changes under it, and both
# go's cached-link copy and codesign rewrite in place. Only a signed binary with
# the virtualization entitlement may create a VM.
build:
    #!/usr/bin/env bash
    set -euo pipefail
    rm -f .build/zerogravity .build/zerogravity.new .build/zerogravity-agent
    just do build go
    cp .build/zerogravity .build/zerogravity.new
    codesign --force --sign - --timestamp=none --entitlements vz.entitlements .build/zerogravity.new
    mv -f .build/zerogravity.new .build/zerogravity

# Regenerate the Connect tree from proto/agent.proto into internal/agentrpc,
# imported by the host client (pkg/agent) and the guest server
# (cmd/zerogravity-agent) alike, so there is no copy to keep in sync.
# protoc-gen-connect-go is built from the root module, so it is the connect
# version go.mod requires for the runtime and cannot skew from it; a generator
# dependency outside that graph would fail this build loudly under
# -mod=readonly. Built here rather than installed, so nothing leaks onto the
# hermetic PATH. (limen keeps tool directives out of the root go.mod.)
proto:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p .build internal/agentrpc
    go build -o .build/protoc-gen-connect-go connectrpc.com/connect/v2/cmd/protoc-gen-connect-go
    protoc -I proto \
      --plugin=protoc-gen-connect-go=.build/protoc-gen-connect-go \
      --go_out=internal/agentrpc --go_opt=paths=source_relative \
      --connect-go_out=internal/agentrpc --connect-go_opt=paths=source_relative \
      proto/agent.proto
    just do fix go
