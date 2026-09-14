# Saved perf baselines

One TSV per machine, named after the L0 kernel release and the CPU model
-- the environment the measurement was taken in, not the kernel being
measured.  `make perf-matrix` (batteries/perf-matrix.pvm, through
tools/pvmtest) compares a fresh sweep against the file matching the machine
it is running on, and fails when a PVM timing is more than `threshold`
percent worse than the recorded median and outside the baseline's own
range.  `pvmtest compare <matrix.tsv> <baseline.tsv>` does the same for any
two files.

Absolute thresholds are not useful here: the same tree is slower on a
laptop and faster on a workstation, and neither is a regression.  What is
worth failing on is *this* machine getting worse than it was.

The kernel the baseline was taken from is recorded in the file header, so
a baseline can be read as "this is what commit X measured here".  Record a
new one with `make perf-baseline` after deliberately changing something
that moves the numbers -- and say in the commit message why.

## On the threshold

The default is 15%.  A single run of the perf suite inside two layers of
virtualisation moves more than that is comfortable: measured back to back
on an idle machine at `MATRIX_REPS=1`, fork-exec swung +12.7% and
parallel-fault -10.8% between two runs of the *same* build.  The median
of five is what makes 15% mean something; do not lower the threshold
without raising the repetition count first, or the matrix will cry wolf.
