# Refactor and phase history

This directory records completed implementation phases, benchmark evidence, PR
bodies, and the active V1 stabilization plan. It is historical/working material,
not public API documentation.

## Active V1 work

| Document | Status | Notes |
|---|---|---|
| [`FinalV1.md`](FinalV1.md) | Active plan | V1 stabilization instructions |
| [`V1-API-Decision.md`](V1-API-Decision.md) | Active decision record | Stable/advanced/experimental API inventory and unresolved gates |
| [`Phase3-Writer-Protocol.md`](Phase3-Writer-Protocol.md) | Current architecture | Serialized writer and lock-free query snapshot decision |

## Completed Phase 3

Phase 3 is implemented on PR #27 (`phase3-3.1`). The phase commit is:

- 3.2 — cost-aware planning
- 3.3 — lock-free coherent query snapshots
- 3.4 — full unified-index recovery fallback
- 3.5 — explicit database checkpoint lifecycle hook

The following are historical evidence:

- [`Phase3-Baseline-Report.md`](Phase3-Baseline-Report.md) — baseline before the
  Phase 3 planner/snapshot changes. Its “no composite read root” statement is
  superseded by PR #27; the measured baseline remains useful for V1 comparison.
- [`Phase3.md`](Phase3.md) — original Phase 3 instructions.
- `pr-phase3-*.md` — archived PR descriptions.

## Completed Phase 2

- [`Phase2.md`](Phase2.md), [`Phase2-Report.md`](Phase2-Report.md)
- [`Phase2-Followup.md`](Phase2-Followup.md),
  [`Phase2-Followup-Design.md`](Phase2-Followup-Design.md),
  [`Phase2-Followup-Report.md`](Phase2-Followup-Report.md),
  [`Phase2-Followup-Status.md`](Phase2-Followup-Status.md)
- `pr-phase2-*.md`

Phase 2 merged in PR #25 at `6fd50b7`.

## Completed Phase 1

- [`Phase1.md`](Phase1.md), [`Phase1-Report.md`](Phase1-Report.md)
- [`Phase1_Followup.md`](Phase1_Followup.md),
  [`Phase1-Followup-Report.md`](Phase1-Followup-Report.md)

## Benchmarks

`bench/` contains historical raw benchmark output. Use the current harness in
`internal/bench` and the package benchmarks described in
[`AGENTS/Bench.MD`](../Bench.MD). Do not treat saved text files as a current
benchstat result without rerunning and recording the commit and environment.
