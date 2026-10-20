// Package protocol holds what host and guest must agree on without talking:
// the vsock port the agent listens on and the revision of the contract. Never
// copy one into each side.
package protocol

// Port is the guest vsock port the agent serves on. Above 1024, where the
// framework reserves nothing.
const Port uint32 = 1960

// Revision changes when proto/agent.proto changes incompatibly; the host
// refuses an agent speaking another.
const Revision = "1"
