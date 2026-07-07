# Mixed-generation tape libraries: drive↔cartridge compatibility routing

Status: OPEN — problem confirmed on hardware (mhvtl road-test 2026-07-07); design
not started. This documents the failure and the constraints a fix must satisfy.

## The failure

A library with drives of different generations (the road-test rig: two LTO-8 and
two LTO-6 drives in one STK L700) fails a spanning run at roll time when the only
remaining writable cartridges are ones the *allocator's* drive cannot load:

```
error: … sink: medium "ltort" has no further writable bay (load or relabel more volumes):
mtx load 39 0: exit status 1: … Sense Key=Hardware Error … MOVE MEDIUM from … Failed
```

What happens today, piece by piece:

- The per-slot **skip works**: `advanceViaLibrary`'s scan treats a failed `Load`
  as "this cartridge holds nothing writable for this drive", marks the slot tried,
  and moves on (`eachLoadableSlot` → `loadErr` path). The run therefore fails
  *cleanly* — catalog consistent, rebuild byte-identical, prior archives
  restorable — with the last load error as the cause.
- But **an allocator is bound to one drive** (`Librarian.drive`, fixed at
  construction; `forDrive` siblings exist only for the multi-drive spool, each
  equally fixed). When drive 0 is LTO-8 and the remaining blanks are L6, the roll
  exhausts the slots and dies — while two idle LTO-6 drives sit in the same
  library, each perfectly able to continue the span.
- Nothing models generation at all: no config surface, no learned state, no
  selection bias. The only compatibility signal in the system is the load
  failure itself.

## Constraints for a design

1. **The load failure is the only reliable oracle.** Generation matrices
   (LTO-N reads N-2, writes N-1) could be configured or inferred from
   `lsscsi` model strings, but the honest primitive is "this drive refused this
   cartridge" — which the library already reports precisely and cheaply.
   Whatever we build should *learn* from failed loads rather than trust a table.
2. **Selection order is the librarian's** (`advanceViaLibrary`,
   `oldestReusable`): any fix lives there, not in the media layer — `mtx` did its
   job; the policy of *which drive to try next* is librarian policy.
3. **The spool owns drive leasing.** A mid-span drive change interacts with the
   worker↔drive binding (one archive writer per drive, rolls serialized on the
   orchestrator): an allocator that hops drives must not collide with a sibling
   allocator's lease.
4. **A cartridge loaded by a *reading* path has the same problem** —
   `MountForRead`/`findSlot` load into `l.drive` too; a restore needing an L6
   cartridge must pick an L6-capable drive.
5. Barcode suffixes (`…L6`, `…L8`) are a useful *hint* for ordering (try
   compatible-looking slots first) but not ground truth — sites relabel media, and
   non-LTO libraries have other schemes.

## Sketch (to be designed properly)

- The librarian learns per-(drive, cartridge-or-slot) refusals within a run
  (a `map[drive]map[barcode]bool` beside `tried`), and on a roll that exhausts
  slot candidates for its own drive, retries the refused candidates on the
  library's *other idle drives* before giving up — for the single-writer case
  a plain "move my binding to drive k" (re-binding `l.drive` between parts is
  safe: parts are drive-agnostic, positions are per-volume).
- For the spool, the roll crossing already serializes on the orchestrator; the
  drive lease would move with the allocator (release drive i, lease drive k).
- The failure message, when *no* drive can load *any* remaining cartridge,
  should say that ("no drive in the library can load the remaining writable
  cartridges") instead of surfacing the last mtx sense error.

## Non-goals

- Modeling generations explicitly in config (a `generations:` matrix) — the
  refusal oracle plus retry-on-other-drives covers it without new config.
- Write-compat vs read-compat distinction in v1 — a refused load refuses both.
