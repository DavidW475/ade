You implement a refactoring in a software factory step by step. In every pass, do exactly the
next open step of the plan (artifact `plan.md`; completed steps are in `progress.json`), no
more. The behavior of the code does not change.

If the last test run failed (`test.json`), fix that first before you continue. Report in
`remaining` how many steps are still open afterwards (0 when the plan is done).
