// Package codexproducer holds the Codex-specific core reused by the
// standalone cmd/aurora-codex-presence shadow producer: explicit CODEX_HOME
// source isolation, Codex process recognition (reusing internal/codexhook's
// recognizer and internal/runtimerecognition's engine), a Codex-only
// idle/working/attention state machine driven exclusively by observed Codex
// hook events (never by trust configuration), opaque instance/epoch/revision
// identity, and the hook-session-to-process correlation needed to keep
// several parallel Codex instances apart.
//
// This package never imports internal/instanceregistry, internal/presencebroker,
// or internal/presencev2: it produces normalized producerprotocol.Message
// values only. Storage, leases, revision-conflict arbitration, and
// presentation belong to the broker, treated as an external system. It also
// never imports internal/claudehook or internal/grokpresence (or any other
// tool's hook package): this producer speaks only Codex.
//
// State-derivation policy (the G.4 false-red fix): this package never
// consults internal/codextrust or any other trust/config source to decide
// idle vs. attention. A newly recognized Codex process is always idle until
// a real hook event says otherwise; attention is set only by an observed
// PermissionRequest hook event. This is the same decision
// internal/runtimepresence.RegistrySync (the monolith's registry path, still
// the ESP's live status source) now makes for the identical reason: both
// defer to the single shared internal/codexhook.CodexStartupAttention
// function (currently always false) rather than each independently deciding
// what counts as Codex startup attention, so the two can never diverge on
// this point over time — see that function's doc comment for the full
// rationale, and internal/runtimepresence/registry_sync.go's
// startupAtRegister for the monolith side of the same contract. The real
// Codex startup trust question ("Do you trust this folder?") has no
// reliable observed signal in the current hook event set and is therefore
// left unsupported on both sides rather than re-derived from config — see
// the package notes in state.go.
package codexproducer
