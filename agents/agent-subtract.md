Make `pisafe` smaller and simpler without making it weaker: fewer concepts, states, branches, special cases, and decisions a future reader must make. Code removed is evidence, not the objective; simpler beats shorter.

Begin with a read-only audit. Propose independent reductions, naming what each removes, affected boundaries or behavior, any breakage, and verification. Wait for approval before implementation.

Think at two scales, and don't let the small crowd out the large:

1. **Local** — delete what is dead, duplicated, unreachable, stale, or needlessly flexible. Collapse N mechanisms that are really one idea.
2. **Systemic** — seek redesigns small refactors cannot reach: a data model that makes errors unrepresentable, a boundary redrawn so several mechanisms become one, or an assumption removed so branching disappears. Propose bold reductions, but do not mistake fewer packages for fewer concepts.

Building is a valid instrument of subtraction. Add an abstraction when it deletes more than it adds, absorbs special cases, or replaces ad-hoc mechanisms with one principled one. Never merely relocate complexity.

Treat documentation as testimony, not ground truth. `pisafe-design.md` records intended guarantees, `IMPLEMENTATION_PROGRESS.md` the believed implementation, and `DECISIONS.md` the reasoning believed to remain relevant; none proves its detailed claims. Investigate against code and tests, propose corrections, and never silently weaken a guarantee or retain complexity solely because it is documented. Keep comments only for contracts, constraints, or reasons the code cannot express.

Rules:

- Preserve the Mac/VM, controller/guest, trusted/untrusted, and staged/original boundaries. Redesign one only through an approved proposal showing equal or stronger guarantees.
- Change public behavior only through an approved proposal naming who could notice and why it is worthwhile. Repository evidence cannot prove nobody depends on it.
- Tests serve current promises. Remove one only with its behavior or replace it with clearer evidence; security, fail-closed, recovery, and acceptance tests are not cleanup targets.
- When Y replaces X, remove X, its obsolete tests, and stale documentation in the same increment.
- Keep the module dependency-free unless separately approved. Run ordinary tests and vet per increment; run stateful live tests only when necessary and explicitly approved.

Before finishing, ask: _what became obsolete, did all of it disappear, which documented claims became false, and what proves the result is no weaker?_
