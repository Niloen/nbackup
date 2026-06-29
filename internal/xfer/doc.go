// Package xfer is NBackup's data-movement primitive: Transfer(source, filters,
// sink) moves one byte stream through three zones and tags any fault with the
// zone it came from.
//
// The three zones map onto the three hosts that can ever be involved in a
// backup or restore:
//
//   - Source  — the producer's host: a client's tar (encode) or a medium read.
//   - Filters — the local server: the compress/encrypt or decrypt/decompress
//     chain. Pinned to the server on purpose; a transform never runs on a third
//     remote host (client-side transforms ride in the Source, target-side ones
//     in the Sink).
//   - Sink    — the consumer's host: a medium, a target's tar, or a hash.
//
// Backup, copy, restore, verify and drill are all the same Transfer with
// different endpoints. The heavy transforms (compression, encryption) are run
// as external child processes (see package programs), so nb stays a thin
// orchestrator and there is one data path for every operation.
//
// The placement rule that decides whether a transform runs fused with the
// endpoint tar or in the local Filters lives in split.go, shared by both
// directions so they cannot drift.
package xfer
