// Package presencebroker holds the tool-agnostic presence broker core:
// projecting an already-normalized instance registry into the read-only
// artifacts external consumers observe (canonical snapshot file, v2
// presentation feed).
//
// This package is deliberately isolated:
//
//   - it imports no Claude-, Codex-, or Grok-specific package (no hooks, no
//     trust observers, no process recognition, no runtime pollers);
//   - it never decides which tool an instance belongs to, and it never
//     branches on Tool for behavior — Tool is data it passes through, never
//     a switch;
//   - it never observes processes, hooks, or any tool signal itself. It
//     only consumes an already-normalized instanceregistry.Registry (or any
//     type satisfying its small source interfaces) and publishes what that
//     registry already computed.
//
// Everything that decides *how* a Claude, Codex, or Grok signal becomes a
// normalized mutation call — hook binding, trust observation, process
// recognition, startup-pending rules — stays in cmd/aurora-presence-local-server
// and its tool-specific packages (claudehook, codexhook, grokpresence,
// claudetrust, codextrust, runtimerecognition, runtimepresence) until each
// producer is extracted in a later step. Only code that was already fully
// generic moved here: nothing in this package changed behavior when it
// moved.
package presencebroker
