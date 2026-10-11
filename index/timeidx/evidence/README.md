# PR #16 final evidence

`pr16-final.tar.gz` retains raw measurements and final validation output.
Baseline: `4c7847cbe4e96e9cfb91ac55c5c474c78bd3b63d`; candidate is the
implementation commit `28cd5363d31f23118fb04ab25d21a6948bb2d610` on `dev/timeidx`.

Extract into an empty directory with `tar -xzf pr16-final.tar.gz`.

- `timeidx-accepted-ab/`: final ten-independent-process interleaved A/B,
  benchstat, environment and source diff metadata.
- `timeidx-accepted-ScalePreparation.txt`: final ten samples for every
  scale/ID-distribution/batch combination.
- `timeidx-accepted-ScaleMatrix.txt`: final three-sample complete shard,
  durable-batch and chronological-insert matrix.
- `timeidx-final-baseline-*`: baseline phase, preparation and scale matrix.
- `timeidx-final-final-CommitComponents.txt`: directory phase diagnostics;
  later changes only affect mutation preparation.
- `timeidx-delete-single-*`: final ten-process single-ID preparation A/B.
- Other A/B files preserve intermediate investigations, including the
  historical regression that motivated the merge-loop CPU profiles (`*.cpu`).
- `timeidx-accepted-validation.txt`: command-by-command final checks and
  growth/rebuild test output.

Some `environment.txt` diff metadata reflects the uncommitted candidate on
reviewed HEAD, not a different implementation. See `../BENCHMARKS.md` for the
current measurement tables and candidate identity. All timings
are specific to the documented Apple M4 Pro / darwin-arm64 / Go 1.27.0 machine.
Short matrix samples are diagnostic and do not establish latency percentiles.
CPU profiles are investigative evidence, not independent benchmark samples.

Reproduce with `../benchmark_ab.sh` and the exact commands in the review.
Copy the two opt-in harness files to a detached baseline worktree; preserve
existing benchmarks in both trees. Do not run other tests concurrently with
performance measurements.
